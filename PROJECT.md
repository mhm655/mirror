# PROJECT.md — MIRROR status

Living status document. Update this when you finish a chunk of work — it's the
first thing a new session should read to know where things stand, before
diving into [docs/BLUEPRINT.md](docs/BLUEPRINT.md) for the reasoning.

**Repo:** https://github.com/mhm655/mirror (public)
**Last updated:** 2026-08-31 (added API/agent tests)

## Status at a glance

| Area | State |
| --- | --- |
| Simulation engine | ✅ Working, deterministic, benchmarked |
| API + WebSocket transport | ✅ Working |
| Frontend console | ✅ Working, all panels functional |
| AI agent | ✅ Working (with or without API key) |
| Persistence | ⚠️ Memory + filesystem only, no Postgres |
| Multi-process distribution | ❌ Not built (single-process, in-process "regions" only) |
| CI | ✅ GitHub Actions on push/PR to `main` |
| Test coverage | ⚠️ Engine, API, agent covered — no frontend tests, no WebSocket/LLM-loop tests |
| Deployment | ⚠️ Manifests written, never applied to a real cluster |
| Docs | ✅ Blueprint + 13 ADRs, current |

## What's done

**Simulation core** (`internal/units`, `internal/rng`, `internal/world`,
`internal/state`, `internal/events`, `internal/systems`, `internal/engine`)
- Integer-only deterministic state; two-phase tick (parallel phase A, serial
  commit phase B).
- Systems: traffic (Greenshields speed-density, A* routing with informed/
  uninformed drivers), signals (fixed-time + adaptive/queue-actuated), transit
  (buses + segregated metro, intrusive rider/stop lists), power (radial grid,
  cascading load-shed, substation trips), hospitals (admission/diversion/
  rejection), emergency dispatch, weather, incidents, population generation
  (gravity-model commute assignment).
- Load-balanced spatial partitioner (`internal/engine/partition.go`) —
  independent of district boundaries, LPT bin-packed, rebalances live every
  simulated minute with **zero effect on the state digest** (tested).
- Checkpointing (`internal/store`) — gzip + CRC32C, memory and filesystem
  backends, header carries map hash / tick / digest for validation on load.
- Fork/compare (`internal/simctl`) — scenarios share the immutable world by
  pointer; comparison reports deltas plus validity warnings (different maps,
  different ticks, too few completed trips).

**Verified, not just claimed:**
- `TestSameSeedSameResult`, `TestSerialEqualsParallel`,
  `TestParallelRegionCounts` (1/2/3/4/8/9 workers), `TestReplayFromLog`,
  `TestCheckpointRestore`, `TestForkIsolation`, `TestMapGenerationStable`,
  `TestMapConnected`, `TestHeuristicAdmissible`, `TestNoFloatInState`,
  `TestISqrtExact` — all in `internal/engine` and `internal/units`, all
  passing.
- Benchmarks in `cmd/mirrorbench`, captured results in `docs/benchmarks/`.
  Medium city (60k residents): 2,923 ticks/s single-worker, 4,528 ticks/s at 4
  workers (453x real time), identical state digest at every worker count.

**API & transport** (`internal/api`, `internal/telemetry`)
- REST control plane: create/list/delete sims, control (play/pause/speed),
  inject events (allow-listed kinds only), set policy, fork, restore, compare,
  chaos-lab experiments.
- Three roles (viewer/operator/admin), API-key auth (SHA-256, constant-time
  compare), per-key token-bucket rate limiting, bounded audit log.
- Hand-rolled WebSocket (`internal/api/websocket.go`) — RFC 6455, no third-
  party dependency (see ADR-010). Three frame types: binary vehicles (8Hz,
  viewport-culled), binary network/congestion (2Hz, dense byte-per-edge), JSON
  snapshot/metrics/events (2Hz).
- Prometheus exposition hand-written (ADR-012), unauthenticated `/metrics`.

**AI agent** (`internal/agent`)
- 12 tools: read tier (traffic, population, hospitals, power, transit, events,
  incidents, metrics), sandboxed tier (simulate_scenario, compare_scenarios),
  mutate tier (inject_event, set_policy) — gated by per-request grant ∩ caller
  role (ADR-013).
- Anthropic tool-use loop (`internal/agent/llm.go`) when `ANTHROPIC_API_KEY`
  is set; deterministic fallback planner (`internal/agent/agent.go`,
  `summarise.go`) otherwise or on LLM outage — calls the *same* tools, same
  boundary, no privileged path.

**Frontend** (`web/`, React + TypeScript + Vite)
- Canvas2D renderer (`web/src/render/renderer.ts`) — two-layer (road network
  offscreen-cached, vehicles redrawn every frame with interpolation). Chosen
  over Three.js deliberately (ADR-011).
- Binary frame decoder (`web/src/lib/stream.ts`) matching the server's wire
  format exactly, by hand — no shared schema.
- Panels: overview, metrics, events, inspector, inject, policy, compare,
  assistant, chaos, recovery/checkpoints. All wired to the real API.

**Docs**
- `docs/BLUEPRINT.md` — 24 sections, product definition through recommended
  build order.
- `docs/adr/ADR-001` through `ADR-013` — one per major decision, each with
  context/decision/reasoning/consequences.
- `docs/benchmarks/` — captured `mirrorbench` output.

**Deployment scaffolding** (`deploy/`) — Dockerfile (multi-stage, distroless),
Kubernetes StatefulSet + headless Service + example Secret + optional
ServiceMonitor, Terraform module for the app layer (not a cloud cluster — see
`deploy/terraform/main.tf` header comment for why).

## What's done (continued)

**CI** (`.github/workflows/ci.yml`) — two jobs on push/PR to `main`:
- `backend` (ubuntu-latest, Go 1.24): `gofmt -l` check (excluding `web/`),
  `go vet ./...`, `go build ./...`, `go test -race ./...`, then re-runs the
  named determinism suite verbosely so failures there are easy to spot in the
  Actions log. `-race` needs cgo, which this Windows dev machine doesn't have
  set up, so it could only be verified locally without `-race`; confirmed
  working on the Ubuntu runner itself on the first real push (run succeeded).
- `frontend` (ubuntu-latest, Node 20): `npm ci`, `npm run typecheck`,
  `npm run build` in `web/`.

**Tests for `internal/api` and `internal/agent`** — previously zero coverage
outside the determinism suite; now:
- `internal/api`: `auth_test.go` (role parsing, key add/lookup, the
  `MIRROR_API_KEYS` env parsing path including the production-mode refusal,
  credential extraction precedence, rate limiter burst/refill, client IP
  resolution, audit log ring behaviour), `server_test.go` (an `httptest`
  harness wired to a real `simctl.Manager` on the `small` preset — auth
  gating per role, sim create/get/delete, input validation, unknown-field
  rejection), `stream_test.go` (the `vehiclePos` interpolation math against a
  hand-built two-node map), `websocket_test.go` (`headerContainsToken`).
- `internal/agent`: `summarise_test.go` (every `summarise*` report writer,
  fed hand-built JSON), `tools_test.go` (`numArg`/`b2i`/`minInt`,
  `policySummary`, tier filtering in `available()`, `simFor` error paths,
  one live-simulation read tool), `agent_test.go` (`Chat` end-to-end against
  a real small simulation with no `ANTHROPIC_API_KEY` set, so it exercises
  the deterministic builtin planner — routing by keyword, the mutate-tier
  gate, and the fork-run-compare counterfactual path).
- Deliberately not covered: the WebSocket wire-level read/write loop and the
  Anthropic tool-use loop in `llm.go` (needs a live or mocked model call) —
  both noted as gaps below rather than silently skipped.

## What's not done

Ordered roughly by "most likely to matter next," not by difficulty:

1. **No frontend tests.** No Vitest/RTL setup, no component tests, no test for
   the binary frame decoder against a known byte sequence.
2. **No test coverage for the WebSocket byte-level loop or the Anthropic LLM
   loop.** `internal/api/websocket.go`'s frame read/write and
   `internal/agent/llm.go`'s tool-use loop are untested — the former needs a
   real or piped TCP connection, the latter needs a live or mocked model
   call. Everything else in both packages now has coverage (see "what's
   done" above).
3. **Postgres backend unbuilt.** The schema is documented in the blueprint
   (§8) but `internal/store` only has memory and filesystem implementations.
   Needed before multi-instance / multi-tenant deployment is real.
4. **Multi-tenant isolation designed, not implemented.** `tenant_id` exists in
   the *documented* schema only; nothing in the running code enforces
   isolation between tenants because there's no tenant concept yet.
5. **True multi-process distribution doesn't exist.** Everything is one
   process with goroutine-based "regions." ADR-008 describes the NATS-based
   migration path; none of it is built. Don't claim distributed execution
   beyond "the consistency model is designed for it and tested in-process."
6. **Deployment manifests are unexercised.** The Dockerfile has never been
   built; the k8s manifests have never been applied; the Terraform has never
   been planned/applied against a real cluster. Treat all of `deploy/` as
   "written, reasoned about, not proven."
7. **No OSM/real-map import.** Only procedural generation
   (`internal/world/gen.go`) exists. `Map` is intentionally generic so an
   importer would be additive (ADR-009), but nobody's written one.
8. **Untested at the largest scale.** The `huge` preset (hundreds of
   thousands of agents) has not been benchmarked — only `small`, `medium`,
   and `large` have captured numbers.

## Conventions for updating this file

- When you finish something in the "not done" list, move it to "what's done"
  with a one-line summary and delete it from the gaps list — don't just leave
  a checkmark, future sessions need the *what* not just the *whether*.
- When you find a new gap, add it to "what's not done" with enough context
  that someone could start on it without re-deriving why it matters.
- Keep "Last updated" current.
