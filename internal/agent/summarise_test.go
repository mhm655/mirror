package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func stepWith(t *testing.T, tool string, payload any) Step {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return Step{Tool: tool, Output: json.RawMessage(b)}
}

func TestDecodeReturnsFalseOnToolError(t *testing.T) {
	s := Step{Error: "boom"}
	type payload struct{ X int }
	if _, ok := decode[payload](s); ok {
		t.Fatal("decode should fail when the step recorded an error")
	}
}

func TestDecodeReturnsFalseOnEmptyOutput(t *testing.T) {
	s := Step{}
	type payload struct{ X int }
	if _, ok := decode[payload](s); ok {
		t.Fatal("decode should fail on empty output")
	}
}

func TestDecodeRoundTrips(t *testing.T) {
	type payload struct {
		X int `json:"x"`
	}
	s := stepWith(t, "t", payload{X: 42})
	got, ok := decode[payload](s)
	if !ok || got.X != 42 {
		t.Fatalf("decode = %+v, ok=%v, want x=42", got, ok)
	}
}

func TestSummariseHospitalsNoIssues(t *testing.T) {
	s := stepWith(t, "get_hospital_capacity", map[string]any{
		"hospitals":       []map[string]any{{"name": "General", "utilisationPct": 40, "beds": 100, "bedsUsed": 40, "ambulancesAvailable": 3}},
		"totalAdmissions": 10, "peakUtilPct": 40,
	})
	out := summariseHospitals(s)
	if !strings.Contains(out, "No diversions and no rejections") {
		t.Errorf("expected the no-issues sentence, got: %s", out)
	}
	if !strings.Contains(out, "General") {
		t.Errorf("expected the hospital name in the report, got: %s", out)
	}
}

func TestSummariseHospitalsFlagsDiversionsAndNoAmbulances(t *testing.T) {
	s := stepWith(t, "get_hospital_capacity", map[string]any{
		"hospitals": []map[string]any{
			{"name": "General", "utilisationPct": 95, "beds": 100, "bedsUsed": 95, "ambulancesAvailable": 0},
		},
		"totalAdmissions": 10, "totalRejections": 2, "totalDiversions": 5, "peakUtilPct": 95,
	})
	out := summariseHospitals(s)
	if !strings.Contains(out, "5 patients were diverted and 2 were turned away") {
		t.Errorf("expected diversion/rejection counts, got: %s", out)
	}
	if !strings.Contains(out, "No ambulances available at: General") {
		t.Errorf("expected the zero-ambulance callout, got: %s", out)
	}
}

func TestSummariseHospitalsUnreadable(t *testing.T) {
	s := Step{Error: "timeout"}
	if out := summariseHospitals(s); out != "I could not read hospital status." {
		t.Errorf("got %q", out)
	}
}

func TestSummarisePowerGridIntact(t *testing.T) {
	s := stepWith(t, "get_power_status", map[string]any{
		"substations": []map[string]any{{"name": "A", "online": true, "utilisationPct": 50}},
		"trips":       0,
	})
	out := summarisePower(s)
	if !strings.Contains(out, "Grid is intact") {
		t.Errorf("expected intact-grid message, got: %s", out)
	}
}

func TestSummarisePowerReportsOutagesAndDarkSignals(t *testing.T) {
	s := stepWith(t, "get_power_status", map[string]any{
		"substations": []map[string]any{
			{"name": "Downtown", "district": "Central", "online": false, "restoreInSeconds": 120},
			{"name": "North", "online": true, "utilisationPct": 90, "loadKW": 900, "capacityKW": 1000},
		},
		"signalsDark": 12, "signalsTotal": 40,
	})
	out := summarisePower(s)
	if !strings.Contains(out, "1 of 2 substations are offline") {
		t.Errorf("expected offline count, got: %s", out)
	}
	if !strings.Contains(out, "12 of 40 traffic signals are dark") {
		t.Errorf("expected dark signal count, got: %s", out)
	}
	if !strings.Contains(out, "Running hot") || !strings.Contains(out, "North") {
		t.Errorf("expected the stressed substation to be called out, got: %s", out)
	}
}

func TestSummariseTransitDeniedPassengers(t *testing.T) {
	s := stepWith(t, "get_transit_load", map[string]any{
		"routes":    []map[string]any{{"name": "Line 1", "mode": "bus", "vehiclesInService": 4, "ridersOnBoard": 200, "capacityPerVehicle": 60}},
		"boardings": 800, "passengersDenied": 200, "transitFleetPct": 100, "stopsWithQueue": 3,
	})
	out := summariseTransit(s)
	if !strings.Contains(out, "200 passengers were left behind") {
		t.Errorf("expected denied-passenger sentence, got: %s", out)
	}
	if !strings.Contains(out, "Line 1") {
		t.Errorf("expected route name, got: %s", out)
	}
}

func TestSummariseTransitNoDenials(t *testing.T) {
	s := stepWith(t, "get_transit_load", map[string]any{
		"routes": []map[string]any{}, "boardings": 100, "passengersDenied": 0,
	})
	out := summariseTransit(s)
	if !strings.Contains(out, "Nobody has been left behind") {
		t.Errorf("expected the no-denials sentence, got: %s", out)
	}
}

func TestSummariseEventsEmpty(t *testing.T) {
	s := stepWith(t, "query_events", map[string]any{"events": []map[string]any{}})
	out := summariseEvents(s)
	if !strings.Contains(out, "Nothing above notice severity") {
		t.Errorf("got %q", out)
	}
}

func TestSummariseEventsListsRows(t *testing.T) {
	s := stepWith(t, "query_events", map[string]any{
		"events": []map[string]any{
			{"simulatedClock": "08:15", "kind": "accident", "severity": "warning", "text": "Accident on Main St"},
		},
	})
	out := summariseEvents(s)
	if !strings.Contains(out, "Accident on Main St") || !strings.Contains(out, "08:15") {
		t.Errorf("got %q", out)
	}
}

func TestSummariseComparisonNothingMoved(t *testing.T) {
	s := stepWith(t, "compare_scenarios", map[string]any{
		"baselineId": "sim-a",
		"scenarios": []map[string]any{
			{"id": "sim-a", "name": "A - baseline", "metrics": map[string]any{"tripsCompleted": 500}},
			{"id": "sim-b", "name": "B - adaptive", "metrics": map[string]any{"tripsCompleted": 500}},
		},
		"rows": []map[string]any{
			{"key": "travelMean", "label": "Mean travel time", "unit": "s", "lowerIsBetter": true,
				"values":   map[string]float64{"sim-a": 400, "sim-b": 400.1},
				"deltaPct": map[string]float64{"sim-b": 0.02}},
		},
	})
	out := summariseComparison(s, 30)
	if !strings.Contains(out, "Nothing moved by more than half a percent") {
		t.Errorf("expected the nothing-moved sentence, got: %s", out)
	}
}

func TestSummariseComparisonReportsImprovement(t *testing.T) {
	s := stepWith(t, "compare_scenarios", map[string]any{
		"baselineId": "sim-a",
		"scenarios": []map[string]any{
			{"id": "sim-a", "name": "A - no intervention", "metrics": map[string]any{"tripsCompleted": 500}},
			{"id": "sim-b", "name": "B - adaptive signals", "metrics": map[string]any{"tripsCompleted": 520}},
		},
		"rows": []map[string]any{
			{"key": "travelMean", "label": "Mean travel time", "unit": "s", "lowerIsBetter": true,
				"values":   map[string]float64{"sim-a": 400, "sim-b": 340},
				"deltaPct": map[string]float64{"sim-b": -15}},
		},
	})
	out := summariseComparison(s, 30)
	if !strings.Contains(out, "-15.0% (better)") {
		t.Errorf("expected an improvement to be labelled 'better', got: %s", out)
	}
}

func TestSummariseComparisonUnreadable(t *testing.T) {
	s := Step{Error: "fork failed"}
	if out := summariseComparison(s, 30); out != "The comparison did not complete." {
		t.Errorf("got %q", out)
	}
}

func TestPlural(t *testing.T) {
	if got := plural(1, "person", "people"); got != "1 person" {
		t.Errorf("plural(1) = %q", got)
	}
	if got := plural(2, "person", "people"); got != "2 people" {
		t.Errorf("plural(2) = %q", got)
	}
	if got := plural(0, "person", "people"); got != "0 people" {
		t.Errorf("plural(0) = %q", got)
	}
}

func TestShortName(t *testing.T) {
	if got := shortName("B - adaptive signals"); got != "B" {
		t.Errorf("shortName with separator = %q, want B", got)
	}
	if got := shortName("short"); got != "short" {
		t.Errorf("shortName passthrough = %q", got)
	}
	long := "this name is definitely longer than eighteen characters"
	if got := shortName(long); len(got) != 18 {
		t.Errorf("shortName truncation = %q (len %d), want len 18", got, len(got))
	}
}
