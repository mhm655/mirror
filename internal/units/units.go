// Package units defines MIRROR's canonical measurement types.
//
// DETERMINISM RULE: no floating point ever enters simulation state.
// Every quantity that participates in a state transition is an integer in a
// fixed unit. Floats are permitted only at the presentation boundary
// (JSON metrics, the wire protocol, the UI) where divergence is harmless.
//
// See docs/adr/ADR-004-determinism.md.
package units

// Millimetres. The base length unit. int64 gives us +/- 9.2e18 mm, i.e.
// ~9.7 billion light years of headroom -- overflow is not a concern.
type MM int64

// MMPerTick is a speed. One tick is TickMillis of simulated time.
type MMPerTick int64

// Tick is a discrete simulation step index. Ticks are the only clock the
// simulation knows about; wall-clock time never influences state.
type Tick uint64

const (
	// TickMillis is the simulated duration of one tick.
	//
	// 100ms chosen because: (a) at urban speeds (50 km/h = 13.9 m/s) a vehicle
	// moves 1.39m per tick, which is finer than the ~2m resolution a human can
	// perceive on a city-scale map; (b) traffic signal phases are naturally
	// expressed in whole seconds, i.e. exactly 10 ticks; (c) 10 Hz is a
	// comfortable upper bound for a WebSocket broadcast rate.
	TickMillis = 100

	TicksPerSecond = 1000 / TickMillis
	TicksPerMinute = 60 * TicksPerSecond
	TicksPerHour   = 60 * TicksPerMinute
	TicksPerDay    = 24 * TicksPerHour

	Metre      = MM(1000)
	Kilometre  = 1000 * Metre
	Centimetre = MM(10)
)

// KmhToMMPerTick converts km/h to the internal speed unit.
// 1 km/h = 1e6 mm / 3600000 ms = 0.2777... mm/ms -> * TickMillis.
// Computed in integers with round-to-nearest to keep the conversion total.
func KmhToMMPerTick(kmh int64) MMPerTick {
	num := kmh * 1_000_000 * TickMillis
	den := int64(3_600_000)
	return MMPerTick((num + den/2) / den)
}

// MMPerTickToKmh is the inverse, for display only.
func MMPerTickToKmh(v MMPerTick) int64 {
	return (int64(v)*3_600_000 + 500_000*TickMillis) / (1_000_000 * TickMillis)
}

// Seconds converts a tick count to whole simulated seconds.
func (t Tick) Seconds() uint64 { return uint64(t) / TicksPerSecond }

// ClockHM returns the simulated wall clock (hour, minute) assuming the run
// started at StartHour.
func (t Tick) ClockHM(startHour int) (int, int) {
	tod := (uint64(t) + uint64(startHour)*TicksPerHour) % TicksPerDay
	h := tod / TicksPerHour
	m := (tod % TicksPerHour) / TicksPerMinute
	return int(h), int(m)
}

// Fixed-point helpers. Scale is 1/1000 -- "permille". Used for congestion
// factors, probabilities and utilisation ratios so that no float sneaks in.
type Permille int32

const One Permille = 1000

// MulP multiplies an integer by a permille factor with round-to-nearest,
// half-away-from-zero. Symmetric rounding matters: asymmetric rounding
// (Go's truncation toward zero) makes results depend on the sign of
// intermediate values, which is a classic source of replay divergence.
func MulP(v int64, p Permille) int64 {
	n := v * int64(p)
	if n >= 0 {
		return (n + 500) / 1000
	}
	return -((-n + 500) / 1000)
}

// DivRound performs round-to-nearest integer division, half away from zero.
func DivRound(a, b int64) int64 {
	if b == 0 {
		return 0
	}
	if (a >= 0) == (b > 0) {
		return (a + b/2) / b
	}
	return -((-a + b/2) / b)
}

// ISqrt is a deterministic integer square root (Newton's method on integers).
// Used by the routing heuristic; math.Sqrt is avoided because its exactness is
// guaranteed by IEEE-754 but its *availability* across GOARCH/compiler versions
// under FMA contraction is not something we want to depend on.
func ISqrt(n int64) int64 {
	if n <= 0 {
		return 0
	}
	x := n
	y := (x + 1) / 2
	for y < x {
		x = y
		y = (x + n/x) / 2
	}
	return x
}
