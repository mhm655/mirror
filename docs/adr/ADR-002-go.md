# ADR-002 — Go as the engine language

**Status:** accepted · **Date:** 2026-08-30

## Context

The engine needs predictable per-tick latency, cheap parallelism across region
workers, and a serviceable HTTP and WebSocket stack. Candidates considered: Go,
Rust, C++, and a JVM language.

## Decision

Go.

## Reasoning

**Against Rust.** Rust would win on raw throughput and would remove the garbage
collector entirely. It loses on the thing that actually dominates this
codebase's difficulty: the state is a large set of parallel arrays mutated by
several workers under an ownership discipline the *borrow checker cannot
express*. Region ownership is a runtime property — which cells a worker owns
changes every simulated minute — so the code would be full of index-based access
plus `unsafe` or interior mutability, giving up most of what Rust was brought in
for. Go's race detector, run in CI, checks the discipline that matters here
empirically instead.

**Against C++.** The concurrency and HTTP story requires assembling third-party
libraries, and the safety story is strictly worse than either alternative for no
compensating benefit at this scale.

**Against the JVM.** GC pauses are the specific enemy of a fixed-step real-time
loop. Go's collector is not pauseless either, but its target is sub-millisecond
and its allocation behaviour is far easier to control from ordinary code — which
this engine does, by allocating essentially nothing in steady state.

**For Go, specifically:**

- Goroutines and `sync.WaitGroup` make the two-phase tick barrier three lines.
- `net/http` with method-and-pattern routing is enough; no framework needed.
- Slices of primitives give C-like memory layout with bounds checking.
- The race detector is a genuine correctness tool for exactly the property this
  design depends on.

## Consequences

- Garbage collection exists. Managed by allocating nothing per tick: per-region
  scratch buffers, a route arena, reusable intent slices. Measured allocation is
  0.6–2.0 KB per tick at 60k residents, which is noise.
- `GOGC` is raised to 200 at startup unless overridden. Checkpoints are the
  largest single allocation and are produced in bursts; trading a little
  resident memory for fewer collections during a checkpoint keeps the tick rate
  flat exactly when a pause would be most visible.
- No SIMD without assembly or `unsafe`. Not a bottleneck: the profile says the
  costs are branches and integer division, not vector arithmetic.
- Generics are used sparingly — the codec and slice copying. Hot paths are
  monomorphic by hand, because generic code over an interface constraint does
  not inline as reliably.
