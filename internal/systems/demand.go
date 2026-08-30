package systems

import (
	"github.com/mirror-sim/mirror/internal/events"
	"github.com/mirror-sim/mirror/internal/rng"
	"github.com/mirror-sim/mirror/internal/state"
	"github.com/mirror-sim/mirror/internal/units"
	"github.com/mirror-sim/mirror/internal/world"
)

// Departures releases agents whose scheduled departure is this tick.
//
// Agents are bucketed by simulated minute at index-build time, so this scans
// ~population/1440 candidates per tick instead of the whole population. At
// 200k agents that is 140 comparisons per tick rather than 200,000 -- the
// difference between the demand system being invisible in a profile and being
// the single biggest cost in the engine.
func Departures(c *Ctx, r *Region) {
	s := c.S
	tod := s.TickOfDay()
	minute := tod / units.TicksPerMinute
	if int(minute) >= len(r.DepartOut) {
		return
	}
	for _, ag := range r.DepartOut[minute] {
		if s.Agents.DepartOut[ag] != tod || s.Agents.Status[ag] != state.AtHome {
			continue
		}
		startTrip(c, r, ag, s.Agents.HomeNode[ag], s.Agents.WorkNode[ag], state.Commuting)
	}
	for _, ag := range r.DepartRet[minute] {
		if s.Agents.DepartRet[ag] != tod || s.Agents.Status[ag] != state.AtWork {
			continue
		}
		startTrip(c, r, ag, s.Agents.WorkNode[ag], s.Agents.HomeNode[ag], state.Returning)
	}
}

func startTrip(c *Ctx, r *Region, ag int32, from, to world.NodeID, st state.AgentStatus) {
	m, s := c.Map, c.S
	a := &s.Agents
	a.TripStart[ag] = uint32(c.Tick)
	a.CurrentTarget[ag] = to
	a.Status[ag] = st
	a.WaitingTicks[ag] = 0

	mode := a.Mode[ag]
	// Adaptive mode switching: an agent whose last car trip ran badly over the
	// free-flow reference will try transit next time, and vice versa. This one
	// rule is what turns congestion into a feedback loop rather than a
	// one-way ratchet -- without it, no policy that improves transit can ever
	// pull cars off the road.
	if a.TripsDone[ag] > 0 && a.TRoute[ag] >= 0 {
		g := rng.Derive(s.Seed, rng.StreamDeparture, uint64(c.Tick), uint64(ag))
		ref := a.FreeRefTicks[ag]
		if ref > 0 {
			ratio := int32(int64(a.LastTravel[ag]) * 1000 / int64(ref))
			switch {
			case mode == state.ModeCar && ratio > 1600 && g.Chance(350):
				mode = state.ModeTransit
			case mode == state.ModeTransit && ratio < 1150 && g.Chance(200):
				mode = state.ModeCar
			}
			a.Mode[ag] = mode
		}
	}

	switch mode {
	case state.ModeWalk:
		d := dist(m, from, to)
		a.WalkTicks[ag] = WalkTicksFor(d)
		a.WaitingTicks[ag] = 0
		a.FreeRefTicks[ag] = a.WalkTicks[ag]
		r.intent(IntentStartWalk, int64(ag), 0, 0, 0)

	case state.ModeTransit:
		rt := a.TRoute[ag]
		if rt < 0 {
			startCarTrip(c, r, ag, from, to)
			return
		}
		board, alight := int32(a.TBoard[ag]), int32(a.TAlight[ag])
		if st == state.Returning {
			board, alight = alight, board
		}
		route := &m.Routes[rt]
		if int(board) >= len(route.Stops) || int(alight) >= len(route.Stops) {
			startCarTrip(c, r, ag, from, to)
			return
		}
		// Walk to the boarding stop first; the agent joins the stop queue when
		// the walk finishes.
		a.WalkTicks[ag] = WalkTicksFor(dist(m, from, route.Stops[board]))
		a.TBoard[ag], a.TAlight[ag] = int16(board), int16(alight)
		var ride int32
		lo, hi := board, alight
		if lo > hi {
			lo, hi = hi, lo
		}
		for k := lo; k < hi && int(k) < len(route.Legs); k++ {
			for _, e := range route.Legs[k] {
				ride += m.EdgeTravelTicksFree(e)
			}
		}
		a.FreeRefTicks[ag] = a.WalkTicks[ag] + ride + route.HeadwayTick/2 +
			WalkTicksFor(dist(m, route.Stops[alight], to))
		r.intent(IntentStartWalk, int64(ag), 0, 0, 0)

	default:
		startCarTrip(c, r, ag, from, to)
	}
	r.TripsStarted++
	r.emit(c.Tick, events.EvtTripStarted, events.SevInfo,
		int64(ag), int64(a.Mode[ag]), int64(from), int64(to))
}

// startCarTrip plans a route and stages a spawn intent.
func startCarTrip(c *Ctx, r *Region, ag int32, from, to world.NodeID) {
	m, s := c.Map, c.S
	a := &s.Agents
	a.Mode[ag] = state.ModeCar
	if from == to {
		a.Status[ag] = arrivedStatus(a.Status[ag])
		return
	}
	g := rng.Derive(s.Seed, rng.StreamRouting, uint64(c.Tick), uint64(ag))
	aware := g.Chance(s.Policy.RerouteAwarenessP)
	r.RouteQueries++
	path, ok := r.Router.Route(m, from, to, CostFn(c, aware, false), r.Path[:0])
	r.Path = path
	if !ok || len(path) == 0 {
		r.RouteFails++
		a.Status[ag] = state.Stranded
		r.emit(c.Tick, events.EvtVehicleStranded, events.SevWarning, -1, int64(ag), int64(from), int64(to))
		return
	}
	var ref int32
	for _, e := range path {
		ref += m.EdgeTravelTicksFree(e)
	}
	a.FreeRefTicks[ag] = ref
	start := int32(len(r.PathArena))
	r.PathArena = append(r.PathArena, path...)
	r.intent(IntentSpawnCar, int64(ag), int64(start), int64(len(path)), int64(to))
}

func arrivedStatus(st state.AgentStatus) state.AgentStatus {
	if st == state.Returning {
		return state.AtHome
	}
	return state.AtWork
}

// Walkers advances walking legs and hands transit users to their stop queue.
func Walkers(c *Ctx, r *Region) {
	s := c.S
	a := &s.Agents
	for _, ag := range r.Walkers {
		if a.WalkTicks[ag] > 0 {
			a.WalkTicks[ag]--
			continue
		}
		r.intent(IntentEndWalk, int64(ag), 0, 0, 0)
	}
}

// ------------------------------------------------------------ rerouting ---

// Reroute lets informed drivers reconsider their path when the road ahead has
// deteriorated.
//
// Three guards keep this from being both slow and wrong:
//
//  1. Only vehicles past their ReplanAt tick are considered, which spreads the
//     cost and prevents route oscillation.
//  2. Only drivers who rolled "informed" at departure participate.
//  3. A reroute is only adopted if it saves more than the agent's patience
//     threshold. Adopting every marginal improvement makes the whole fleet
//     flip together, which produces a traffic wave that bounces between two
//     corridors forever -- a real and well-documented failure of naive
//     dynamic assignment.
func Reroute(c *Ctx, r *Region) {
	m, s := c.Map, c.S
	v := &s.Vehicles
	tick := uint32(c.Tick)
	cost := CostFn(c, true, false)
	for _, id := range r.Vehicles {
		if v.Kind[id] != state.VehCar || v.Status[id] == state.VehIdle {
			continue
		}
		if v.ReplanAt[id] > tick {
			continue
		}
		ag := v.Agent[id]
		if ag < 0 {
			continue
		}
		route := s.Route(id)
		ri := v.RouteIdx[id]
		if int(ri) >= len(route) {
			continue
		}
		remaining := route[ri:]
		// Current perceived cost of the plan we are on.
		var cur int32
		blocked := false
		for _, e := range remaining {
			ce := cost(e)
			if ce >= world.Impassable {
				blocked = true
				break
			}
			cur += ce
		}
		g := rng.Derive(s.Seed, rng.StreamDriver, uint64(c.Tick), uint64(id))
		informed := g.Chance(s.Policy.RerouteAwarenessP)
		if !blocked && !informed {
			v.ReplanAt[id] = tick + uint32(g.Range(20, 60)*units.TicksPerSecond)
			continue
		}
		r.RouteQueries++
		from := m.Edges[v.Edge[id]].To
		alt, ok := r.Router.Route(m, from, v.Dest[id], cost, r.Path[:0])
		r.Path = alt
		if !ok || len(alt) == 0 {
			r.RouteFails++
			v.ReplanAt[id] = tick + uint32(30*units.TicksPerSecond)
			if blocked {
				r.intent(IntentStranded, int64(id), 0, 0, 0)
			}
			continue
		}
		var altCost int32
		for _, e := range alt {
			altCost += cost(e)
		}
		threshold := int32(s.Agents.PatienceP[ag]) * units.TicksPerSecond / 40
		if !blocked && altCost+threshold >= cur {
			v.ReplanAt[id] = tick + uint32(g.Range(25, 75)*units.TicksPerSecond)
			continue
		}
		start := int32(len(r.PathArena))
		r.PathArena = append(r.PathArena, alt...)
		r.intent(IntentInstallRoute, int64(id), int64(start), int64(len(alt)), 0)
		v.ReplanAt[id] = tick + uint32(g.Range(30, 90)*units.TicksPerSecond)
		r.Reroutes++
		r.emit(c.Tick, events.EvtVehicleRerouted, events.SevInfo,
			int64(id), int64(v.Edge[id]), int64(altCost), int64(cur))
	}
}

// Rate limiting note.
//
// An earlier version of Reroute carried a per-region A* budget. That was a
// determinism bug: the budget was a function of how many vehicles a region
// happened to own, so a four-region run replanned a different set of vehicles
// than a one-region run and the two diverged within a few hundred ticks.
//
// Rate limiting now lives entirely in per-vehicle state -- ReplanAt, jittered
// by vehicle id -- which is invariant under partitioning. The bound is just as
// real: with replan intervals drawn from 20-90 simulated seconds at most a
// fraction of a percent of the fleet replans on any given tick, and a mass
// closure spreads its replans over 37 ticks instead of landing them all on
// one.
