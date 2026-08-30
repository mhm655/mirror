# MIRROR — technical blueprint

> A real-time, deterministic digital twin of a city, with counterfactual
> simulation as a first-class operation.

This document is the engineering plan. It is opinionated, it says what not to
build as clearly as what to build, and where the implementation has already
answered a question it reports the answer rather than the theory. Numbers in
this document that describe MIRROR's own behaviour come from
[`cmd/mirrorbench`](../cmd/mirrorbench) and are reproducible.

---

## 1. Product definition

**MIRROR simulates a city and lets you ask what would have happened if you had
done something differently.**

That second half is the product. A live map of moving dots is a screensaver.
The thing that makes this a tool is:

1. The simulation is **deterministic**, so two runs that differ only in a policy
   differ *because of* that policy and nothing else.
2. A scenario can be **forked from a running state**, so a comparison starts
   from a world that is identical by construction rather than by luck.
3. The comparison reports **when it is not valid** — different maps, different
   simulated times, too few completed trips for a percentile to mean anything.

Everything else — the renderer, the AI assistant, the chaos lab — exists to make
those three properties usable.

### Who it is for

A transport or emergency planner who has a decision to make, is used to reading
instruments, and will not trust a number they cannot trace to a mechanism.

### The two claims it must be able to defend

- *"Turning on adaptive signals would cut P95 commute time by N%."* — backed by
  two runs from one state that differ in exactly one field.
- *"This run is reproducible."* — backed by a state digest that anyone can
  recompute.

---

## 2. Minimum viable version

The smallest thing that is still the product:

| Included | Excluded |
| --- | --- |
| One generated city, one process | Multiple cities, distributed processes |
| Cars only, one road class model | Transit, walking, freight |
| Fixed-time traffic signals | Adaptive control |
| Injectable accident + road closure | Power, water, comms, weather |
| Deterministic tick + state digest | Checkpoints, replay |
| Fork + compare on two metrics | Full metric set |
| Canvas map + play/pause | Inspector, event feed, AI |

If forking a running simulation, changing one thing and getting a defensible
difference in mean travel time does not work, nothing built on top of it will.
Everything in the "excluded" column is an extension of a working core; nothing
in it rescues a broken one.

### Long-term vision

A platform where an operator asks a question in English, the system forks a
dozen scenarios, runs them across a worker pool, and returns a ranked comparison
with confidence bounds across seeds — and where the whole session, including
what the AI did, is a replayable artefact.

---

## 3. Core domain model

Two categories, and the split is load-bearing.

### Immutable world — [`internal/world`](../internal/world)

Written once at generation, read-only thereafter: the road graph (nodes,
directed edges, CSR adjacency), districts, land use, signal geometry, the power
distribution tree, hospitals, depots, cell sites, transit routes.

Because it never changes, **every forked scenario shares one copy by pointer.**
This is the entire memory story for counterfactuals: twenty scenarios on the
`large` preset cost one ~40 MB world plus twenty copies of dynamic state.

### Mutable state — [`internal/state`](../internal/state)

Struct-of-arrays throughout: agents, vehicles, per-edge occupancy and speed,
signal phases, substation energisation, hospital beds, incidents, weather,
policy, metrics.

SoA is not stylistic. The movement system touches four vehicle fields per tick
and skips the rest; at 100k vehicles the array-of-structs layout pulls roughly
19 MB of cache lines per tick where SoA pulls about 3 MB. It also makes
checkpointing, hashing and forking a handful of `memcpy`s.

### Agents

An agent is ~120 bytes: home, workplace, departure times drawn once, mode,
status, health, patience, risk tolerance, and a precomputed transit itinerary.
100k agents cost about 6 MB.

**No agent gets an LLM call.** Not as a cost compromise — as a modelling
statement. A commuter's decision is "is my route now bad enough to be worth
changing", which is a threshold comparison, and heterogeneity across the
population comes from the per-agent patience and risk parameters. An LLM would
add latency, nondeterminism and expense to a decision that is genuinely a
comparison.

---

## 4. Simulation model

### Units and time

- Tick = **100 ms** simulated. Fine enough that a vehicle at 50 km/h moves 1.39 m
  per tick; coarse enough that signal phases are whole numbers of ticks; 10 Hz
  is a comfortable ceiling for a broadcast rate.
- Length in **millimetres**, speed in **mm/tick**, all `int64`.
- **No floating point in simulation state**, enforced by a reflection test that
  walks `state.State` and fails on any float field.

### Traffic

Speed-density is **Greenshields** (`v = v_free · (1 − k/k_jam)`), not BPR. BPR
is a steady-state *link performance* function that takes an hourly volume; it
never lets a link actually fill up. Greenshields is defined on instantaneous
density — which is what a microsimulation has — and reproduces the behaviour the
platform exists to show: throughput peaks near half density, and past that,
adding vehicles reduces flow.

Queues propagate backwards because a vehicle that cannot enter the next link
stays occupying the current one. That is where gridlock comes from, and it is
why a single blocked lane can seize a corridor.

### Routing

A\* over the directed graph with a Euclidean-over-max-speed heuristic. The
heuristic's admissibility is not a performance detail: an inadmissible heuristic
makes the chosen path depend on expansion order, which would make routing
nondeterministic. There is a test.

Rerouting is gated three ways, and each gate is a correctness measure as much as
a performance one:

1. A per-vehicle `ReplanAt`, jittered by vehicle id.
2. Only drivers who rolled "informed" (`RerouteAwarenessP`) participate.
3. A new route is adopted only if it beats the current one by more than that
   agent's patience threshold.

Without (3), the whole fleet flips to the same detour on the same tick and the
traffic wave oscillates between two corridors forever — a well-documented
failure of naive dynamic assignment.

### Cascades

The chains are mechanisms, not scripts:

```
substation overloaded 45s
  → trips
     → its signals go dark        → junctions become all-way stops, ~60% throughput
     → its street lighting fails  → night speed and crash hazard change
     → its hospitals to generators → capacity collapses when fuel runs out
     → its cell sites to battery  → then drop
     → its load sheds to neighbours → which may trip in turn
```

Nobody wrote "blackout causes traffic jam". Traffic jams because signals stop
cycling, and signals stop cycling because they are downstream of a substation
that tripped.

### Known simplifications, stated rather than hidden

- **Transit has no transfers.** A trip needing two lines falls back to driving.
  A full RAPTOR implementation would move the modal split a few points and
  change nothing about congestion or response times.
- **Emergency units teleport back to base** after clearing. The return leg
  affects no reported metric and would double the fleet's contribution to
  congestion.
- **Flooding uses road class as a proxy for elevation.** There is no terrain
  model; local streets flood first because that is broadly true, not because the
  simulation knows where the low ground is.
- **Weather is city-wide.** A spatial weather field is a lot of state for an
  effect invisible at this scale over a few hours.

---

## 5. Tick lifecycle

Fixed and load-bearing. Reordering any two of these changes results, so the
order is part of the replay contract and is versioned with the codec.

```
0  commands scheduled for this tick          serial
1  city-wide systems                          serial
     weather · power · comms · hospitals · incidents · transit dispatch
2  PHASE A1                                   parallel, one goroutine per region
     edge speeds · signal control
   ──────────────── barrier ────────────────
3  PHASE A2                                   parallel
     departures · walking · rerouting · movement
   ──────────────── barrier ────────────────
4  PHASE B  commit                            serial
     merged intent stream, globally sorted, then applied
5  maintenance                                serial
     route arena compaction · per-minute metric sample
```

**The rule that makes it work:** a region never writes state another region
might read in the same phase. Anything that would break the rule becomes an
`Intent`, and intents are applied serially in an order that is a pure function
of state.

The barrier between A1 and A2 is not decoration. A2 reads every edge's travel
time through the routing cost function; A1 writes those. Without the barrier
that is a data race *and* a nondeterminism source, because different regions
would observe different values depending on timing.

Measured phase split, medium city, 60k residents, 2,617 concurrent vehicles:

| regions | global | A1 speeds/signals | A2 move/route | B commit | total |
| --- | --- | --- | --- | --- | --- |
| 1 | 0.010 ms | 0.095 ms | 0.198 ms | 0.038 ms | 0.34 ms |
| 4 | 0.011 ms | 0.049 ms | 0.124 ms | 0.033 ms | 0.22 ms |

---

## 6. Event architecture

The single most important idea: **commands and effects are different things.**

| | Command | Effect |
| --- | --- | --- |
| Example | `cmd.inject_accident` | `vehicle.entered_road` |
| Origin | Outside the simulation | The simulation itself |
| Derivable? | No | Yes, from state + commands |
| Persisted | Always — system of record | Ring buffer, sampled |
| On replay | Re-applied | Regenerated, never replayed |
| Volume | Hundreds per simulated hour | ~400k per simulated minute |

Event-sourcing everything is the obvious move and it is wrong here. At 100k
vehicles, persisting effects as the system of record means writing ~2 GB/hour to
reconstruct something a 30 MB checkpoint plus a few hundred commands
reconstructs exactly.

```
system of record  = generation params + seed + command log      (kilobytes)
fast recovery     = checkpoint + commands since                 (megabytes)
observability     = effect ring, sampled, never load-bearing    (bounded)
```

The effect ring is **bounded on purpose**. Dropping the oldest effects under
pressure is correct behaviour, not data loss — and it means a slow WebSocket
client can never apply backpressure to the simulation loop. Drops are counted
and exported.

Two details that were bugs before they were design:

- The command log is kept sorted by `(tick, seq)` **on insert**. Commands can be
  scheduled for the future, so a plain append leaves the log unsorted and lookup
  silently stops finding things. No error — the command just never happens.
- Commit handlers can generate intents (a transit vehicle at a stop puts every
  alighting passenger onto a walking leg). Those arrive after the merged stream
  is built, so the commit phase drains its own output in a second bounded pass.

---

## 7. State architecture and determinism

### The digest

`State.Digest()` is FNV-1a over the canonical checkpoint encoding. One encoder
serves both jobs, so it is impossible for a field to be checkpointed but not
hashed, or hashed but not checkpointed.

Two runs are identical iff their digests match at every tick. The digest is what
every determinism claim in this project is measured against.

### How determinism is guaranteed

| Threat | Mitigation |
| --- | --- |
| Floating point | Banned from state; reflection test enforces it |
| `math/rand` changing between Go releases | Own PCG32; a log written by go1.24 replays under go1.30 |
| Draw order | RNG derived from `(seed, stream, tick, entity)`, never shared |
| Map iteration | No map iteration affects results |
| Goroutine scheduling | Phase A cannot write shared state; phase B is serial |
| Commit order | Merged intent stream sorted by `(kind, id)` globally |
| A\* tie-breaking | Frontier ordered by `(f, nodeID)` |
| Rounding | Round-half-away-from-zero, so results do not depend on sign |
| Engine state outside the checkpoint | Caught by the restore test |

The RNG derivation deserves a note. A generator is never shared: the value an
entity draws does not depend on how many other entities drew before it. That is
what lets phase A run in parallel and still produce byte-identical results.

### Integer square root

`ISqrt` seeds from the hardware square root and corrects with integer
arithmetic. The correction is what preserves determinism — IEEE-754 `sqrt` is
correctly rounded and identical everywhere, but truncating it directly would
give `floor(√n)` on one input and `floor(√n)+1` on another. Two integer loops
force the exact answer regardless.

This was found by profiling: integer Newton was **42% of all CPU**, because A\*
calls it once per node expansion. The replacement is verified against the old
implementation across a million inputs, and the state digest after the change is
the identical value measured before it.

---

## 8. Data model

Nothing in the running engine touches a database. Persistence is:

- **Checkpoints** — gzip + CRC32C behind a versioned header carrying map hash,
  tick and state digest. Restore validates all three before use.
- **Command log** — fixed-width binary records, append-only.

The `store.Store` interface has memory and filesystem implementations. A
Postgres schema is included for the multi-tenant deployment where scenarios must
outlive a pod:

```sql
CREATE TABLE simulation (
  id           text PRIMARY KEY,
  tenant_id    text NOT NULL,
  name         text NOT NULL,
  parent_id    text REFERENCES simulation(id),
  branch_tick  bigint,
  map_preset   text NOT NULL,
  seed         bigint NOT NULL,
  map_hash     bytea NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE command (
  sim_id  text NOT NULL REFERENCES simulation(id) ON DELETE CASCADE,
  seq     bigint NOT NULL,
  tick    bigint NOT NULL,
  kind    smallint NOT NULL,
  a, b, c, d bigint NOT NULL,
  PRIMARY KEY (sim_id, seq)
);
CREATE INDEX ON command (sim_id, tick);

CREATE TABLE checkpoint (
  sim_id  text NOT NULL REFERENCES simulation(id) ON DELETE CASCADE,
  tick    bigint NOT NULL,
  digest  bytea NOT NULL,
  raw_len integer NOT NULL,
  crc     integer NOT NULL,
  blob    bytea NOT NULL,
  PRIMARY KEY (sim_id, tick)
);
```

**When Postgres becomes necessary:** when more than one process must see the
same simulation, or when a scenario must survive a restart. Not before. A
database in front of a single-process simulation is a network hop in the way of
a memcpy.

---

## 9. API and WebSocket architecture

REST for control, WebSocket for the live view. The split is not fashion: control
operations are rare, individually meaningful and want an HTTP status code; the
live view is a high-rate lossy stream where the newest frame supersedes the last.

### Wire protocol

Three frame types at three rates for three reasons:

| Frame | Rate | Encoding | Why |
| --- | --- | --- | --- |
| Vehicles | 8 Hz | binary, 10 B/vehicle | The only high-rate payload |
| Network | 2 Hz | dense byte per edge | Client has the map; no ids need to travel |
| Snapshot/metrics/events | 2 Hz | JSON | Readability worth more than bytes |

**Viewport culling** is the interesting decision. At 60k vehicles a full frame
is ~600 KB, so 4.8 MB/s per client. The client cannot draw 60k distinct dots on
a 1600-pixel canvas. Culling to the viewport and capping the count bounds the
frame at ~40 KB and loses nothing a human could have seen. Emergency and transit
vehicles are never sampled out, because those are what an operator is watching.

Positions are sent as 16-bit fractions *of the viewport*, so the wire cost of a
vehicle is independent of city size: a 16 km map streams at the same rate as a
4 km one.

The dense congestion frame is a byte per edge with no ids at all — 40 KB for
40,000 edges, against ~200 KB for an id-value stream.

The WebSocket implementation is [written, not imported](../internal/api/websocket.go);
ADR-010 gives the reasoning and the limits.

---

## 10. Distributed execution

### The honest position

**Everything below runs in one process today**, with region workers as
goroutines. The design is the distributed design — regions, ownership, handoff,
a serial commit — and the transport is the only thing that would change. That
ordering is deliberate: the hard part of distributed simulation is the
*consistency model*, not the RPC. Getting it right in-process, where it can be
tested exhaustively in seconds, is how you find out that your commit order
depends on partition count before you have four machines disagreeing about it.

### Partitioning: districts are for humans, cells are for schedulers

The obvious partition is one district per worker, and the first version did
exactly that. It scaled badly and the benchmark said why: downtown carried ~4×
the load of a peripheral district, every barrier waited for that worker, and
nine workers delivered 1.8× against a theoretical 6.6×.

Districts are a unit of **meaning** — they name places and group metrics. They
are a terrible unit of **scheduling**, because their sizes are set by geography.

So execution is partitioned on a separate finer grid of cells, assigned to
workers by measured load using longest-processing-time-first bin packing, and
reassigned once per simulated minute.

### Why rebalancing mid-run is safe

It would be terrifying in most engines. It is safe here because the engine is
*provably partition-independent* — the same property that makes 1 worker and 9
workers agree makes "9 workers, differently arranged" agree too. And because
load is measured in simulation state rather than wall-clock time, the partition
layout is itself a pure function of the run: a replay reproduces not just the
same city but the same schedule.

### Entity handoff

An edge belongs to the cell of its **destination** node; a vehicle belongs to
whoever owns its current edge. So ownership changes at exactly one well-defined
point — the moment a vehicle enters a new edge — rather than in a fuzzy boundary
zone. The transfer happens in the serial commit, so duplication and loss are not
possible by construction rather than by protocol.

### When to actually distribute

Move to multiple processes when **one machine cannot hold the tick budget for
the target city**, and not before. Concretely: when the measured tick time at
GOMAXPROCS exceeds 100 ms for the city you need. On the numbers below, that is
somewhere past a million agents.

The migration is then: replace the in-process barrier with a NATS
request/reply barrier, ship intent batches instead of appending to a slice, and
elect one process to run the commit. The systems code does not change — it
already has no idea whether it is running on one core or twelve.

**NATS, not Kafka**, when that day comes: this needs low-latency request/reply
and ephemeral fan-out, not a durable partitioned log. The durable log here is
the command log, and it is kilobytes.

---

## 11. Failure and recovery

Recovery and the user-facing "rewind" button are **the same code path**, on
purpose. A recovery mechanism only exercised during an incident is a recovery
mechanism that does not work.

```
restore(tick):
  load checkpoint          → reject on bad magic, version, CRC, or digest
  rebuild derived indices  → partition, power fan-out, departure buckets
  replay commands since    → deterministic, so it lands exactly where it was
```

Recovery is *exact*, not approximate: after replay the digest equals the digest
before the crash. The chaos lab's `checkpoint_recovery` experiment reports that
comparison rather than just reporting success.

### The chaos lab

| Experiment | What it proves |
| --- | --- |
| `checkpoint_recovery` | Recovery reproduces state bit for bit |
| `corrupt_checkpoint` | A damaged checkpoint is refused, and which layer caught it |
| `determinism_probe` | The CI determinism test, run against a live system |
| `event_storm` | Observability degrades while the tick rate holds |
| `region_overload` | Partition imbalance is visible in the phase timings |

Every experiment measures, breaks, recovers and measures again — and reports
honestly when recovery was not exact. An experiment that can only report success
is theatre.

---

## 12. Counterfactual simulation

```
fork(parent):
    clone dynamic state      ~30 MB at 100k agents
    clone command log        kilobytes
    share the map            0 bytes — it is immutable
```

A fork does **not** re-simulate from tick zero. The value of a counterfactual is
that both arms start from a state identical *by construction*; re-simulating
would make them identical only by luck, and only if nothing had drifted.

`Log.Clone()` copies the entire log **including commands scheduled in the
future**, and that is the right default. "B is A with adaptive signals" means the
two runs share their history *and* their scripted future — the same accident at
the same tick, the same storm at the same tick. A fork that silently dropped
pending commands would measure the difference in event streams as much as the
difference in policies, which is exactly the confound a counterfactual exists to
eliminate. `Log.Fork(tick)` is the separate, explicit "write your own future"
operation.

Comparison reports mean/P50/P95/P99 travel time, delay against free flow,
emergency response, trips completed and abandoned, vehicle-hours stopped, fuel,
CO₂, distance, peak hospital utilisation, diversions, transit boardings and
passengers left behind, incidents, substation trips, and reroutes — each with a
`lowerIsBetter` flag so the UI cannot colour an improvement as a regression.

**And it reports warnings**: different maps, different simulated times, fewer
than 200 completed trips. That is the difference between an analysis tool and a
number generator.

---

## 13. AI agent architecture

The design rule: **the agent has no privileged path into the engine.** Every
tool is a thin wrapper over the same APIs the HTTP layer uses. Anything the
agent can do, an operator could do by hand; anything it cannot do, it cannot do
by finding a cleverer prompt. There is no shell tool, no SQL tool, and no way to
name a raw numeric event kind.

Three tiers:

| Tier | Access | Gate |
| --- | --- | --- |
| **read** | Traffic, population, hospitals, power, transit, events, incidents, metrics | Always |
| **sandboxed** | Fork a scenario, apply a policy, run it, return metrics; compare | Always |
| **mutate** | Change the live simulation | Per-request grant **∩** caller's role |

The sandboxed tier is the interesting one. The agent can experiment freely
because a fork cannot affect its parent — counterfactual reasoning is exactly
the capability an operations agent needs *and* exactly the capability that is
safe to hand over.

Mutation authority is the **intersection** of what the caller asked for and what
their role permits. A viewer cannot grant an agent powers they do not have
themselves. Earthquakes and every chaos command are excluded from the agent's
event list entirely.

Every tool call is surfaced in the transcript. An agent that reports a
conclusion without showing which tools produced it is indistinguishable from one
that made the conclusion up.

**Without an API key the feature still works.** A deterministic planner
classifies the request, calls the same tools, and writes a report from the
results — including forking, running and comparing scenarios for a "what if"
question. Two reasons: a portfolio project whose headline feature needs someone
else's billing account is not demonstrable, and it is the fallback when the
model is unreachable, so a third-party outage does not become an outage in the
operations console. It is worse at open-ended questions and says so.

---

## 14. Security model

| Control | Implementation |
| --- | --- |
| Authentication | API keys, SHA-256 stored, constant-time compare |
| Authorisation | Three roles: viewer / operator / admin |
| Rate limiting | Token bucket per key, lazily refilled |
| Audit | Bounded in-memory ring + structured log per mutating request |
| Transport | Strict CSP, `nosniff`, `DENY` framing, no-referrer |
| CORS | Exact-origin match only; never reflects arbitrary origins |
| Input | 1 MB body cap, unknown fields rejected, allow-listed event kinds |
| WebSocket | Credential accepted as a query parameter on that route only |
| AI | Tiered tools, per-request mutation grant, no arbitrary execution |

Three decisions worth defending:

- **Production mode with no keys configured refuses to start.** An
  authentication system whose failure mode is "allow everything" is not an
  authentication system.
- **The dev-session endpoint 404s in production**, so its existence is not a hint.
- **The event API takes names, not numeric kinds.** Commands are the replay
  contract; accepting an arbitrary integer means a future renumbering silently
  changes what stored logs mean.

Multi-tenant isolation is *designed but not built*: the `tenant_id` column
exists in the schema and every simulation id would be namespaced. It is called
out here rather than claimed.

---

## 15. Observability

Structured logs (`log/slog`), Prometheus metrics, and an in-process event
stream. Exported per simulation: tick, tick rate, tick duration, **serial
fraction**, active vehicles, intents, dropped events, checkpoints, trips
completed and abandoned, travel-time and response-time quantiles, open
incidents, substations online, hospital utilisation, route queries and failures.
Plus per-route API latency histograms and status counts.

The scrape endpoint is deliberately unauthenticated — a scrape endpoint behind a
bearer token is a scrape endpoint nobody scrapes — and it exports counts, rates
and durations only, never entity ids or district names.

Route labels are normalised to templates. Using the raw path would create one
time series per simulation id, which is the canonical way to melt a Prometheus
instance.

The Prometheus client library is not used; ADR-012 explains why, and what would
make that trade flip.

---

## 16. Testing strategy

| Layer | What it asserts |
| --- | --- |
| `TestSameSeedSameResult` | Identical inputs → identical digest at every tick |
| `TestSerialEqualsParallel` | 1 worker and 4 workers agree tick for tick |
| `TestParallelRegionCounts` | 1, 2, 3, 4, 8, 9 workers all agree |
| `TestReplayFromLog` | Params + seed + commands reproduce the run |
| `TestCheckpointRestore` | Round-trip is lossless; continuation matches |
| `TestForkIsolation` | A fork tracks its parent until it intervenes, then diverges, and never leaks back |
| `TestMapGenerationStable` | The generator is a pure function of its params |
| `TestMapConnected` | No unreachable island in the road network |
| `TestHeuristicAdmissible` | A\* stays optimal, so routes stay order-independent |
| `TestNoFloatInState` | Reflection walk over `state.State` |
| `TestISqrtExact` | Fast root vs. reference over ~1M inputs |

**Every one of these has caught a real bug.** The determinism suite found four
during development:

1. Per-region intent commit order made the winner of a contended link depend on
   partition count.
2. A per-region reroute budget was a function of how many vehicles a region
   happened to own.
3. Dispatcher cooldown state lived outside the checkpoint, so a restored engine
   dispatched on different ticks.
4. An unsorted command log silently dropped commands scheduled out of order.

And the rebalance test found a fifth: intents generated *during* the commit phase
were dropped, so transit passengers never walked their last leg.

That is the argument for writing the determinism tests first. None of these
would have been found by a unit test of any individual system, and all of them
would have been extremely unpleasant to diagnose from a production replay
mismatch.

CI runs the suite under the race detector on Linux.

---

## 17. Benchmarking

Rules the harness follows: fixed seed and workload, a warm-up before measuring
(the first ticks have an empty network and would flatter every result),
percentiles rather than a mean, and the serial fraction reported next to the
speed-up so the Amdahl prediction can be checked against the measurement.

**Medium city, 45×45 grid, 60k residents, 2,617 concurrent vehicles**
(Intel i5-10400F, 6C/12T, Go 1.27):

| workers | ticks/s | × real time | mean tick | p99 | serial |
| --- | --- | --- | --- | --- | --- |
| 1 | 2,923 | 292× | 0.34 ms | 1.01 ms | 4.7% |
| 2 | 3,904 | 390× | 0.26 ms | 1.01 ms | 4.5% |
| 4 | 4,528 | 453× | 0.22 ms | 1.01 ms | 4.9% |
| 9 | 4,439 | 444× | 0.23 ms | 1.01 ms | 4.6% |

**Large city, 81×81 grid, 200k residents, 10,173 concurrent vehicles:**

| workers | ticks/s | × real time | mean tick | p99 |
| --- | --- | --- | --- | --- |
| 1 | 326 | 33× | 3.07 ms | 11.0 ms |
| 4 | 717 | 72× | 1.39 ms | 5.0 ms |
| 8 | 805 | 81× | 1.24 ms | 4.0 ms |

Every configuration produces the **same state digest**, which is the number that
makes the rest of the table mean anything.

### What these numbers do not say

- Scaling flattens past ~4 workers on this machine. Some is the serial commit,
  some is memory bandwidth, and past 6 physical cores hyperthreads share
  execution units. It is reported, not explained away.
- Per-tick percentiles are quantised by the Windows timer; `ticks/s` is measured
  over the whole run and is the authoritative figure.
- **"Hundreds of thousands of entities" is a population claim, not a concurrency
  claim.** 200k residents produce ~10k concurrent vehicles at peak, because trips
  take minutes and are spread over a departure distribution. Simulating 200k
  *simultaneously moving* vehicles is a different problem and this project has
  not measured it.

---

## 18. Deployment

```
mirrord ── one container: engine + API + UI
   ├── /data   checkpoints (PVC)
   ├── :8080   HTTP + WebSocket
   └── /metrics
```

One process, one image. The UI is static files served by the same binary — a
separate nginx would add a hop and a deployment unit to serve 200 KB.

Kubernetes manifests, a Dockerfile and Terraform are in [`deploy/`](../deploy).
The deployment is a `StatefulSet`, not a `Deployment`, because a simulation has
identity and its checkpoints belong to it, and sessions are sticky because a
WebSocket stream is bound to the process holding that simulation's state.

**Scaling is vertical until it cannot be.** Horizontal scaling of a single
simulation is section 10's migration; horizontal scaling of *many independent*
simulations is a routing problem, not a distributed-systems problem, and should
be solved by a consistent-hash router in front of N pods.

---

## 19. Repository structure

```
cmd/mirrord        server: engine + API + UI
cmd/mirrorbench    reproducible benchmarks
internal/units     integer units; the no-float rule lives here
internal/rng       PCG32 with per-entity stream derivation
internal/world     immutable map, generator, spatial index, A*
internal/state     mutable state, canonical codec, digest, clone
internal/events    command/effect split, log, bounded ring
internal/systems   simulation logic; pure functions over (Ctx, Region)
internal/engine    tick orchestration, partitioning, rebalancing
internal/store     checkpoints and command persistence
internal/simctl    lifecycle, forking, comparison, recovery
internal/api       HTTP, WebSocket, auth, chaos lab
internal/agent     AI tools, LLM loop, deterministic planner
internal/telemetry Prometheus exposition
web/               React + TypeScript console
deploy/            Docker, Kubernetes, Terraform
docs/              this blueprint, ADRs, benchmarks
```

`internal/systems` depends on `world`, `state` and `events` and on nothing else.
It has no idea goroutines exist.

---

## 20. Phases and acceptance criteria

| Phase | Deliverable | Acceptance |
| --- | --- | --- |
| 1 | Deterministic core | Same seed → same digest over 3,000 ticks |
| 2 | Traffic + routing | A jam forms, propagates backwards, and clears |
| 3 | Parallel regions | 1 and N workers produce identical digests |
| 4 | Events + checkpoints | Replay and restore reproduce state exactly |
| 5 | Fork + compare | Uninterventioned fork stays in lockstep; policy change diverges |
| 6 | Infrastructure cascades | A substation trip changes traffic with no scripted link |
| 7 | Transport | Binary stream holds ≤50 KB/frame at 60k vehicles |
| 8 | Console | A jam is visible and clickable without reading a log |
| 9 | AI tools | A what-if answered by forking, running and comparing |
| 10 | Chaos lab | Recovery reports exactness, corruption is refused |
| 11 | Benchmarks | Reproducible, with the serial fraction reported |

Phases 1–11 are implemented. Everything below is not.

---

## 21. Major risks

| Risk | Why it is dangerous | Mitigation |
| --- | --- | --- |
| **Silent nondeterminism** | Invisible until a replay mismatch months later | Digest tests across partition counts, in CI |
| **Traffic model instability** | Oscillation looks like emergent behaviour | Patience thresholds and replan jitter, both tested |
| **Plausible-but-wrong metrics** | The most dangerous failure: confident and untrue | Validity warnings on every comparison |
| **Serial commit becoming the ceiling** | Caps scaling silently | Serial fraction exported and benchmarked |
| **Checkpoint drift** | Engine state outside the checkpoint | Restore test compares digests |
| **AI overreach** | An agent doing something irreversible | Tiered tools, per-request grants, sandbox-first |
| **Demo-driven modelling** | Tuning until the demo looks good | Calibration choices stated as choices |

---

## 22. What not to build

Each of these was considered and rejected. The rejection is the point.

- **A 3D city.** The view is a 2D information display. A perspective camera puts
  the far side of the city at a different scale from the near side, which is
  precisely wrong for comparing districts, and extruded buildings occlude the
  thing the page exists to show. (ADR-011)
- **An LLM per agent.** Architecturally and economically absurd, and it would
  destroy determinism. The decision being modelled is a threshold comparison.
- **Kafka.** The durable log is kilobytes. Kafka solves a problem this system
  does not have.
- **A microservice per subsystem.** The systems share one state array and run
  inside a 300 µs tick. Splitting them means a network hop inside that tick.
- **Event-sourcing everything.** ~2 GB/hour to reconstruct what a 30 MB
  checkpoint reconstructs exactly.
- **Postgres from day one.** A network hop in front of a memcpy.
- **Real OSM data in v1.** A generated map is reproducible from 40 bytes; an OSM
  extract is a content-addressed artefact that must ship with every replay. The
  `Map` type is agnostic, so an importer is strictly additive.
- **A charting library.** Six sparklines against a 1,440-point series, updating
  twice a second. Forty lines of canvas, and no imported opinions about
  typography.
- **Redis.** There is no cross-process state to share yet. When there is, the
  question is a barrier, and that is NATS.

---

## 23. What makes this genuinely impressive rather than overengineered

Not the component count. These:

1. **A falsifiable determinism claim.** `TestParallelRegionCounts` runs the same
   simulation at 1, 2, 3, 4, 8 and 9 workers and requires identical digests.
   That is a claim that can fail, and it did — four times, each a real bug.
2. **Two optimisations proven bit-exact.** The state digest after replacing
   `ISqrt` and making the edge pass incremental is the *identical value*
   measured before them. The engine got 2.4× faster and provably did not change.
3. **Live repartitioning that is invisible.** Workers get reassigned every
   simulated minute and the digest does not move, because the engine is
   partition-independent by construction.
4. **Measurement driving the architecture.** District-per-worker was replaced
   because a benchmark said 1.8× against 6.6×. `ISqrt` was replaced because a
   profile said 42%.
5. **Comparisons that refuse to mislead.** Warnings when the sample is too small
   or the arms are at different times.
6. **An AI with real, bounded authority.** Sandboxed by default; mutation is the
   intersection of a per-request grant and the caller's role.
7. **Simplifications stated as simplifications.** No transfers in transit,
   emergency units teleport home, flooding uses road class as a proxy for
   elevation, the crash rate is tuned for legibility.

---

## 24. Recommended implementation order

If starting again, in this order, and not moving on until the acceptance
criterion holds:

1. **Units and RNG.** Ban floats first. Retrofitting determinism is misery.
2. **Immutable map + generator.** Pin the map hash immediately.
3. **State + canonical codec + digest.** One encoder for checkpoint and hash.
4. **Serial tick with one system.** Get `TestSameSeedSameResult` green with
   almost nothing in the engine.
5. **Traffic and routing.** Verify a jam forms and clears before adding anything.
6. **Intents and the serial commit — before parallelism.** The commit is the
   consistency model. Introducing it after goroutines means debugging both.
7. **Regions and the barrier.** `TestSerialEqualsParallel` should pass the day
   parallelism is added, because the model was already correct.
8. **Commands, checkpoints, replay.** Now recovery is testable.
9. **Fork and compare.** The product exists at this point.
10. **Infrastructure cascades.** Emergent behaviour, no scripted links.
11. **Transport and console.** Only now, because you need something to look at.
12. **Benchmarks, then optimise.** Never before.
13. **AI tools.** Last: they are a layer over a working platform, not a
    substitute for one.
14. **Chaos lab.** Last, and treat every "recovery was not exact" as a bug.

The ordering principle: **every step must be verifiable by a test that can
fail.** Steps 6 and 7 in that order are the single most important choice in the
list.

---

## Architecture decision records

| ADR | Decision |
| --- | --- |
| [001](adr/ADR-001-discrete-time-simulation.md) | Fixed-step discrete time, not discrete-event |
| [002](adr/ADR-002-go.md) | Go as the engine language |
| [003](adr/ADR-003-integer-only-state.md) | Integer-only simulation state |
| [004](adr/ADR-004-determinism.md) | How determinism is guaranteed |
| [005](adr/ADR-005-two-phase-tick.md) | Two-phase tick with a serial commit |
| [006](adr/ADR-006-event-sourcing.md) | Commands are sourced; effects are derived |
| [007](adr/ADR-007-partitioning.md) | Load-balanced cells, not districts |
| [008](adr/ADR-008-no-message-broker.md) | No broker yet, and NATS when there is |
| [009](adr/ADR-009-generated-maps.md) | Generated maps before OSM |
| [010](adr/ADR-010-own-websocket.md) | Own WebSocket implementation |
| [011](adr/ADR-011-canvas-not-three.md) | Canvas2D, not Three.js |
| [012](adr/ADR-012-own-prometheus-exposition.md) | Own Prometheus exposition |
| [013](adr/ADR-013-ai-tool-boundaries.md) | AI tool permission boundaries |
