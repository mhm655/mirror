# ADR-001 — Fixed-step discrete time, not discrete-event

**Status:** accepted · **Date:** 2026-08-30

## Context

Two families of simulation engine were available.

A **discrete-event** engine keeps a priority queue of scheduled events and jumps
the clock to the next one. Nothing is computed between events. It is the right
choice when activity is sparse and bursty — queueing networks, packet
simulation, manufacturing lines — because it does no work when nothing is
happening.

A **fixed-step** engine advances a global clock by a constant interval and
updates every active entity each step.

## Decision

Fixed-step, at 100 ms per tick.

## Reasoning

The workload is the opposite of sparse. At peak, thousands of vehicles are each
moving continuously, and every one of them would generate an event every time it
advanced. A discrete-event engine would spend its life doing heap operations to
schedule "vehicle 4,812 has moved 1.4 metres" — the queue becomes the
simulation's dominant cost, and it is pure overhead against a workload with no
idle periods to exploit.

Fixed-step also gives three properties this project needs:

1. **A natural broadcast rate.** Ten ticks per simulated second maps directly
   onto a UI frame rate. A discrete-event engine has to synthesise sampling
   instants for rendering anyway.
2. **A trivially checkpointable state.** State is "the arrays as they are at
   tick N". A discrete-event checkpoint must also capture the pending event
   queue, which is far more of the engine's internals than the arrays are.
3. **A far simpler parallel decomposition.** Fixed-step gives an obvious barrier
   — the tick — to synchronise on. Parallel discrete-event simulation needs
   conservative lookahead or optimistic rollback (Time Warp), and both are an
   order of magnitude more machinery than this problem justifies.

## Consequences

- Work is done for entities that did not need updating. Mitigated where it
  mattered: the edge-speed pass is incremental, and the departure scan is
  bucketed by simulated minute.
- Sub-tick ordering does not exist. Two things in the same tick are simultaneous
  and are ordered by the commit's canonical sort, not by timestamp. This is
  simpler to reason about, and it is what makes partition-independence
  achievable.
- Tick length is a hard resolution limit: nothing shorter than 100 ms can be
  represented. Signal phases and vehicle dynamics all sit comfortably above it.

## When this would be revisited

If the model grew a large population of genuinely idle entities — for example
individually simulated buildings that change state a few times a day — a hybrid
would be right: fixed-step for the moving fleet, a scheduled-event queue for the
slow layer. The tick loop already has the shape for this; the city-wide systems
are effectively that queue, run on a coarse cadence.
