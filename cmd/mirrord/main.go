// Command mirrord runs the MIRROR simulation server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/mirror-sim/mirror/internal/api"
	"github.com/mirror-sim/mirror/internal/engine"
	"github.com/mirror-sim/mirror/internal/simctl"
	"github.com/mirror-sim/mirror/internal/store"
	"github.com/mirror-sim/mirror/internal/units"
)

var (
	version = "dev"
	commit  = "none"
)

func main() {
	var (
		addr        = flag.String("addr", envOr("MIRROR_ADDR", ":8080"), "listen address")
		dataDir     = flag.String("data", os.Getenv("MIRROR_DATA_DIR"), "checkpoint directory; empty uses memory only")
		webDir      = flag.String("web", envOr("MIRROR_WEB_DIR", "web/dist"), "directory containing the built UI")
		preset      = flag.String("preset", envOr("MIRROR_PRESET", "medium"), "city preset: small, medium, large, huge")
		population  = flag.Int("population", envInt("MIRROR_POPULATION", 40000), "simulated residents")
		seed        = flag.Uint64("seed", uint64(envInt("MIRROR_SEED", 20260830)), "world seed")
		startHour   = flag.Int("start-hour", envInt("MIRROR_START_HOUR", 7), "simulated clock hour at tick 0")
		regions     = flag.Int("regions", envInt("MIRROR_REGIONS", 0), "region workers; 0 uses one per district")
		workers     = flag.Int("workers", envInt("MIRROR_WORKERS", 0), "goroutines for the parallel phase; 0 uses GOMAXPROCS")
		ckptEvery   = flag.Int("checkpoint-minutes", envInt("MIRROR_CHECKPOINT_MINUTES", 30), "simulated minutes between checkpoints")
		noBootstrap = flag.Bool("no-bootstrap", false, "start with no simulations")
		logJSON     = flag.Bool("log-json", os.Getenv("MIRROR_LOG_JSON") == "1", "emit structured JSON logs")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("mirrord %s (%s) %s\n", version, commit, runtime.Version())
		return
	}

	setupLogging(*logJSON)
	api.Version = version
	api.GoVersion = runtime.Version()

	// Checkpoints are the largest single allocation this process makes, and
	// they are produced in bursts. Raising the GC target trades a little
	// resident memory for materially fewer collections during a checkpoint,
	// which is exactly when a GC pause is most visible as a tick-rate dip.
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(200)
	}

	st, err := buildStore(*dataDir)
	if err != nil {
		slog.Error("failed to open the checkpoint store", "err", err)
		os.Exit(1)
	}

	opt := simctl.DefaultOptions()
	opt.Store = st
	opt.CheckpointEvery = units.Tick(*ckptEvery) * units.TicksPerMinute
	mgr := simctl.NewManager(opt)

	if !*noBootstrap {
		cfg := engine.DefaultConfig()
		cfg.Preset, cfg.Seed, cfg.Population = *preset, *seed, *population
		cfg.StartHour, cfg.Regions, cfg.Workers = int32(*startHour), *regions, *workers
		start := time.Now()
		sim, err := mgr.Create("Baseline", cfg)
		if err != nil {
			slog.Error("failed to create the baseline simulation", "err", err)
			os.Exit(1)
		}
		sim.Read(func(e *engine.Engine) {
			slog.Info("baseline simulation ready",
				"id", sim.ID, "preset", cfg.Preset, "seed", cfg.Seed,
				"population", cfg.Population, "nodes", len(e.Map.Nodes),
				"edges", len(e.Map.Edges), "signals", len(e.Map.Signals),
				"districts", len(e.Map.Districts), "regions", cfg.Regions,
				"mapHash", fmt.Sprintf("%016x", e.Map.Hash),
				"buildMillis", time.Since(start).Milliseconds())
		})
		_ = mgr.Play(sim.ID)
	}

	acfg := api.DefaultConfig()
	acfg.Addr, acfg.WebDir = *addr, *webDir
	srv, err := api.NewServer(acfg, mgr)
	if err != nil {
		slog.Error("failed to start the API", "err", err)
		os.Exit(1)
	}
	if k := srv.DevKey(); k != "" {
		// Printed once, to stderr, never logged structurally. In production
		// mode this branch does not execute because no dev key exists.
		fmt.Fprintf(os.Stderr, "\n  MIRROR development API key: %s\n"+
			"  The UI picks this up automatically. Set MIRROR_API_KEYS and\n"+
			"  MIRROR_AUTH_MODE=production to disable it.\n\n", k)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("server stopped", "err", err)
			os.Exit(1)
		}
	case sig := <-stop:
		slog.Info("shutting down", "signal", sig.String())
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			slog.Warn("graceful shutdown did not finish", "err", err)
		}
		mgr.Shutdown()
	}
}

func buildStore(dir string) (store.Store, error) {
	if dir == "" {
		slog.Warn("no data directory configured; checkpoints are in memory and will not survive a restart")
		return store.NewMemory(), nil
	}
	return store.NewFilesystem(dir)
}

func setupLogging(asJSON bool) {
	level := slog.LevelInfo
	if os.Getenv("MIRROR_LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}
	var h slog.Handler
	if asJSON {
		h = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	} else {
		h = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	}
	slog.SetDefault(slog.New(h).With("service", "mirrord", "version", version))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return def
	}
	return n
}
