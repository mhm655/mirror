# ADR-004 — How determinism is guaranteed

**Status:** accepted · **Date:** 2026-08-30

## Context

The product claim is: same initial state, same seed, same command stream, same
final state — including when execution is spread across workers, and including
after a crash and recovery.

"We were careful" is not a guarantee. This ADR lists the specific threats, the
specific mitigation for each, and the test that fails if one regresses.

## Decision

| Threat | Mitigation | Test |
| --- | --- | --- |
| Floating-point divergence | Integers only (ADR-003) | `TestNoFloatInState` |
| `math/rand` changing across Go releases | Own PCG32 in `internal/rng` | `TestReplayFromLog` |
| Order-dependent random draws | RNG derived from `(seed, stream, tick, entity)` | `TestParallelRegionCounts` |
| A new system shifting another's stream | Each system owns a distinct stream id | — (convention) |
| Go map iteration order | No map iteration affects results | `TestSerialEqualsParallel` |
| Goroutine scheduling | Phase A never writes state another region reads | race detector in CI |
| Commit order | Merged intents sorted globally by `(kind, id)` | `TestParallelRegionCounts` |
| A\* tie-breaking | Frontier ordered by `(f, nodeID)` | `TestHeuristicAdmissible` |
| Inadmissible heuristic | Euclidean over max free-flow speed | `TestHeuristicAdmissible` |
| Asymmetric rounding | Round-half-away-from-zero everywhere | — (in `units`) |
| Engine state outside the checkpoint | Everything derived is rebuilt on load | `TestCheckpointRestore` |
| Repartitioning changing results | `assign` is a pure function of measured load, which is state | `TestParallelRegionCounts` |

## The digest

`State.Digest()` is FNV-1a over the canonical checkpoint encoding. One encoder
serves both jobs, so it is impossible for a field to be checkpointed but not
hashed, or hashed but not checkpointed.

Comparing digests rather than states matters practically: a mismatch localises
to a tick, and comparing tens of megabytes of state per tick would make the
suite unusable.

## The RNG derivation

A generator is never shared between systems or across regions. Instead it is
*derived* from `(worldSeed, stream, tick, entity)` through a SplitMix64-style
mix, so the value an entity draws does not depend on how many other entities
drew before it.

The mix matters, not just the combination: entity ids and ticks are both small
and highly correlated, and a weak mix produces visibly correlated behaviour
between neighbouring vehicles — they all decide to reroute on the same tick,
which looks like a bug and behaves like one.

## Integer square root

Profiling found integer Newton's method consuming **42% of all CPU**, because
the A\* heuristic calls it once per node expansion. The replacement seeds from
`math.Sqrt` and corrects with two integer loops.

The correction is what preserves determinism. IEEE-754 `sqrt` is correctly
rounded and therefore identical on every conforming platform, but "correctly
rounded" can still land one unit in the last place above the true root, so
truncating it directly would give `floor(√n)` for one input and `floor(√n)+1`
for another. The loops force the exact integer answer regardless of which way
the rounding went.

`TestISqrtExact` checks it against the old implementation over roughly a million
values, including every perfect square and its neighbours. And the state digest
after the change is the *identical value* measured before it.

## What these tests actually caught

Four real bugs, none of which a per-system unit test would have found:

1. **Per-region commit order.** Two vehicles competing for the last slot on a
   link: whichever region was processed first won, so four workers made a
   different choice than one. Fixed by merging all intents into a single
   globally sorted stream.
2. **A partition-dependent reroute budget.** An A\* budget computed from
   `len(region.Vehicles)`, so a four-region run replanned a different set of
   vehicles than a one-region run. Rate limiting now lives entirely in
   per-vehicle state, which is invariant under partitioning.
3. **Dispatcher state outside the checkpoint.** An emergency-dispatch cooldown
   held in a Go map on the dispatcher. A restored engine did not have it and
   dispatched on different ticks. Moved into `Incident`.
4. **An unsorted command log.** Commands can be scheduled for a future tick, so
   a plain append left the log unsorted and lookup silently stopped finding
   things. No error — the command simply never happened.

A fifth was found by the repartitioning test: intents generated *during* the
commit phase were dropped, so transit passengers never walked their last leg.
The commit now drains its own output in a bounded second pass.

## Consequences

- Every new system must claim a stream id and must not touch shared state in
  phase A. This is a real constraint on contributors and is documented in the
  `internal/systems` package comment.
- The determinism suite takes a few seconds and runs on every commit. It is the
  most valuable test in the repository by a wide margin.
- Optimisations become verifiable: an optimisation that changes the digest
  changed behaviour, whether or not that was intended.
