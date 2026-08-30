package events

import (
	"fmt"

	"github.com/mirror-sim/mirror/internal/units"
	"github.com/mirror-sim/mirror/internal/world"
)

// Payload slot meanings, by Kind. Kept in one place so the encoder, the
// describer and the AI tool layer cannot drift apart.
//
//	cmd.seed_population           A=count            B=startHour
//	cmd.set_policy                A..D packed policy deltas (see simctl)
//	cmd.inject_accident           A=edge  B=severity C=casualties D=blockTicks
//	cmd.close_road                A=edge  B=durationTicks
//	cmd.reopen_road               A=edge
//	cmd.power_failure             A=substation B=durationTicks
//	cmd.power_restore             A=substation
//	cmd.set_weather               A=condition B=tempC C=windKph D=durationTicks
//	cmd.hospital_surge            A=hospital B=patients
//	cmd.transit_failure           A=route B=durationTicks
//	cmd.flood_district            A=district B=severity C=durationTicks
//	cmd.earthquake                A=magnitudeP B=epicentreNode C=radiusMM
//	cmd.comms_outage              A=tower B=durationTicks
//	cmd.spawn_traffic             A=count B=originDistrict C=destDistrict
//
//	vehicle.entered_road          A=vehicle B=edge
//	vehicle.rerouted              A=vehicle B=fromEdge C=newCostTicks D=oldCostTicks
//	vehicle.arrived               A=vehicle B=agent C=travelTicks D=delayTicks
//	trip.started                  A=agent  B=mode  C=originNode D=destNode
//	signal.changed                A=signal B=phase
//	signal.power_lost             A=signal B=substation
//	incident.accident             A=incident B=edge C=severity D=casualties
//	road.closed                   A=edge B=untilTick
//	traffic.congestion_critical   A=edge B=speedKph C=count D=jam
//	emergency.dispatched          A=vehicle B=incident C=depotOrHospital D=etaTicks
//	emergency.on_scene            A=vehicle B=incident C=responseTicks
//	hospital.admission            A=hospital B=agent C=bedsUsed D=beds
//	hospital.overloaded           A=hospital B=bedsUsed C=beds
//	hospital.diverted             A=fromHospital B=toHospital C=agent
//	power.substation_tripped      A=substation B=loadKW C=capacityKW
//	power.blackout_district       A=district B=poiCount
//	comms.tower_overloaded        A=tower B=loadErl C=capacityErl
//	transit.denied                A=agent B=route C=stop
//	weather.changed               A=condition B=tempC
//	region.handoff                A=fromRegion B=toRegion C=vehicle D=edge
//	determinism.divergence        A=tick B=expectedDigestLo C=actualDigestLo

// C builds a command.
func C(tick units.Tick, k Kind, a, b, c, d int64) Event {
	return Event{Tick: tick, Kind: k, Severity: SevNotice, A: a, B: b, C: c, D: d}
}

// E builds an effect.
func E(tick units.Tick, k Kind, sev Severity, region int16, a, b, c, d int64) Event {
	return Event{Tick: tick, Kind: k, Severity: sev, Region: region, A: a, B: b, C: c, D: d}
}

// Describe renders a human-readable line for the UI event feed.
//
// This lives server-side rather than in the browser because the payload slots
// need the map to be meaningful (edge 8412 means nothing; "Kessler / arterial
// northbound" does), and shipping the whole map to every client just to render
// a log line would be absurd.
func Describe(m *world.Map, e Event) string {
	edge := func(id int64) string {
		if id < 0 || int(id) >= len(m.Edges) {
			return "unknown road"
		}
		ed := &m.Edges[id]
		return fmt.Sprintf("%s %s #%d", m.Districts[ed.District].Name, ed.Class, id)
	}
	hosp := func(id int64) string {
		if id < 0 || int(id) >= len(m.Hospitals) {
			return "unknown hospital"
		}
		return m.Hospitals[id].Name
	}
	sub := func(id int64) string {
		if id < 0 || int(id) >= len(m.Substations) {
			return "unknown substation"
		}
		return m.Substations[id].Name
	}
	dist := func(id int64) string {
		if id < 0 || int(id) >= len(m.Districts) {
			return "unknown district"
		}
		return m.Districts[id].Name
	}

	switch e.Kind {
	case CmdSeedPopulation:
		return fmt.Sprintf("Seeded %d residents, day starts at %02d:00", e.A, e.B)
	case CmdSetPolicy:
		return "Policy updated"
	case CmdInjectAccident, EvtAccidentOccurred:
		return fmt.Sprintf("Collision on %s - severity %d/1000, %d casualties", edge(e.B), e.C, e.D)
	case CmdCloseRoad, EvtRoadClosed:
		return fmt.Sprintf("%s closed", edge(e.A))
	case CmdReopenRoad, EvtRoadReopened:
		return fmt.Sprintf("%s reopened", edge(e.A))
	case CmdPowerFailure:
		return fmt.Sprintf("%s forced offline for %ds", sub(e.A), e.B/units.TicksPerSecond)
	case EvtSubstationTripped:
		return fmt.Sprintf("%s tripped - load %d kW exceeded %d kW capacity", sub(e.A), e.B, e.C)
	case EvtSubstationRestored:
		return fmt.Sprintf("%s back online", sub(e.A))
	case EvtBlackoutDistrict:
		return fmt.Sprintf("Blackout across %s - %d premises without power", dist(e.A), e.B)
	case EvtSignalPowerLost:
		return fmt.Sprintf("Signal %d dark (%s) - reverting to all-way stop", e.A, sub(e.B))
	case EvtCongestionCritical:
		return fmt.Sprintf("Gridlock on %s - %d km/h, %d/%d vehicles", edge(e.A), e.B, e.C, e.D)
	case EvtEmergencyDispatched:
		return fmt.Sprintf("Unit dispatched to incident %d, ETA %ds", e.B, e.D/units.TicksPerSecond)
	case EvtEmergencyOnScene:
		return fmt.Sprintf("Unit on scene at incident %d after %ds", e.B, e.C/units.TicksPerSecond)
	case EvtIncidentResolved:
		return fmt.Sprintf("Incident %d cleared", e.A)
	case EvtHospitalAdmission:
		return fmt.Sprintf("%s admitted a patient (%d/%d beds)", hosp(e.A), e.C, e.D)
	case EvtHospitalOverloaded:
		return fmt.Sprintf("%s at capacity - %d/%d beds", hosp(e.A), e.B, e.C)
	case EvtHospitalDiverted:
		return fmt.Sprintf("%s diverting to %s", hosp(e.A), hosp(e.B))
	case EvtHospitalOnBackup:
		return fmt.Sprintf("%s switched to backup generators", hosp(e.A))
	case EvtTowerOverloaded:
		return fmt.Sprintf("Cell site %d congested - %d/%d sessions", e.A, e.B, e.C)
	case EvtTowerDown:
		return fmt.Sprintf("Cell site %d off air", e.A)
	case EvtTransitDenied:
		return fmt.Sprintf("Passenger left behind at stop %d on route %d", e.C, e.B)
	case EvtWeatherChanged:
		return fmt.Sprintf("Weather: %s, %d degC", WeatherName(int32(e.A)), e.B)
	case EvtVehicleStranded:
		return fmt.Sprintf("Vehicle %d stranded - no route to destination", e.A)
	case EvtWorkerLost:
		return fmt.Sprintf("Region worker %d lost", e.A)
	case EvtWorkerRecovered:
		return fmt.Sprintf("Region worker %d recovered from checkpoint at tick %d, replayed %d ticks", e.A, e.B, e.C)
	case EvtDivergenceDetected:
		return fmt.Sprintf("DIVERGENCE at tick %d: expected %016x got %016x", e.A, uint64(e.B), uint64(e.C))
	case EvtCheckpointWritten:
		return fmt.Sprintf("Checkpoint at tick %d (%d KB, %dms)", e.A, e.B/1024, e.C)
	case CmdFloodDistrict:
		return fmt.Sprintf("Flooding in %s, severity %d/1000", dist(e.A), e.B)
	case CmdEarthquake:
		return fmt.Sprintf("Earthquake, magnitude %d.%d", e.A/1000, (e.A%1000)/100)
	case CmdSetWeather:
		return fmt.Sprintf("Weather set to %s, %d degC", WeatherName(int32(e.A)), e.B)
	case CmdHospitalSurge:
		return fmt.Sprintf("Mass casualty: %d patients to %s", e.B, hosp(e.A))
	case CmdTransitFailure:
		return fmt.Sprintf("Route %d suspended", e.A)
	case CmdSpawnTraffic:
		return fmt.Sprintf("Injected %d extra trips", e.A)
	}
	return e.Kind.String()
}

var weatherNames = [...]string{"clear", "rain", "storm", "snow", "heatwave", "fog"}

func WeatherName(c int32) string {
	if int(c) < len(weatherNames) && c >= 0 {
		return weatherNames[c]
	}
	return "unknown"
}

func WeatherCode(name string) (int32, bool) {
	for i, n := range weatherNames {
		if n == name {
			return int32(i), true
		}
	}
	return 0, false
}
