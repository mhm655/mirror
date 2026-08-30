package api

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/mirror-sim/mirror/internal/engine"
	"github.com/mirror-sim/mirror/internal/events"
	"github.com/mirror-sim/mirror/internal/simctl"
	"github.com/mirror-sim/mirror/internal/state"
	"github.com/mirror-sim/mirror/internal/telemetry"
	"github.com/mirror-sim/mirror/internal/units"
	"github.com/mirror-sim/mirror/internal/world"
)

// handlePrometheus renders the scrape endpoint.
//
// It is intentionally unauthenticated: a scrape endpoint behind a bearer token
// is a scrape endpoint nobody scrapes, and in every realistic deployment it is
// reachable only from inside the cluster. What it must NOT do is leak
// simulation content -- so it exports counts, rates and durations, and never
// an entity id, a district name or anything a viewer's own credentials would
// otherwise gate.
func (s *Server) handlePrometheus(w http.ResponseWriter, r *http.Request) {
	tw := telemetry.NewWriter()

	tw.Help("mirror_build_info", "Build metadata", "gauge")
	tw.Gauge("mirror_build_info", 1, "version", Version, "goversion", GoVersion)

	tw.Help("mirror_websocket_connections", "Currently connected stream clients", "gauge")
	tw.Gauge("mirror_websocket_connections", float64(s.wsConns.Load()))

	tw.Help("mirror_http_requests_total_all", "All API requests handled", "counter")
	tw.Counter("mirror_http_requests_total_all", float64(s.reqTotal.Load()))
	tw.Help("mirror_http_request_errors_total", "API requests answered with 4xx or 5xx", "counter")
	tw.Counter("mirror_http_request_errors_total", float64(s.reqErr.Load()))

	sims := s.mgr.List()
	tw.Help("mirror_simulations", "Registered simulations", "gauge")
	tw.Gauge("mirror_simulations", float64(len(sims)))

	tw.Help("mirror_sim_tick", "Current simulation tick", "gauge")
	tw.Help("mirror_sim_ticks_per_second", "Achieved tick rate", "gauge")
	tw.Help("mirror_sim_tick_duration_ms", "Duration of the most recent tick", "gauge")
	tw.Help("mirror_sim_tick_serial_fraction", "Fraction of tick time spent in the serial commit phase", "gauge")
	tw.Help("mirror_sim_active_vehicles", "Vehicles currently on the network", "gauge")
	tw.Help("mirror_sim_agents", "Simulated population", "gauge")
	tw.Help("mirror_sim_regions", "Region workers", "gauge")
	tw.Help("mirror_sim_intents", "Intents committed in the most recent tick", "gauge")
	tw.Help("mirror_sim_events_dropped_total", "Effect events dropped from the ring buffer", "counter")
	tw.Help("mirror_sim_checkpoints_total", "Checkpoints written", "counter")
	tw.Help("mirror_sim_trips_completed_total", "Completed trips", "counter")
	tw.Help("mirror_sim_trips_abandoned_total", "Abandoned trips", "counter")
	tw.Help("mirror_sim_travel_seconds", "Travel time distribution over completed trips", "summary")
	tw.Help("mirror_sim_emergency_response_seconds", "Emergency response time distribution", "summary")
	tw.Help("mirror_sim_incidents_open", "Currently unresolved incidents", "gauge")
	tw.Help("mirror_sim_substations_online", "Substations energised", "gauge")
	tw.Help("mirror_sim_hospital_utilisation", "Occupied beds as a fraction of capacity", "gauge")
	tw.Help("mirror_sim_route_queries_total", "A* queries issued", "counter")
	tw.Help("mirror_sim_route_failures_total", "Routing attempts that found no path", "counter")

	for _, sim := range sims {
		id := sim.ID
		sim.Read(func(e *engine.Engine) {
			st := e.S
			mt := &st.Metrics
			l := []string{"sim", id, "name", sim.Name}
			tw.Gauge("mirror_sim_tick", float64(st.Tick), l...)
			tw.Gauge("mirror_sim_ticks_per_second", float64(sim.TPS()), l...)
			tw.Gauge("mirror_sim_tick_duration_ms", float64(e.Stat.TotalNanos)/1e6, l...)
			tw.Gauge("mirror_sim_tick_serial_fraction", float64(e.Stat.SerialPercent)/100, l...)
			tw.Gauge("mirror_sim_active_vehicles", float64(e.Stat.ActiveVeh), l...)
			tw.Gauge("mirror_sim_agents", float64(st.Agents.Len()), l...)
			tw.Gauge("mirror_sim_regions", float64(sim.Cfg.Regions), l...)
			tw.Gauge("mirror_sim_intents", float64(e.Stat.Intents), l...)
			tw.Counter("mirror_sim_events_dropped_total", float64(e.Ring.Dropped()), l...)
			tw.Counter("mirror_sim_checkpoints_total", float64(sim.Checkpoints()), l...)
			tw.Counter("mirror_sim_trips_completed_total", float64(mt.TripsCompleted), l...)
			tw.Counter("mirror_sim_trips_abandoned_total", float64(mt.TripsAbandoned), l...)
			for _, q := range []struct {
				label string
				p     int64
			}{{"0.5", 500}, {"0.95", 950}, {"0.99", 990}} {
				tw.Gauge("mirror_sim_travel_seconds",
					float64(mt.Travel.Quantile(q.p))/units.TicksPerSecond,
					"sim", id, "name", sim.Name, "quantile", q.label)
				tw.Gauge("mirror_sim_emergency_response_seconds",
					float64(mt.EmergencyResponse.Quantile(q.p))/units.TicksPerSecond,
					"sim", id, "name", sim.Name, "quantile", q.label)
			}
			open := 0
			for i := range st.Incidents {
				if !st.Incidents[i].Resolved {
					open++
				}
			}
			tw.Gauge("mirror_sim_incidents_open", float64(open), l...)
			online := 0
			for i := range st.Subs.Online {
				if st.Subs.Online[i] {
					online++
				}
			}
			tw.Gauge("mirror_sim_substations_online", float64(online), l...)
			var used, total int64
			for i := range e.Map.Hospitals {
				used += int64(st.Hosps.BedsUsed[i])
				total += int64(e.Map.Hospitals[i].Beds)
			}
			util := 0.0
			if total > 0 {
				util = float64(used) / float64(total)
			}
			tw.Gauge("mirror_sim_hospital_utilisation", util, l...)
			tw.Counter("mirror_sim_route_queries_total", float64(mt.RouteQueries), l...)
			tw.Counter("mirror_sim_route_failures_total", float64(mt.RouteFailures), l...)
		})
	}

	s.metric.WriteAPI(tw)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = io.WriteString(w, tw.String())
}

// Version metadata, overridden at build time with -ldflags.
var (
	Version   = "dev"
	GoVersion = "unknown"
)

// --------------------------------------------------------- entity inspect --

// inspectEntity answers "what is this thing I clicked on".
//
// One handler across every entity type rather than a route each: the UI's
// inspector is one panel, the shapes are small, and a switch here is far easier
// to keep consistent than eight nearly-identical handlers.
func inspectEntity(e *engine.Engine, kind string, id int32) (any, error) {
	m, st := e.Map, e.S
	switch kind {
	case "vehicle":
		if id < 0 || int(id) >= st.Vehicles.Len() {
			return nil, errors.New("no such vehicle")
		}
		v := &st.Vehicles
		out := map[string]any{
			"id": id, "kind": v.Kind[id].String(), "status": vehStatusName(v.Status[id]),
			"speedKph":       units.MMPerTickToKmh(v.Speed[id]),
			"distanceKm":     float64(v.DistanceMM[id]) / 1e6,
			"stoppedSec":     v.StopTicks[id] / units.TicksPerSecond,
			"ageSec":         (uint32(st.Tick) - v.SpawnTick[id]) / units.TicksPerSecond,
			"routeRemaining": v.RouteLen[id] - v.RouteIdx[id],
			"region":         e.RegionOfEdge(v.Edge[id]),
		}
		if ed := v.Edge[id]; ed >= 0 {
			out["edge"] = int32(ed)
			out["road"] = fmt.Sprintf("%s %s", m.Districts[m.Edges[ed].District].Name, m.Edges[ed].Class)
			out["edgeOccupancy"] = st.Edges.Count[ed]
			out["edgeJam"] = m.Edges[ed].Jam
			out["x"] = int64(m.Nodes[m.Edges[ed].From].X)
			out["y"] = int64(m.Nodes[m.Edges[ed].From].Y)
		}
		if ag := v.Agent[id]; ag >= 0 {
			out["agent"] = ag
		}
		if rt := v.TransitRoute[id]; rt >= 0 {
			out["transitRoute"] = m.Routes[rt].Name
			out["occupancy"] = v.Occupancy[id]
			out["capacity"] = m.Routes[rt].Capacity
		}
		return out, nil

	case "agent":
		if id < 0 || int(id) >= st.Agents.Len() {
			return nil, errors.New("no such agent")
		}
		a := &st.Agents
		out := map[string]any{
			"id": id, "status": a.Status[id].String(), "mode": a.Mode[id].String(),
			"homeDistrict":   m.Districts[a.District[id]].Name,
			"workDistrict":   m.Districts[m.Nodes[a.WorkNode[id]].District].Name,
			"departOut":      fmtTOD(a.DepartOut[id]),
			"departReturn":   fmtTOD(a.DepartRet[id]),
			"tripsDone":      a.TripsDone[id],
			"lastTravelSec":  a.LastTravel[id] / units.TicksPerSecond,
			"freeFlowRefSec": a.FreeRefTicks[id] / units.TicksPerSecond,
			"health":         a.Health[id],
			"patience":       a.PatienceP[id],
			"risk":           a.RiskP[id],
			"homeX":          int64(m.Nodes[a.HomeNode[id]].X),
			"homeY":          int64(m.Nodes[a.HomeNode[id]].Y),
			"workX":          int64(m.Nodes[a.WorkNode[id]].X),
			"workY":          int64(m.Nodes[a.WorkNode[id]].Y),
			"region":         e.RegionOfAgent(id),
		}
		if a.Vehicle[id] >= 0 {
			out["vehicle"] = a.Vehicle[id]
		}
		if a.TRoute[id] >= 0 && int(a.TRoute[id]) < len(m.Routes) {
			out["transitRoute"] = m.Routes[a.TRoute[id]].Name
		}
		return out, nil

	case "edge":
		if id < 0 || int(id) >= len(m.Edges) {
			return nil, errors.New("no such road")
		}
		ed := &m.Edges[id]
		ratio := 100
		if ed.FreeSpeed > 0 {
			ratio = int(int64(st.Edges.Speed[id]) * 100 / int64(ed.FreeSpeed))
		}
		return map[string]any{
			"id": id, "class": ed.Class.String(),
			"district":      m.Districts[ed.District].Name,
			"lengthM":       int64(ed.Length) / 1000,
			"lanes":         ed.Lanes,
			"jam":           ed.Jam,
			"occupancy":     st.Edges.Count[id],
			"speedKph":      units.MMPerTickToKmh(st.Edges.Speed[id]),
			"freeSpeedKph":  units.MMPerTickToKmh(ed.FreeSpeed),
			"speedRatioPct": ratio,
			"blockedLanes":  st.Edges.BlockedLanes[id],
			"closed":        st.Edges.ClosedUntil[id] > uint32(st.Tick),
			"lit":           st.Edges.Lit[id],
			"totalEntries":  st.Edges.EnteredTotal[id],
			"region":        e.RegionOfEdge(world.EdgeID(id)),
			"x1":            int64(m.Nodes[ed.From].X), "y1": int64(m.Nodes[ed.From].Y),
			"x2": int64(m.Nodes[ed.To].X), "y2": int64(m.Nodes[ed.To].Y),
		}, nil

	case "hospital":
		if id < 0 || int(id) >= len(m.Hospitals) {
			return nil, errors.New("no such hospital")
		}
		h := &m.Hospitals[id]
		return map[string]any{
			"id": id, "name": h.Name, "district": m.Districts[h.District].Name,
			"beds": h.Beds, "bedsUsed": st.Hosps.BedsUsed[id],
			"utilisationPct": pct(int64(st.Hosps.BedsUsed[id]), int64(h.Beds)),
			"erBays":         h.ERBays, "erUsed": st.Hosps.ERUsed[id],
			"ambulancesTotal": h.Ambulances, "ambulancesAvailable": st.Hosps.AmbAvail[id],
			"onBackupPower":     st.Hosps.OnBackup[id],
			"backupMinutesLeft": st.Hosps.BackupLeft[id] / units.TicksPerMinute,
			"admissions":        st.Hosps.Admissions[id], "rejections": st.Hosps.Rejections[id],
			"diversions": st.Hosps.Diverted[id],
			"x":          int64(h.X), "y": int64(h.Y),
		}, nil

	case "substation":
		if id < 0 || int(id) >= len(m.Substations) {
			return nil, errors.New("no such substation")
		}
		ss := &m.Substations[id]
		return map[string]any{
			"id": id, "name": ss.Name, "district": m.Districts[ss.District].Name,
			"online": st.Subs.Online[id], "loadKW": st.Subs.LoadKW[id],
			"capacityKW": ss.CapacityKW, "connectedKW": ss.BaseKW,
			"utilisationPct": pct(int64(st.Subs.LoadKW[id]), int64(ss.CapacityKW)),
			"trips":          st.Subs.Trips[id],
			"restoreInSec":   restoreIn(st, id),
			"neighbours":     ss.Neighbours,
			"x":              int64(ss.X), "y": int64(ss.Y),
		}, nil

	case "signal":
		if id < 0 || int(id) >= len(m.Signals) {
			return nil, errors.New("no such signal")
		}
		sg := &m.Signals[id]
		return map[string]any{
			"id": id, "node": int32(sg.Node),
			"phase": st.Signals.Phase[id], "powered": st.Signals.Powered[id],
			"greenEastWestSec":   st.Signals.Green0[id] / units.TicksPerSecond,
			"greenNorthSouthSec": st.Signals.Green1[id] / units.TicksPerSecond,
			"queueEastWest":      st.Signals.Queue0[id],
			"queueNorthSouth":    st.Signals.Queue1[id],
			"preemptTicks":       st.Signals.Preempt[id],
			"x":                  int64(m.Nodes[sg.Node].X), "y": int64(m.Nodes[sg.Node].Y),
		}, nil

	case "district":
		stats := simctl.DistrictStats(e)
		if id < 0 || int(id) >= len(stats) {
			return nil, errors.New("no such district")
		}
		return stats[id], nil
	}
	return nil, errors.New("kind must be one of: vehicle, agent, edge, hospital, substation, signal, district")
}

func vehStatusName(s state.VehicleStatus) string {
	switch s {
	case state.VehMoving:
		return "moving"
	case state.VehQueued:
		return "queued"
	case state.VehArrived:
		return "arrived"
	case state.VehDisabled:
		return "disabled"
	}
	return "idle"
}

func pct(a, b int64) int64 {
	if b == 0 {
		return 0
	}
	return a * 100 / b
}

func fmtTOD(t int32) string {
	h := t / units.TicksPerHour
	mn := (t % units.TicksPerHour) / units.TicksPerMinute
	return fmt.Sprintf("%02d:%02d", h, mn)
}

func restoreIn(st *state.State, id int32) int64 {
	if st.Subs.Online[id] || st.Subs.RestoreAt[id] == 0 {
		return 0
	}
	d := int64(st.Subs.RestoreAt[id]) - int64(st.Tick)
	if d < 0 {
		return 0
	}
	return d / units.TicksPerSecond
}

// ------------------------------------------------------------ chaos lab ----

// ChaosResult is what an experiment reports back.
type ChaosResult struct {
	Experiment       string   `json:"experiment"`
	Sim              string   `json:"sim"`
	Description      string   `json:"description"`
	DigestBefore     string   `json:"digestBefore"`
	DigestAfter      string   `json:"digestAfter"`
	TickBefore       uint64   `json:"tickBefore"`
	TickAfter        uint64   `json:"tickAfter"`
	RecoveredFrom    uint64   `json:"recoveredFromTick,omitempty"`
	TicksReplayed    int      `json:"ticksReplayed,omitempty"`
	RecoveryMillis   int64    `json:"recoveryMillis,omitempty"`
	StateDiverged    *bool    `json:"stateDiverged,omitempty"`
	EventsDropped    uint64   `json:"eventsDropped"`
	ThroughputBefore float64  `json:"ticksPerSecBefore"`
	ThroughputAfter  float64  `json:"ticksPerSecAfter"`
	Notes            []string `json:"notes"`
}

// runChaos executes one controlled failure experiment.
//
// Every experiment follows the same shape: measure, break, recover, measure,
// and report what actually happened -- including when the answer is "recovery
// was not exact". An experiment that can only report success is theatre.
func (s *Server) runChaos(req chaosReq) (*ChaosResult, error) {
	sim, ok := s.mgr.Get(req.Sim)
	if !ok {
		return nil, errors.New("no such simulation")
	}
	res := &ChaosResult{Experiment: req.Experiment, Sim: req.Sim}
	var beforeDigest uint64
	sim.Read(func(e *engine.Engine) {
		beforeDigest = e.S.Digest()
		res.TickBefore = uint64(e.S.Tick)
		res.EventsDropped = e.Ring.Dropped()
	})
	res.DigestBefore = fmt.Sprintf("%016x", beforeDigest)
	res.ThroughputBefore = float64(sim.TPS())

	switch req.Experiment {
	case "checkpoint_recovery":
		// Kill and restore from the last good checkpoint, then replay forward
		// to where we were. The interesting number is not that it worked but
		// whether the digest matches: if it does, recovery is exact.
		res.Description = "Restore from the most recent checkpoint and replay to the original tick"
		start := time.Now()
		from, replayed, err := s.mgr.Restore(req.Sim, 0)
		if err != nil {
			return nil, err
		}
		res.RecoveredFrom, res.TicksReplayed = uint64(from), replayed
		res.RecoveryMillis = time.Since(start).Milliseconds()
		var after uint64
		sim.Read(func(e *engine.Engine) {
			after = e.S.Digest()
			res.TickAfter = uint64(e.S.Tick)
		})
		res.DigestAfter = fmt.Sprintf("%016x", after)
		diverged := after != beforeDigest
		res.StateDiverged = &diverged
		if diverged {
			res.Notes = append(res.Notes,
				"State diverged after recovery. Something the engine depends on is not in the checkpoint or not in the command log.")
		} else {
			res.Notes = append(res.Notes,
				fmt.Sprintf("Recovery was exact: replayed %d ticks from tick %d and reproduced the state bit for bit.", replayed, from))
		}

	case "corrupt_checkpoint":
		// Prove that a damaged checkpoint is refused rather than loaded.
		res.Description = "Flip bytes in the newest checkpoint and attempt to restore from it"
		hs, err := s.mgr.StoreRef().ListCheckpoints(req.Sim)
		if err != nil || len(hs) == 0 {
			return nil, errors.New("no checkpoints yet; let the simulation run first")
		}
		h := hs[len(hs)-1]
		_, raw, err := s.mgr.StoreRef().GetCheckpoint(req.Sim, h.Tick)
		if err != nil {
			return nil, err
		}
		bad := bytes.Clone(raw)
		for i := len(bad) / 3; i < len(bad)/3+64 && i < len(bad); i++ {
			bad[i] ^= 0xFF
		}
		if _, err := s.mgr.StoreRef().PutCheckpoint(req.Sim+"__corrupt", h, bad); err != nil {
			return nil, err
		}
		_, _, derr := s.mgr.StoreRef().GetCheckpoint(req.Sim+"__corrupt", h.Tick)
		// The CRC covers the uncompressed payload, so a corrupted blob is
		// caught either by gzip or by the checksum -- both are ErrCorrupt.
		if derr == nil {
			// Re-decoding succeeded, which means the flip landed somewhere the
			// integrity check does not cover. That is a finding, and it is
			// reported as one rather than swallowed.
			st, sErr := state.Decode(mustRaw(s, req.Sim+"__corrupt", h.Tick))
			if sErr != nil || st.Digest() != h.Digest {
				res.Notes = append(res.Notes, "Corruption slipped past the CRC but was caught by the state digest.")
			} else {
				res.Notes = append(res.Notes, "WARNING: corrupted checkpoint was accepted. Integrity checking is insufficient.")
			}
		} else {
			res.Notes = append(res.Notes, "Corrupted checkpoint was rejected: "+derr.Error())
		}
		res.TickAfter, res.DigestAfter = res.TickBefore, res.DigestBefore

	case "event_storm":
		// Overwhelm the effect ring and confirm the simulation keeps its rate
		// while observability degrades -- the correct trade, since effects are
		// not the system of record.
		res.Description = "Flood the network with incidents and observe whether the tick rate holds while the event ring overflows"
		n := req.Param
		if n <= 0 || n > 400 {
			n = 60
		}
		var edges int64
		sim.Read(func(e *engine.Engine) { edges = int64(len(e.Map.Edges)) })
		for i := int64(0); i < n; i++ {
			edge := (i * 7919) % edges
			if _, err := s.mgr.Inject(req.Sim, events.C(0, events.CmdInjectAccident, edge, 900, 2, 0)); err != nil {
				break
			}
		}
		time.Sleep(2 * time.Second)
		sim.Read(func(e *engine.Engine) {
			res.TickAfter = uint64(e.S.Tick)
			res.DigestAfter = fmt.Sprintf("%016x", e.S.Digest())
			res.EventsDropped = e.Ring.Dropped()
		})
		res.Notes = append(res.Notes, fmt.Sprintf("Injected %d incidents.", n))

	case "region_overload":
		// Concentrate all demand in one district to expose partition
		// imbalance in the phase A timings.
		res.Description = "Force all pending demand into one district and measure the effect on tick time"
		var district int64 = req.Param
		if _, err := s.mgr.Inject(req.Sim, events.C(0, events.CmdSpawnTraffic, 4000, district, 0, 0)); err != nil {
			return nil, err
		}
		time.Sleep(2 * time.Second)
		sim.Read(func(e *engine.Engine) {
			res.TickAfter = uint64(e.S.Tick)
			res.DigestAfter = fmt.Sprintf("%016x", e.S.Digest())
			res.Notes = append(res.Notes,
				fmt.Sprintf("Tick %.2f ms, of which %.2f ms serial (%d%%).",
					float64(e.Stat.TotalNanos)/1e6, float64(e.Stat.PhaseBNanos)/1e6, e.Stat.SerialPercent))
		})

	case "determinism_probe":
		// Fork, run both arms with no intervention, and confirm they stay
		// identical. This is the determinism test, runnable against a live
		// system rather than only in CI.
		res.Description = "Fork the simulation, advance both arms without intervention, and compare digests"
		child, err := s.mgr.Fork(req.Sim, "determinism probe")
		if err != nil {
			return nil, err
		}
		defer func() { _ = s.mgr.Delete(child.ID) }()
		n := int(req.Param)
		if n <= 0 || n > 20000 {
			n = 600
		}
		var da, db uint64
		sim.Write(func(e *engine.Engine) {
			for i := 0; i < n; i++ {
				e.Tick()
			}
			da = e.S.Digest()
			res.TickAfter = uint64(e.S.Tick)
		})
		child.Write(func(e *engine.Engine) {
			for i := 0; i < n; i++ {
				e.Tick()
			}
			db = e.S.Digest()
		})
		res.DigestAfter = fmt.Sprintf("%016x", da)
		diverged := da != db
		res.StateDiverged = &diverged
		if diverged {
			res.Notes = append(res.Notes, fmt.Sprintf("DIVERGENCE: parent %016x, fork %016x after %d ticks.", da, db, n))
		} else {
			res.Notes = append(res.Notes, fmt.Sprintf("Parent and fork produced identical state after %d ticks.", n))
		}

	default:
		return nil, errors.New("experiment must be one of: checkpoint_recovery, corrupt_checkpoint, event_storm, region_overload, determinism_probe")
	}

	res.ThroughputAfter = float64(sim.TPS())
	sort.Strings(res.Notes)
	return res, nil
}

func mustRaw(s *Server, id string, tick units.Tick) []byte {
	_, raw, _ := s.mgr.StoreRef().GetCheckpoint(id, tick)
	return raw
}
