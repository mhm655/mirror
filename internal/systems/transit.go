package systems

import (
	"github.com/mirror-sim/mirror/internal/events"
	"github.com/mirror-sim/mirror/internal/state"
	"github.com/mirror-sim/mirror/internal/units"
	"github.com/mirror-sim/mirror/internal/world"
)

// TransitDispatch releases vehicles from each route's terminus on its headway.
//
// Buses are ordinary vehicles on ordinary road edges, so they queue at signals
// and sit in the same jams as everyone else. That is the whole point: a policy
// that adds buses without giving them priority produces slower buses, and the
// counterfactual comparison shows it. The metro runs on segregated right of way
// (VehMetro ignores road congestion in MoveVehicles), so the comparison between
// "more buses" and "more metro" is meaningful rather than cosmetic.
func TransitDispatch(c *Ctx, g *Region) {
	m, s := c.Map, c.S
	boost := s.Policy.TransitExtraVehiclesP
	if boost <= 0 {
		boost = 1000
	}
	for ri := range m.Routes {
		rt := &m.Routes[ri]
		if len(rt.Legs) == 0 {
			continue
		}
		// More vehicles on the same route means proportionally shorter
		// headway; that is the correct first-order model and it keeps the
		// policy lever honest (you cannot add buses without adding operating
		// cost, which shows up as more vehicles on the road).
		headway := int32(int64(rt.HeadwayTick) * 1000 / int64(boost))
		if headway < 20*units.TicksPerSecond {
			headway = 20 * units.TicksPerSecond
		}
		if s.NextDepart[ri] > int32(c.Tick) {
			continue
		}
		s.NextDepart[ri] = int32(c.Tick) + headway
		leg := rt.Legs[0]
		if len(leg) == 0 {
			continue
		}
		kind := state.VehBus
		if rt.Mode == 1 {
			kind = state.VehMetro
		}
		id := s.NewVehicle(kind, -1, leg[0], rt.Stops[1])
		s.AllocRoute(id, leg)
		s.Vehicles.RouteIdx[id] = 1
		s.Vehicles.TransitRoute[id] = world.RouteID(ri)
		s.Vehicles.TransitLeg[id] = 0
		s.Vehicles.TransitDir[id] = 0
		s.Edges.Count[leg[0]]++
		s.Edges.EnteredTotal[leg[0]]++
		g.emit(c.Tick, events.EvtTransitDeparted, events.SevInfo, int64(id), int64(ri), 0, 0)
		g.intent(IntentTransitDepart, int64(id), int64(ri), 0, 0)
	}
}

// TransitArrived handles a transit vehicle reaching the end of a leg.
// Called from the serial commit phase.
func TransitArrived(c *Ctx, g *Region, id int32) {
	m, s := c.Map, c.S
	v := &s.Vehicles
	ri := v.TransitRoute[id]
	if ri < 0 || int(ri) >= len(m.Routes) {
		releaseEdge(s, id)
		s.FreeVehicle(id)
		return
	}
	rt := &m.Routes[ri]
	dir := v.TransitDir[id]
	leg := v.TransitLeg[id]

	// Stop index we just arrived at.
	var stop int32
	if dir == 0 {
		stop = leg + 1
	} else {
		stop = leg
	}
	if stop < 0 {
		stop = 0
	}
	if int(stop) >= len(rt.Stops) {
		stop = int32(len(rt.Stops) - 1)
	}

	alightRiders(c, g, id, stop)
	boardRiders(c, g, id, ri, stop, dir)

	// Dwell time scales with the number of people exchanged; a flat 12 seconds
	// plus 0.4s per boarding is the standard transit planning approximation.
	v.DwellTicks[id] = 12*units.TicksPerSecond + v.Occupancy[id]*4/10

	// Advance to the next leg, turning around at the terminus.
	nleg := leg
	ndir := dir
	if dir == 0 {
		nleg = leg + 1
		if int(nleg) >= len(rt.Legs) {
			ndir = 1
			nleg = int32(len(rt.Legs) - 1)
		}
	} else {
		nleg = leg - 1
		if nleg < 0 {
			ndir = 0
			nleg = 0
		}
	}
	v.TransitLeg[id], v.TransitDir[id] = nleg, ndir

	path := legPath(m, rt, nleg, ndir, g.Path[:0])
	g.Path = path
	if len(path) == 0 {
		// The return direction does not exist on this link (one-way street);
		// retire the vehicle rather than leaving it stuck forever.
		releaseEdge(s, id)
		s.FreeVehicle(id)
		return
	}
	releaseEdge(s, id)
	s.AllocRoute(id, path)
	v.Edge[id] = path[0]
	v.RouteIdx[id] = 1
	v.Pos[id] = 0
	v.Status[id] = state.VehMoving
	if ndir == 0 {
		v.Dest[id] = rt.Stops[min32(nleg+1, int32(len(rt.Stops)-1))]
	} else {
		v.Dest[id] = rt.Stops[nleg]
	}
	s.Edges.Count[path[0]]++
	s.Edges.EnteredTotal[path[0]]++
}

// legPath returns the edge sequence for a leg in the requested direction.
// The reverse run walks the forward leg backwards through Map.ReverseEdge.
func legPath(m *world.Map, rt *world.TransitRoute, leg int32, dir int8, out []world.EdgeID) []world.EdgeID {
	out = out[:0]
	if leg < 0 || int(leg) >= len(rt.Legs) {
		return out
	}
	fwd := rt.Legs[leg]
	if dir == 0 {
		return append(out, fwd...)
	}
	for i := len(fwd) - 1; i >= 0; i-- {
		re := m.ReverseEdge[fwd[i]]
		if re == world.NoEdge {
			return out[:0]
		}
		out = append(out, re)
	}
	return out
}

func min32(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}

// alightRiders drops passengers whose alight stop is this one.
func alightRiders(c *Ctx, g *Region, id, stop int32) {
	s := c.S
	a := &s.Agents
	v := &s.Vehicles
	prev := int32(-1)
	cur := v.RiderHead[id]
	for cur >= 0 {
		next := a.NextInList[cur]
		target := int32(a.TAlight[cur])
		if a.Status[cur] == state.Returning {
			target = int32(a.TBoard[cur])
		}
		if target == stop {
			// Unlink.
			if prev < 0 {
				v.RiderHead[id] = next
			} else {
				a.NextInList[prev] = next
			}
			a.NextInList[cur] = -1
			v.Occupancy[id]--
			a.TransitRide[cur] = -1
			// Final walking leg to the true destination.
			a.WalkTicks[cur] = WalkTicksFor(dist(c.Map, c.Map.Edges[v.Edge[id]].To, a.CurrentTarget[cur]))
			g.intent(IntentStartWalk, int64(cur), 0, 0, 0)
		} else {
			prev = cur
		}
		cur = next
	}
}

// boardRiders loads waiting passengers, respecting capacity.
func boardRiders(c *Ctx, g *Region, id int32, ri world.RouteID, stop int32, dir int8) {
	m, s := c.Map, c.S
	a := &s.Agents
	v := &s.Vehicles
	rt := &m.Routes[ri]
	fs := flatStop(m, ri, stop)
	if int(fs) >= len(s.StopHead) {
		return
	}
	capacity := rt.Capacity
	prev := int32(-1)
	cur := s.StopHead[fs]
	for cur >= 0 {
		next := a.NextInList[cur]
		// Direction check: an agent riding to a higher stop index needs the
		// outbound vehicle.
		want := int8(0)
		target := int32(a.TAlight[cur])
		if a.Status[cur] == state.Returning {
			target = int32(a.TBoard[cur])
		}
		if target < stop {
			want = 1
		}
		if want != dir || v.Occupancy[id] >= capacity {
			if want == dir {
				// Denied for capacity: the passenger stays queued and the
				// event is recorded. This is the metric that makes "add more
				// buses" a defensible intervention rather than a guess.
				s.Metrics.TransitDenied++
				g.emit(c.Tick, events.EvtTransitDenied, events.SevNotice,
					int64(cur), int64(ri), int64(stop), 0)
			}
			prev = cur
			cur = next
			continue
		}
		if prev < 0 {
			s.StopHead[fs] = next
		} else {
			a.NextInList[prev] = next
		}
		a.NextInList[cur] = v.RiderHead[id]
		v.RiderHead[id] = cur
		v.Occupancy[id]++
		a.TransitRide[cur] = id
		a.WaitingTicks[cur] = int32(uint32(c.Tick) - a.TripStart[cur])
		s.Metrics.TransitBoardings++
		cur = next
	}
}

// JoinStop places an agent in a stop's waiting queue. Serial phase only.
func JoinStop(c *Ctx, ag int32) {
	m, s := c.Map, c.S
	a := &s.Agents
	rt := a.TRoute[ag]
	if rt < 0 {
		return
	}
	stop := int32(a.TBoard[ag])
	if a.Status[ag] == state.Returning {
		stop = int32(a.TAlight[ag])
	}
	fs := flatStop(m, world.RouteID(rt), stop)
	if fs < 0 || int(fs) >= len(s.StopHead) {
		return
	}
	a.NextInList[ag] = s.StopHead[fs]
	s.StopHead[fs] = ag
}
