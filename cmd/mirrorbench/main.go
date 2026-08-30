// Command mirrorbench runs reproducible simulation benchmarks.
//
// Every number this project claims about its own performance comes from here.
// The rules it follows are the ones that make a benchmark worth reading:
//
//   - Fixed seed, fixed map, fixed population. A benchmark whose workload
//     varies between runs measures the workload, not the code.
//   - A warm-up period before measurement, because the first few hundred ticks
//     have almost no vehicles on the network and would flatter every result.
//   - Percentiles, not just a mean. Mean tick time hides exactly the stalls
//     that make a real-time system feel broken.
//   - The serial fraction is reported alongside the parallel speed-up, because
//     that is the number that predicts where scaling stops.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"time"

	"github.com/mirror-sim/mirror/internal/engine"
	"github.com/mirror-sim/mirror/internal/events"
	"github.com/mirror-sim/mirror/internal/units"
)

type result struct {
	Name           string  `json:"name"`
	Preset         string  `json:"preset"`
	Population     int     `json:"population"`
	Regions        int     `json:"regions"`
	Nodes          int     `json:"nodes"`
	Edges          int     `json:"edges"`
	Ticks          int     `json:"ticks"`
	WallSeconds    float64 `json:"wallSeconds"`
	TicksPerSec    float64 `json:"ticksPerSecond"`
	RealtimeRatio  float64 `json:"realtimeMultiple"`
	TickMeanMs     float64 `json:"tickMeanMs"`
	TickP50Ms      float64 `json:"tickP50Ms"`
	TickP95Ms      float64 `json:"tickP95Ms"`
	TickP99Ms      float64 `json:"tickP99Ms"`
	TickMaxMs      float64 `json:"tickMaxMs"`
	SerialPct      float64 `json:"serialPercentMean"`
	PeakVehicles   int32   `json:"peakActiveVehicles"`
	VehicleTicks   int64   `json:"vehicleTicksPerSecond"`
	Crossings      int64   `json:"edgeCrossings"`
	RouteQueries   int64   `json:"routeQueries"`
	TripsDone      int64   `json:"tripsCompleted"`
	AllocMB        float64 `json:"heapAllocMB"`
	AllocPerTickKB float64 `json:"allocPerTickKB"`
	GlobalMs       float64 `json:"globalPhaseMs"`
	PhaseA1Ms      float64 `json:"phaseA1Ms"`
	PhaseA2Ms      float64 `json:"phaseA2Ms"`
	PhaseBMs       float64 `json:"phaseBCommitMs"`
	Digest         string  `json:"digest"`
}

func main() {
	var (
		preset   = flag.String("preset", "medium", "city preset")
		pop      = flag.Int("population", 60000, "population")
		ticks    = flag.Int("ticks", 6000, "measured ticks")
		warmup   = flag.Int("warmup", 42000, "warm-up ticks, not measured; the default advances the clock into the morning peak")
		regions  = flag.String("regions", "1,2,3,4,6,9", "comma-separated region counts to sweep")
		seed     = flag.Uint64("seed", 20260830, "world seed")
		start    = flag.Int("start-hour", 7, "simulated start hour")
		asJSON   = flag.Bool("json", false, "emit JSON instead of a table")
		incident = flag.Bool("incident", true, "inject a major incident during the measured window")
	)
	flag.Parse()

	counts := parseInts(*regions)
	results := make([]result, 0, len(counts))

	fmt.Fprintf(os.Stderr, "MIRROR benchmark  go=%s  GOMAXPROCS=%d  preset=%s  population=%d\n",
		runtime.Version(), runtime.GOMAXPROCS(0), *preset, *pop)
	fmt.Fprintf(os.Stderr, "warm-up=%d ticks, measured=%d ticks (%s of simulated time)\n\n",
		*warmup, *ticks, dur(*ticks))

	for _, n := range counts {
		results = append(results, run(*preset, *pop, *seed, int32(*start), n, *warmup, *ticks, *incident))
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"go": runtime.Version(), "gomaxprocs": runtime.GOMAXPROCS(0),
			"results": results,
		})
		return
	}
	printTable(results)
}

func run(preset string, pop int, seed uint64, startHour int32, regions, warmup, ticks int, incident bool) result {
	cfg := engine.DefaultConfig()
	cfg.Preset, cfg.Population, cfg.Seed = preset, pop, seed
	cfg.StartHour, cfg.Regions, cfg.Workers = startHour, regions, regions

	e := engine.New(cfg)
	r := result{
		Name:   fmt.Sprintf("%s/%dk/r%d", preset, pop/1000, regions),
		Preset: preset, Population: pop, Regions: e.Cfg.Regions,
		Nodes: len(e.Map.Nodes), Edges: len(e.Map.Edges), Ticks: ticks,
	}

	// Warm-up. The network is empty at tick 0; measuring from there would
	// report the speed of an empty city.
	for i := 0; i < warmup; i++ {
		e.Tick()
	}

	if incident {
		// A reproducible shock in the middle of the measured window: this is
		// where rerouting cost spikes, and a benchmark that never triggers it
		// systematically understates the worst case.
		mid := e.S.Tick + units.Tick(ticks/2)
		_, _ = e.Inject(events.C(mid, events.CmdInjectAccident, int64(len(e.Map.Edges)/3), 900, 3, 0))
		_, _ = e.Inject(events.C(mid+50, events.CmdCloseRoad, int64(len(e.Map.Edges)/2), 4000, 0, 0))
	}

	var ms0, ms1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms0)

	durations := make([]float64, 0, ticks)
	var serialSum, globalSum, a1Sum, a2Sum, bSum float64
	var crossings, vehTicks int64
	var peak int32

	wall0 := time.Now()
	for i := 0; i < ticks; i++ {
		e.Tick()
		st := e.Stat
		durations = append(durations, float64(st.TotalNanos)/1e6)
		serialSum += float64(st.SerialPercent)
		globalSum += float64(st.GlobalNanos) / 1e6
		a1Sum += float64(st.PhaseA1Nanos) / 1e6
		a2Sum += float64(st.PhaseANanos-st.PhaseA1Nanos) / 1e6
		bSum += float64(st.PhaseBNanos) / 1e6
		crossings += st.Crossings
		vehTicks += int64(st.ActiveVeh)
		if st.ActiveVeh > peak {
			peak = st.ActiveVeh
		}
	}
	wall := time.Since(wall0)
	runtime.ReadMemStats(&ms1)

	sort.Float64s(durations)
	r.WallSeconds = wall.Seconds()
	r.TicksPerSec = float64(ticks) / wall.Seconds()
	r.RealtimeRatio = r.TicksPerSec / units.TicksPerSecond
	r.TickMeanMs = mean(durations)
	r.TickP50Ms = pctile(durations, 0.50)
	r.TickP95Ms = pctile(durations, 0.95)
	r.TickP99Ms = pctile(durations, 0.99)
	r.TickMaxMs = durations[len(durations)-1]
	r.SerialPct = serialSum / float64(ticks)
	r.PeakVehicles = peak
	r.VehicleTicks = int64(float64(vehTicks) / wall.Seconds())
	r.Crossings = crossings
	r.RouteQueries = e.S.Metrics.RouteQueries
	r.TripsDone = e.S.Metrics.TripsCompleted
	r.AllocMB = float64(ms1.HeapAlloc) / (1 << 20)
	r.AllocPerTickKB = float64(ms1.TotalAlloc-ms0.TotalAlloc) / float64(ticks) / 1024
	r.GlobalMs = globalSum / float64(ticks)
	r.PhaseA1Ms = a1Sum / float64(ticks)
	r.PhaseA2Ms = a2Sum / float64(ticks)
	r.PhaseBMs = bSum / float64(ticks)
	r.Digest = fmt.Sprintf("%016x", e.S.Digest())
	return r
}

func printTable(rs []result) {
	fmt.Printf("%-18s %7s %7s %8s %9s %8s %8s %8s %8s %7s %10s %12s\n",
		"scenario", "regions", "peakVeh", "ticks/s", "realtime", "meanMs", "p95Ms", "p99Ms", "maxMs", "ser%", "allocKB/t", "vehTicks/s")
	fmt.Println(repeat('-', 126))
	for _, r := range rs {
		fmt.Printf("%-18s %7d %7d %8.0f %8.1fx %8.2f %8.2f %8.2f %8.2f %6.1f%% %10.1f %12d\n",
			r.Name, r.Regions, r.PeakVehicles, r.TicksPerSec, r.RealtimeRatio,
			r.TickMeanMs, r.TickP95Ms, r.TickP99Ms, r.TickMaxMs, r.SerialPct,
			r.AllocPerTickKB, r.VehicleTicks)
	}
	if len(rs) > 1 {
		base := rs[0]
		fmt.Println()
		fmt.Println("Parallel scaling against the single-region run:")
		for _, r := range rs[1:] {
			speedup := r.TicksPerSec / base.TicksPerSec
			// Amdahl's ceiling from the measured serial fraction. Printing the
			// prediction next to the measurement is the honest way to present
			// a scaling result: if they diverge, the model is wrong and that
			// is worth knowing.
			f := base.SerialPct / 100
			ideal := 1 / (f + (1-f)/float64(r.Regions))
			fmt.Printf("  regions=%-2d  speedup %4.2fx   Amdahl ceiling at %.0f%% serial: %4.2fx   efficiency %3.0f%%\n",
				r.Regions, speedup, base.SerialPct, ideal, speedup/ideal*100)
		}
	}
	fmt.Println()
	fmt.Println("Mean phase breakdown (ms):")
	fmt.Printf("  %-18s %10s %14s %14s %10s\n", "scenario", "global", "A1 speeds/sig", "A2 move/route", "B commit")
	for _, r := range rs {
		fmt.Printf("  %-18s %10.3f %14.3f %14.3f %10.3f\n",
			r.Name, r.GlobalMs, r.PhaseA1Ms, r.PhaseA2Ms, r.PhaseBMs)
	}
	if rs[0].TickP50Ms == 0 {
		fmt.Println()
		fmt.Println("Note: the per-tick clock on this platform is coarser than a tick takes, so the")
		fmt.Println("percentile columns are quantised. ticks/s is measured over the whole run and is")
		fmt.Println("unaffected; treat it as the authoritative throughput number.")
	}
	fmt.Println()
	fmt.Println("Digests (identical across region counts means the partitioning is deterministic):")
	for _, r := range rs {
		fmt.Printf("  %-18s %s\n", r.Name, r.Digest)
	}
	same := true
	for _, r := range rs {
		if r.Digest != rs[0].Digest {
			same = false
		}
	}
	if same {
		fmt.Println("\n  All region counts produced identical state.")
	} else {
		fmt.Println("\n  WARNING: region counts produced different state. Determinism is broken.")
	}
}

func mean(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

func pctile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p)
	return sorted[i]
}

func parseInts(s string) []int {
	var out []int
	cur, has := 0, false
	for i := 0; i <= len(s); i++ {
		if i < len(s) && s[i] >= '0' && s[i] <= '9' {
			cur = cur*10 + int(s[i]-'0')
			has = true
			continue
		}
		if has {
			out = append(out, cur)
		}
		cur, has = 0, false
	}
	if len(out) == 0 {
		out = []int{1}
	}
	return out
}

func dur(ticks int) string {
	sec := ticks / units.TicksPerSecond
	return fmt.Sprintf("%dm%02ds", sec/60, sec%60)
}

func repeat(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
