package api

import (
	"testing"

	"github.com/mirror-sim/mirror/internal/state"
	"github.com/mirror-sim/mirror/internal/units"
	"github.com/mirror-sim/mirror/internal/world"
)

// twoNodeMap is a single edge from (0,0) to (1000,0) millimetres, just enough
// for vehiclePos to interpolate along.
func twoNodeMap(length units.MM) *world.Map {
	return &world.Map{
		Nodes: []world.Node{
			{X: 0, Y: 0},
			{X: 1000, Y: 0},
		},
		Edges: []world.Edge{
			{From: 0, To: 1, Length: length},
		},
	}
}

func TestVehiclePosInterpolatesAlongEdge(t *testing.T) {
	m := twoNodeMap(1000)
	st := &state.State{Vehicles: state.Vehicles{
		Edge: []world.EdgeID{0},
		Pos:  []units.MM{250}, // a quarter of the way along
	}}
	x, y := vehiclePos(m, st, 0)
	if x != 250 || y != 0 {
		t.Errorf("vehiclePos at 25%% = (%d, %d), want (250, 0)", x, y)
	}
}

func TestVehiclePosClampsPastEdgeEnd(t *testing.T) {
	m := twoNodeMap(1000)
	st := &state.State{Vehicles: state.Vehicles{
		Edge: []world.EdgeID{0},
		Pos:  []units.MM{5000}, // overshoots the edge length
	}}
	x, y := vehiclePos(m, st, 0)
	if x != 1000 || y != 0 {
		t.Errorf("vehiclePos overshooting edge = (%d, %d), want clamped to (1000, 0)", x, y)
	}
}

func TestVehiclePosZeroLengthEdgeReturnsOrigin(t *testing.T) {
	m := twoNodeMap(0)
	st := &state.State{Vehicles: state.Vehicles{
		Edge: []world.EdgeID{0},
		Pos:  []units.MM{0},
	}}
	x, y := vehiclePos(m, st, 0)
	if x != 0 || y != 0 {
		t.Errorf("vehiclePos on a zero-length edge = (%d, %d), want the from-node position", x, y)
	}
}
