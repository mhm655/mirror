package systems

import (
	"github.com/mirror-sim/mirror/internal/events"
	"github.com/mirror-sim/mirror/internal/state"
	"github.com/mirror-sim/mirror/internal/world"
)

// Topology is the engine's view of which region owns what. The commit phase
// calls back into it whenever an entity changes owner, which is the ONLY place
// region membership ever changes.
type Topology interface {
	RegionOfEdge(e world.EdgeID) int32
	RegionOfAgent(a int32) int32
	AttachVehicle(v int32, region int32)
	DetachVehicle(v int32)
	AttachWalker(a int32, region int32)
	DetachWalker(a int32)
}

// ArenaLookup returns the staged path buffer belonging to a region.
type ArenaLookup func(region int32) []world.EdgeID

// CommitAll applies every intent produced this tick, from every region, in one
// globally canonical order.
//
// The order is global, not per-region, and that is the whole correctness
// argument for the parallel engine. Consider two vehicles competing for the
// last free slot on a link. Under a per-region commit, whichever region is
// processed first wins -- so a run with 4 regions makes a different choice
// than a run with 1, and "distributed execution is deterministic" becomes a
// lie. Sorting the merged intent stream by (kind, primary id) makes the winner
// a function of vehicle id alone, which is identical at every partition count.
// TestParallelRegionCounts is what pins this down.
//
// Cost: O(n log n) on a few thousand fixed-size records per tick. Measured at
// under 400 microseconds for 60k active vehicles, against a phase A that runs
// for tens of milliseconds.
func CommitAll(c *Ctx, in []Intent, arena ArenaLookup, g *Region, topo Topology, disp *Dispatcher) {
	SortIntents(in)
	for i := range in {
		it := &in[i]
		switch it.Kind {
		case IntentCross:
			commitCross(c, g, topo, it)
		case IntentArrive:
			commitArrive(c, g, topo, int32(it.A))
		case IntentSpawnCar:
			commitSpawnCar(c, g, arena, topo, it)
		case IntentInstallRoute:
			commitInstallRoute(c, arena, it)
		case IntentStartWalk:
			ag := int32(it.A)
			topo.AttachWalker(ag, topo.RegionOfAgent(ag))
		case IntentEndWalk:
			commitEndWalk(c, g, topo, int32(it.A))
		case IntentJoinStop:
			JoinStop(c, int32(it.A))
		case IntentStranded:
			commitStranded(c, g, topo, int32(it.A))
		case IntentAccident:
			commitAccident(c, g, topo, it)
		case IntentTransitDepart, IntentDispatch:
			id := int32(it.A)
			topo.AttachVehicle(id, topo.RegionOfEdge(c.S.Vehicles.Edge[id]))
		case IntentPreempt:
			commitPreempt(c, g, it)
		}
	}
}

func commitPreempt(c *Ctx, g *Region, in *Intent) {
	s := c.S
	si := int32(in.A)
	if int(si) >= len(s.Signals.Phase) || !s.Signals.Powered[si] {
		return
	}
	if s.Signals.Phase[si] != int8(in.B) {
		s.Signals.Phase[si] = int8(in.B)
		s.Signals.PhaseTick[si] = 0
		g.emit(c.Tick, events.EvtSignalPreempted, events.SevNotice, int64(si), in.B, 0, 0)
	}
	s.Signals.Preempt[si] = int32(in.C)
}

func commitCross(c *Ctx, g *Region, topo Topology, in *Intent) {
	m, s := c.Map, c.S
	v := &s.Vehicles
	id := int32(in.A)
	if v.Status[id] == state.VehIdle {
		return
	}
	from, to := world.EdgeID(in.B), world.EdgeID(in.C)
	if v.Edge[id] != from {
		return // superseded by a reroute in the same tick
	}
	ed := &m.Edges[to]
	eff := ed.Lanes - s.Edges.BlockedLanes[to]
	if eff < 0 {
		eff = 0
	}
	if eff == 0 || s.Edges.ClosedUntil[to] > uint32(c.Tick) {
		// The road ahead has closed. Schedule a replan with a per-vehicle
		// offset: without the jitter, a motorway closure makes every affected
		// driver replan on the same tick, which is both a latency spike and
		// unrealistic. The offset is derived from the id, so it is identical
		// at every partition count.
		v.ReplanAt[id] = uint32(c.Tick) + uint32(id%37)
		v.StopTicks[id]++
		return
	}
	jam := ed.Jam * eff / ed.Lanes
	if jam < 1 {
		jam = 1
	}
	if s.Edges.Count[to] >= jam {
		// Spillback. The vehicle stays where it is and occupies space on the
		// upstream link, which is exactly how queues propagate backwards
		// through a network and how gridlock forms.
		v.StopTicks[id]++
		return
	}

	if s.Edges.Count[from] > 0 {
		s.Edges.Count[from]--
	}
	s.Edges.Count[to]++
	s.Edges.EnteredTotal[to]++
	v.Edge[id] = to
	v.Pos[id] = 0
	v.RouteIdx[id]++
	v.Status[id] = state.VehMoving
	g.Crossings++

	fromReg := topo.RegionOfEdge(from)
	toReg := topo.RegionOfEdge(to)
	if fromReg != toReg {
		topo.AttachVehicle(id, toReg)
		g.emit(c.Tick, events.EvtRegionHandoff, events.SevInfo,
			int64(fromReg), int64(toReg), int64(id), int64(to))
	}

	// Effect sampling. Emitting one event per road entry would be ~7,000
	// events per tick at full load; the UI cannot use them and the ring would
	// churn many times per second. Emergency and transit movements are always
	// emitted because operators actually watch those; ordinary cars are
	// sampled at 1/512, which is enough to drive a live flow visualisation.
	k := v.Kind[id]
	if k.IsEmergency() || k == state.VehBus || k == state.VehMetro ||
		(uint64(id)+uint64(c.Tick))%512 == 0 {
		g.emit(c.Tick, events.EvtVehicleEnteredRoad, events.SevInfo,
			int64(id), int64(to), 0, 0)
	}
}

func commitArrive(c *Ctx, g *Region, topo Topology, id int32) {
	s := c.S
	v := &s.Vehicles
	if v.Status[id] == state.VehIdle {
		return
	}
	switch {
	case v.TransitRoute[id] >= 0:
		TransitArrived(c, g, id)
		if v.Status[id] == state.VehIdle {
			topo.DetachVehicle(id)
		} else {
			topo.AttachVehicle(id, topo.RegionOfEdge(v.Edge[id]))
		}
		return
	case v.Kind[id].IsEmergency():
		EmergencyArrived(c, g, id)
		if v.Status[id] == state.VehIdle {
			topo.DetachVehicle(id)
		} else {
			topo.AttachVehicle(id, topo.RegionOfEdge(v.Edge[id]))
		}
		return
	}

	ag := v.Agent[id]
	travel := int64(uint32(c.Tick) - v.SpawnTick[id])
	s.Metrics.TripsCompleted++
	s.Metrics.Travel.Observe(travel)
	s.Metrics.DistanceMM += v.DistanceMM[id]
	s.Metrics.StoppedVehicleTicks += int64(v.StopTicks[id])
	if ag >= 0 {
		ref := int64(s.Agents.FreeRefTicks[ag])
		if d := travel - ref; d > 0 {
			s.Metrics.Delay.Observe(d)
		} else {
			s.Metrics.Delay.Observe(0)
		}
		s.Agents.LastTravel[ag] = int32(travel)
		s.Agents.TripsDone[ag]++
		s.Agents.Vehicle[ag] = -1
		s.Agents.Status[ag] = arrivedStatus(s.Agents.Status[ag])
		g.emit(c.Tick, events.EvtVehicleArrived, events.SevInfo,
			int64(id), int64(ag), travel, travel-ref)
	}
	releaseEdge(s, id)
	s.FreeVehicle(id)
	topo.DetachVehicle(id)
}

func commitSpawnCar(c *Ctx, g *Region, arena ArenaLookup, topo Topology, in *Intent) {
	s := c.S
	ag := int32(in.A)
	buf := arena(in.Reg)
	start, n := int32(in.B), int32(in.C)
	if n <= 0 || int(start+n) > len(buf) {
		return
	}
	path := buf[start : start+n]
	first := path[0]
	id := s.NewVehicle(state.VehCar, ag, first, world.NodeID(in.D))
	s.AllocRoute(id, path)
	s.Vehicles.RouteIdx[id] = 1
	s.Edges.Count[first]++
	s.Edges.EnteredTotal[first]++
	s.Agents.Vehicle[ag] = id
	topo.AttachVehicle(id, topo.RegionOfEdge(first))
	_ = g
}

func commitInstallRoute(c *Ctx, arena ArenaLookup, in *Intent) {
	s := c.S
	id := int32(in.A)
	if s.Vehicles.Status[id] == state.VehIdle {
		return
	}
	buf := arena(in.Reg)
	start, n := int32(in.B), int32(in.C)
	if n <= 0 || int(start+n) > len(buf) {
		return
	}
	s.AllocRoute(id, buf[start:start+n])
	s.Vehicles.RouteIdx[id] = 0
	s.Metrics.Reroutes++
}

func commitEndWalk(c *Ctx, g *Region, topo Topology, ag int32) {
	s := c.S
	a := &s.Agents
	topo.DetachWalker(ag)
	a.WalkTicks[ag] = NotWalking
	if a.Mode[ag] == state.ModeTransit && a.TransitRide[ag] < 0 &&
		(a.Status[ag] == state.Commuting || a.Status[ag] == state.Returning) &&
		a.WaitingTicks[ag] == 0 {
		// The walk to the stop is done; queue for the next vehicle. The
		// second walking leg (from the alighting stop to the destination) is
		// distinguished by WaitingTicks having been stamped at boarding.
		JoinStop(c, ag)
		return
	}
	// Arrived on foot.
	travel := int64(uint32(c.Tick) - a.TripStart[ag])
	s.Metrics.TripsCompleted++
	s.Metrics.Travel.Observe(travel)
	ref := int64(a.FreeRefTicks[ag])
	if d := travel - ref; d > 0 {
		s.Metrics.Delay.Observe(d)
	} else {
		s.Metrics.Delay.Observe(0)
	}
	a.LastTravel[ag] = int32(travel)
	a.TripsDone[ag]++
	a.Status[ag] = arrivedStatus(a.Status[ag])
	g.emit(c.Tick, events.EvtVehicleArrived, events.SevInfo, -1, int64(ag), travel, travel-ref)
}

func commitStranded(c *Ctx, g *Region, topo Topology, id int32) {
	s := c.S
	v := &s.Vehicles
	if v.Status[id] == state.VehIdle {
		return
	}
	ag := v.Agent[id]
	if ag >= 0 {
		s.Agents.Status[ag] = state.Stranded
		s.Agents.Vehicle[ag] = -1
	}
	s.Metrics.TripsAbandoned++
	g.emit(c.Tick, events.EvtVehicleStranded, events.SevWarning, int64(id), int64(ag), 0, 0)
	releaseEdge(s, id)
	s.FreeVehicle(id)
	topo.DetachVehicle(id)
}

func commitAccident(c *Ctx, g *Region, topo Topology, in *Intent) {
	s := c.S
	e := world.EdgeID(in.A)
	sev := int32(in.B)
	cas := int32(in.C)
	id := int32(in.D)
	OpenIncident(c, g, state.IncCrash, e, sev, cas)
	if id >= 0 && int(id) < s.Vehicles.Len() && s.Vehicles.Status[id] != state.VehIdle {
		ag := s.Vehicles.Agent[id]
		if ag >= 0 {
			if cas > 0 {
				s.Agents.Health[ag] = 300
				s.Agents.Status[ag] = state.InHospital
			} else {
				s.Agents.Status[ag] = state.Stranded
			}
			s.Agents.Vehicle[ag] = -1
		}
		s.Metrics.TripsAbandoned++
		releaseEdge(s, id)
		s.FreeVehicle(id)
		topo.DetachVehicle(id)
	}
}
