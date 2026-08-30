# ADR-012 — Own Prometheus exposition

**Status:** accepted · **Date:** 2026-08-30

## Context

The `/metrics` endpoint needs to emit Prometheus text-exposition-format output:
gauges, counters, and histograms for the HTTP layer and for each running
simulation.

## Decision

A hand-written exposition writer (`internal/telemetry`) rather than the
official `prometheus/client_golang` library.

## Reasoning

The client library's value is threefold: a metric registry, a collector
abstraction for pulling values at scrape time, and a histogram implementation.
This process needs none of the first two — it already computes every exported
value for its own API responses and simply writes the same numbers in a
different format at scrape time — and the third is actively a liability here:
MIRROR's simulation already has its own integer histogram (`state.Histogram`,
with fixed 120-tick buckets matching the domain, used for travel-time and
response-time percentiles). Bridging that into the client library's own
histogram type would mean maintaining a translation layer between two bucket
representations for no benefit, since the format being emitted (`_bucket`,
`_sum`, `_count` with cumulative `le` labels) is a one-page, stable, documented
specification that is straightforward to emit directly from the buckets that
already exist.

Route labels are explicitly normalised to templates
(`/api/v1/simulations/{id}`) before being used as a label value, precisely
because Prometheus label cardinality is a real production hazard: using the raw
path would create one time series per simulation id ever created, unboundedly,
which is the canonical way to degrade a Prometheus instance over time. The
client library would not have prevented this mistake; the discipline has to
exist regardless of which writer is used.

## Consequences

- If this service later needs OpenMetrics content negotiation, exemplars tied
  to trace IDs, or native histograms, that need would justify adopting the
  client library — those are genuinely nontrivial to hand-roll well. That trade
  is explicitly noted in the package doc comment as the condition under which
  this decision should flip.
- The `/metrics` endpoint is deliberately unauthenticated (a scrape endpoint
  behind a bearer token is a scrape endpoint nobody's Prometheus can reach) and
  exports only counts, rates, and durations — never an entity id or a district
  name — so this decision doesn't interact with the security model.
