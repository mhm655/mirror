# ADR-006 — Commands are sourced; effects are derived

**Status:** accepted · **Date:** 2026-08-30

## Context

"Just event-source everything" is the default advice for a simulation with a
replay requirement. At MIRROR's scale that advice is wrong, and it is worth
being specific about why.

## Decision

Split the event model into two kinds that look similar but are treated
completely differently.

**Commands** are inputs the simulation could not have derived on its own: an
operator injects an accident, a policy changes, the population is seeded. They
are the authoritative log — small, append-only, persisted forever.

**Effects** are observations about something the simulation just did: a vehicle
entered a road, a hospital hit capacity. They are *derived* — a deterministic
function of (initial state, seed, commands so far). They live in a bounded ring
buffer, are sampled for observability, and are never replayed.

## Reasoning

At 100k vehicles the engine produces on the order of 400k `VehicleEnteredRoad`
effects per simulated minute. Persisting all of that as the system of record
would mean writing roughly 2 GB per simulated hour of data whose only purpose is
to reconstruct something a 30 MB checkpoint plus a few hundred commands
reconstructs exactly, bit for bit, because the simulation is deterministic.

Once determinism holds, event-sourcing every effect stops being a durability
strategy and becomes pure overhead: you would be persisting a value you could
recompute for free.

So the layering is:

```
system of record  = generation params + seed + command log     (kilobytes)
fast recovery     = checkpoint + commands since                (megabytes)
observability     = effect ring, sampled, never load-bearing   (bounded)
```

The effect ring is deliberately bounded and drops the oldest entries under
pressure. That is correct behaviour, not data loss: effects are not the system
of record, so dropping them costs nothing that matters, and it means a slow
WebSocket client can never apply backpressure to the simulation loop. Drops are
counted and exported so the choice is visible rather than silent.

## Consequences

- Two code paths must never be confused. `Log.Append` panics if handed anything
  that is not a command; the type system does not prevent an engineer from
  emitting a command-shaped effect, so this is enforced at the boundary.
- A stored replay is tiny. A week of simulated time with a few hundred operator
  interventions is a few kilobytes of log, not gigabytes of event history.
- The UI's "what just happened" feed is explicitly not an audit trail. The
  audit trail (for state-changing API calls) is a separate, smaller, bounded
  structure in `internal/api/auth.go`.
