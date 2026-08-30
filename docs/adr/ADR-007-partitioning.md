# ADR-007 — Load-balanced cells, not districts

**Status:** accepted (supersedes an earlier district-per-worker design) · **Date:** 2026-08-30

## Context

Execution must be split across region workers. Districts already exist as a
human-meaningful unit — they name places and group metrics — so making them the
unit of scheduling as well was the first design.

## Decision

Partition execution on a separate, finer grid of cells, independent of district
boundaries. Assign cells to workers by measured vehicle load using
longest-processing-time-first (LPT) bin packing, and recompute the assignment
once per simulated minute.

## Reasoning

District-per-worker measured badly. On the medium preset (9 districts), the
central district carried roughly four times the vehicle load of a peripheral
one at peak. Every phase-A barrier waits for the slowest worker, so nine workers
delivered a 1.8x speed-up against a theoretical 6.6x ceiling predicted from the
measured serial fraction — most of the parallelism was being thrown away
waiting on one hot worker.

Districts are the wrong unit for scheduling because their *sizes are set by
geography*, not by load, and load is what a scheduler needs to balance.

Cells solve this by being deliberately meaningless: a uniform grid, sized so
each worker owns roughly 8-12 cells (`cellsPerSide`), with no relationship to
district boundaries. LPT bin packing is a 4/3-approximation to optimal makespan
and costs one sort over a few hundred cells — optimal bin packing is NP-hard and
would be absurd to reach for when the input is this small and the measurement
it is based on is already a minute stale by the time it is used.

**Why rebalancing mid-run is safe rather than terrifying:** the engine is
provably partition-independent (ADR-004, ADR-005). The same property that makes
a 1-worker run and a 9-worker run agree makes "9 workers, differently arranged"
agree too. Moving a cell from one worker to another is exactly as safe as
choosing a different worker count from the start, because both are instances of
the same operation: rebuild the ownership indices from state.

Load is measured *in simulation state* (vehicle occupancy per cell, sampled
every simulated minute), not wall-clock timing. That means the partition layout
is itself a pure function of the run — a replay reproduces not just the same
city but the same schedule, down to which worker did what on which tick.

## Consequences

- Districts remain purely presentational. `Engine.RegionOfDistrict` answers
  "which worker is mostly responsible for this place" for the map overlay, but
  it is a display convenience, not an ownership fact — a district's edges can
  and do span multiple workers.
- An edge is owned by the cell of its *destination* node, matching the vehicle
  ownership rule, so ownership never straddles a moving entity.
- `TestParallelRegionCounts` runs with rebalancing on and off and requires
  identical digests either way.
- Measured result: at 60k residents, 4 workers went from 1.57x to 1.91x
  speed-up after this change, and the serial fraction (not the partitioning)
  became the visible ceiling — which is the honest bottleneck to report.
