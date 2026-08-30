package engine

import (
	"fmt"
	"testing"
	"time"

	"github.com/mirror-sim/mirror/internal/units"
)

func TestSmoke(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Preset = "small"
	cfg.Population = 8000
	cfg.Regions = 1
	e := New(cfg)
	t.Logf("map: %d nodes %d edges %d signals %d districts %d routes %d subs %d hosps",
		len(e.Map.Nodes), len(e.Map.Edges), len(e.Map.Signals), len(e.Map.Districts),
		len(e.Map.Routes), len(e.Map.Substations), len(e.Map.Hospitals))
	start := time.Now()
	for i := 0; i < 3600; i++ {
		e.Tick()
		if i%600 == 0 {
			h, m := e.S.ClockHM()
			fmt.Printf("t=%5d %02d:%02d active=%6d started=%6d done=%6d meanTravel=%4ds p95=%4ds crossings=%d tick=%.2fms\n",
				i, h, m, e.Stat.ActiveVeh, e.S.Metrics.TripsStarted, e.S.Metrics.TripsCompleted,
				e.S.Metrics.Travel.Mean()/units.TicksPerSecond,
				e.S.Metrics.Travel.Quantile(950)/units.TicksPerSecond,
				e.Stat.Crossings, float64(e.Stat.TotalNanos)/1e6)
		}
	}
	t.Logf("3600 ticks in %v; digest=%016x", time.Since(start), e.Digest())
	t.Logf("trips started=%d completed=%d abandoned=%d reroutes=%d routeFails=%d",
		e.S.Metrics.TripsStarted, e.S.Metrics.TripsCompleted, e.S.Metrics.TripsAbandoned,
		e.S.Metrics.Reroutes, e.S.Metrics.RouteFailures)
	t.Logf("incidents=%d resolved=%d emergDispatched=%d arrived=%d admissions=%d",
		e.S.Metrics.IncidentsOpened, e.S.Metrics.IncidentsResolved,
		e.S.Metrics.EmergencyDispatched, e.S.Metrics.EmergencyArrived, e.S.Metrics.HospitalAdmissions)
	if e.S.Metrics.TripsStarted == 0 {
		t.Fatal("no trips started")
	}
}
