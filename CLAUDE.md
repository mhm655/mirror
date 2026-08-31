# CLAUDE.md — instructions for working on MIRROR

This file is for Claude Code (or any future session) picking up this project.
Read [PROJECT.md](PROJECT.md) first for current status; this file is about how
to work in this specific codebase.

## What this project is

A deterministic, real-time digital-twin simulation of a city, with
counterfactual scenario forking as the headline feature. Full design reasoning
is in [docs/BLUEPRINT.md](docs/BLUEPRINT.md) and [docs/adr/](docs/adr/) — read
those before making architectural changes, not just this file.

## The one rule that overrides all others here

**Determinism is the product.** Any change to `internal/systems`,
`internal/engine`, `internal/state`, `internal/world`, or `internal/events`
must keep these true:

- No floating point enters simulation state (checked by
  `TestNoFloatInState` in `internal/engine`).
- No `math/rand`, no unkeyed randomness — use `internal/rng`, derived from
  `(seed, stream, tick, entity)`. If you add a system that needs randomness,
  claim a new `Stream*` constant in `internal/rng/rng.go`; never reuse one.
- Phase A (parallel, per-region) may only write state the region itself owns.
  Anything that could be visible to another region becomes an `Intent` and is
  applied in Phase B (serial, globally sorted). If you're not sure whether a
  write is safe in Phase A, it probably isn't — make it an Intent.
- Nothing computed by the engine may depend on wall-clock time, Go map
  iteration order, or goroutine scheduling.

After any change to those packages, run:
```bash
go test ./internal/engine/ -run 'TestSameSeed|TestSerialEqualsParallel|TestParallelRegionCounts|TestReplayFromLog|TestCheckpointRestore|TestForkIsolation' -v
```
All six must pass. This is not optional and it is not slow (a few seconds).

## Build & verify

```bash
go build ./...
go vet ./...
go test ./...
cd web && npm run typecheck && npm run build
```
All four must be clean before considering work done. `gofmt -l .` (excluding
`web/`) should print nothing — run `gofmt -w .` if it does.

## Toolchain note for this machine

This machine had no system Go when this project started. If a fresh session
finds `go: command not found`, check `C:\Users\user\toolchains\go\bin` first
(a portable Go 1.27 install was placed there) before assuming Go needs
reinstalling — add it to PATH:
```bash
export PATH="/c/Users/user/toolchains/go/bin:$PATH"
```
If the user has since installed Go system-wide (via winget or the official
installer), a plain `go version` will confirm — prefer the system one if both
exist.

## Running it locally

```bash
cd web && npm install && npm run build && cd ..
go run ./cmd/mirrord -preset medium -population 45000
```
Open `http://localhost:8080`. Dev mode auto-issues an API key (printed to
stderr on startup); the browser fetches it automatically via
`/api/v1/auth/dev-session`.

For frontend hot-reload during UI work:
```bash
cd web && npm run dev
```
Open `http://localhost:5173` — Vite proxies `/api` and `/ws` to `:8080`.

## Where things live (see also README.md's repo map)

- Simulation logic → `internal/systems/*.go`, one file per subsystem
  (traffic.go, power.go, emergency.go, transit.go, env.go, demand.go,
  population.go, commands.go, commit.go).
- Tick orchestration & partitioning → `internal/engine/engine.go`,
  `partition.go`.
- Wire protocol (binary vehicle/network frames) → `internal/api/stream.go`;
  matching decoder → `web/src/lib/stream.ts`. **Keep these two in sync
  manually** — there is no shared schema/codegen between them.
- AI tool definitions → `internal/agent/tools.go` (schemas + implementations),
  `internal/agent/llm.go` (Anthropic loop), `internal/agent/agent.go`
  (deterministic fallback planner + dispatch).
- Frontend map rendering → `web/src/render/renderer.ts` (Canvas2D, not
  Three.js — see ADR-011 before "fixing" this).

## Known gaps (see PROJECT.md for the full list)

`internal/api` and `internal/agent` have test coverage now, except for the
WebSocket byte-level read/write loop and the Anthropic tool-use loop in
`llm.go` (both need a live connection or model call). No Postgres backend
(memory/filesystem stores only), and deployment manifests in `deploy/` have
not been applied against a real cluster. Don't assume any of these work
until they're actually exercised.

## House style

- No comments that restate what code does; comments explain *why*, especially
  non-obvious constraints (see any file in `internal/systems` for the
  established tone — most functions have a paragraph on the reasoning above
  them, not below).
- Don't add abstraction, config options, or "just in case" flexibility beyond
  what's asked. This codebase already got bitten once by premature
  district-based partitioning (see ADR-007) — measure before generalizing.
- When you fix a bug in the deterministic core, write the regression test
  first, watch it fail, then fix it. That is how the four determinism bugs and
  the ISqrt performance bug were caught during development — don't lose that
  discipline going forward.
