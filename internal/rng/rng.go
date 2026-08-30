// Package rng provides MIRROR's only source of randomness.
//
// Rules enforced by convention and by test:
//   - math/rand and crypto/rand are banned inside internal/systems and
//     internal/engine (see TestNoForbiddenImports).
//   - A generator is never shared across systems or across regions. Instead a
//     generator is *derived* from (worldSeed, stream, tick, entityID) so that
//     the value an entity draws does not depend on how many other entities
//     drew before it. This is what makes parallel region execution produce
//     byte-identical results to serial execution.
//
// See docs/adr/ADR-004-determinism.md.
package rng

import "math/bits"

// PCG32 is a permuted congruential generator (O'Neill 2014). Chosen over
// xorshift/splitmix because it has a cheap, well-defined *stream* parameter
// which is exactly the property we need for per-entity derivation, and over
// math/rand because the Go runtime reserves the right to change math/rand's
// algorithm between releases -- a replay log written by go1.24 must still
// replay bit-identically under go1.30.
type PCG32 struct {
	state uint64
	inc   uint64 // stream selector, always odd
}

const (
	pcgMult = 6364136223846793005
	pcgInit = 1442695040888963407
)

// New creates a generator from a seed and a stream id.
func New(seed, stream uint64) PCG32 {
	p := PCG32{state: 0, inc: (stream << 1) | 1}
	p.Uint32()
	p.state += seed
	p.Uint32()
	return p
}

func (p *PCG32) Uint32() uint32 {
	old := p.state
	p.state = old*pcgMult + p.inc
	xorshifted := uint32(((old >> 18) ^ old) >> 27)
	rot := uint32(old >> 59)
	return bits.RotateLeft32(xorshifted, -int(rot))
}

func (p *PCG32) Uint64() uint64 {
	return uint64(p.Uint32())<<32 | uint64(p.Uint32())
}

// IntN returns a uniform value in [0,n) using Lemire's debiased multiply-shift.
// The rejection loop is what makes it exactly uniform; a plain modulo would be
// biased, and the bias is large enough at small n to show up in aggregate
// simulation metrics.
func (p *PCG32) IntN(n int32) int32 {
	if n <= 1 {
		return 0
	}
	un := uint32(n)
	x := p.Uint32()
	m := uint64(x) * uint64(un)
	l := uint32(m)
	if l < un {
		t := (-un) % un
		for l < t {
			x = p.Uint32()
			m = uint64(x) * uint64(un)
			l = uint32(m)
		}
	}
	return int32(m >> 32)
}

// Permille returns a value in [0,1000).
func (p *PCG32) Permille() int32 { return p.IntN(1000) }

// Chance reports whether an event with probability p/1000 occurs.
func (p *PCG32) Chance(perMille int32) bool { return p.Permille() < perMille }

// Range returns a uniform value in [lo,hi].
func (p *PCG32) Range(lo, hi int32) int32 {
	if hi <= lo {
		return lo
	}
	return lo + p.IntN(hi-lo+1)
}

// Stream ids. Every system that draws randomness owns a distinct constant so
// that adding a new system can never shift the number sequence consumed by an
// existing one -- i.e. adding a feature does not silently invalidate every
// stored replay.
const (
	StreamWorldGen   uint64 = 1
	StreamPopulation uint64 = 2
	StreamDeparture  uint64 = 3
	StreamRouting    uint64 = 4
	StreamIncident   uint64 = 5
	StreamWeather    uint64 = 6
	StreamHealth     uint64 = 7
	StreamPower      uint64 = 8
	StreamTransit    uint64 = 9
	StreamDriver     uint64 = 10
	StreamChaos      uint64 = 11
)

// Derive builds a generator for one (stream, tick, entity) triple.
//
// The mixing function is SplitMix64's finaliser applied to a combination of the
// inputs. It matters that this is a *hash*, not a concatenation: entity ids and
// ticks are both small and highly correlated, and a weak mix produces visibly
// correlated behaviour between neighbouring vehicles (they all decide to
// re-route on the same tick, which looks like a bug and behaves like one).
func Derive(worldSeed uint64, stream uint64, tick uint64, entity uint64) PCG32 {
	h := mix(worldSeed ^ (stream * 0x9E3779B97F4A7C15))
	h = mix(h ^ (tick * 0xBF58476D1CE4E5B9))
	h = mix(h ^ (entity * 0x94D049BB133111EB))
	return New(h, stream*0x2545F4914F6CDD1D+entity+1)
}

func mix(z uint64) uint64 {
	z += 0x9E3779B97F4A7C15
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}
