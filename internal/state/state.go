// Package state holds MIRROR's mutable simulation state.
//
// Layout is struct-of-arrays throughout. This is not premature optimisation:
// the movement system touches exactly four vehicle fields per tick and skips
// the rest, and at 100k vehicles the AoS version pulls ~19MB of cache lines per
// tick where the SoA version pulls ~3MB. It also makes checkpointing, hashing
// and forking a handful of memcpys instead of a pointer walk.
//
// Everything here is a pure function of (map, seed, command log). Nothing in
// this package may read the wall clock, iterate a Go map in a way that affects
// results, or use floating point.
package state

import (
	"github.com/mirror-sim/mirror/internal/units"
	"github.com/mirror-sim/mirror/internal/world"
)

// ---------------------------------------------------------------- agents ---

type AgentStatus uint8

const (
	AtHome AgentStatus = iota
	Commuting
	AtWork
	Returning
	InHospital
	Stranded
)

var agentStatusName = [...]string{"at_home", "commuting", "at_work", "returning", "in_hospital", "stranded"}

func (s AgentStatus) String() string { return agentStatusName[s] }

type TravelMode uint8

const (
	ModeCar TravelMode = iota
	ModeTransit
	ModeWalk
)

var travelModeName = [...]string{"car", "transit", "walk"}

func (m TravelMode) String() string { return travelModeName[m] }

// Agents is the population. One entry per simulated person.
//
// 100k agents cost about 5.6 MB here. That is the number that makes "hundreds
// of thousands of entities" a memory question rather than an architecture
// question -- and the reason no agent gets an LLM call.
type Agents struct {
	HomeNode []world.NodeID
	WorkNode []world.NodeID
	HomePOI  []world.POIID
	WorkPOI  []world.POIID
	District []world.DistrictID // owning region = home district

	// Departure times as tick-of-day, drawn once at population time from a
	// bimodal distribution. Kept in state (not recomputed) so that a change to
	// the population generator cannot silently alter an existing replay.
	DepartOut []int32
	DepartRet []int32

	Mode   []TravelMode
	Status []AgentStatus
	// Vehicle index, or -1 when not driving.
	Vehicle []int32
	// TransitRide is the transit vehicle index, or -1.
	TransitRide []int32
	// Health in permille. Below HealthCritical the agent needs a hospital.
	Health []int16
	// PatienceP: how congested a route must get before this agent reroutes.
	// Heterogeneity here is what stops the whole population from flipping to
	// the same detour on the same tick, which is both unrealistic and produces
	// a pathological oscillation in the traffic model.
	PatienceP []int16
	// RiskP: willingness to take faster but more accident-prone routes.
	RiskP []int16

	TripStart     []uint32
	TripsDone     []int32
	LastTravel    []int32
	WaitingTicks  []int32
	CurrentTarget []world.NodeID

	// Precomputed transit itinerary: which route to take, where to board and
	// where to alight. Computed once when the population is seeded rather than
	// at every departure, because a full transit search per trip would cost
	// more than the entire road-movement system.
	TRoute  []int16
	TBoard  []int16
	TAlight []int16
	// WalkTicks is the remaining walking leg (home->stop, stop->work).
	WalkTicks []int32
	// FreeRefTicks is the free-flow duration of the trip the agent is on,
	// captured at departure. Delay = actual - this. Stored rather than
	// recomputed because the route the agent actually took may no longer
	// exist by the time they arrive.
	FreeRefTicks []int32
	// NextInList is an intrusive singly-linked-list pointer. An agent is in at
	// most one list at a time -- either the queue at a transit stop or the
	// rider list of a transit vehicle -- so one field serves both. This is what
	// makes boarding O(1) instead of a scan over the whole population.
	NextInList []int32
}

func (a *Agents) Len() int { return len(a.Status) }

const HealthCritical int16 = 400

// -------------------------------------------------------------- vehicles ---

type VehicleKind uint8

const (
	VehCar VehicleKind = iota
	VehBus
	VehMetro
	VehAmbulance
	VehFire
	VehPolice
)

var vehicleKindName = [...]string{"car", "bus", "metro", "ambulance", "fire", "police"}

func (k VehicleKind) String() string { return vehicleKindName[k] }

// IsEmergency reports whether this vehicle gets signal preemption and the
// blue-light routing discount.
func (k VehicleKind) IsEmergency() bool {
	return k == VehAmbulance || k == VehFire || k == VehPolice
}

type VehicleStatus uint8

const (
	VehIdle VehicleStatus = iota
	VehMoving
	VehQueued // at an intersection, waiting for green or for space
	VehArrived
	VehDisabled // crashed / broken down; blocks a lane
)

// Vehicles is the moving fleet.
type Vehicles struct {
	Kind   []VehicleKind
	Status []VehicleStatus
	Agent  []int32 // owning agent, or -1 for transit/emergency
	Edge   []world.EdgeID
	// Pos is the distance travelled along Edge, in mm.
	Pos   []units.MM
	Speed []units.MMPerTick
	// Route arena coordinates. RouteIdx is the index of the *next* edge.
	RouteStart []int32
	RouteLen   []int32
	RouteIdx   []int32
	Dest       []world.NodeID
	SpawnTick  []uint32
	// StopTicks counts ticks spent at zero speed; it feeds the congestion and
	// fuel models and is the single most legible congestion metric we have.
	StopTicks []int32
	// DistanceMM travelled, for fuel/emissions.
	DistanceMM []int64
	// ReplanAt is the earliest tick this vehicle may reconsider its route.
	// Rate limiting reroutes is a correctness measure as much as a performance
	// one: without it, congestion-aware routing oscillates.
	ReplanAt []uint32
	// Payload: for an ambulance, the agent being transported; else -1.
	Payload []int32
	// TransitRoute for buses/metros, else -1. Leg is the index into Route.Legs.
	TransitRoute []world.RouteID
	TransitLeg   []int32
	Occupancy    []int32
	// RiderHead is the head of the intrusive rider list (see Agents.NextInList).
	RiderHead []int32
	// DwellTicks remaining at a stop.
	DwellTicks []int32
	// TransitDir is 0 outbound along the route, 1 on the return run.
	TransitDir []int8
	// Free list of reusable slots, LIFO. Deterministic because every
	// allocation and release happens in the serial commit phase in a fixed
	// order (see engine.Tick phase B).
	free []int32
}

func (v *Vehicles) Len() int { return len(v.Kind) }

// ---------------------------------------------------------- infrastructure --

// EdgeState is the per-road dynamic overlay. One entry per static edge.
type EdgeState struct {
	// Count of vehicles currently on the edge.
	Count []int32
	// Blocked lanes (from a crash or works). Reduces effective capacity.
	BlockedLanes []int32
	// ClosedUntil tick; 0 means open.
	ClosedUntil []uint32
	// Speed is the current realised speed, recomputed each tick from density.
	Speed []units.MMPerTick
	// TravelTicks is the current perceived cost, published to routers.
	TravelTicks []int32
	// Lit indicates street lighting/signal power. Unlit arterials at night get
	// a speed and accident-rate penalty.
	Lit []bool
	// EnteredTotal is a lifetime counter used for throughput metrics.
	EnteredTotal []int64
}

type SignalState struct {
	Phase     []int8
	PhaseTick []int32
	// Powered signals cycle; unpowered ones become all-way stops, which is
	// exactly the failure mode that makes a power cut interesting for traffic.
	Powered []bool
	// Preempt is >0 while an emergency vehicle is approaching.
	Preempt []int32
	// QueueLen per approach phase, used by the adaptive controller.
	Queue0 []int32
	Queue1 []int32
	// GreenTicks currently granted per phase (adaptive control writes here).
	Green0 []int32
	Green1 []int32
}

type SubstationState struct {
	Online []bool
	// LoadKW is the current demand routed through this substation.
	LoadKW []int32
	// OverTicks counts consecutive ticks above capacity.
	OverTicks []int32
	// RestoreAt is the tick at which a tripped substation comes back, or 0.
	RestoreAt []uint32
	Trips     []int32
}

type HospitalState struct {
	BedsUsed []int32
	ERUsed   []int32
	// Queue of patients waiting for an ER bay.
	Waiting []int32
	// AmbulancesAvailable at the hospital.
	AmbAvail []int32
	OnBackup []bool
	// BackupTicksLeft when running on generators.
	BackupLeft []int32
	Admissions []int64
	Rejections []int64
	// Diverted counts patients redirected to another hospital because this one
	// was saturated. This is the metric that makes a mass-casualty event
	// legible.
	Diverted []int64
}

type TowerState struct {
	Powered []bool
	// LoadErl is current session demand; above capacity, calls fail.
	LoadErl    []int32
	BatteryMin []int32
	Dropped    []int64
}

type DepotState struct {
	Available  []int32
	Dispatched []int64
}

// Incident is an active event on the network -- a crash, a flood, road works.
type Incident struct {
	ID         int64
	Kind       uint8
	Edge       world.EdgeID
	Node       world.NodeID
	District   world.DistrictID
	StartTick  uint32
	EndTick    uint32 // 0 = open ended
	Severity   int32  // 0..1000
	Casualties int32
	// Responders still required.
	NeedAmbulance int32
	NeedFire      int32
	NeedPolice    int32
	// FirstResponseTick, 0 until the first unit arrives.
	FirstResponseTick uint32
	// NextConsiderTick throttles the dispatcher. Held in state rather than on
	// the dispatcher so a restored engine dispatches on the same ticks as the
	// original run.
	NextConsiderTick uint32
	Resolved         bool
}

const (
	IncCrash uint8 = iota
	IncRoadClosure
	IncFlood
	IncEarthquake
	IncFire
	IncPowerFault
	IncMedical
)

var incidentKindName = [...]string{"crash", "road_closure", "flood", "earthquake", "fire", "power_fault", "medical"}

func IncidentKindName(k uint8) string {
	if int(k) < len(incidentKindName) {
		return incidentKindName[k]
	}
	return "unknown"
}

// Weather is city-wide and affects speed, accident rate and power demand.
type Weather struct {
	// Condition: 0 clear, 1 rain, 2 storm, 3 snow, 4 heatwave, 5 fog.
	Condition int32
	TempC     int32
	// WindKph, VisibilityM.
	WindKph     int32
	VisibilityM int32
	// UntilTick, after which it reverts to clear.
	UntilTick uint32
}

// Policy is the set of levers a counterfactual can pull. A fork differs from
// its parent by exactly this struct plus whatever commands are appended after
// the branch point -- which is what makes "compare A and B" a meaningful
// statement rather than a comparison of two unrelated runs.
type Policy struct {
	AdaptiveSignals bool
	// AdaptiveMaxExtendTicks caps how far the adaptive controller may stretch
	// a green. Unbounded extension starves the cross street and is the classic
	// way a naive adaptive controller makes things worse.
	AdaptiveMaxExtendTicks int32
	EmergencyPreemption    bool
	// TransitExtraVehiclesP scales the fleet on every route, in permille.
	TransitExtraVehiclesP int32
	// RerouteAwarenessP is the fraction of drivers with live traffic info.
	RerouteAwarenessP int32
	// SpeedLimitP scales every free-flow speed.
	SpeedLimitP int32
	// CongestionCharge adds a routing penalty for entering the central
	// district, diverting through-traffic.
	CongestionCharge bool
	// AmbulanceSurgeP scales ambulance availability.
	AmbulanceSurgeP int32
	// RoadworksClosures are edges closed for the whole run.
	Name string
}

func DefaultPolicy() Policy {
	return Policy{
		AdaptiveSignals:        false,
		AdaptiveMaxExtendTicks: 15 * units.TicksPerSecond,
		EmergencyPreemption:    true,
		TransitExtraVehiclesP:  1000,
		RerouteAwarenessP:      450,
		SpeedLimitP:            1000,
		AmbulanceSurgeP:        1000,
		Name:                   "baseline",
	}
}

// ----------------------------------------------------------------- state ---

// State is everything that changes. Forking copies this and nothing else.
type State struct {
	Tick      units.Tick
	Seed      uint64
	MapHash   uint64
	StartHour int32

	Policy Policy

	Agents   Agents
	Vehicles Vehicles
	Edges    EdgeState
	Signals  SignalState
	Subs     SubstationState
	Hosps    HospitalState
	Towers   TowerState
	Depots   DepotState

	Weather   Weather
	Incidents []Incident
	NextIncID int64

	// RouteBuf is the arena backing every vehicle route. One flat slice means
	// forking a scenario is a single copy instead of 100k small allocations,
	// and the checkpoint is contiguous on disk.
	RouteBuf []world.EdgeID
	// RouteLive is the number of arena entries actually referenced; when it
	// falls below half of len(RouteBuf) the arena is compacted (deterministic,
	// in vehicle-id order).
	RouteLive int32

	// StopHead indexes the waiting queue at every transit stop, flattened as
	// map.RouteStopBase[route]+stopIndex.
	StopHead []int32
	// NextDepart is the tick each route next releases a vehicle from its
	// terminus.
	NextDepart []int32

	Metrics Metrics

	// SpawnCursor is the population index the departure scan resumes from.
	// Scanning a slice of the population each tick instead of all of it keeps
	// the departure system O(population/scanDivisor) rather than O(population).
	SpawnCursor int32
}

// NewState allocates state sized for a map and a population.
func NewState(m *world.Map, seed uint64, population int, startHour int32) *State {
	s := &State{
		Seed: seed, MapHash: m.Hash, StartHour: startHour,
		Policy: DefaultPolicy(),
	}
	na := population
	s.Agents = Agents{
		HomeNode: make([]world.NodeID, na), WorkNode: make([]world.NodeID, na),
		HomePOI: make([]world.POIID, na), WorkPOI: make([]world.POIID, na),
		District:  make([]world.DistrictID, na),
		DepartOut: make([]int32, na), DepartRet: make([]int32, na),
		Mode: make([]TravelMode, na), Status: make([]AgentStatus, na),
		Vehicle: make([]int32, na), TransitRide: make([]int32, na),
		Health: make([]int16, na), PatienceP: make([]int16, na), RiskP: make([]int16, na),
		TripStart: make([]uint32, na), TripsDone: make([]int32, na),
		LastTravel: make([]int32, na), WaitingTicks: make([]int32, na),
		CurrentTarget: make([]world.NodeID, na),
		TRoute:        make([]int16, na), TBoard: make([]int16, na),
		TAlight: make([]int16, na), WalkTicks: make([]int32, na),
		FreeRefTicks: make([]int32, na), NextInList: make([]int32, na),
	}
	for i := 0; i < na; i++ {
		s.Agents.Vehicle[i] = -1
		s.Agents.TransitRide[i] = -1
		s.Agents.TRoute[i] = -1
		s.Agents.NextInList[i] = -1
		s.Agents.WalkTicks[i] = -1
		s.Agents.Health[i] = 1000
	}
	ne := len(m.Edges)
	s.Edges = EdgeState{
		Count: make([]int32, ne), BlockedLanes: make([]int32, ne),
		ClosedUntil: make([]uint32, ne), Speed: make([]units.MMPerTick, ne),
		TravelTicks: make([]int32, ne), Lit: make([]bool, ne),
		EnteredTotal: make([]int64, ne),
	}
	for i := range s.Edges.Speed {
		s.Edges.Speed[i] = m.Edges[i].FreeSpeed
		s.Edges.TravelTicks[i] = m.EdgeTravelTicksFree(world.EdgeID(i))
		s.Edges.Lit[i] = true
	}
	ns := len(m.Signals)
	s.Signals = SignalState{
		Phase: make([]int8, ns), PhaseTick: make([]int32, ns),
		Powered: make([]bool, ns), Preempt: make([]int32, ns),
		Queue0: make([]int32, ns), Queue1: make([]int32, ns),
		Green0: make([]int32, ns), Green1: make([]int32, ns),
	}
	for i := range s.Signals.Powered {
		s.Signals.Powered[i] = true
		s.Signals.Green0[i] = m.Signals[i].PhaseTicks[0]
		s.Signals.Green1[i] = m.Signals[i].PhaseTicks[1]
		// Offset shifts the initial phase clock, producing the green wave.
		off := m.Signals[i].Offset
		total := m.Signals[i].PhaseTicks[0] + m.Signals[i].PhaseTicks[1]
		if total > 0 {
			off %= total
			if off < m.Signals[i].PhaseTicks[0] {
				s.Signals.Phase[i], s.Signals.PhaseTick[i] = 0, off
			} else {
				s.Signals.Phase[i], s.Signals.PhaseTick[i] = 1, off-m.Signals[i].PhaseTicks[0]
			}
		}
	}
	nsub := len(m.Substations)
	s.Subs = SubstationState{
		Online: make([]bool, nsub), LoadKW: make([]int32, nsub),
		OverTicks: make([]int32, nsub), RestoreAt: make([]uint32, nsub),
		Trips: make([]int32, nsub),
	}
	for i := range s.Subs.Online {
		s.Subs.Online[i] = true
	}
	nh := len(m.Hospitals)
	s.Hosps = HospitalState{
		BedsUsed: make([]int32, nh), ERUsed: make([]int32, nh),
		Waiting: make([]int32, nh), AmbAvail: make([]int32, nh),
		OnBackup: make([]bool, nh), BackupLeft: make([]int32, nh),
		Admissions: make([]int64, nh), Rejections: make([]int64, nh),
		Diverted: make([]int64, nh),
	}
	for i := range s.Hosps.AmbAvail {
		s.Hosps.AmbAvail[i] = m.Hospitals[i].Ambulances
		s.Hosps.BackupLeft[i] = m.Hospitals[i].BackupMinutes * units.TicksPerMinute
		// Hospitals do not start empty; a hospital at 0% occupancy would
		// absorb any surge and make the whole health model uninteresting.
		s.Hosps.BedsUsed[i] = m.Hospitals[i].Beds * 62 / 100
	}
	nt := len(m.Towers)
	s.Towers = TowerState{
		Powered: make([]bool, nt), LoadErl: make([]int32, nt),
		BatteryMin: make([]int32, nt), Dropped: make([]int64, nt),
	}
	for i := range s.Towers.Powered {
		s.Towers.Powered[i] = true
		s.Towers.BatteryMin[i] = m.Towers[i].BatteryMin
	}
	nd := len(m.Depots)
	s.Depots = DepotState{Available: make([]int32, nd), Dispatched: make([]int64, nd)}
	for i := range s.Depots.Available {
		s.Depots.Available[i] = m.Depots[i].Units
	}
	s.StopHead = make([]int32, m.TotalStops)
	for i := range s.StopHead {
		s.StopHead[i] = -1
	}
	s.NextDepart = make([]int32, len(m.Routes))
	s.Weather = Weather{Condition: 0, TempC: 18, WindKph: 8, VisibilityM: 10000}
	s.Metrics.init()
	return s
}

// ---------------------------------------------------- vehicle allocation ---

// NewVehicle allocates a vehicle slot. MUST only be called from the serial
// commit phase of a tick -- see docs/architecture/distributed-execution.md.
func (s *State) NewVehicle(kind VehicleKind, agent int32, edge world.EdgeID, dest world.NodeID) int32 {
	v := &s.Vehicles
	var id int32
	if n := len(v.free); n > 0 {
		id = v.free[n-1]
		v.free = v.free[:n-1]
	} else {
		id = int32(len(v.Kind))
		v.Kind = append(v.Kind, 0)
		v.Status = append(v.Status, 0)
		v.Agent = append(v.Agent, -1)
		v.Edge = append(v.Edge, world.NoEdge)
		v.Pos = append(v.Pos, 0)
		v.Speed = append(v.Speed, 0)
		v.RouteStart = append(v.RouteStart, 0)
		v.RouteLen = append(v.RouteLen, 0)
		v.RouteIdx = append(v.RouteIdx, 0)
		v.Dest = append(v.Dest, world.NoNode)
		v.SpawnTick = append(v.SpawnTick, 0)
		v.StopTicks = append(v.StopTicks, 0)
		v.DistanceMM = append(v.DistanceMM, 0)
		v.ReplanAt = append(v.ReplanAt, 0)
		v.Payload = append(v.Payload, -1)
		v.TransitRoute = append(v.TransitRoute, -1)
		v.TransitLeg = append(v.TransitLeg, 0)
		v.Occupancy = append(v.Occupancy, 0)
		v.RiderHead = append(v.RiderHead, -1)
		v.DwellTicks = append(v.DwellTicks, 0)
		v.TransitDir = append(v.TransitDir, 0)
	}
	v.Kind[id] = kind
	v.Status[id] = VehMoving
	v.Agent[id] = agent
	v.Edge[id] = edge
	v.Pos[id] = 0
	v.Speed[id] = 0
	v.RouteStart[id], v.RouteLen[id], v.RouteIdx[id] = 0, 0, 0
	v.Dest[id] = dest
	v.SpawnTick[id] = uint32(s.Tick)
	v.StopTicks[id] = 0
	v.DistanceMM[id] = 0
	v.ReplanAt[id] = uint32(s.Tick)
	v.Payload[id] = -1
	v.TransitRoute[id] = -1
	v.TransitLeg[id] = 0
	v.Occupancy[id] = 0
	v.RiderHead[id] = -1
	v.DwellTicks[id] = 0
	v.TransitDir[id] = 0
	return id
}

// FreeVehicle releases a slot. Serial phase only.
func (s *State) FreeVehicle(id int32) {
	v := &s.Vehicles
	if v.Status[id] == VehIdle {
		return
	}
	s.RouteLive -= v.RouteLen[id]
	v.Status[id] = VehIdle
	v.Edge[id] = world.NoEdge
	v.Agent[id] = -1
	v.RouteLen[id] = 0
	v.RouteStart[id] = 0
	v.free = append(v.free, id)
}

// AllocRoute copies a path into the arena and points the vehicle at it.
func (s *State) AllocRoute(id int32, path []world.EdgeID) {
	v := &s.Vehicles
	s.RouteLive -= v.RouteLen[id]
	start := int32(len(s.RouteBuf))
	s.RouteBuf = append(s.RouteBuf, path...)
	v.RouteStart[id] = start
	v.RouteLen[id] = int32(len(path))
	v.RouteIdx[id] = 0
	s.RouteLive += int32(len(path))
}

// Route returns a vehicle's remaining path.
func (s *State) Route(id int32) []world.EdgeID {
	v := &s.Vehicles
	st := v.RouteStart[id]
	return s.RouteBuf[st : st+v.RouteLen[id]]
}

// CompactRoutes garbage-collects the route arena. Called from the serial phase
// when waste exceeds 50%; walks vehicles in id order so the resulting layout is
// a pure function of state, which keeps checkpoints byte-comparable.
func (s *State) CompactRoutes() {
	if len(s.RouteBuf) < 4096 || int(s.RouteLive)*2 > len(s.RouteBuf) {
		return
	}
	v := &s.Vehicles
	nb := make([]world.EdgeID, 0, s.RouteLive+s.RouteLive/4+64)
	for id := 0; id < v.Len(); id++ {
		if v.Status[id] == VehIdle || v.RouteLen[id] == 0 {
			v.RouteStart[id], v.RouteLen[id] = 0, 0
			continue
		}
		st := v.RouteStart[id]
		seg := s.RouteBuf[st : st+v.RouteLen[id]]
		v.RouteStart[id] = int32(len(nb))
		nb = append(nb, seg...)
	}
	s.RouteBuf = nb
	s.RouteLive = int32(len(nb))
}

// ClockHM returns the simulated wall clock.
func (s *State) ClockHM() (int, int) { return s.Tick.ClockHM(int(s.StartHour)) }

// TickOfDay is the tick index within the simulated day, used by every
// schedule-driven system.
func (s *State) TickOfDay() int32 {
	return int32((uint64(s.Tick) + uint64(s.StartHour)*units.TicksPerHour) % units.TicksPerDay)
}
