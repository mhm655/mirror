# MIRROR

A real-time, deterministic digital twin of a city — with counterfactual
simulation as a first-class operation, not an afterthought.

Fork a running simulation, change one policy, run both arms forward, and get
back a comparison that tells you when it isn't valid to draw a conclusion.
Everything underneath that sentence — the tick engine, the partitioning, the
event model — exists to make it true.

```
"Would adaptive traffic signals help?"
   → fork the live city twice
   → run baseline and adaptive-signals for 30 simulated minutes each
   → compare mean/P95 travel time, emergency response, fuel, hospital load
   → report the delta, and say plainly if the sample is too small to trust
```

**[Read the full technical blueprint →](docs/BLUEPRINT.md)** for the reasoning
behind every major decision, what was deliberately left out, and the
benchmarks. This README is the map to the territory, not a substitute for it.

## What's actually here

This isn't a mockup. Every claim below is backed by a test or a benchmark that
can fail.

- **A deterministic simulation engine** (`internal/engine`, `internal/systems`)
  — integer-only state, a two-phase tick with a serial commit, and a state
  digest that must match across 1, 2, 3, 4, 8, and 9 parallel workers. It's
  checked in CI, not just claimed: `TestParallelRegionCounts`.
- **A load-balanced spatial partitioner** (`internal/engine/partition.go`) that
  rebalances live, once per simulated minute, and provably cannot change the
  result — the digest doesn't move when the layout does.
- **Cascading infrastructure failure**: a substation trip darkens its signals,
  which turns intersections into all-way stops, which changes traffic, with no
  scripted link between any of those — it falls out of the same mechanisms that
  run every tick.
- **Real event sourcing**, split honestly: a tiny authoritative command log
  (kilobytes) versus a bounded, sampled effect stream (never the system of
  record) — see [ADR-006](docs/adr/ADR-006-event-sourcing.md).
- **Checkpoint + replay recovery** that is provably exact: restore from a
  checkpoint, replay the command log, and the resulting state digest matches
  the original bit for bit. The chaos lab runs this against a live process, not
  just in a test.
- **An AI operations assistant with real, bounded tool access** — no chat
  wrapper. It reads live simulation state and can fork/run/compare scenarios,
  gated by an explicit tier system so mutation requires both a per-request
  grant and an operator role. See [ADR-013](docs/adr/ADR-013-ai-tool-boundaries.md).
  Works without an API key too: a deterministic planner drives the same tools.
- **A real-time console** (`web/`, React + TypeScript) — a hand-rolled binary
  WebSocket protocol with viewport culling so bandwidth doesn't scale with city
  size, and a Canvas2D renderer chosen deliberately over Three.js
  ([ADR-011](docs/adr/ADR-011-canvas-not-three.md)).
- **Two "optimize, then prove nothing broke" case studies**: replacing the A\*
  heuristic's square root (42% of all CPU, per a profile) and making the
  edge-speed pass incremental — both verified by requiring the *exact same*
  state digest before and after.

## Quick start

Requires Go 1.24+ and Node 20+.

```bash
# backend
go build -o bin/mirrord ./cmd/mirrord
./bin/mirrord -preset medium -population 45000

# frontend (separate terminal, dev mode with hot reload)
cd web
npm install
npm run dev
```

Open `http://localhost:5173` (dev) or `http://localhost:8080` (built UI served
by `mirrord` directly — build it first with `npm run build` from `web/`). In
development mode the server hands out a bootstrap API key automatically; in
production (`MIRROR_AUTH_MODE=production`) you must set `MIRROR_API_KEYS`
yourself — see [ADR-013](docs/adr/ADR-013-ai-tool-boundaries.md) and
`internal/api/auth.go`.

### Run the tests

```bash
go test ./...              # full suite, ~10s
go test ./internal/engine/ -run TestParallelRegionCounts -v   # the headline claim
```

### Run the benchmarks

```bash
go run ./cmd/mirrorbench -preset medium -population 60000 -ticks 6000 -regions 1,2,4,9
```

Reports ticks/second, tick-time percentiles, the measured serial fraction next
to the Amdahl's-law prediction it implies, and — critically — the state digest
at every region count, which must be identical. See
[docs/benchmarks](docs/benchmarks) for captured runs.

## Repository map

```
cmd/mirrord         the server binary: engine + API + UI in one process
cmd/mirrorbench      reproducible performance benchmarks
internal/units       integer units — the no-float rule lives here
internal/rng         deterministic PCG32 with per-entity stream derivation
internal/world       immutable map, procedural generator, spatial index, A*
internal/state       mutable simulation state, canonical codec, digest, clone
internal/events      command/effect split, the authoritative log, bounded ring
internal/systems     simulation logic — pure functions over (Ctx, Region)
internal/engine      tick orchestration, partitioning, live rebalancing
internal/store       checkpoint and command-log persistence (memory/fs)
internal/simctl      simulation lifecycle, forking, comparison, recovery
internal/api         HTTP, hand-rolled WebSocket, auth, the chaos lab
internal/agent       AI tool definitions, LLM loop, deterministic fallback planner
internal/telemetry   Prometheus text-exposition output
web/                 React + TypeScript operations console
deploy/              Dockerfile, Kubernetes manifests, Terraform
docs/                the blueprint, 13 ADRs, captured benchmark runs
```

`internal/systems` depends on `world`, `state`, and `events` — nothing else. It
has no idea goroutines exist; `internal/engine` is the only package that knows
about parallelism.

## Why the numbers matter more than the pitch

Four determinism bugs and one 42%-of-CPU performance bug are described, with
root causes, in [ADR-004](docs/adr/ADR-004-determinism.md) and the blueprint's
testing section — because a project whose thesis is "trustworthy simulation"
should show its work when the trust was earned by a failing test, not just
assert it.

If you read one section of the blueprint, read
[§22, "What not to build"](docs/BLUEPRINT.md#22-what-not-to-build) — the
technologies and patterns that were deliberately left out, and why leaving them
out was the harder, more defensible call.
