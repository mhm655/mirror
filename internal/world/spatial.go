package world

import (
	"sort"

	"github.com/mirror-sim/mirror/internal/units"
)

// SpatialGrid is a uniform bucket grid over node positions.
//
// Why not a quadtree/R-tree: the road network is close to uniformly dense by
// construction (city blocks), so a uniform grid gives the same asymptotics with
// a fraction of the constant factor and, crucially, with zero pointer chasing.
// Bucket contents are sorted by id, so every query returns results in a
// canonical order -- a k-nearest query must never depend on insertion order or
// the whole simulation stops being reproducible.
type SpatialGrid struct {
	minX, minY units.MM
	cell       units.MM
	cols, rows int32
	starts     []int32 // len = cols*rows+1, CSR
	items      []NodeID
}

func BuildSpatialGrid(nodes []Node, w, h, cell units.MM) *SpatialGrid {
	g := &SpatialGrid{cell: cell}
	g.cols = int32(w/cell) + 1
	g.rows = int32(h/cell) + 1
	counts := make([]int32, g.cols*g.rows+1)
	idx := make([]int32, len(nodes))
	for i := range nodes {
		c := g.cellOf(nodes[i].X, nodes[i].Y)
		idx[i] = c
		counts[c+1]++
	}
	for i := int32(1); i < int32(len(counts)); i++ {
		counts[i] += counts[i-1]
	}
	g.starts = counts
	g.items = make([]NodeID, len(nodes))
	cursor := make([]int32, g.cols*g.rows)
	for i := range nodes {
		c := idx[i]
		g.items[g.starts[c]+cursor[c]] = NodeID(i)
		cursor[c]++
	}
	for c := int32(0); c < g.cols*g.rows; c++ {
		s := g.items[g.starts[c]:g.starts[c+1]]
		sort.Slice(s, func(a, b int) bool { return s[a] < s[b] })
	}
	return g
}

func (g *SpatialGrid) cellOf(x, y units.MM) int32 {
	cx := int32(x / g.cell)
	cy := int32(y / g.cell)
	if cx < 0 {
		cx = 0
	}
	if cy < 0 {
		cy = 0
	}
	if cx >= g.cols {
		cx = g.cols - 1
	}
	if cy >= g.rows {
		cy = g.rows - 1
	}
	return cy*g.cols + cx
}

// Nearest returns the closest node to (x,y), ties broken by lowest NodeID.
// Search expands ring by ring and stops one ring after the first hit, which is
// the standard correctness condition for grid nearest-neighbour.
func (g *SpatialGrid) Nearest(nodes []Node, x, y units.MM) NodeID {
	c := g.cellOf(x, y)
	ccx, ccy := c%g.cols, c/g.cols
	best := NoNode
	var bestD int64 = 1<<62 - 1
	maxR := g.cols
	if g.rows > maxR {
		maxR = g.rows
	}
	foundRing := int32(-1)
	for r := int32(0); r <= maxR; r++ {
		if foundRing >= 0 && r > foundRing+1 {
			break
		}
		for dy := -r; dy <= r; dy++ {
			for dx := -r; dx <= r; dx++ {
				// Only the ring's perimeter.
				if r > 0 && dx != -r && dx != r && dy != -r && dy != r {
					continue
				}
				cx, cy := ccx+dx, ccy+dy
				if cx < 0 || cy < 0 || cx >= g.cols || cy >= g.rows {
					continue
				}
				cc := cy*g.cols + cx
				for _, n := range g.items[g.starts[cc]:g.starts[cc+1]] {
					ddx := int64(nodes[n].X - x)
					ddy := int64(nodes[n].Y - y)
					d := ddx*ddx + ddy*ddy
					if d < bestD || (d == bestD && n < best) {
						bestD, best = d, n
					}
				}
			}
		}
		if best != NoNode && foundRing < 0 {
			foundRing = r
		}
	}
	return best
}

// InRect returns node ids whose position falls in the rectangle, in ascending
// id order. Used by the viewport query on the WebSocket path.
func (g *SpatialGrid) InRect(nodes []Node, x0, y0, x1, y1 units.MM, out []NodeID) []NodeID {
	out = out[:0]
	c0 := g.cellOf(x0, y0)
	c1 := g.cellOf(x1, y1)
	for cy := c0 / g.cols; cy <= c1/g.cols; cy++ {
		for cx := c0 % g.cols; cx <= c1%g.cols; cx++ {
			cc := cy*g.cols + cx
			for _, n := range g.items[g.starts[cc]:g.starts[cc+1]] {
				nd := &nodes[n]
				if nd.X >= x0 && nd.X <= x1 && nd.Y >= y0 && nd.Y <= y1 {
					out = append(out, n)
				}
			}
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
	return out
}
