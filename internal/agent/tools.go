// Package agent gives an LLM real control of the simulation platform through a
// bounded tool surface.
//
// The design rule that matters most here: the agent has no privileged path
// into the engine. Every tool is a thin wrapper over the same simctl and
// engine APIs the HTTP layer uses, so anything the agent can do, an operator
// could do by hand, and anything it cannot do, it cannot do by finding a
// cleverer prompt. There is no "execute arbitrary command" tool, no SQL tool,
// and no way to name a raw numeric event kind.
//
// Tools are partitioned into three tiers:
//
//	READ       always available.
//	MUTATE     changes a simulation's state; requires the caller to have
//	           explicitly granted mutation authority AND to hold at least the
//	           operator role. The grant is per-request, not per-session.
//	SANDBOXED  creates and runs a FORK. This is the interesting tier: the agent
//	           can freely experiment on a copy, because a fork cannot affect its
//	           parent. Counterfactual reasoning is exactly the capability an
//	           operations agent needs, and it is also the capability that is
//	           safe to hand over.
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mirror-sim/mirror/internal/engine"
	"github.com/mirror-sim/mirror/internal/events"
	"github.com/mirror-sim/mirror/internal/simctl"
	"github.com/mirror-sim/mirror/internal/state"
	"github.com/mirror-sim/mirror/internal/units"
)

type Tier uint8

const (
	TierRead Tier = iota
	TierMutate
	TierSandboxed
)

var tierName = [...]string{"read", "mutate", "sandboxed"}

func (t Tier) String() string { return tierName[t] }

// ToolSpec is the schema handed to the model and to the /agent/tools endpoint.
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
	Tier        string         `json:"tier"`
}

type toolImpl struct {
	spec ToolSpec
	tier Tier
	run  func(a *Agent, req Request, in map[string]any) (any, error)
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func str(desc string) map[string]any   { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any   { return map[string]any{"type": "integer", "description": desc} }
func boolp(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }

func (a *Agent) registry() []toolImpl {
	return []toolImpl{
		{
			spec: ToolSpec{
				Name:        "list_scenarios",
				Description: "List every simulation and scenario, with its id, name, parent, current simulated time and run state. Call this first if the user refers to a scenario by name.",
				InputSchema: obj(map[string]any{}),
			},
			tier: TierRead,
			run:  toolListScenarios,
		},
		{
			spec: ToolSpec{
				Name:        "get_traffic",
				Description: "Current road network conditions: mean speed, share of congested links, active vehicles, and the worst corridors. Optionally scoped to one district by name.",
				InputSchema: obj(map[string]any{
					"sim":      str("Simulation id. Defaults to the one in view."),
					"district": str("Optional district name to scope the answer to."),
				}),
			},
			tier: TierRead,
			run:  toolGetTraffic,
		},
		{
			spec: ToolSpec{
				Name:        "get_population",
				Description: "Population totals and where people currently are: at home, at work, travelling, stranded. Includes the modal split.",
				InputSchema: obj(map[string]any{"sim": str("Simulation id.")}),
			},
			tier: TierRead,
			run:  toolGetPopulation,
		},
		{
			spec: ToolSpec{
				Name:        "get_hospital_capacity",
				Description: "Per-hospital bed occupancy, ER load, ambulance availability, backup power state, admissions, rejections and diversions.",
				InputSchema: obj(map[string]any{"sim": str("Simulation id.")}),
			},
			tier: TierRead,
			run:  toolGetHospitals,
		},
		{
			spec: ToolSpec{
				Name:        "get_power_status",
				Description: "Electrical distribution status: which substations are online, their loading against capacity, how many signals are dark, and which districts are affected.",
				InputSchema: obj(map[string]any{"sim": str("Simulation id.")}),
			},
			tier: TierRead,
			run:  toolGetPower,
		},
		{
			spec: ToolSpec{
				Name:        "get_transit_load",
				Description: "Public transport: vehicles in service per route, occupancy, boardings and how many passengers have been left behind at stops.",
				InputSchema: obj(map[string]any{"sim": str("Simulation id.")}),
			},
			tier: TierRead,
			run:  toolGetTransit,
		},
		{
			spec: ToolSpec{
				Name:        "query_events",
				Description: "Recent events from the simulation's effect stream. Filter by substring of the event kind (for example 'hospital', 'power', 'incident') and by minimum severity.",
				InputSchema: obj(map[string]any{
					"sim":         str("Simulation id."),
					"kind":        str("Substring to match against the event kind."),
					"minSeverity": str("One of info, notice, warning, critical."),
					"limit":       num("Maximum events to return, default 40."),
				}),
			},
			tier: TierRead,
			run:  toolQueryEvents,
		},
		{
			spec: ToolSpec{
				Name:        "inspect_incident",
				Description: "Full detail on one incident: where it is, how severe, how many casualties, whether units have arrived, and how long the response took.",
				InputSchema: obj(map[string]any{
					"sim": str("Simulation id."),
					"id":  num("Incident id. Omit to list all open incidents."),
				}),
			},
			tier: TierRead,
			run:  toolInspectIncident,
		},
		{
			spec: ToolSpec{
				Name:        "get_metrics",
				Description: "The full analytical metric set for a scenario: travel time percentiles, delay, emergency response times, fuel, emissions, hospital load and transit performance.",
				InputSchema: obj(map[string]any{"sim": str("Simulation id.")}),
			},
			tier: TierRead,
			run:  toolGetMetrics,
		},
		{
			spec: ToolSpec{
				Name: "simulate_scenario",
				Description: "Fork the simulation into an isolated scenario, apply a policy, run it forward, and return its metrics. " +
					"This is the primary tool for answering 'what if' questions. The fork cannot affect the parent simulation, " +
					"so it is safe to run several and compare. Always run the scenarios you intend to compare for the same number of minutes.",
				InputSchema: obj(map[string]any{
					"sim":                 str("Simulation id to fork from."),
					"name":                str("A short descriptive name for the scenario."),
					"minutes":             num("Simulated minutes to run, 1 to 240. Default 30."),
					"adaptiveSignals":     boolp("Enable queue-actuated traffic signal control."),
					"emergencyPreemption": boolp("Give emergency vehicles signal preemption."),
					"transitVehiclesPct":  num("Transit fleet size as a percentage of baseline, 50 to 400."),
					"rerouteAwarenessPct": num("Percentage of drivers with live traffic information, 0 to 100."),
					"speedLimitPct":       num("Speed limits as a percentage of baseline, 50 to 130."),
					"congestionCharge":    boolp("Apply a routing penalty for entering the central district."),
				}, "sim", "name"),
			},
			tier: TierSandboxed,
			run:  toolSimulateScenario,
		},
		{
			spec: ToolSpec{
				Name:        "compare_scenarios",
				Description: "Compare two or more scenarios side by side across every reported metric. The first id is treated as the baseline and the others are reported as percentage changes against it.",
				InputSchema: obj(map[string]any{
					"ids": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Scenario ids, baseline first.",
					},
				}, "ids"),
			},
			tier: TierRead,
			run:  toolCompare,
		},
		{
			spec: ToolSpec{
				Name: "inject_event",
				Description: "Inject a real event into a LIVE simulation. This changes the simulation the operator is watching and cannot be undone except by restoring a checkpoint. " +
					"Prefer simulate_scenario for exploratory questions.",
				InputSchema: obj(map[string]any{
					"sim":  str("Simulation id."),
					"kind": str("One of: accident, close_road, reopen_road, power_failure, power_restore, weather, hospital_surge, transit_failure, flood, comms_outage, spawn_traffic."),
					"a":    num("First parameter, meaning depends on kind (usually the target entity id)."),
					"b":    num("Second parameter (usually severity, duration in ticks, or a count)."),
					"c":    num("Third parameter."),
					"d":    num("Fourth parameter."),
				}, "sim", "kind"),
			},
			tier: TierMutate,
			run:  toolInjectEvent,
		},
		{
			spec: ToolSpec{
				Name:        "set_policy",
				Description: "Change policy on a LIVE simulation. Prefer simulate_scenario unless the operator has explicitly asked to apply a change for real.",
				InputSchema: obj(map[string]any{
					"sim":                 str("Simulation id."),
					"adaptiveSignals":     boolp("Enable queue-actuated signal control."),
					"emergencyPreemption": boolp("Emergency vehicle signal preemption."),
					"transitVehiclesPct":  num("Transit fleet size, percent of baseline."),
					"rerouteAwarenessPct": num("Share of drivers with live traffic information, percent."),
					"speedLimitPct":       num("Speed limits, percent of baseline."),
					"congestionCharge":    boolp("Central district routing penalty."),
				}, "sim"),
			},
			tier: TierMutate,
			run:  toolSetPolicy,
		},
	}
}

// ToolSpecs returns the full catalogue, for the /agent/tools endpoint.
func (a *Agent) ToolSpecs() []ToolSpec {
	reg := a.registry()
	out := make([]ToolSpec, 0, len(reg))
	for _, t := range reg {
		s := t.spec
		s.Tier = t.tier.String()
		out = append(out, s)
	}
	return out
}

// available filters the catalogue by what this request is authorised to use.
func (a *Agent) available(req Request) []toolImpl {
	reg := a.registry()
	out := make([]toolImpl, 0, len(reg))
	for _, t := range reg {
		if t.tier == TierMutate && !req.AllowMutations {
			continue
		}
		out = append(out, t)
	}
	return out
}

// ------------------------------------------------------- implementations ---

func (a *Agent) simFor(req Request, in map[string]any) (*simctl.Sim, error) {
	id, _ := in["sim"].(string)
	if id == "" {
		id = req.SimID
	}
	if id == "" {
		return nil, errors.New("no simulation specified and none in view; call list_scenarios first")
	}
	s, ok := a.mgr.Get(id)
	if !ok {
		return nil, fmt.Errorf("no simulation with id %q; call list_scenarios to see what exists", id)
	}
	return s, nil
}

func toolListScenarios(a *Agent, req Request, in map[string]any) (any, error) {
	sims := a.mgr.List()
	type row struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Parent   string `json:"parent,omitempty"`
		State    string `json:"state"`
		Clock    string `json:"simulatedClock"`
		Minutes  int64  `json:"simulatedMinutes"`
		Vehicles int32  `json:"activeVehicles"`
		Policy   string `json:"policySummary"`
	}
	out := make([]row, 0, len(sims))
	for _, s := range sims {
		s.Read(func(e *engine.Engine) {
			snap := simctl.BuildSnapshot(s, e)
			out = append(out, row{
				ID: s.ID, Name: s.Name, Parent: s.ParentID, State: snap.State,
				Clock:    fmt.Sprintf("%02d:%02d", snap.ClockHour, snap.ClockMin),
				Minutes:  int64(snap.Tick) / units.TicksPerMinute,
				Vehicles: snap.Live.ActiveVehicles,
				Policy:   policySummary(snap.Policy),
			})
		})
	}
	return map[string]any{"scenarios": out}, nil
}

func policySummary(p simctl.PolicyView) string {
	var parts []string
	if p.AdaptiveSignals {
		parts = append(parts, "adaptive signals")
	}
	if p.EmergencyPreemption {
		parts = append(parts, "emergency preemption")
	}
	if p.TransitVehiclesPct != 100 {
		parts = append(parts, fmt.Sprintf("transit %d%%", p.TransitVehiclesPct))
	}
	if p.SpeedLimitPct != 100 {
		parts = append(parts, fmt.Sprintf("speed limit %d%%", p.SpeedLimitPct))
	}
	if p.CongestionCharge {
		parts = append(parts, "congestion charge")
	}
	parts = append(parts, fmt.Sprintf("%d%% informed drivers", p.RerouteAwarenessPct))
	return strings.Join(parts, ", ")
}

func toolGetTraffic(a *Agent, req Request, in map[string]any) (any, error) {
	s, err := a.simFor(req, in)
	if err != nil {
		return nil, err
	}
	want, _ := in["district"].(string)
	var out map[string]any
	s.Read(func(e *engine.Engine) {
		snap := simctl.BuildSnapshot(s, e)
		ds := simctl.DistrictStats(e)
		if want != "" {
			for _, d := range ds {
				if strings.EqualFold(d.Name, want) {
					out = map[string]any{
						"simulatedClock": fmt.Sprintf("%02d:%02d", snap.ClockHour, snap.ClockMin),
						"district":       d,
					}
					return
				}
			}
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i].CongestedPct > ds[j].CongestedPct })
		out = map[string]any{
			"simulatedClock":    fmt.Sprintf("%02d:%02d", snap.ClockHour, snap.ClockMin),
			"activeVehicles":    snap.Live.ActiveVehicles,
			"meanSpeedKph":      snap.Live.AvgSpeedKph,
			"congestedLinksPct": snap.Live.CongestedPct,
			"openIncidents":     snap.Live.OpenIncidents,
			"weather":           snap.Live.Weather,
			"worstDistricts":    ds[:minInt(4, len(ds))],
			"worstRoads":        worstRoads(e, 6),
		}
	})
	if out == nil {
		return nil, fmt.Errorf("no district named %q", want)
	}
	return out, nil
}

type roadRow struct {
	Edge      int32  `json:"edge"`
	District  string `json:"district"`
	Class     string `json:"class"`
	SpeedKph  int64  `json:"speedKph"`
	FreeKph   int64  `json:"freeFlowKph"`
	Occupancy int32  `json:"vehicles"`
	Jam       int32  `json:"jamCapacity"`
	Closed    bool   `json:"closed"`
}

func worstRoads(e *engine.Engine, n int) []roadRow {
	m, st := e.Map, e.S
	type scored struct {
		i     int
		score int64
	}
	var all []scored
	for i := range m.Edges {
		if st.Edges.Count[i] < 3 {
			continue
		}
		fs := m.Edges[i].FreeSpeed
		if fs <= 0 {
			continue
		}
		// Rank by delay-vehicles: how much slower than free flow, weighted by
		// how many people are experiencing it. Ranking on speed alone surfaces
		// empty back streets that happen to have one stopped car.
		ratio := 1000 - int64(st.Edges.Speed[i])*1000/int64(fs)
		all = append(all, scored{i, ratio * int64(st.Edges.Count[i])})
	}
	sort.Slice(all, func(x, y int) bool {
		if all[x].score != all[y].score {
			return all[x].score > all[y].score
		}
		return all[x].i < all[y].i
	})
	out := make([]roadRow, 0, n)
	for _, s := range all[:minInt(n, len(all))] {
		ed := &m.Edges[s.i]
		out = append(out, roadRow{
			Edge: int32(s.i), District: m.Districts[ed.District].Name, Class: ed.Class.String(),
			SpeedKph:  units.MMPerTickToKmh(st.Edges.Speed[s.i]),
			FreeKph:   units.MMPerTickToKmh(ed.FreeSpeed),
			Occupancy: st.Edges.Count[s.i], Jam: ed.Jam,
			Closed: st.Edges.ClosedUntil[s.i] > uint32(st.Tick),
		})
	}
	return out
}

func toolGetPopulation(a *Agent, req Request, in map[string]any) (any, error) {
	s, err := a.simFor(req, in)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	s.Read(func(e *engine.Engine) {
		st := e.S
		var car, transit, walk int
		for i := range st.Agents.Mode {
			switch st.Agents.Mode[i] {
			case state.ModeCar:
				car++
			case state.ModeTransit:
				transit++
			default:
				walk++
			}
		}
		snap := simctl.BuildSnapshot(s, e)
		out = map[string]any{
			"population":        st.Agents.Len(),
			"simulatedClock":    fmt.Sprintf("%02d:%02d", snap.ClockHour, snap.ClockMin),
			"atHome":            snap.Live.AgentsAtHome,
			"atWork":            snap.Live.AgentsAtWork,
			"travelling":        snap.Live.AgentsTravelling,
			"stranded":          snap.Live.AgentsStranded,
			"modalSplitCar":     car,
			"modalSplitTransit": transit,
			"modalSplitWalk":    walk,
			"tripsStarted":      st.Metrics.TripsStarted,
			"tripsCompleted":    st.Metrics.TripsCompleted,
		}
	})
	return out, nil
}

func toolGetHospitals(a *Agent, req Request, in map[string]any) (any, error) {
	s, err := a.simFor(req, in)
	if err != nil {
		return nil, err
	}
	type row struct {
		ID         int    `json:"id"`
		Name       string `json:"name"`
		District   string `json:"district"`
		Beds       int32  `json:"beds"`
		BedsUsed   int32  `json:"bedsUsed"`
		UtilPct    int64  `json:"utilisationPct"`
		Ambulances int32  `json:"ambulancesAvailable"`
		OnBackup   bool   `json:"onBackupPower"`
		Admissions int64  `json:"admissions"`
		Rejections int64  `json:"rejections"`
		Diversions int64  `json:"diversions"`
	}
	var out map[string]any
	s.Read(func(e *engine.Engine) {
		m, st := e.Map, e.S
		rows := make([]row, 0, len(m.Hospitals))
		for i := range m.Hospitals {
			h := &m.Hospitals[i]
			u := int64(0)
			if h.Beds > 0 {
				u = int64(st.Hosps.BedsUsed[i]) * 100 / int64(h.Beds)
			}
			rows = append(rows, row{
				ID: i, Name: h.Name, District: m.Districts[h.District].Name,
				Beds: h.Beds, BedsUsed: st.Hosps.BedsUsed[i], UtilPct: u,
				Ambulances: st.Hosps.AmbAvail[i], OnBackup: st.Hosps.OnBackup[i],
				Admissions: st.Hosps.Admissions[i], Rejections: st.Hosps.Rejections[i],
				Diversions: st.Hosps.Diverted[i],
			})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].UtilPct > rows[j].UtilPct })
		out = map[string]any{
			"hospitals":       rows,
			"totalAdmissions": st.Metrics.HospitalAdmissions,
			"totalRejections": st.Metrics.HospitalRejections,
			"totalDiversions": st.Metrics.HospitalDiversions,
			"peakUtilPct":     st.Metrics.PeakHospitalUtilP / 10,
		}
	})
	return out, nil
}

func toolGetPower(a *Agent, req Request, in map[string]any) (any, error) {
	s, err := a.simFor(req, in)
	if err != nil {
		return nil, err
	}
	type row struct {
		ID        int    `json:"id"`
		Name      string `json:"name"`
		District  string `json:"district"`
		Online    bool   `json:"online"`
		LoadKW    int32  `json:"loadKW"`
		CapKW     int32  `json:"capacityKW"`
		UtilPct   int64  `json:"utilisationPct"`
		Trips     int32  `json:"trips"`
		RestoreIn int64  `json:"restoreInSeconds"`
	}
	var out map[string]any
	s.Read(func(e *engine.Engine) {
		m, st := e.Map, e.S
		rows := make([]row, 0, len(m.Substations))
		for i := range m.Substations {
			ss := &m.Substations[i]
			u := int64(0)
			if ss.CapacityKW > 0 {
				u = int64(st.Subs.LoadKW[i]) * 100 / int64(ss.CapacityKW)
			}
			restore := int64(0)
			if !st.Subs.Online[i] && st.Subs.RestoreAt[i] > uint32(st.Tick) {
				restore = int64(st.Subs.RestoreAt[i]-uint32(st.Tick)) / units.TicksPerSecond
			}
			rows = append(rows, row{
				ID: i, Name: ss.Name, District: m.Districts[ss.District].Name,
				Online: st.Subs.Online[i], LoadKW: st.Subs.LoadKW[i],
				CapKW: ss.CapacityKW, UtilPct: u, Trips: st.Subs.Trips[i],
				RestoreIn: restore,
			})
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Online != rows[j].Online {
				return !rows[i].Online
			}
			return rows[i].UtilPct > rows[j].UtilPct
		})
		dark := 0
		for i := range st.Signals.Powered {
			if !st.Signals.Powered[i] {
				dark++
			}
		}
		out = map[string]any{
			"substations":          rows,
			"signalsDark":          dark,
			"signalsTotal":         len(st.Signals.Powered),
			"substationTrips":      st.Metrics.SubstationTrips,
			"commsSessionsDropped": st.Metrics.CommsDropped,
		}
	})
	return out, nil
}

func toolGetTransit(a *Agent, req Request, in map[string]any) (any, error) {
	s, err := a.simFor(req, in)
	if err != nil {
		return nil, err
	}
	type row struct {
		ID         int    `json:"id"`
		Name       string `json:"name"`
		Mode       string `json:"mode"`
		InService  int    `json:"vehiclesInService"`
		Riders     int32  `json:"ridersOnBoard"`
		Capacity   int32  `json:"capacityPerVehicle"`
		HeadwaySec int32  `json:"headwaySeconds"`
	}
	var out map[string]any
	s.Read(func(e *engine.Engine) {
		m, st := e.Map, e.S
		rows := make([]row, len(m.Routes))
		for i := range m.Routes {
			mode := "bus"
			if m.Routes[i].Mode == 1 {
				mode = "metro"
			}
			rows[i] = row{
				ID: i, Name: m.Routes[i].Name, Mode: mode,
				Capacity:   m.Routes[i].Capacity,
				HeadwaySec: m.Routes[i].HeadwayTick / units.TicksPerSecond,
			}
		}
		for v := range st.Vehicles.Status {
			if st.Vehicles.Status[v] == state.VehIdle {
				continue
			}
			if rt := st.Vehicles.TransitRoute[v]; rt >= 0 && int(rt) < len(rows) {
				rows[rt].InService++
				rows[rt].Riders += st.Vehicles.Occupancy[v]
			}
		}
		waiting := 0
		for _, h := range st.StopHead {
			if h >= 0 {
				waiting++
			}
		}
		out = map[string]any{
			"routes":           rows,
			"stopsWithQueue":   waiting,
			"boardings":        st.Metrics.TransitBoardings,
			"passengersDenied": st.Metrics.TransitDenied,
			"transitFleetPct":  st.Policy.TransitExtraVehiclesP / 10,
		}
	})
	return out, nil
}

func toolQueryEvents(a *Agent, req Request, in map[string]any) (any, error) {
	s, err := a.simFor(req, in)
	if err != nil {
		return nil, err
	}
	kindFilter, _ := in["kind"].(string)
	minSev := events.SevInfo
	if v, ok := in["minSeverity"].(string); ok {
		for i, n := range []string{"info", "notice", "warning", "critical"} {
			if n == v {
				minSev = events.Severity(i)
			}
		}
	}
	limit := 40
	if v, ok := numArg(in, "limit"); ok && v > 0 && v <= 200 {
		limit = int(v)
	}
	type row struct {
		Tick     uint64 `json:"tick"`
		Clock    string `json:"simulatedClock"`
		Kind     string `json:"kind"`
		Severity string `json:"severity"`
		Text     string `json:"text"`
	}
	var out []row
	s.Read(func(e *engine.Engine) {
		var buf []events.Event
		head := e.Ring.Head()
		from := uint64(0)
		if head > 4000 {
			from = head - 4000
		}
		buf, _, _ = e.Ring.ReadFrom(from, buf, 4000)
		for i := len(buf) - 1; i >= 0 && len(out) < limit; i-- {
			ev := buf[i]
			if ev.Severity < minSev {
				continue
			}
			if kindFilter != "" && !strings.Contains(ev.Kind.String(), kindFilter) {
				continue
			}
			h, mn := ev.Tick.ClockHM(int(e.S.StartHour))
			out = append(out, row{
				Tick: uint64(ev.Tick), Clock: fmt.Sprintf("%02d:%02d", h, mn),
				Kind: ev.Kind.String(), Severity: ev.Severity.String(),
				Text: events.Describe(e.Map, ev),
			})
		}
	})
	return map[string]any{"events": out, "count": len(out)}, nil
}

func toolInspectIncident(a *Agent, req Request, in map[string]any) (any, error) {
	s, err := a.simFor(req, in)
	if err != nil {
		return nil, err
	}
	wantID, hasID := numArg(in, "id")
	type row struct {
		ID           int64  `json:"id"`
		Kind         string `json:"kind"`
		District     string `json:"district"`
		Road         string `json:"road,omitempty"`
		StartedClock string `json:"startedAt"`
		Severity     int32  `json:"severity"`
		Casualties   int32  `json:"casualties"`
		Awaiting     int32  `json:"unitsStillNeeded"`
		ResponseSec  int32  `json:"firstResponseSeconds"`
		Resolved     bool   `json:"resolved"`
		OpenForSec   int64  `json:"openForSeconds"`
	}
	var out []row
	s.Read(func(e *engine.Engine) {
		m, st := e.Map, e.S
		for i := range st.Incidents {
			inc := &st.Incidents[i]
			if hasID && inc.ID != wantID {
				continue
			}
			if !hasID && inc.Resolved {
				continue
			}
			h, mn := units.Tick(inc.StartTick).ClockHM(int(st.StartHour))
			r := row{
				ID: inc.ID, Kind: state.IncidentKindName(inc.Kind),
				StartedClock: fmt.Sprintf("%02d:%02d", h, mn),
				Severity:     inc.Severity, Casualties: inc.Casualties,
				Awaiting:   inc.NeedAmbulance + inc.NeedFire + inc.NeedPolice,
				Resolved:   inc.Resolved,
				OpenForSec: (int64(st.Tick) - int64(inc.StartTick)) / units.TicksPerSecond,
			}
			if int(inc.District) < len(m.Districts) {
				r.District = m.Districts[inc.District].Name
			}
			if inc.Edge >= 0 && int(inc.Edge) < len(m.Edges) {
				r.Road = fmt.Sprintf("%s #%d", m.Edges[inc.Edge].Class, inc.Edge)
			}
			if inc.FirstResponseTick > 0 {
				r.ResponseSec = int32(inc.FirstResponseTick-inc.StartTick) / units.TicksPerSecond
			}
			out = append(out, r)
		}
	})
	if hasID && len(out) == 0 {
		return nil, fmt.Errorf("no incident with id %d", wantID)
	}
	return map[string]any{"incidents": out, "count": len(out)}, nil
}

func toolGetMetrics(a *Agent, req Request, in map[string]any) (any, error) {
	s, err := a.simFor(req, in)
	if err != nil {
		return nil, err
	}
	var out any
	s.Read(func(e *engine.Engine) {
		snap := simctl.BuildSnapshot(s, e)
		out = map[string]any{
			"scenario":         s.Name,
			"simulatedMinutes": int64(e.S.Tick) / units.TicksPerMinute,
			"policy":           snap.Policy,
			"metrics":          simctl.BuildMetrics(e),
		}
	})
	return out, nil
}

func toolSimulateScenario(a *Agent, req Request, in map[string]any) (any, error) {
	s, err := a.simFor(req, in)
	if err != nil {
		return nil, err
	}
	name, _ := in["name"].(string)
	if name == "" {
		name = "scenario"
	}
	minutes := int64(30)
	if v, ok := numArg(in, "minutes"); ok {
		minutes = v
	}
	if minutes < 1 {
		minutes = 1
	}
	if minutes > 240 {
		minutes = 240
	}

	child, err := a.mgr.Fork(s.ID, name)
	if err != nil {
		return nil, err
	}

	// Policy is applied through the same command path an operator would use,
	// so the scenario's command log is a complete, replayable description of
	// what the agent did.
	applied := map[string]any{}
	setBool := func(key string, field int64) {
		if v, ok := in[key].(bool); ok {
			_, _ = a.mgr.Inject(child.ID, events.C(0, events.CmdSetPolicy, field, b2i(v), 0, 0))
			applied[key] = v
		}
	}
	setPct := func(key string, field int64, lo, hi int64) {
		if v, ok := numArg(in, key); ok {
			if v < lo {
				v = lo
			}
			if v > hi {
				v = hi
			}
			_, _ = a.mgr.Inject(child.ID, events.C(0, events.CmdSetPolicy, field, v*10, 0, 0))
			applied[key] = v
		}
	}
	setBool("adaptiveSignals", 0)
	setBool("emergencyPreemption", 2)
	setPct("transitVehiclesPct", 3, 50, 400)
	setPct("rerouteAwarenessPct", 4, 0, 100)
	setPct("speedLimitPct", 5, 50, 130)
	setBool("congestionCharge", 6)

	var startTick units.Tick
	child.Read(func(e *engine.Engine) { startTick = e.S.Tick })
	target := startTick + units.Tick(minutes*units.TicksPerMinute)

	// Run the fork headless and as fast as the CPU allows. Holding the write
	// lock for the whole run is correct here: nothing else may observe a
	// half-finished scenario, and the fork is not the simulation the operator
	// is watching.
	deadline := time.Now().Add(90 * time.Second)
	truncated := false
	child.Write(func(e *engine.Engine) {
		for e.S.Tick < target {
			e.Tick()
			if uint64(e.S.Tick)%512 == 0 && time.Now().After(deadline) {
				truncated = true
				return
			}
		}
	})

	var out map[string]any
	child.Read(func(e *engine.Engine) {
		out = map[string]any{
			"scenarioId":       child.ID,
			"name":             child.Name,
			"forkedFrom":       s.ID,
			"branchedAtMinute": int64(startTick) / units.TicksPerMinute,
			"ranForMinutes":    int64(e.S.Tick-startTick) / units.TicksPerMinute,
			"policyApplied":    applied,
			"metrics":          simctl.BuildMetrics(e),
		}
		if truncated {
			out["warning"] = "The run was cut short by the 90 second wall-clock budget. Compare only against scenarios that ran for the same number of simulated minutes."
		}
	})
	return out, nil
}

func toolCompare(a *Agent, req Request, in map[string]any) (any, error) {
	raw, ok := in["ids"].([]any)
	if !ok || len(raw) < 2 {
		return nil, errors.New("provide at least two scenario ids, baseline first")
	}
	ids := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			ids = append(ids, s)
		}
	}
	cmp, err := a.mgr.Compare(ids)
	if err != nil {
		return nil, err
	}
	return cmp, nil
}

func toolInjectEvent(a *Agent, req Request, in map[string]any) (any, error) {
	s, err := a.simFor(req, in)
	if err != nil {
		return nil, err
	}
	kindName, _ := in["kind"].(string)
	kind, ok := agentInjectable[kindName]
	if !ok {
		return nil, fmt.Errorf("unknown event kind %q", kindName)
	}
	av, _ := numArg(in, "a")
	bv, _ := numArg(in, "b")
	cv, _ := numArg(in, "c")
	dv, _ := numArg(in, "d")
	ev, err := a.mgr.Inject(s.ID, events.C(0, kind, av, bv, cv, dv))
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"accepted": true, "kind": ev.Kind.String(), "tick": uint64(ev.Tick), "seq": ev.Seq,
		"note": "This changed the live simulation. It is recorded in the command log and is fully replayable.",
	}, nil
}

// agentInjectable deliberately excludes earthquake and every chaos command.
// The agent may cause a crash or a substation trip -- things an operator does
// routinely -- but city-scale destruction and fault injection into the
// platform itself stay behind a human.
var agentInjectable = map[string]events.Kind{
	"accident":        events.CmdInjectAccident,
	"close_road":      events.CmdCloseRoad,
	"reopen_road":     events.CmdReopenRoad,
	"power_failure":   events.CmdPowerFailure,
	"power_restore":   events.CmdPowerRestore,
	"weather":         events.CmdSetWeather,
	"hospital_surge":  events.CmdHospitalSurge,
	"transit_failure": events.CmdTransitFailure,
	"flood":           events.CmdFloodDistrict,
	"comms_outage":    events.CmdCommsOutage,
	"spawn_traffic":   events.CmdSpawnTraffic,
}

func toolSetPolicy(a *Agent, req Request, in map[string]any) (any, error) {
	s, err := a.simFor(req, in)
	if err != nil {
		return nil, err
	}
	applied := map[string]any{}
	setBool := func(key string, field int64) {
		if v, ok := in[key].(bool); ok {
			_, _ = a.mgr.Inject(s.ID, events.C(0, events.CmdSetPolicy, field, b2i(v), 0, 0))
			applied[key] = v
		}
	}
	setPct := func(key string, field int64, lo, hi int64) {
		if v, ok := numArg(in, key); ok {
			if v < lo {
				v = lo
			}
			if v > hi {
				v = hi
			}
			_, _ = a.mgr.Inject(s.ID, events.C(0, events.CmdSetPolicy, field, v*10, 0, 0))
			applied[key] = v
		}
	}
	setBool("adaptiveSignals", 0)
	setBool("emergencyPreemption", 2)
	setPct("transitVehiclesPct", 3, 50, 400)
	setPct("rerouteAwarenessPct", 4, 0, 100)
	setPct("speedLimitPct", 5, 50, 130)
	setBool("congestionCharge", 6)
	if len(applied) == 0 {
		return nil, errors.New("no policy fields were supplied")
	}
	return map[string]any{"applied": applied, "sim": s.ID}, nil
}

// ------------------------------------------------------------- helpers -----

func numArg(in map[string]any, key string) (int64, bool) {
	switch v := in[key].(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	}
	return 0, false
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
