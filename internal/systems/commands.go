package systems

import (
	"github.com/mirror-sim/mirror/internal/events"
	"github.com/mirror-sim/mirror/internal/rng"
	"github.com/mirror-sim/mirror/internal/state"
	"github.com/mirror-sim/mirror/internal/units"
	"github.com/mirror-sim/mirror/internal/world"
)

// ApplyCommand executes one operator command against the state.
//
// This function IS the replay contract. Anything a user can do to a running
// simulation must go through here, and it must depend on nothing except
// (map, state, command). If a command handler read the wall clock, or a random
// number from anywhere but the seeded stream, or a value from an HTTP request
// that was not captured in the command's payload, replay would silently
// diverge. Reviewing changes to this file is the highest-leverage review in
// the codebase.
func ApplyCommand(c *Ctx, g *Region, idx *PowerIndex, e events.Event) {
	m, s := c.Map, c.S
	switch e.Kind {
	case events.CmdSeedPopulation:
		// Handled by the engine before the first tick; seeding mid-run would
		// invalidate the region agent index, so it is rejected here.
		return

	case events.CmdSetPolicy:
		applyPolicyDelta(s, e)

	case events.CmdInjectAccident:
		edge := world.EdgeID(e.A)
		if edge < 0 || int(edge) >= len(m.Edges) {
			return
		}
		sev := clampP(int32(e.B))
		cas := int32(e.C)
		i := OpenIncident(c, g, state.IncCrash, edge, sev, cas)
		if e.D > 0 {
			s.Incidents[i].EndTick = uint32(int64(c.Tick) + e.D)
		}

	case events.CmdCloseRoad:
		edge := world.EdgeID(e.A)
		if edge < 0 || int(edge) >= len(m.Edges) {
			return
		}
		d := e.B
		if d <= 0 {
			d = 30 * units.TicksPerMinute
		}
		s.Edges.ClosedUntil[edge] = uint32(int64(c.Tick) + d)
		g.emit(c.Tick, events.EvtRoadClosed, events.SevWarning, int64(edge), int64(s.Edges.ClosedUntil[edge]), 0, 0)

	case events.CmdReopenRoad:
		edge := world.EdgeID(e.A)
		if edge >= 0 && int(edge) < len(m.Edges) {
			s.Edges.ClosedUntil[edge] = 0
			s.Edges.BlockedLanes[edge] = 0
			g.emit(c.Tick, events.EvtRoadReopened, events.SevNotice, int64(edge), 0, 0, 0)
		}

	case events.CmdPowerFailure:
		ForceOutage(c, g, idx, world.SubstationID(e.A), e.B)

	case events.CmdPowerRestore:
		id := world.SubstationID(e.A)
		if int(id) < len(s.Subs.Online) {
			s.Subs.Online[id] = true
			s.Subs.RestoreAt[id] = 0
			g.emit(c.Tick, events.EvtSubstationRestored, events.SevNotice, int64(id), 0, 0, 0)
			applyPowerDownstream(c, g, idx)
		}

	case events.CmdSetWeather:
		SetWeather(c, g, int32(e.A), int32(e.B), int32(e.C), e.D)

	case events.CmdHospitalSurge:
		h := world.HospitalID(e.A)
		n := int32(e.B)
		if h < 0 || int(h) >= len(m.Hospitals) {
			return
		}
		for i := int32(0); i < n; i++ {
			AdmitPatient(c, g, h)
		}

	case events.CmdTransitFailure:
		ri := int(e.A)
		if ri < 0 || ri >= len(m.Routes) {
			return
		}
		// Suspending a route means no new departures until the outage ends.
		d := e.B
		if d <= 0 {
			d = 30 * units.TicksPerMinute
		}
		s.NextDepart[ri] = int32(int64(c.Tick) + d)
		g.emit(c.Tick, events.EvtTransitDelayed, events.SevWarning, int64(ri), d, 0, 0)

	case events.CmdFloodDistrict:
		floodDistrict(c, g, world.DistrictID(e.A), int32(e.B), e.C)

	case events.CmdEarthquake:
		earthquake(c, g, idx, int32(e.A), world.NodeID(e.B), units.MM(e.C))

	case events.CmdCommsOutage:
		t := world.TowerID(e.A)
		if int(t) < len(s.Towers.Powered) {
			s.Towers.Powered[t] = false
			s.Towers.BatteryMin[t] = 0
			g.emit(c.Tick, events.EvtTowerDown, events.SevCritical, int64(t), 0, 0, 0)
		}

	case events.CmdSpawnTraffic:
		// Extra demand is realised by pulling forward the departure of agents
		// who have not yet left. It deliberately does NOT create agents from
		// nothing: a vehicle with no owning agent has no schedule, no home and
		// no destination preference, and would distort every per-agent metric.
		injectTrips(c, g, int32(e.A), world.DistrictID(e.B))
	}
}

// applyPolicyDelta unpacks a policy command.
//
// A=field id, B=value. One field per command rather than a struct blob so the
// log stays readable, diffable, and forward-compatible: an unknown field id is
// ignored by an older binary instead of corrupting the whole policy.
const (
	PolAdaptiveSignals = iota
	PolAdaptiveMaxExtend
	PolEmergencyPreemption
	PolTransitExtra
	PolRerouteAwareness
	PolSpeedLimit
	PolCongestionCharge
	PolAmbulanceSurge
)

func applyPolicyDelta(s *state.State, e events.Event) {
	p := &s.Policy
	switch e.A {
	case PolAdaptiveSignals:
		p.AdaptiveSignals = e.B != 0
	case PolAdaptiveMaxExtend:
		p.AdaptiveMaxExtendTicks = int32(e.B)
	case PolEmergencyPreemption:
		p.EmergencyPreemption = e.B != 0
	case PolTransitExtra:
		p.TransitExtraVehiclesP = int32(e.B)
	case PolRerouteAwareness:
		p.RerouteAwarenessP = clampP(int32(e.B))
	case PolSpeedLimit:
		p.SpeedLimitP = int32(e.B)
	case PolCongestionCharge:
		p.CongestionCharge = e.B != 0
	case PolAmbulanceSurge:
		p.AmbulanceSurgeP = int32(e.B)
	}
}

// floodDistrict closes the lowest-lying roads in a district and slows the rest.
// "Lowest-lying" is approximated by road class -- local streets flood first,
// motorways are engineered with drainage -- because the map has no elevation
// model. That approximation is stated here rather than dressed up.
func floodDistrict(c *Ctx, g *Region, d world.DistrictID, severity int32, durationTicks int64) {
	m, s := c.Map, c.S
	if durationTicks <= 0 {
		durationTicks = 90 * units.TicksPerMinute
	}
	until := uint32(int64(c.Tick) + durationTicks)
	closed := 0
	for e := range m.Edges {
		if m.Edges[e].District != d {
			continue
		}
		gr := rng.Derive(s.Seed, rng.StreamIncident, uint64(c.Tick), uint64(e))
		threshold := severity
		switch m.Edges[e].Class {
		case world.ClassMotorway:
			threshold = severity / 5
		case world.ClassArterial:
			threshold = severity / 2
		case world.ClassCollector:
			threshold = severity * 3 / 4
		}
		if gr.Chance(threshold) {
			s.Edges.ClosedUntil[e] = until
			closed++
		} else {
			s.Edges.BlockedLanes[e] = m.Edges[e].Lanes / 2
		}
	}
	i := OpenIncident(c, g, state.IncFlood, world.NoEdge, severity, 0)
	s.Incidents[i].District = d
	s.Incidents[i].EndTick = until
	g.emit(c.Tick, events.EvtRoadClosed, events.SevCritical, int64(d), int64(closed), 0, 0)
}

// earthquake damages infrastructure within a radius of the epicentre.
// It is the widest-reaching event in the model on purpose: it exercises the
// road network, the power grid, the hospitals and the comms layer at once,
// which makes it the best single smoke test for cascade behaviour.
func earthquake(c *Ctx, g *Region, idx *PowerIndex, magnitudeP int32, epicentre world.NodeID, radius units.MM) {
	m, s := c.Map, c.S
	if epicentre < 0 || int(epicentre) >= len(m.Nodes) {
		epicentre = world.NodeID(len(m.Nodes) / 2)
	}
	if radius <= 0 {
		radius = 2500 * units.Metre
	}
	ex, ey := m.Nodes[epicentre].X, m.Nodes[epicentre].Y
	within := func(x, y units.MM) (bool, int32) {
		dx, dy := int64(x-ex), int64(y-ey)
		d := units.MM(units.ISqrt(dx*dx + dy*dy))
		if d > radius {
			return false, 0
		}
		// Intensity falls off linearly to the radius.
		return true, int32(int64(magnitudeP) * int64(radius-d) / int64(radius))
	}
	for e := range m.Edges {
		nd := &m.Nodes[m.Edges[e].To]
		ok, intensity := within(nd.X, nd.Y)
		if !ok {
			continue
		}
		gr := rng.Derive(s.Seed, rng.StreamIncident, uint64(c.Tick), uint64(e)*7+1)
		if gr.Chance(intensity / 6) {
			s.Edges.ClosedUntil[e] = uint32(int64(c.Tick) + int64(gr.Range(20, 240))*units.TicksPerMinute)
		} else if gr.Chance(intensity / 3) {
			s.Edges.BlockedLanes[e] = 1
		}
	}
	for i := range m.Substations {
		ok, intensity := within(m.Substations[i].X, m.Substations[i].Y)
		if !ok {
			continue
		}
		gr := rng.Derive(s.Seed, rng.StreamPower, uint64(c.Tick), uint64(i)*13+3)
		if gr.Chance(intensity / 2) {
			ForceOutage(c, g, idx, world.SubstationID(i), int64(gr.Range(20, 180))*units.TicksPerMinute)
		}
	}
	for i := range m.Towers {
		ok, intensity := within(m.Towers[i].X, m.Towers[i].Y)
		if !ok {
			continue
		}
		gr := rng.Derive(s.Seed, rng.StreamPower, uint64(c.Tick), uint64(i)*29+5)
		if gr.Chance(intensity / 3) {
			s.Towers.Powered[i] = false
			s.Towers.BatteryMin[i] = 0
		}
	}
	inc := OpenIncident(c, g, state.IncEarthquake, world.NoEdge, magnitudeP, magnitudeP/8)
	s.Incidents[inc].Node = epicentre
	// Casualties present at hospitals immediately.
	cas := magnitudeP / 8
	for i := int32(0); i < cas; i++ {
		h := nearestAdmittingHospital(c, epicentre)
		if h < 0 {
			s.Metrics.HospitalRejections++
			continue
		}
		AdmitPatient(c, g, h)
	}
}

// injectTrips departs agents who are currently at home, immediately.
//
// It deliberately does NOT fabricate vehicles: a vehicle with no owning agent
// has no home, no destination preference and no schedule, and would distort
// every per-agent metric the platform reports. Extra demand is always extra
// *people* travelling. Trips are started on the global region so their route
// intents commit alongside every other serial mutation -- mutating the
// per-region departure buckets from a command handler would leave the engine's
// derived indices inconsistent with state.
func injectTrips(c *Ctx, g *Region, n int32, d world.DistrictID) {
	s := c.S
	a := &s.Agents
	moved := int32(0)
	for i := 0; i < a.Len() && moved < n; i++ {
		if a.Status[i] != state.AtHome {
			continue
		}
		if d >= 0 && int(d) < len(c.Map.Districts) && a.District[i] != d {
			continue
		}
		startTrip(c, g, int32(i), a.HomeNode[i], a.WorkNode[i], state.Commuting)
		moved++
	}
}
