package systems

import (
	"github.com/mirror-sim/mirror/internal/events"
	"github.com/mirror-sim/mirror/internal/rng"
	"github.com/mirror-sim/mirror/internal/state"
	"github.com/mirror-sim/mirror/internal/units"
)

// WeatherSystem evolves conditions.
//
// Weather is a first-class *input* to three other systems (speed, crash
// hazard, electrical demand) rather than a visual effect. It is deliberately
// coarse -- one condition for the whole city -- because a spatially varying
// weather field would add a large amount of state for an effect that is
// invisible at city scale over a few simulated hours.
func WeatherSystem(c *Ctx, g *Region) {
	s := c.S
	if s.Weather.UntilTick != 0 && uint32(c.Tick) >= s.Weather.UntilTick {
		SetWeather(c, g, 0, 18, 8, 0)
		return
	}
	// Natural drift, evaluated once per simulated 10 minutes.
	if uint64(c.Tick)%(10*units.TicksPerMinute) != 0 || s.Weather.UntilTick != 0 {
		return
	}
	gr := rng.Derive(s.Seed, rng.StreamWeather, uint64(c.Tick), 0)
	if !gr.Chance(45) {
		return
	}
	cond := gr.IntN(6)
	dur := int64(gr.Range(30, 180)) * units.TicksPerMinute
	temp := int32(18)
	switch cond {
	case 3:
		temp = gr.Range(-6, 2)
	case 4:
		temp = gr.Range(31, 41)
	case 2:
		temp = gr.Range(8, 18)
	default:
		temp = gr.Range(6, 26)
	}
	SetWeather(c, g, cond, temp, gr.Range(3, 70), dur)
}

// SetWeather applies a condition. Used by both the natural drift and the
// cmd.set_weather command so the two paths cannot diverge.
func SetWeather(c *Ctx, g *Region, cond, temp, wind int32, durationTicks int64) {
	s := c.S
	prev := s.Weather.Condition
	s.Weather.Condition = cond
	s.Weather.TempC = temp
	s.Weather.WindKph = wind
	switch cond {
	case 5:
		s.Weather.VisibilityM = 180
	case 2, 3:
		s.Weather.VisibilityM = 1200
	case 1:
		s.Weather.VisibilityM = 4000
	default:
		s.Weather.VisibilityM = 10000
	}
	if durationTicks > 0 {
		s.Weather.UntilTick = uint32(int64(c.Tick) + durationTicks)
	} else {
		s.Weather.UntilTick = 0
	}
	if prev != cond {
		sev := events.SevInfo
		if cond == 2 || cond == 3 || cond == 4 {
			sev = events.SevWarning
		}
		g.emit(c.Tick, events.EvtWeatherChanged, sev, int64(cond), int64(temp), int64(wind), 0)
	}
}

// SampleMetrics pushes one time-series sample per simulated minute and derives
// the fuel and emissions totals from the cumulative counters.
func SampleMetrics(c *Ctx, activeVehicles int32) {
	m, s := c.Map, c.S
	var speedSum, speedN int64
	var congested, counted int64
	for e := range s.Edges.Speed {
		if s.Edges.Count[e] == 0 {
			continue
		}
		speedSum += units.MMPerTickToKmh(s.Edges.Speed[e]) * int64(s.Edges.Count[e])
		speedN += int64(s.Edges.Count[e])
		counted++
		if m.Edges[e].FreeSpeed > 0 && int64(s.Edges.Speed[e])*100/int64(m.Edges[e].FreeSpeed) < 45 {
			congested++
		}
	}
	avgKph := int32(0)
	if speedN > 0 {
		avgKph = int32(speedSum / speedN)
	}
	congP := int32(0)
	if counted > 0 {
		congP = int32(congested * 1000 / counted)
	}
	var used, total int64
	for i := range m.Hospitals {
		used += int64(s.Hosps.BedsUsed[i])
		total += int64(m.Hospitals[i].Beds)
	}
	hospP := int32(0)
	if total > 0 {
		hospP = int32(used * 1000 / total)
	}
	online := 0
	for i := range s.Subs.Online {
		if s.Subs.Online[i] {
			online++
		}
	}
	poweredP := int32(1000)
	if len(s.Subs.Online) > 0 {
		poweredP = int32(online * 1000 / len(s.Subs.Online))
	}
	open := int32(0)
	for i := range s.Incidents {
		if !s.Incidents[i].Resolved {
			open++
		}
	}
	s.Metrics.FuelMl, s.Metrics.CO2G = FuelAndEmissions(s.Metrics.DistanceMM, s.Metrics.StoppedVehicleTicks)
	s.Metrics.MaybeSample(c.Tick, activeVehicles, avgKph, congP, hospP, poweredP, open)
}

// ActiveVehicleCount counts vehicles currently on the network.
func ActiveVehicleCount(s *state.State) int32 {
	var n int32
	for i := range s.Vehicles.Status {
		if s.Vehicles.Status[i] != state.VehIdle {
			n++
		}
	}
	return n
}
