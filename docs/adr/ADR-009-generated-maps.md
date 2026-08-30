# ADR-009 — Generated maps before OSM

**Status:** accepted · **Date:** 2026-08-30

## Context

A city map could come from real-world data (OpenStreetMap extracts) or from a
procedural generator. Real data is more visually convincing; a generator is
cheaper to reason about.

## Decision

Procedural generation for the initial version, from a small parameter struct
(`world.GenParams`) plus a seed. `world.Map` is deliberately generic — an OSM
importer that produces the same struct is a strictly additive change, not a
redesign.

## Reasoning

The map is not just scenery — it is an *input to a determinism contract*.
Every claim this project makes ("same seed, same result") is a claim about
reproducing a run exactly, and the run starts from the map. A generated map is
reproducible from roughly 40 bytes of parameters and is `TestMapGenerationStable`-checked
to be a pure function of them. An OSM extract, by contrast, is a multi-hundred-
megabyte artefact that would need to be content-addressed and shipped alongside
every stored replay, turning "replay this run" into a data-distribution problem
on top of a simulation problem.

Generation also gives exact control over the properties the simulation model
depends on: guaranteed road-network connectivity (`TestMapConnected`), a
land-use gradient that produces realistic commute flows without hand-curated
zoning data, and infrastructure (substations, hospitals, depots) placed at a
density that makes the cascade models interesting rather than either trivial or
absurd.

## Consequences

- The city is not a real place, which is the honest trade for the above. It
  looks like a city — grid with arterials, ring road, radial land-use gradient,
  transit corridors — because those are the structural regularities that make
  real cities legible, not because it is one.
- The `Map` struct has no field that assumes procedural origin (no seed baked
  into node data, for instance); everything generation-specific lives in
  `world/gen.go`, so a future OSM importer only needs to populate the same
  struct and does not touch the simulation, routing, or rendering code at all.
