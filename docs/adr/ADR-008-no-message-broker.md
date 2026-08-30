# ADR-008 — No message broker yet, and NATS when there is

**Status:** accepted · **Date:** 2026-08-30

## Context

The blueprint's original technology list included Kafka and Redis. Both are
absent from the shipped system. This ADR records the reasoning so it reads as a
decision rather than an omission.

## Decision

No message broker in the current, single-process design. When multi-process
distributed execution becomes necessary (see the blueprint's distributed
execution section for the trigger condition), use NATS for the inter-process
tick barrier and intent transport, not Kafka.

## Reasoning

**Kafka** is a durable, partitioned, replayable log built for high-throughput
event streams with many independent consumers. MIRROR's actual durable log — the
command log — is kilobytes per simulated day: a few hundred operator actions.
Running Kafka to persist that is running a distributed log-structured storage
system to hold less data than fits in an HTTP response. It would also be the
wrong shape for the tick barrier itself, which needs low-latency
request/reply-style coordination between a fixed, known set of workers once per
tick — not a durable topic with consumer groups.

**Redis** would earn its place the moment there is cross-process state to share
— a cache, a pub/sub fan-out, a distributed lock. In the current design there is
none: one process holds all simulation state in memory, and forking/sharing
happens by Go pointer, which is strictly cheaper than a network round trip to an
external store.

**NATS**, when multi-process execution is actually built, fits because the
problem at that point is exactly what NATS is for: ephemeral request/reply and
fan-out between a small number of long-lived peers, with no need for durable
replay (the command log already provides durability, independently). The
migration path described in the blueprint is: replace the in-process
`sync.WaitGroup` barrier with a NATS request/reply barrier, ship intent batches
as NATS messages instead of appending to a shared slice, and elect one process
to run the serial commit.

## Consequences

- Nothing in `internal/systems` or `internal/engine` mentions a broker. The
  tick orchestration is broker-agnostic by construction, so introducing NATS
  later is additive to `internal/engine`, not a rewrite of the simulation
  logic.
- This is a case where "add the fashionable infrastructure" was considered and
  explicitly rejected for the current scale, per the project's engineering
  principle: complexity must be earned.
