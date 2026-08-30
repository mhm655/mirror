// Package world holds MIRROR's immutable static world: the road graph, land
// use, and the topology of the power/water/comms networks.
//
// Everything in this package is written once at generation time and is
// thereafter READ-ONLY. That is not a stylistic preference -- it is the single
// most load-bearing decision in the counterfactual architecture. Because the
// map never changes, a thousand forked scenarios share one copy of it by
// pointer. Forking a simulation copies only the dynamic state (see
// internal/state), which is roughly 200 bytes per vehicle rather than the tens
// of megabytes a full world copy would cost.
//
// Things that *do* change (a road closed by an accident, a substation that
// tripped) live in internal/state as an overlay keyed by the static id.
package world

import (
	"github.com/mirror-sim/mirror/internal/units"
)

type (
	NodeID       int32
	EdgeID       int32
	DistrictID   int32
	POIID        int32
	SubstationID int32
	HospitalID   int32
	DepotID      int32
	TowerID      int32
	RouteID      int32 // transit line
)

const NoEdge EdgeID = -1
const NoNode NodeID = -1

type RoadClass uint8

const (
	ClassLocal RoadClass = iota
	ClassCollector
	ClassArterial
	ClassMotorway
)

var roadClassName = [...]string{"local", "collector", "arterial", "motorway"}

func (c RoadClass) String() string { return roadClassName[c] }

type POIKind uint8

const (
	POIHome POIKind = iota
	POIWork
	POISchool
	POIHospital
	POIBusiness
	POIDepot
)

var poiKindName = [...]string{"home", "work", "school", "hospital", "business", "depot"}

func (k POIKind) String() string { return poiKindName[k] }

// Node is an intersection.
type Node struct {
	X, Y     units.MM
	District DistrictID
	Signal   int32 // index into Map.Signals, or -1
	// CSR slices into Map.OutEdges / Map.InEdges.
	OutStart, OutEnd int32
	InStart, InEnd   int32
}

// Edge is a one-way road segment. A two-way street is two edges; this doubles
// edge count but removes every "which direction am I going" branch from the
// movement hot loop, which is worth far more than the memory.
type Edge struct {
	From, To  NodeID
	Length    units.MM
	FreeSpeed units.MMPerTick
	Lanes     int32
	// Jam is the number of vehicles that fit on the edge bumper to bumper.
	// Derived at generation time from length and lanes (7m per vehicle slot).
	Jam      int32
	Class    RoadClass
	District DistrictID // owner region = district of the To node
	// SignalPhase is which phase of the destination node's signal must be
	// active for a vehicle to leave this edge. -1 = uncontrolled.
	SignalPhase int32
	// Serving substation for the streetlight/signal feed on this edge.
	Feeder SubstationID
}

// Signal describes a fixed-time traffic controller. Adaptive control is a
// *policy* applied in state, not a different static object -- so the same map
// serves both the baseline and the adaptive-lights counterfactual.
type Signal struct {
	Node NodeID
	// PhaseTicks is the baseline green duration of each phase.
	PhaseTicks [4]int32
	NumPhases  int32
	// Offset staggers adjacent signals to create a green wave along arterials.
	Offset int32
	Feeder SubstationID
}

type District struct {
	ID                     DistrictID
	Name                   string
	MinX, MinY, MaxX, MaxY units.MM
	CX, CY                 units.MM
}

type POI struct {
	ID       POIID
	Kind     POIKind
	Node     NodeID
	X, Y     units.MM
	District DistrictID
	Capacity int32 // residents / jobs / pupils / beds
	Feeder   SubstationID
	Name     string
}

// Substation feeds a contiguous set of POIs, signals and towers. The grid is a
// forest, not a mesh: each substation has an upstream Grid connection and each
// consumer has exactly one feeder. Real grids are meshed and self-healing; we
// model a radial distribution network because that is what actually fails in
// the interesting way (one substation trips, everything downstream goes dark).
type Substation struct {
	ID       SubstationID
	Name     string
	X, Y     units.MM
	District DistrictID
	// CapacityKW is the thermal limit. Exceeding it for SustainTicks trips it.
	CapacityKW int32
	// BaseKW is the connected load of everything fed by this substation at a
	// demand factor of 1.0. Precomputed at generation time because it is a
	// property of the static network, not of the simulation.
	BaseKW int32
	// Neighbours it can shed load onto, sorted ascending for determinism.
	Neighbours []SubstationID
}

type Hospital struct {
	ID         HospitalID
	Name       string
	Node       NodeID
	X, Y       units.MM
	District   DistrictID
	Beds       int32
	ERBays     int32
	Ambulances int32
	Feeder     SubstationID
	// BackupMinutes of generator fuel.
	BackupMinutes int32
}

// Depot is a fire/police station or ambulance base.
type Depot struct {
	ID       DepotID
	Name     string
	Kind     uint8 // 0 fire, 1 police, 2 ambulance
	Node     NodeID
	X, Y     units.MM
	District DistrictID
	Units    int32
}

// Tower is a comms cell site.
type Tower struct {
	ID          TowerID
	Node        NodeID
	X, Y        units.MM
	District    DistrictID
	CapacityErl int32 // simultaneous sessions
	Feeder      SubstationID
	BatteryMin  int32
}

// TransitRoute is a bus or metro line: an ordered list of stop nodes plus the
// precomputed edge path between consecutive stops. Precomputing here means the
// transit system never calls the router at runtime, which keeps it O(1).
type TransitRoute struct {
	ID          RouteID
	Name        string
	Mode        uint8 // 0 bus, 1 metro
	Stops       []NodeID
	Legs        [][]EdgeID // Legs[i] is stop i -> stop i+1
	HeadwayTick int32      // baseline service interval
	Capacity    int32
	Vehicles    int32
}

// Map is the immutable world. Never mutate after Generate returns.
type Map struct {
	Name        string
	Seed        uint64
	Width       units.MM
	Height      units.MM
	Nodes       []Node
	Edges       []Edge
	OutEdges    []EdgeID // CSR, sorted by (Class desc, EdgeID) per node
	InEdges     []EdgeID
	Signals     []Signal
	Districts   []District
	POIs        []POI
	Substations []Substation
	Hospitals   []Hospital
	Depots      []Depot
	Towers      []Tower
	Routes      []TransitRoute

	// Derived indices, all read-only.
	HomesByDistrict [][]POIID
	WorksByDistrict [][]POIID
	Grid            *SpatialGrid
	// ReverseEdge[e] is the edge running the opposite way along the same link,
	// or NoEdge. Transit routes traverse it on the return run, and the
	// emergency dispatcher uses it to reach the far side of a blocked road.
	ReverseEdge []EdgeID
	// RouteStopBase flattens (route, stopIndex) into a single index space so
	// per-stop dynamic arrays can be plain slices instead of nested ones.
	RouteStopBase []int32
	TotalStops    int32
	// MaxSpeed is the highest free-flow speed in the network. Precomputed at
	// generation time rather than memoised lazily: the Map is shared read-only
	// across every region goroutine and every forked scenario, so a lazily
	// filled cache field would be a data race on the hottest object we have.
	MaxSpeed units.MMPerTick
	// Hash of the entire structure; two runs with the same map hash are
	// comparable, two runs with different hashes are not.
	Hash uint64
}

func (m *Map) Out(n NodeID) []EdgeID {
	nd := &m.Nodes[n]
	return m.OutEdges[nd.OutStart:nd.OutEnd]
}

func (m *Map) In(n NodeID) []EdgeID {
	nd := &m.Nodes[n]
	return m.InEdges[nd.InStart:nd.InEnd]
}

// EdgeTravelTicksFree is the free-flow traversal time, used as the A*
// heuristic's lower bound and as the denominator for delay metrics.
func (m *Map) EdgeTravelTicksFree(e EdgeID) int32 {
	ed := &m.Edges[e]
	if ed.FreeSpeed <= 0 {
		return 1 << 20
	}
	t := int32(units.DivRound(int64(ed.Length), int64(ed.FreeSpeed)))
	if t < 1 {
		t = 1
	}
	return t
}

// MaxFreeSpeed is used to keep the A* heuristic admissible.
func (m *Map) MaxFreeSpeed() units.MMPerTick {
	var s units.MMPerTick
	for i := range m.Edges {
		if m.Edges[i].FreeSpeed > s {
			s = m.Edges[i].FreeSpeed
		}
	}
	if s == 0 {
		s = 1
	}
	return s
}
