package units

import (
	"math/rand"
	"testing"
)

// TestISqrtExact checks the fast seeded square root against the slow but
// obviously-correct integer Newton method it replaced, across the full range
// of values the simulation can produce.
func TestISqrtExact(t *testing.T) {
	newton := func(n int64) int64 {
		if n <= 0 {
			return 0
		}
		x, y := n, (n+1)/2
		for y < x {
			x = y
			y = (x + n/x) / 2
		}
		return x
	}
	check := func(n int64) {
		t.Helper()
		got, want := ISqrt(n), newton(n)
		if got != want {
			t.Fatalf("ISqrt(%d) = %d, want %d", n, got, want)
		}
		if got*got > n || (got+1)*(got+1) <= n {
			t.Fatalf("ISqrt(%d) = %d is not the floor of the true root", n, got)
		}
	}
	for n := int64(0); n < 100000; n++ {
		check(n)
	}
	// Perfect squares and their neighbours are where a seeded root goes wrong.
	for r := int64(1); r < 30000000; r += 99991 {
		check(r * r)
		check(r*r - 1)
		check(r*r + 1)
	}
	g := rand.New(rand.NewSource(7))
	for i := 0; i < 400000; i++ {
		check(g.Int63n(450000000000000))
	}
}

func BenchmarkISqrt(b *testing.B) {
	var acc int64
	for i := 0; i < b.N; i++ {
		acc += ISqrt(int64(i)*7919 + 1)
	}
	_ = acc
}
