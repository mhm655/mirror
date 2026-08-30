package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The built-in planner's report writers.
//
// These render tool OUTPUT, never assumptions. Every sentence below is
// traceable to a field in a tool result, which is the same standard the LLM
// path is held to by its system prompt.

func decode[T any](s Step) (T, bool) {
	var v T
	if s.Error != "" || len(s.Output) == 0 {
		return v, false
	}
	if err := json.Unmarshal(s.Output, &v); err != nil {
		return v, false
	}
	return v, true
}

func summariseHospitals(s Step) string {
	type hosp struct {
		Name       string `json:"name"`
		District   string `json:"district"`
		Beds       int32  `json:"beds"`
		BedsUsed   int32  `json:"bedsUsed"`
		UtilPct    int64  `json:"utilisationPct"`
		Ambulances int32  `json:"ambulancesAvailable"`
		OnBackup   bool   `json:"onBackupPower"`
		Rejections int64  `json:"rejections"`
		Diversions int64  `json:"diversions"`
	}
	type payload struct {
		Hospitals  []hosp `json:"hospitals"`
		Admissions int64  `json:"totalAdmissions"`
		Rejections int64  `json:"totalRejections"`
		Diversions int64  `json:"totalDiversions"`
		PeakPct    int32  `json:"peakUtilPct"`
	}
	p, ok := decode[payload](s)
	if !ok {
		return "I could not read hospital status."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Health system: %d admissions so far, peak occupancy %d%%.\n",
		p.Admissions, p.PeakPct)
	if p.Rejections > 0 || p.Diversions > 0 {
		fmt.Fprintf(&b, "%d patients were diverted and %d were turned away. That is the number to watch.\n",
			p.Diversions, p.Rejections)
	} else {
		b.WriteString("No diversions and no rejections: the network is absorbing demand.\n")
	}
	b.WriteString("\nBusiest sites:\n")
	for i, h := range p.Hospitals {
		if i >= 4 {
			break
		}
		fmt.Fprintf(&b, "  %-28s %3d%% (%d/%d beds), %d ambulances free",
			h.Name, h.UtilPct, h.BedsUsed, h.Beds, h.Ambulances)
		if h.OnBackup {
			b.WriteString("  [ON BACKUP POWER]")
		}
		b.WriteByte('\n')
	}
	var noAmb []string
	for _, h := range p.Hospitals {
		if h.Ambulances == 0 {
			noAmb = append(noAmb, h.Name)
		}
	}
	if len(noAmb) > 0 {
		fmt.Fprintf(&b, "\nNo ambulances available at: %s. Response times in those districts will be driven by travel from further away.\n",
			strings.Join(noAmb, ", "))
	}
	return b.String()
}

func summarisePower(s Step) string {
	type sub struct {
		Name      string `json:"name"`
		District  string `json:"district"`
		Online    bool   `json:"online"`
		LoadKW    int32  `json:"loadKW"`
		CapKW     int32  `json:"capacityKW"`
		UtilPct   int64  `json:"utilisationPct"`
		RestoreIn int64  `json:"restoreInSeconds"`
	}
	type payload struct {
		Substations  []sub `json:"substations"`
		SignalsDark  int   `json:"signalsDark"`
		SignalsTotal int   `json:"signalsTotal"`
		Trips        int64 `json:"substationTrips"`
		Dropped      int64 `json:"commsSessionsDropped"`
	}
	p, ok := decode[payload](s)
	if !ok {
		return "I could not read the power network."
	}
	var offline, stressed []sub
	for _, x := range p.Substations {
		if !x.Online {
			offline = append(offline, x)
		} else if x.UtilPct >= 85 {
			stressed = append(stressed, x)
		}
	}
	var b strings.Builder
	if len(offline) == 0 {
		fmt.Fprintf(&b, "Grid is intact: all %d substations energised, %d cumulative trips.\n",
			len(p.Substations), p.Trips)
	} else {
		fmt.Fprintf(&b, "%d of %d substations are offline.\n", len(offline), len(p.Substations))
		for _, x := range offline {
			fmt.Fprintf(&b, "  %s (%s) - restoring in about %ds\n", x.Name, x.District, x.RestoreIn)
		}
		fmt.Fprintf(&b, "\n%d of %d traffic signals are dark. Those junctions are operating as all-way stops, "+
			"which cuts their throughput by roughly 60%% and is usually the dominant traffic effect of an outage.\n",
			p.SignalsDark, p.SignalsTotal)
	}
	if len(stressed) > 0 {
		b.WriteString("\nRunning hot (a trip here would cascade to its neighbours):\n")
		for i, x := range stressed {
			if i >= 5 {
				break
			}
			fmt.Fprintf(&b, "  %s %d%% (%d/%d kW)\n", x.Name, x.UtilPct, x.LoadKW, x.CapKW)
		}
	}
	if p.Dropped > 0 {
		fmt.Fprintf(&b, "\nComms: %d sessions dropped, which happens when a cell site loses mains and exhausts its battery, or when incident-driven call volume exceeds capacity.\n", p.Dropped)
	}
	return b.String()
}

func summariseTransit(s Step) string {
	type route struct {
		Name      string `json:"name"`
		Mode      string `json:"mode"`
		InService int    `json:"vehiclesInService"`
		Riders    int32  `json:"ridersOnBoard"`
		Capacity  int32  `json:"capacityPerVehicle"`
	}
	type payload struct {
		Routes    []route `json:"routes"`
		Boardings int64   `json:"boardings"`
		Denied    int64   `json:"passengersDenied"`
		FleetPct  int32   `json:"transitFleetPct"`
		Queued    int     `json:"stopsWithQueue"`
	}
	p, ok := decode[payload](s)
	if !ok {
		return "I could not read transit status."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Transit: %d boardings, fleet at %d%% of baseline, %d stops with people waiting.\n",
		p.Boardings, p.FleetPct, p.Queued)
	if p.Denied > 0 {
		share := float64(p.Denied) / float64(p.Denied+p.Boardings) * 100
		fmt.Fprintf(&b, "%d passengers were left behind for lack of capacity (%.1f%% of everyone who tried to board). "+
			"That is the specific number that adding vehicles would reduce.\n", p.Denied, share)
	} else {
		b.WriteString("Nobody has been left behind: capacity is not the binding constraint right now.\n")
	}
	sort.Slice(p.Routes, func(i, j int) bool { return p.Routes[i].Riders > p.Routes[j].Riders })
	b.WriteString("\nBusiest routes:\n")
	for i, r := range p.Routes {
		if i >= 5 || r.Riders == 0 {
			break
		}
		load := 0
		if r.InService > 0 && r.Capacity > 0 {
			load = int(r.Riders) * 100 / (r.InService * int(r.Capacity))
		}
		fmt.Fprintf(&b, "  %-26s %-5s %d in service, %d riders (%d%% loaded)\n",
			r.Name, r.Mode, r.InService, r.Riders, load)
	}
	return b.String()
}

func summariseIncidents(steps []Step, metricStep Step) string {
	type inc struct {
		ID          int64  `json:"id"`
		Kind        string `json:"kind"`
		District    string `json:"district"`
		Road        string `json:"road"`
		StartedAt   string `json:"startedAt"`
		Severity    int32  `json:"severity"`
		Casualties  int32  `json:"casualties"`
		Awaiting    int32  `json:"unitsStillNeeded"`
		ResponseSec int32  `json:"firstResponseSeconds"`
		OpenForSec  int64  `json:"openForSeconds"`
	}
	type ipayload struct {
		Incidents []inc `json:"incidents"`
	}
	var b strings.Builder
	for _, s := range steps {
		if s.Tool != "inspect_incident" {
			continue
		}
		p, ok := decode[ipayload](s)
		if !ok {
			continue
		}
		if len(p.Incidents) == 0 {
			b.WriteString("No open incidents.\n")
			break
		}
		fmt.Fprintf(&b, "%d open incidents:\n", len(p.Incidents))
		for i, x := range p.Incidents {
			if i >= 8 {
				fmt.Fprintf(&b, "  ... and %d more\n", len(p.Incidents)-8)
				break
			}
			fmt.Fprintf(&b, "  #%d %s in %s (%s), severity %d, %d casualties, open %ds",
				x.ID, x.Kind, x.District, x.Road, x.Severity, x.Casualties, x.OpenForSec)
			if x.ResponseSec > 0 {
				fmt.Fprintf(&b, ", first unit arrived after %ds", x.ResponseSec)
			} else if x.Awaiting > 0 {
				fmt.Fprintf(&b, ", STILL AWAITING %d units", x.Awaiting)
			}
			b.WriteByte('\n')
		}
	}
	type mpayload struct {
		Metrics struct {
			ResponseMeanSec float64 `json:"responseMeanSec"`
			ResponseP95Sec  float64 `json:"responseP95Sec"`
			Dispatched      int64   `json:"emergencyDispatched"`
			Arrived         int64   `json:"emergencyArrived"`
		} `json:"metrics"`
	}
	if p, ok := decode[mpayload](metricStep); ok && p.Metrics.Dispatched > 0 {
		fmt.Fprintf(&b, "\nEmergency response across the run: %d dispatched, %d on scene, mean %.0fs, P95 %.0fs.\n",
			p.Metrics.Dispatched, p.Metrics.Arrived, p.Metrics.ResponseMeanSec, p.Metrics.ResponseP95Sec)
		if p.Metrics.Dispatched-p.Metrics.Arrived > 3 {
			b.WriteString("The gap between dispatched and on-scene means units are still travelling, or are stuck.\n")
		}
	}
	return b.String()
}

func summariseEvents(s Step) string {
	type row struct {
		Clock    string `json:"simulatedClock"`
		Kind     string `json:"kind"`
		Severity string `json:"severity"`
		Text     string `json:"text"`
	}
	type payload struct {
		Events []row `json:"events"`
	}
	p, ok := decode[payload](s)
	if !ok || len(p.Events) == 0 {
		return "Nothing above notice severity in the recent event window."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d notable events, most recent first:\n", len(p.Events))
	for _, e := range p.Events {
		fmt.Fprintf(&b, "  %s  [%-8s] %s\n", e.Clock, e.Severity, e.Text)
	}
	return b.String()
}

func summariseCity(steps []Step, metricStep Step) string {
	var b strings.Builder
	type tpayload struct {
		Clock          string `json:"simulatedClock"`
		ActiveVehicles int32  `json:"activeVehicles"`
		MeanSpeed      int32  `json:"meanSpeedKph"`
		Congested      int32  `json:"congestedLinksPct"`
		OpenIncidents  int32  `json:"openIncidents"`
		Weather        string `json:"weather"`
		WorstRoads     []struct {
			District  string `json:"district"`
			Class     string `json:"class"`
			SpeedKph  int64  `json:"speedKph"`
			FreeKph   int64  `json:"freeFlowKph"`
			Occupancy int32  `json:"vehicles"`
		} `json:"worstRoads"`
		WorstDistricts []struct {
			Name         string `json:"name"`
			CongestedPct int32  `json:"congestedPct"`
			AvgSpeedKph  int32  `json:"avgSpeedKph"`
			Vehicles     int32  `json:"vehicles"`
		} `json:"worstDistricts"`
	}
	type ppayload struct {
		Population     int   `json:"population"`
		Travelling     int32 `json:"travelling"`
		Stranded       int32 `json:"stranded"`
		Car            int   `json:"modalSplitCar"`
		Transit        int   `json:"modalSplitTransit"`
		Walk           int   `json:"modalSplitWalk"`
		TripsCompleted int64 `json:"tripsCompleted"`
	}
	for _, s := range steps {
		switch s.Tool {
		case "get_traffic":
			if p, ok := decode[tpayload](s); ok {
				fmt.Fprintf(&b, "City at %s, weather %s.\n", p.Clock, p.Weather)
				fmt.Fprintf(&b, "%d vehicles moving, mean speed %d km/h, %d%% of occupied links congested, %d open incidents.\n",
					p.ActiveVehicles, p.MeanSpeed, p.Congested, p.OpenIncidents)
				if len(p.WorstDistricts) > 0 {
					b.WriteString("\nWorst districts:\n")
					for i, d := range p.WorstDistricts {
						if i >= 3 {
							break
						}
						fmt.Fprintf(&b, "  %-14s %d%% congested, %d km/h, %d vehicles\n",
							d.Name, d.CongestedPct, d.AvgSpeedKph, d.Vehicles)
					}
				}
				if len(p.WorstRoads) > 0 {
					b.WriteString("\nWorst corridors (ranked by delay times vehicles affected):\n")
					for i, r := range p.WorstRoads {
						if i >= 4 {
							break
						}
						fmt.Fprintf(&b, "  %-14s %-10s %d km/h against %d free flow, %d vehicles\n",
							r.District, r.Class, r.SpeedKph, r.FreeKph, r.Occupancy)
					}
				}
			}
		case "get_population":
			if p, ok := decode[ppayload](s); ok {
				total := p.Car + p.Transit + p.Walk
				if total == 0 {
					total = 1
				}
				fmt.Fprintf(&b, "\nPopulation %d: %d travelling, %d trips completed.\n",
					p.Population, p.Travelling, p.TripsCompleted)
				fmt.Fprintf(&b, "Modal split: %d%% car, %d%% transit, %d%% walk.\n",
					p.Car*100/total, p.Transit*100/total, p.Walk*100/total)
				if p.Stranded > 0 {
					fmt.Fprintf(&b, "%d people are stranded with no viable route.\n", p.Stranded)
				}
			}
		}
	}
	type mpayload struct {
		Metrics struct {
			TravelMeanSec float64 `json:"travelMeanSec"`
			TravelP95Sec  float64 `json:"travelP95Sec"`
			DelayMeanSec  float64 `json:"delayMeanSec"`
			Completed     int64   `json:"tripsCompleted"`
		} `json:"metrics"`
	}
	if p, ok := decode[mpayload](metricStep); ok && p.Metrics.Completed > 0 {
		fmt.Fprintf(&b, "\nTravel time over %d completed trips: mean %.0fs, P95 %.0fs, mean delay against free flow %.0fs.\n",
			p.Metrics.Completed, p.Metrics.TravelMeanSec, p.Metrics.TravelP95Sec, p.Metrics.DelayMeanSec)
		if p.Metrics.Completed < 200 {
			b.WriteString("That is a small sample; the percentile is noisy.\n")
		}
	}
	return b.String()
}

func summariseComparison(s Step, minutes int64) string {
	type row struct {
		Key           string             `json:"key"`
		Label         string             `json:"label"`
		Unit          string             `json:"unit"`
		LowerIsBetter bool               `json:"lowerIsBetter"`
		Values        map[string]float64 `json:"values"`
		DeltaPct      map[string]float64 `json:"deltaPct"`
	}
	type scenario struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Metrics struct {
			TripsCompleted int64 `json:"tripsCompleted"`
		} `json:"metrics"`
	}
	type payload struct {
		BaselineID string     `json:"baselineId"`
		Scenarios  []scenario `json:"scenarios"`
		Rows       []row      `json:"rows"`
		Warnings   []string   `json:"warnings"`
	}
	p, ok := decode[payload](s)
	if !ok {
		return "The comparison did not complete."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Ran %d scenarios for %d simulated minutes each from the same starting state.\n\n", len(p.Scenarios), minutes)
	for _, sc := range p.Scenarios {
		fmt.Fprintf(&b, "  %-38s %s  (%d trips completed)\n", sc.Name, sc.ID, sc.Metrics.TripsCompleted)
	}

	// Only report the metrics that moved. A wall of unchanged rows is how a
	// comparison stops being read.
	headline := map[string]bool{
		"travelMean": true, "travelP95": true, "delayMean": true,
		"responseMean": true, "stoppedHours": true, "fuel": true,
		"tripsCompleted": true, "transitDenied": true,
	}
	b.WriteString("\nAgainst the baseline:\n")
	reported := 0
	for _, r := range p.Rows {
		if !headline[r.Key] {
			continue
		}
		var parts []string
		for _, sc := range p.Scenarios[1:] {
			d := r.DeltaPct[sc.ID]
			if d > -0.5 && d < 0.5 {
				continue
			}
			verdict := "worse"
			if (d < 0) == r.LowerIsBetter {
				verdict = "better"
			}
			parts = append(parts, fmt.Sprintf("%s %+.1f%% (%s)", shortName(sc.Name), d, verdict))
		}
		if len(parts) == 0 {
			continue
		}
		base := r.Values[p.BaselineID]
		fmt.Fprintf(&b, "  %-26s baseline %.1f%s -> %s\n", r.Label, base, r.Unit, strings.Join(parts, ", "))
		reported++
	}
	if reported == 0 {
		b.WriteString("  Nothing moved by more than half a percent. Over this window the interventions are indistinguishable from doing nothing.\n")
	}
	for _, w := range p.Warnings {
		fmt.Fprintf(&b, "\nCaveat: %s\n", w)
	}
	b.WriteString("\nThese are single runs on one seed. Before acting on a difference this small, re-run the same comparison on several seeds.\n")
	return b.String()
}

// plural picks the right noun and verb for a count. Small, but "1 people are
// stranded" in an operations report undermines confidence in every other
// number on the page.
func plural(n int64, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func shortName(n string) string {
	if i := strings.Index(n, " - "); i > 0 {
		return n[:i]
	}
	if len(n) > 18 {
		return n[:18]
	}
	return n
}
