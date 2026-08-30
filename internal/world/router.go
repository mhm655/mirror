package world

import (
	"github.com/mirror-sim/mirror/internal/units"
)

// Impassable marks an edge a router must never take.
const Impassable int32 = 1 << 28

// CostFn returns the perceived traversal cost of an edge in ticks.
// It is supplied by the caller so the same router serves:
//   - free-flow routing (cost = length/freeSpeed),
//   - congestion-aware routing (cost from the live edge speed),
//   - emergency routing (congestion-aware but with a blue-light discount),
//   - counterfactual routing under a different policy.
type CostFn func(e EdgeID) int32

// Router is per-goroutine scratch space for A*.
//
// It is explicitly NOT safe for concurrent use, and that is the point: each
// region worker owns one Router, so pathfinding -- by far the most expensive
// operation in the simulation -- allocates nothing in steady state. The
// generation-tag trick (visit) avoids clearing len(nodes) int32s on every
// query, which at 50k queries/sec on a 20k-node graph would otherwise be a
// gigabyte of memset per second.
type Router struct {
	dist    []int32
	prev    []EdgeID
	visit   []uint32
	tag     uint32
	heap    []hitem
	settled []uint32
	// Stats, for the /metrics endpoint. Reset by the caller.
	Expansions uint64
	Queries    uint64
}

type hitem struct {
	f int32
	n NodeID
}

func NewRouter(nodeCount int) *Router {
	return &Router{
		dist:  make([]int32, nodeCount),
		prev:  make([]EdgeID, nodeCount),
		visit: make([]uint32, nodeCount),
		heap:  make([]hitem, 0, 1024),
	}
}

// less defines the total order on the frontier. The node-id tiebreak is
// load-bearing for determinism: without it, two nodes with equal f are popped
// in an order that depends on heap history, and two runs that reach the same
// state by different paths would diverge.
func hless(a, b hitem) bool {
	if a.f != b.f {
		return a.f < b.f
	}
	return a.n < b.n
}

func (r *Router) push(it hitem) {
	r.heap = append(r.heap, it)
	i := len(r.heap) - 1
	for i > 0 {
		p := (i - 1) / 2
		if hless(r.heap[i], r.heap[p]) {
			r.heap[i], r.heap[p] = r.heap[p], r.heap[i]
			i = p
			continue
		}
		break
	}
}

func (r *Router) pop() hitem {
	top := r.heap[0]
	last := len(r.heap) - 1
	r.heap[0] = r.heap[last]
	r.heap = r.heap[:last]
	i := 0
	for {
		l, rr := 2*i+1, 2*i+2
		s := i
		if l < last && hless(r.heap[l], r.heap[s]) {
			s = l
		}
		if rr < last && hless(r.heap[rr], r.heap[s]) {
			s = rr
		}
		if s == i {
			break
		}
		r.heap[i], r.heap[s] = r.heap[s], r.heap[i]
		i = s
	}
	return top
}

// Route computes a least-cost edge path from `from` to `to`.
//
// The heuristic is straight-line distance divided by the network's maximum
// free-flow speed, which is admissible for any CostFn that never returns less
// than the free-flow traversal time -- a property every CostFn in this codebase
// upholds and which TestHeuristicAdmissible checks by brute force on a small
// map. Admissibility matters beyond optimality: an inadmissible heuristic makes
// the path depend on expansion order, which would make routing nondeterministic
// under a different heap implementation.
func (r *Router) Route(m *Map, from, to NodeID, cost CostFn, out []EdgeID) ([]EdgeID, bool) {
	out = out[:0]
	r.Queries++
	if from == to {
		return out, true
	}
	if from < 0 || to < 0 || int(from) >= len(m.Nodes) || int(to) >= len(m.Nodes) {
		return out, false
	}
	r.tag++
	if r.tag == 0 { // wrapped after 4 billion queries
		for i := range r.visit {
			r.visit[i] = 0
		}
		r.tag = 1
	}
	maxSpeed := m.MaxSpeed
	if maxSpeed <= 0 {
		maxSpeed = 1
	}
	tx, ty := m.Nodes[to].X, m.Nodes[to].Y
	h := func(n NodeID) int32 {
		dx := int64(m.Nodes[n].X - tx)
		dy := int64(m.Nodes[n].Y - ty)
		d := units.ISqrt(dx*dx + dy*dy)
		return int32(d / int64(maxSpeed))
	}

	r.heap = r.heap[:0]
	r.visit[from] = r.tag
	r.dist[from] = 0
	r.prev[from] = NoEdge
	r.push(hitem{f: h(from), n: from})

	settled := r.visitScratch(len(m.Nodes))

	for len(r.heap) > 0 {
		cur := r.pop()
		if settled[cur.n] == r.tag {
			continue
		}
		settled[cur.n] = r.tag
		if cur.n == to {
			break
		}
		r.Expansions++
		g := r.dist[cur.n]
		for _, e := range m.Out(cur.n) {
			c := cost(e)
			if c >= Impassable {
				continue
			}
			if c < 1 {
				c = 1
			}
			nb := m.Edges[e].To
			nd := g + c
			if r.visit[nb] != r.tag || nd < r.dist[nb] {
				r.visit[nb] = r.tag
				r.dist[nb] = nd
				r.prev[nb] = e
				r.push(hitem{f: nd + h(nb), n: nb})
			}
		}
	}

	if r.visit[to] != r.tag {
		return out, false
	}
	// Reconstruct backwards, then reverse.
	n := to
	for n != from {
		e := r.prev[n]
		if e == NoEdge {
			return out[:0], false
		}
		out = append(out, e)
		n = m.Edges[e].From
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, true
}

// visitScratch lazily allocates the "settled" bitmap alongside `visit`.
func (r *Router) visitScratch(n int) []uint32 {
	if len(r.settled) < n {
		r.settled = make([]uint32, n)
	}
	return r.settled
}

// PathLength sums edge lengths, used for fuel and distance metrics.
func PathLength(m *Map, path []EdgeID) units.MM {
	var t units.MM
	for _, e := range path {
		t += m.Edges[e].Length
	}
	return t
}
