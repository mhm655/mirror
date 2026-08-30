package simctl

import (
	"sort"

	"github.com/mirror-sim/mirror/internal/engine"
	"github.com/mirror-sim/mirror/internal/state"
	"github.com/mirror-sim/mirror/internal/units"
	"github.com/mirror-sim/mirror/internal/world"
)

// Snapshot is the JSON-facing summary of a simulation at an instant.
//
// This is where integers become floats and ticks become seconds. Keeping that
// conversion at the boundary -- and only here -- is what lets the engine stay
// integer-only without every consumer having to know about tick arithmetic.
type Snapshot struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ParentID    string `json:"parentId,omitempty"`
	BranchTick  uint64 `json:"branchTick,omitempty"`
	State       string `json:"state"`
	Speed       int32  `json:"speed"`
	Tick        uint64 `json:"tick"`
	ClockHour   int    `json:"clockHour"`
	ClockMin    int    `json:"clockMin"`
	SimSeconds  uint64 `json:"simSeconds"`
	TPS         int64  `json:"ticksPerSecond"`
	Population  int    `json:"population"`
	MapPreset   string `json:"mapPreset"`
	MapHash     string `json:"mapHash"`
	Seed        uint64 `json:"seed"`
	Digest      string `json:"digest"`
	Regions     int    `json:"regions"`
	Workers     int    `json:"workers"`
	Checkpoints uint64 `json:"checkpoints"`
	LastError   string `json:"lastError,omitempty"`

	Policy PolicyView `json:"policy"`
	Live   LiveView   `json:"live"`
	Perf   PerfView   `json:"perf"`
}

type PolicyView struct {
	Name                 string `json:"name"`
	AdaptiveSignals      bool   `json:"adaptiveSignals"`
	AdaptiveMaxExtendSec int32  `json:"adaptiveMaxExtendSec"`
	EmergencyPreemption  bool   `json:"emergencyPreemption"`
	TransitVehiclesPct   int32  `json:"transitVehiclesPct"`
	RerouteAwarenessPct  int32  `json:"rerouteAwarenessPct"`
	SpeedLimitPct        int32  `json:"speedLimitPct"`
	CongestionCharge     bool   `json:"congestionCharge"`
	AmbulanceSurgePct    int32  `json:"ambulanceSurgePct"`
}

type LiveView struct {
	ActiveVehicles   int32  `json:"activeVehicles"`
	Cars             int32  `json:"cars"`
	Buses            int32  `json:"buses"`
	Metros           int32  `json:"metros"`
	Emergency        int32  `json:"emergency"`
	AgentsTravelling int32  `json:"agentsTravelling"`
	AgentsAtHome     int32  `json:"agentsAtHome"`
	AgentsAtWork     int32  `json:"agentsAtWork"`
	AgentsStranded   int32  `json:"agentsStranded"`
	OpenIncidents    int32  `json:"openIncidents"`
	SubsOnline       int32  `json:"substationsOnline"`
	SubsTotal        int32  `json:"substationsTotal"`
	SignalsDark      int32  `json:"signalsDark"`
	HospitalUtilPct  int32  `json:"hospitalUtilPct"`
	AvgSpeedKph      int32  `json:"avgSpeedKph"`
	CongestedPct     int32  `json:"congestedPct"`
	Weather          string `json:"weather"`
	TempC            int32  `json:"tempC"`
}

type PerfView struct {
	TickMillis    float64 `json:"tickMillis"`
	PhaseAMillis  float64 `json:"phaseAMillis"`
	PhaseBMillis  float64 `json:"phaseBMillis"`
	SerialPercent int32   `json:"serialPercent"`
	Intents       int     `json:"intents"`
	Crossings     int64   `json:"crossings"`
	RouteQueries  int64   `json:"routeQueries"`
	EventsDropped uint64  `json:"eventsDropped"`
}

// Metrics is the analytical result set: the numbers a scenario comparison is
// actually about.
type Metrics struct {
	TripsStarted   int64 `json:"tripsStarted"`
	TripsCompleted int64 `json:"tripsCompleted"`
	TripsAbandoned int64 `json:"tripsAbandoned"`

	TravelMeanSec float64 `json:"travelMeanSec"`
	TravelP50Sec  float64 `json:"travelP50Sec"`
	TravelP95Sec  float64 `json:"travelP95Sec"`
	TravelP99Sec  float64 `json:"travelP99Sec"`
	DelayMeanSec  float64 `json:"delayMeanSec"`
	DelayP95Sec   float64 `json:"delayP95Sec"`

	EmergencyDispatched int64   `json:"emergencyDispatched"`
	EmergencyArrived    int64   `json:"emergencyArrived"`
	ResponseMeanSec     float64 `json:"responseMeanSec"`
	ResponseP50Sec      float64 `json:"responseP50Sec"`
	ResponseP95Sec      float64 `json:"responseP95Sec"`

	IncidentsOpened   int64 `json:"incidentsOpened"`
	IncidentsResolved int64 `json:"incidentsResolved"`
	Casualties        int64 `json:"casualties"`

	HospitalAdmissions  int64 `json:"hospitalAdmissions"`
	HospitalRejections  int64 `json:"hospitalRejections"`
	HospitalDiversions  int64 `json:"hospitalDiversions"`
	PeakHospitalUtilPct int32 `json:"peakHospitalUtilPct"`

	VehicleKm    float64 `json:"vehicleKm"`
	FuelLitres   float64 `json:"fuelLitres"`
	CO2Kg        float64 `json:"co2Kg"`
	StoppedHours float64 `json:"stoppedVehicleHours"`

	TransitBoardings int64 `json:"transitBoardings"`
	TransitDenied    int64 `json:"transitDenied"`

	OutageUnitTicks int64 `json:"outageUnitTicks"`
	SubstationTrips int64 `json:"substationTrips"`
	CommsDropped    int64 `json:"commsDropped"`

	Reroutes      int64 `json:"reroutes"`
	RouteFailures int64 `json:"routeFailures"`
}

// Series are the per-simulated-minute time series for the charts.
type Series struct {
	ActiveVehicles []int32 `json:"activeVehicles"`
	AvgSpeedKph    []int32 `json:"avgSpeedKph"`
	CongestionPct  []int32 `json:"congestionPct"`
	HospitalPct    []int32 `json:"hospitalPct"`
	PoweredPct     []int32 `json:"poweredPct"`
	OpenIncidents  []int32 `json:"openIncidents"`
}

func ticksToSec(t int64) float64 { return float64(t) / float64(units.TicksPerSecond) }

// BuildSnapshot summarises a running simulation. Caller must hold a read lock.
func BuildSnapshot(s *Sim, e *engine.Engine) Snapshot {
	st := e.S
	h, mn := st.ClockHM()
	var cars, buses, metros, emer int32
	for i := range st.Vehicles.Status {
		if st.Vehicles.Status[i] == state.VehIdle {
			continue
		}
		switch st.Vehicles.Kind[i] {
		case state.VehCar:
			cars++
		case state.VehBus:
			buses++
		case state.VehMetro:
			metros++
		default:
			emer++
		}
	}
	var travelling, atHome, atWork, stranded int32
	for i := range st.Agents.Status {
		switch st.Agents.Status[i] {
		case state.Commuting, state.Returning:
			travelling++
		case state.AtHome:
			atHome++
		case state.AtWork:
			atWork++
		case state.Stranded:
			stranded++
		}
	}
	var open int32
	for i := range st.Incidents {
		if !st.Incidents[i].Resolved {
			open++
		}
	}
	var online int32
	for i := range st.Subs.Online {
		if st.Subs.Online[i] {
			online++
		}
	}
	var dark int32
	for i := range st.Signals.Powered {
		if !st.Signals.Powered[i] {
			dark++
		}
	}
	var used, total int64
	for i := range e.Map.Hospitals {
		used += int64(st.Hosps.BedsUsed[i])
		total += int64(e.Map.Hospitals[i].Beds)
	}
	hospPct := int32(0)
	if total > 0 {
		hospPct = int32(used * 100 / total)
	}
	avgSpeed, congPct := liveNetwork(e)

	return Snapshot{
		ID: s.ID, Name: s.Name, ParentID: s.ParentID, BranchTick: uint64(s.BranchTick),
		State: s.State().String(), Speed: s.Speed(), Tick: uint64(st.Tick),
		ClockHour: h, ClockMin: mn, SimSeconds: st.Tick.Seconds(), TPS: s.TPS(),
		Population: st.Agents.Len(), MapPreset: s.Cfg.Preset,
		MapHash: hex16(e.Map.Hash), Seed: st.Seed, Digest: hex16(st.Digest()),
		Regions: s.Cfg.Regions, Workers: s.Cfg.Workers,
		Checkpoints: s.Checkpoints(), LastError: s.LastError(),
		Policy: PolicyView{
			Name: st.Policy.Name, AdaptiveSignals: st.Policy.AdaptiveSignals,
			AdaptiveMaxExtendSec: st.Policy.AdaptiveMaxExtendTicks / units.TicksPerSecond,
			EmergencyPreemption:  st.Policy.EmergencyPreemption,
			TransitVehiclesPct:   st.Policy.TransitExtraVehiclesP / 10,
			RerouteAwarenessPct:  st.Policy.RerouteAwarenessP / 10,
			SpeedLimitPct:        st.Policy.SpeedLimitP / 10,
			CongestionCharge:     st.Policy.CongestionCharge,
			AmbulanceSurgePct:    st.Policy.AmbulanceSurgeP / 10,
		},
		Live: LiveView{
			ActiveVehicles: cars + buses + metros + emer,
			Cars:           cars, Buses: buses, Metros: metros, Emergency: emer,
			AgentsTravelling: travelling, AgentsAtHome: atHome,
			AgentsAtWork: atWork, AgentsStranded: stranded,
			OpenIncidents: open, SubsOnline: online, SubsTotal: int32(len(st.Subs.Online)),
			SignalsDark: dark, HospitalUtilPct: hospPct,
			AvgSpeedKph: avgSpeed, CongestedPct: congPct,
			Weather: weatherName(st.Weather.Condition), TempC: st.Weather.TempC,
		},
		Perf: PerfView{
			TickMillis:    float64(e.Stat.TotalNanos) / 1e6,
			PhaseAMillis:  float64(e.Stat.PhaseANanos) / 1e6,
			PhaseBMillis:  float64(e.Stat.PhaseBNanos) / 1e6,
			SerialPercent: e.Stat.SerialPercent,
			Intents:       e.Stat.Intents,
			Crossings:     e.Stat.Crossings,
			RouteQueries:  st.Metrics.RouteQueries,
			EventsDropped: e.Ring.Dropped(),
		},
	}
}

func liveNetwork(e *engine.Engine) (avgKph, congPct int32) {
	st := e.S
	var sum, n, cong, counted int64
	for i := range st.Edges.Count {
		c := int64(st.Edges.Count[i])
		if c == 0 {
			continue
		}
		sum += units.MMPerTickToKmh(st.Edges.Speed[i]) * c
		n += c
		counted++
		if fs := e.Map.Edges[i].FreeSpeed; fs > 0 && int64(st.Edges.Speed[i])*100/int64(fs) < 45 {
			cong++
		}
	}
	if n > 0 {
		avgKph = int32(sum / n)
	}
	if counted > 0 {
		congPct = int32(cong * 100 / counted)
	}
	return
}

var weatherNames = [...]string{"clear", "rain", "storm", "snow", "heatwave", "fog"}

func weatherName(c int32) string {
	if c >= 0 && int(c) < len(weatherNames) {
		return weatherNames[c]
	}
	return "unknown"
}

func hex16(v uint64) string {
	const digits = "0123456789abcdef"
	var b [16]byte
	for i := 15; i >= 0; i-- {
		b[i] = digits[v&0xF]
		v >>= 4
	}
	return string(b[:])
}

// BuildMetrics converts the engine's integer counters into reportable numbers.
func BuildMetrics(e *engine.Engine) Metrics {
	m := &e.S.Metrics
	fuelMl, co2G := m.FuelMl, m.CO2G
	return Metrics{
		TripsStarted: m.TripsStarted, TripsCompleted: m.TripsCompleted,
		TripsAbandoned:      m.TripsAbandoned,
		TravelMeanSec:       ticksToSec(m.Travel.Mean()),
		TravelP50Sec:        ticksToSec(m.Travel.Quantile(500)),
		TravelP95Sec:        ticksToSec(m.Travel.Quantile(950)),
		TravelP99Sec:        ticksToSec(m.Travel.Quantile(990)),
		DelayMeanSec:        ticksToSec(m.Delay.Mean()),
		DelayP95Sec:         ticksToSec(m.Delay.Quantile(950)),
		EmergencyDispatched: m.EmergencyDispatched,
		EmergencyArrived:    m.EmergencyArrived,
		ResponseMeanSec:     ticksToSec(m.EmergencyResponse.Mean()),
		ResponseP50Sec:      ticksToSec(m.EmergencyResponse.Quantile(500)),
		ResponseP95Sec:      ticksToSec(m.EmergencyResponse.Quantile(950)),
		IncidentsOpened:     m.IncidentsOpened,
		IncidentsResolved:   m.IncidentsResolved,
		Casualties:          m.Casualties,
		HospitalAdmissions:  m.HospitalAdmissions,
		HospitalRejections:  m.HospitalRejections,
		HospitalDiversions:  m.HospitalDiversions,
		PeakHospitalUtilPct: m.PeakHospitalUtilP / 10,
		VehicleKm:           float64(m.DistanceMM) / 1e6,
		FuelLitres:          float64(fuelMl) / 1000,
		CO2Kg:               float64(co2G) / 1000,
		StoppedHours:        float64(m.StoppedVehicleTicks) / float64(units.TicksPerHour),
		TransitBoardings:    m.TransitBoardings,
		TransitDenied:       m.TransitDenied,
		OutageUnitTicks:     m.OutageNodeTicks,
		SubstationTrips:     m.SubstationTrips,
		CommsDropped:        m.CommsDropped,
		Reroutes:            m.Reroutes,
		RouteFailures:       m.RouteFailures,
	}
}

func BuildSeries(e *engine.Engine) Series {
	m := &e.S.Metrics
	var buf []int32
	out := Series{}
	out.ActiveVehicles = append([]int32(nil), m.SeriesActiveVehicles.Snapshot(buf)...)
	out.AvgSpeedKph = append([]int32(nil), m.SeriesAvgSpeedKph.Snapshot(buf)...)
	out.CongestionPct = append([]int32(nil), m.SeriesCongestionP.Snapshot(buf)...)
	out.HospitalPct = append([]int32(nil), m.SeriesHospitalUtilP.Snapshot(buf)...)
	out.PoweredPct = append([]int32(nil), m.SeriesPoweredP.Snapshot(buf)...)
	out.OpenIncidents = append([]int32(nil), m.SeriesOpenIncidents.Snapshot(buf)...)
	return out
}

// ---------------------------------------------------------- comparison ----

// ComparisonRow is one metric across every scenario in a comparison.
type ComparisonRow struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Unit  string `json:"unit"`
	// LowerIsBetter drives the direction of the improvement colouring. Getting
	// this wrong is the single easiest way to make a comparison view actively
	// misleading, so it is data rather than a UI heuristic.
	LowerIsBetter bool               `json:"lowerIsBetter"`
	Values        map[string]float64 `json:"values"`
	// DeltaPct is each scenario's change against the baseline (the first id).
	DeltaPct map[string]float64 `json:"deltaPct"`
}

// Comparison is the result of comparing several scenarios.
type Comparison struct {
	BaselineID string          `json:"baselineId"`
	Scenarios  []ScenarioBrief `json:"scenarios"`
	Rows       []ComparisonRow `json:"rows"`
	// Warnings surface conditions that make a comparison less meaningful --
	// different ticks, different maps, too few completed trips. Reporting them
	// alongside the numbers is the difference between an analysis tool and a
	// number generator.
	Warnings []string `json:"warnings"`
}

type ScenarioBrief struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Tick       uint64     `json:"tick"`
	ParentID   string     `json:"parentId,omitempty"`
	BranchTick uint64     `json:"branchTick,omitempty"`
	Policy     PolicyView `json:"policy"`
	Metrics    Metrics    `json:"metrics"`
}

// Compare builds a side-by-side view. The first id is the baseline.
func (mgr *Manager) Compare(ids []string) (*Comparison, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	cmp := &Comparison{BaselineID: ids[0]}
	type entry struct {
		brief ScenarioBrief
		snap  Snapshot
	}
	var entries []entry
	var mapHashes = map[string]bool{}
	var ticks = map[uint64]bool{}

	for _, id := range ids {
		s, ok := mgr.Get(id)
		if !ok {
			cmp.Warnings = append(cmp.Warnings, "unknown simulation "+id)
			continue
		}
		var en entry
		s.Read(func(e *engine.Engine) {
			en.snap = BuildSnapshot(s, e)
			en.brief = ScenarioBrief{
				ID: s.ID, Name: s.Name, Tick: uint64(e.S.Tick),
				ParentID: s.ParentID, BranchTick: uint64(s.BranchTick),
				Policy: en.snap.Policy, Metrics: BuildMetrics(e),
			}
			mapHashes[hex16(e.Map.Hash)] = true
			ticks[uint64(e.S.Tick)] = true
		})
		entries = append(entries, en)
		cmp.Scenarios = append(cmp.Scenarios, en.brief)
	}
	if len(entries) == 0 {
		return cmp, nil
	}
	if len(mapHashes) > 1 {
		cmp.Warnings = append(cmp.Warnings,
			"scenarios were run on different maps; the comparison is not valid")
	}
	if len(ticks) > 1 {
		cmp.Warnings = append(cmp.Warnings,
			"scenarios are at different simulated times; run them to the same tick before drawing conclusions")
	}
	for _, en := range entries {
		if en.brief.Metrics.TripsCompleted < 200 {
			cmp.Warnings = append(cmp.Warnings,
				en.brief.Name+" has fewer than 200 completed trips; percentiles are noisy")
		}
	}

	defs := []struct {
		key, label, unit string
		lower            bool
		get              func(Metrics) float64
	}{
		{"travelMean", "Mean travel time", "s", true, func(m Metrics) float64 { return m.TravelMeanSec }},
		{"travelP50", "Travel time P50", "s", true, func(m Metrics) float64 { return m.TravelP50Sec }},
		{"travelP95", "Travel time P95", "s", true, func(m Metrics) float64 { return m.TravelP95Sec }},
		{"travelP99", "Travel time P99", "s", true, func(m Metrics) float64 { return m.TravelP99Sec }},
		{"delayMean", "Mean delay vs free flow", "s", true, func(m Metrics) float64 { return m.DelayMeanSec }},
		{"responseMean", "Emergency response mean", "s", true, func(m Metrics) float64 { return m.ResponseMeanSec }},
		{"responseP95", "Emergency response P95", "s", true, func(m Metrics) float64 { return m.ResponseP95Sec }},
		{"tripsCompleted", "Trips completed", "", false, func(m Metrics) float64 { return float64(m.TripsCompleted) }},
		{"tripsAbandoned", "Trips abandoned", "", true, func(m Metrics) float64 { return float64(m.TripsAbandoned) }},
		{"stoppedHours", "Vehicle hours stopped", "h", true, func(m Metrics) float64 { return m.StoppedHours }},
		{"fuel", "Fuel consumed", "L", true, func(m Metrics) float64 { return m.FuelLitres }},
		{"co2", "CO2 emitted", "kg", true, func(m Metrics) float64 { return m.CO2Kg }},
		{"vehicleKm", "Vehicle distance", "km", true, func(m Metrics) float64 { return m.VehicleKm }},
		{"peakHospital", "Peak hospital utilisation", "%", true, func(m Metrics) float64 { return float64(m.PeakHospitalUtilPct) }},
		{"hospitalDiversions", "Hospital diversions", "", true, func(m Metrics) float64 { return float64(m.HospitalDiversions) }},
		{"transitBoardings", "Transit boardings", "", false, func(m Metrics) float64 { return float64(m.TransitBoardings) }},
		{"transitDenied", "Passengers left behind", "", true, func(m Metrics) float64 { return float64(m.TransitDenied) }},
		{"incidents", "Incidents opened", "", true, func(m Metrics) float64 { return float64(m.IncidentsOpened) }},
		{"substationTrips", "Substation trips", "", true, func(m Metrics) float64 { return float64(m.SubstationTrips) }},
		{"reroutes", "Reroutes", "", false, func(m Metrics) float64 { return float64(m.Reroutes) }},
	}

	base := entries[0].brief.Metrics
	for _, d := range defs {
		row := ComparisonRow{
			Key: d.key, Label: d.label, Unit: d.unit, LowerIsBetter: d.lower,
			Values: map[string]float64{}, DeltaPct: map[string]float64{},
		}
		b := d.get(base)
		for _, en := range entries {
			v := d.get(en.brief.Metrics)
			row.Values[en.brief.ID] = round2(v)
			if b != 0 {
				row.DeltaPct[en.brief.ID] = round2((v - b) / b * 100)
			} else if v != 0 {
				row.DeltaPct[en.brief.ID] = 100
			}
		}
		cmp.Rows = append(cmp.Rows, row)
	}
	sort.SliceStable(cmp.Warnings, func(i, j int) bool { return cmp.Warnings[i] < cmp.Warnings[j] })
	return cmp, nil
}

func round2(v float64) float64 {
	if v > 1e15 || v < -1e15 {
		return v
	}
	return float64(int64(v*100+sign(v)*0.5)) / 100
}

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}

// DistrictStat summarises one district for the map overlays and the AI tools.
type DistrictStat struct {
	ID            int32  `json:"id"`
	Name          string `json:"name"`
	Vehicles      int32  `json:"vehicles"`
	AvgSpeedKph   int32  `json:"avgSpeedKph"`
	CongestedPct  int32  `json:"congestedPct"`
	OpenIncidents int32  `json:"openIncidents"`
	PoweredPct    int32  `json:"poweredPct"`
	Region        int32  `json:"region"`
}

// DistrictStats aggregates live network state per district.
func DistrictStats(e *engine.Engine) []DistrictStat {
	m, st := e.Map, e.S
	out := make([]DistrictStat, len(m.Districts))
	sums := make([]int64, len(m.Districts))
	counts := make([]int64, len(m.Districts))
	cong := make([]int64, len(m.Districts))
	links := make([]int64, len(m.Districts))
	for i := range st.Edges.Count {
		d := m.Edges[i].District
		c := int64(st.Edges.Count[i])
		if c == 0 {
			continue
		}
		sums[d] += units.MMPerTickToKmh(st.Edges.Speed[i]) * c
		counts[d] += c
		links[d]++
		if fs := m.Edges[i].FreeSpeed; fs > 0 && int64(st.Edges.Speed[i])*100/int64(fs) < 45 {
			cong[d]++
		}
	}
	subOn := make([]int32, len(m.Districts))
	subAll := make([]int32, len(m.Districts))
	for i := range m.Substations {
		d := m.Substations[i].District
		subAll[d]++
		if st.Subs.Online[i] {
			subOn[d]++
		}
	}
	inc := make([]int32, len(m.Districts))
	for i := range st.Incidents {
		if !st.Incidents[i].Resolved && int(st.Incidents[i].District) < len(inc) {
			inc[st.Incidents[i].District]++
		}
	}
	for d := range m.Districts {
		o := DistrictStat{
			ID: int32(d), Name: m.Districts[d].Name,
			Vehicles: int32(counts[d]), OpenIncidents: inc[d],
			Region: e.RegionOfDistrict(world.DistrictID(d)),
		}
		if counts[d] > 0 {
			o.AvgSpeedKph = int32(sums[d] / counts[d])
		}
		if links[d] > 0 {
			o.CongestedPct = int32(cong[d] * 100 / links[d])
		}
		if subAll[d] > 0 {
			o.PoweredPct = subOn[d] * 100 / subAll[d]
		} else {
			o.PoweredPct = 100
		}
		out[d] = o
	}
	return out
}
