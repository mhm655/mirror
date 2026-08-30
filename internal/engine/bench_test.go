package engine

import "testing"

func BenchmarkTickMedium60k(b *testing.B) {
	cfg := DefaultConfig()
	cfg.Preset, cfg.Population, cfg.Seed = "medium", 60000, 20260830
	cfg.Regions, cfg.Workers = 1, 1
	e := New(cfg)
	for i := 0; i < 42000; i++ {
		e.Tick()
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.Tick()
	}
}
