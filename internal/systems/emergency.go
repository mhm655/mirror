package systems

import (
	"github.com/mirror-sim/mirror/internal/events"
	"github.com/mirror-sim/mirror/internal/rng"
	"github.com/mirror-sim/mirror/internal/state"
	"github.com/mirror-sim/mirror/internal/units"
	"github.com/mirror-sim/mirror/internal/world"
)

// IncidentSystem ages open incidents, clears them once responders have done
// their work, and reopens blocked lanes.
//
// Runs serially: there are tens of incidents, not tens of thousands, and every
// incident is cross-cutting by nature (it involves a road in one district, an
// ambulance from another, and a hospital in a third). Partitioning it would
// mean a distributed transaction to clear a single crash.
func IncidentSystem(c *Ctx, g *Region, idx *PowerIndex, disp *Dispatcher) {
	s := c.S
	tick := uint32(c.Tick)
	for i := range s.Incidents {
		in := &s.Incidents[i]
		if in.Resolved {
			continue
		}
		if in.EndTick != 0 && tick >= in.EndTick {
			resolveIncident(c, g, i)
			continue
		}
		// An incident with no outstanding response need and an expired
		// clearance window resolves itself.
		if in.NeedAmbulance <= 0 && in.NeedFire <= 0 && in.NeedPolice <= 0 &&
			in.FirstResponseTick != 0 && tick >= in.FirstResponseTick+clearanceTicks(in) {
			resolveIncident(c, g, i)
			continue
		}
		disp.Consider(c, g, int32(i))
	}
	// Reopen roads whose closure has lapsed.
	//
	// Scanned once per simulated second rather than every tick. Closures are
	// specified to the second, so nothing is lost, and the full scan over
	// every edge was one of the largest serial costs in the tick on a network
	// with no closures at all -- the pathological case for an unconditional
	// full-array sweep.
	if uint64(c.Tick)%units.TicksPerSecond == 0 {
		for e := range s.Edges.ClosedUntil {
			if s.Edges.ClosedUntil[e] != 0 && tick >= s.Edges.ClosedUntil[e] {
				s.Edges.ClosedUntil[e] = 0
				g.emit(c.Tick, events.EvtRoadReopened, events.SevNotice, int64(e), 0, 0, 0)
			}
		}
	}
}

func clearanceTicks(in *state.Incident) uint32 {
	// Clearance scales with severity: a fender-bender clears in 4 minutes, a
	// serious collision closes a lane for half an hour.
	return uint32(4*units.TicksPerMinute + in.Severity*26*units.TicksPerMinute/1000)
}

func resolveIncident(c *Ctx, g *Region, i int) {
	s := c.S
	in := &s.Incidents[i]
	if in.Resolved {
		return
	}
	in.Resolved = true
	s.Metrics.IncidentsResolved++
	if in.Edge >= 0 && int(in.Edge) < len(s.Edges.BlockedLanes) {
		s.Edges.BlockedLanes[in.Edge] = 0
	}
	g.emit(c.Tick, events.EvtIncidentResolved, events.SevNotice, in.ID, int64(in.Edge), 0, 0)
}

// OpenIncident registers a new incident and blocks lanes on the affected road.
func OpenIncident(c *Ctx, g *Region, kind uint8, edge world.EdgeID, severity, casualties int32) int32 {
	m, s := c.Map, c.S
	node := world.NoNode
	district := world.DistrictID(0)
	if edge >= 0 && int(edge) < len(m.Edges) {
		node = m.Edges[edge].To
		district = m.Edges[edge].District
		// Lane blocking: proportional to severity, always leaving at least the
		// possibility of full closure on a single-lane street. This is the
		// mechanism by which one crash gridlocks a corridor.
		lanes := m.Edges[edge].Lanes
		blocked := 1 + severity*lanes/1200
		if blocked > lanes {
			blocked = lanes
		}
		s.Edges.BlockedLanes[edge] = blocked
	}
	in := state.Incident{
		ID: s.NextIncID, Kind: kind, Edge: edge, Node: node, District: district,
		StartTick: uint32(c.Tick), Severity: severity, Casualties: casualties,
	}
	s.NextIncID++
	if casualties > 0 {
		in.NeedAmbulance = casualties
	}
	if severity > 700 {
		in.NeedFire = 1
	}
	if severity > 350 {
		in.NeedPolice = 1
	}
	s.Incidents = append(s.Incidents, in)
	s.Metrics.IncidentsOpened++
	s.Metrics.Casualties += int64(casualties)
	g.emit(c.Tick, events.EvtAccidentOccurred, events.SevCritical,
		in.ID, int64(edge), int64(severity), int64(casualties))
	return int32(len(s.Incidents) - 1)
}

// Dispatcher assigns emergency units to incidents.
//
// The policy is "nearest available unit by straight-line distance, then route
// for real". Straight-line pre-filtering matters: routing from every depot to
// every incident would be O(depots x incidents) A* queries, and during a mass
// casualty event that is exactly when the engine can least afford it.
type Dispatcher struct {
	Router *world.Router
	Path   []world.EdgeID
}

func NewDispatcher(nodeCount int) *Dispatcher {
	return &Dispatcher{Router: world.NewRouter(nodeCount)}
}

// Consider re-evaluates one incident, subject to a cooldown.
//
// The cooldown lives on the incident in simulation state, not in a map on the
// dispatcher. That is not a style preference: anything the dispatcher
// remembers across ticks but does not checkpoint is state a restored engine
// does not have, and the restored run then dispatches on different ticks than
// the original. It is exactly the bug TestCheckpointRestore exists to catch.
func (d *Dispatcher) Consider(c *Ctx, g *Region, inc int32) {
	s := c.S
	in := &s.Incidents[inc]
	if uint32(c.Tick) < in.NextConsiderTick {
		return
	}
	in.NextConsiderTick = uint32(c.Tick) + 3*units.TicksPerSecond
	if in.NeedAmbulance > 0 {
		d.dispatch(c, g, inc, state.VehAmbulance)
	}
	if in.NeedFire > 0 {
		d.dispatch(c, g, inc, state.VehFire)
	}
	if in.NeedPolice > 0 {
		d.dispatch(c, g, inc, state.VehPolice)
	}
}

func (d *Dispatcher) dispatch(c *Ctx, g *Region, inc int32, kind state.VehicleKind) {
	m, s := c.Map, c.S
	in := &s.Incidents[inc]
	if in.Node < 0 {
		return
	}

	// Find the nearest source with an available unit.
	src := world.NoNode
	srcHosp := world.HospitalID(-1)
	srcDepot := world.DepotID(-1)
	var best int64 = 1<<62 - 1

	if kind == state.VehAmbulance {
		for i := range m.Hospitals {
			if s.Hosps.AmbAvail[i] <= 0 {
				continue
			}
			dd := int64(dist(m, m.Hospitals[i].Node, in.Node))
			if dd < best {
				best, src, srcHosp = dd, m.Hospitals[i].Node, world.HospitalID(i)
			}
		}
	}
	depotKind := uint8(2)
	switch kind {
	case state.VehFire:
		depotKind = 0
	case state.VehPolice:
		depotKind = 1
	}
	for i := range m.Depots {
		if m.Depots[i].Kind != depotKind || s.Depots.Available[i] <= 0 {
			continue
		}
		dd := int64(dist(m, m.Depots[i].Node, in.Node))
		if dd < best {
			best, src, srcDepot, srcHosp = dd, m.Depots[i].Node, world.DepotID(i), -1
		}
	}
	if src == world.NoNode {
		return // no units available; the incident waits, and the metric records it
	}

	path, ok := d.Router.Route(m, src, in.Node, CostFn(c, true, true), d.Path[:0])
	d.Path = path
	if !ok || len(path) == 0 {
		return
	}
	if srcHosp >= 0 {
		s.Hosps.AmbAvail[srcHosp]--
	} else if srcDepot >= 0 {
		s.Depots.Available[srcDepot]--
		s.Depots.Dispatched[srcDepot]++
	}
	switch kind {
	case state.VehAmbulance:
		in.NeedAmbulance--
	case state.VehFire:
		in.NeedFire--
	case state.VehPolice:
		in.NeedPolice--
	}

	// Emergency vehicles are allocated in the serial phase like everything
	// else; the dispatcher runs there already, so it may allocate directly.
	id := s.NewVehicle(kind, -1, path[0], in.Node)
	s.AllocRoute(id, path)
	// The unit is placed ON the first edge of its route, so the next edge to
	// traverse is index 1. Leaving this at 0 made every dispatched unit
	// re-traverse its first link, adding a link's worth of latency to every
	// response time we report.
	s.Vehicles.RouteIdx[id] = 1
	s.Vehicles.Payload[id] = int64ToI32(int64(inc))
	s.Vehicles.SpawnTick[id] = uint32(c.Tick)
	s.Edges.Count[path[0]]++
	s.Edges.EnteredTotal[path[0]]++
	s.Metrics.EmergencyDispatched++

	var eta int32
	for _, e := range path {
		eta += s.Edges.TravelTicks[e]
	}
	g.emit(c.Tick, events.EvtEmergencyDispatched, events.SevWarning,
		int64(id), in.ID, int64(srcHosp), int64(eta))
	g.intent(IntentDispatch, int64(id), 0, 0, 0)
}

func int64ToI32(v int64) int32 { return int32(v) }

// EmergencyArrived is called from the commit phase when an emergency vehicle
// reaches its destination.
func EmergencyArrived(c *Ctx, g *Region, id int32) {
	m, s := c.Map, c.S
	v := &s.Vehicles
	inc := v.Payload[id]
	kind := v.Kind[id]

	if inc >= 0 && int(inc) < len(s.Incidents) {
		in := &s.Incidents[inc]
		resp := uint32(c.Tick) - v.SpawnTick[id]
		if in.FirstResponseTick == 0 {
			in.FirstResponseTick = uint32(c.Tick)
		}
		s.Metrics.EmergencyArrived++
		s.Metrics.EmergencyResponse.Observe(int64(resp))
		g.emit(c.Tick, events.EvtEmergencyOnScene, events.SevNotice,
			int64(id), in.ID, int64(resp), 0)

		if kind == state.VehAmbulance && in.Casualties > 0 {
			// Load a casualty and run to the nearest hospital with capacity.
			in.Casualties--
			h := nearestAdmittingHospital(c, in.Node)
			if h >= 0 {
				path, ok := g.Router.Route(m, in.Node, m.Hospitals[h].Node, CostFn(c, true, true), g.Path[:0])
				g.Path = path
				if ok && len(path) > 0 {
					// Re-target the same vehicle rather than allocating a new
					// one: an ambulance is a single unit for its whole mission.
					releaseEdge(s, id)
					v.Edge[id] = path[0]
					v.Pos[id] = 0
					v.Status[id] = state.VehMoving
					v.Dest[id] = m.Hospitals[h].Node
					v.Payload[id] = -2 - int32(h) // encodes "carrying to hospital h"
					s.AllocRoute(id, path)
					v.RouteIdx[id] = 1
					s.Edges.Count[path[0]]++
					s.Edges.EnteredTotal[path[0]]++
					return
				}
			}
		}
	} else if inc <= -2 {
		// Arrived at a hospital carrying a patient.
		h := world.HospitalID(-inc - 2)
		AdmitPatient(c, g, h)
		s.Hosps.AmbAvail[h]++
	}

	// Return the unit to service. Modelled as an instant teleport back to base
	// rather than a return trip: the return leg has no effect on any metric we
	// report and would double the emergency fleet's contribution to congestion
	// for no analytical gain. Called out in docs as a deliberate simplification.
	if kind != state.VehAmbulance {
		returnUnitToDepot(c, id)
	}
	releaseEdge(s, id)
	s.FreeVehicle(id)
}

func releaseEdge(s *state.State, id int32) {
	e := s.Vehicles.Edge[id]
	if e >= 0 && int(e) < len(s.Edges.Count) && s.Edges.Count[e] > 0 {
		s.Edges.Count[e]--
	}
}

func returnUnitToDepot(c *Ctx, id int32) {
	m, s := c.Map, c.S
	kind := s.Vehicles.Kind[id]
	depotKind := uint8(2)
	switch kind {
	case state.VehFire:
		depotKind = 0
	case state.VehPolice:
		depotKind = 1
	}
	e := s.Vehicles.Edge[id]
	if e < 0 {
		return
	}
	at := m.Edges[e].To
	best := world.DepotID(-1)
	var bd int64 = 1<<62 - 1
	for i := range m.Depots {
		if m.Depots[i].Kind != depotKind {
			continue
		}
		if d := int64(dist(m, m.Depots[i].Node, at)); d < bd {
			bd, best = d, world.DepotID(i)
		}
	}
	if best >= 0 {
		s.Depots.Available[best]++
	}
}

func nearestAdmittingHospital(c *Ctx, from world.NodeID) world.HospitalID {
	m, s := c.Map, c.S
	best := world.HospitalID(-1)
	var bd int64 = 1<<62 - 1
	for i := range m.Hospitals {
		if s.Hosps.BedsUsed[i] >= m.Hospitals[i].Beds {
			continue
		}
		if s.Hosps.OnBackup[i] && s.Hosps.BackupLeft[i] <= 0 {
			continue // no power, no admissions
		}
		if d := int64(dist(m, m.Hospitals[i].Node, from)); d < bd {
			bd, best = d, world.HospitalID(i)
		}
	}
	return best
}

// ------------------------------------------------------------ hospitals ---

// HospitalSystem handles admissions, discharges and diversion.
func HospitalSystem(c *Ctx, g *Region) {
	m, s := c.Map, c.S
	// Discharge on a 20-minute cadence: roughly 3% of occupied beds per hour.
	if uint64(c.Tick)%(20*units.TicksPerMinute) == 0 {
		for i := range m.Hospitals {
			gr := rng.Derive(s.Seed, rng.StreamHealth, uint64(c.Tick), uint64(i))
			d := s.Hosps.BedsUsed[i] / 100
			if d < 1 && s.Hosps.BedsUsed[i] > 0 && gr.Chance(400) {
				d = 1
			}
			s.Hosps.BedsUsed[i] -= d
			if s.Hosps.BedsUsed[i] < 0 {
				s.Hosps.BedsUsed[i] = 0
			}
			if s.Hosps.ERUsed[i] > 0 {
				s.Hosps.ERUsed[i]--
			}
		}
	}
	// Capacity alarms and peak tracking.
	var used, total int64
	for i := range m.Hospitals {
		used += int64(s.Hosps.BedsUsed[i])
		total += int64(m.Hospitals[i].Beds)
		if s.Hosps.BedsUsed[i] >= m.Hospitals[i].Beds &&
			uint64(c.Tick)%(60*units.TicksPerSecond) == uint64(i)%(60*units.TicksPerSecond) {
			g.emit(c.Tick, events.EvtHospitalOverloaded, events.SevCritical,
				int64(i), int64(s.Hosps.BedsUsed[i]), int64(m.Hospitals[i].Beds), 0)
		}
	}
	if total > 0 {
		util := int32(used * 1000 / total)
		if util > s.Metrics.PeakHospitalUtilP {
			s.Metrics.PeakHospitalUtilP = util
		}
	}
}

// AdmitPatient admits one patient to a hospital, diverting if it is full.
func AdmitPatient(c *Ctx, g *Region, h world.HospitalID) bool {
	m, s := c.Map, c.S
	if h < 0 || int(h) >= len(m.Hospitals) {
		return false
	}
	if s.Hosps.BedsUsed[h] < m.Hospitals[h].Beds {
		s.Hosps.BedsUsed[h]++
		s.Hosps.ERUsed[h]++
		s.Hosps.Admissions[h]++
		s.Metrics.HospitalAdmissions++
		g.emit(c.Tick, events.EvtHospitalAdmission, events.SevInfo,
			int64(h), 0, int64(s.Hosps.BedsUsed[h]), int64(m.Hospitals[h].Beds))
		return true
	}
	// Divert to the nearest hospital that can take the patient.
	alt := nearestAdmittingHospital(c, m.Hospitals[h].Node)
	if alt >= 0 && alt != h {
		s.Hosps.Diverted[h]++
		s.Metrics.HospitalDiversions++
		g.emit(c.Tick, events.EvtHospitalDiverted, events.SevWarning,
			int64(h), int64(alt), 0, 0)
		s.Hosps.BedsUsed[alt]++
		s.Hosps.Admissions[alt]++
		s.Metrics.HospitalAdmissions++
		return true
	}
	s.Hosps.Rejections[h]++
	s.Metrics.HospitalRejections++
	g.emit(c.Tick, events.EvtHospitalOverloaded, events.SevCritical,
		int64(h), int64(s.Hosps.BedsUsed[h]), int64(m.Hospitals[h].Beds), 0)
	return false
}
