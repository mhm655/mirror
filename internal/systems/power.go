package systems

import (
	"github.com/mirror-sim/mirror/internal/events"
	"github.com/mirror-sim/mirror/internal/rng"
	"github.com/mirror-sim/mirror/internal/state"
	"github.com/mirror-sim/mirror/internal/units"
	"github.com/mirror-sim/mirror/internal/world"
)

// PowerIndex caches the map-derived fan-out of the distribution network so the
// power system does not rescan every edge, signal and tower each tick.
// Rebuilt whenever a state is loaded; never part of state.
type PowerIndex struct {
	SignalsBySub [][]int32
	EdgesBySub   [][]world.EdgeID
	TowersBySub  [][]world.TowerID
	HospsBySub   [][]world.HospitalID
	// SubsByDistrict lets the blackout event name a district.
	POIsBySub [][]world.POIID
}

func BuildPowerIndex(m *world.Map) *PowerIndex {
	n := len(m.Substations)
	p := &PowerIndex{
		SignalsBySub: make([][]int32, n),
		EdgesBySub:   make([][]world.EdgeID, n),
		TowersBySub:  make([][]world.TowerID, n),
		HospsBySub:   make([][]world.HospitalID, n),
		POIsBySub:    make([][]world.POIID, n),
	}
	for i := range m.Signals {
		if f := m.Signals[i].Feeder; f >= 0 {
			p.SignalsBySub[f] = append(p.SignalsBySub[f], int32(i))
		}
	}
	for i := range m.Edges {
		if f := m.Edges[i].Feeder; f >= 0 {
			p.EdgesBySub[f] = append(p.EdgesBySub[f], world.EdgeID(i))
		}
	}
	for i := range m.Towers {
		if f := m.Towers[i].Feeder; f >= 0 {
			p.TowersBySub[f] = append(p.TowersBySub[f], world.TowerID(i))
		}
	}
	for i := range m.Hospitals {
		if f := m.Hospitals[i].Feeder; f >= 0 {
			p.HospsBySub[f] = append(p.HospsBySub[f], world.HospitalID(i))
		}
	}
	for i := range m.POIs {
		if f := m.POIs[i].Feeder; f >= 0 {
			p.POIsBySub[f] = append(p.POIsBySub[f], m.POIs[i].ID)
		}
	}
	return p
}

// demandFactorP is the diurnal load curve in permille of connected load.
// Two peaks -- morning ramp and the 18:00-21:00 domestic peak -- which is the
// shape that makes a heatwave interesting: air conditioning lifts the whole
// curve at exactly the hour it is already highest.
func demandFactorP(hour int, w state.Weather) int32 {
	base := [...]int32{380, 350, 340, 340, 360, 430, 560, 690, 760, 780,
		790, 800, 810, 800, 790, 790, 820, 880, 950, 960,
		920, 830, 680, 500}
	f := base[hour%24]
	switch w.Condition {
	case 4: // heatwave
		f += 60 + (w.TempC-28)*22
	case 3: // snow
		f += 120
	case 2:
		f += 60
	}
	if w.TempC < 2 {
		f += (2 - w.TempC) * 14
	}
	return f
}

// PowerSystem recomputes substation loading, trips overloaded substations, and
// propagates the consequences downstream.
//
// This is where the "power cut changes traffic" chain actually lives:
//
//	substation offline
//	  -> its signals lose power        (traffic.go turns them into all-way stops)
//	  -> its street lighting goes out  (night speed and crash rate change)
//	  -> its hospitals switch to generators, and when the fuel runs out their
//	     effective capacity collapses
//	  -> its cell sites run on battery, then drop
//	  -> its load is shed onto neighbouring substations, which may in turn
//	     overload -- a cascading blackout that nobody scripted
//
// It runs serially rather than per-region. There are ~50 substations on the
// default map; partitioning them would cost more in coordination than the
// entire system costs to execute.
func PowerSystem(c *Ctx, g *Region, idx *PowerIndex) {
	m, s := c.Map, c.S
	hour, _ := s.ClockHM()
	df := demandFactorP(hour, s.Weather)
	tick := uint32(c.Tick)

	// Restore anything whose outage has expired.
	for i := range s.Subs.Online {
		if !s.Subs.Online[i] && s.Subs.RestoreAt[i] != 0 && tick >= s.Subs.RestoreAt[i] {
			s.Subs.Online[i] = true
			s.Subs.RestoreAt[i] = 0
			g.emit(c.Tick, events.EvtSubstationRestored, events.SevNotice, int64(i), 0, 0, 0)
		}
	}

	// Load, including load shed from offline neighbours.
	for i := range m.Substations {
		sub := &m.Substations[i]
		if !s.Subs.Online[i] {
			s.Subs.LoadKW[i] = 0
			s.Subs.OverTicks[i] = 0
			continue
		}
		load := units.MulP(int64(sub.BaseKW), units.Permille(df))
		for _, nb := range sub.Neighbours {
			if s.Subs.Online[nb] {
				continue
			}
			// A radial network sheds a dead feeder's load onto whichever
			// adjacent feeders can be switched in. We model that as an even
			// split across online neighbours of the dead substation.
			online := 0
			for _, nn := range m.Substations[nb].Neighbours {
				if s.Subs.Online[nn] {
					online++
				}
			}
			if online > 0 {
				load += units.MulP(int64(m.Substations[nb].BaseKW), units.Permille(df)) / int64(online)
			}
		}
		s.Subs.LoadKW[i] = int32(load)

		if int32(load) > sub.CapacityKW {
			s.Subs.OverTicks[i]++
			// 45 seconds of sustained overload before protection operates.
			if s.Subs.OverTicks[i] > 45*units.TicksPerSecond {
				tripSubstation(c, g, idx, world.SubstationID(i), int32(load), sub.CapacityKW)
			}
		} else if s.Subs.OverTicks[i] > 0 {
			s.Subs.OverTicks[i]--
		}
	}

	// Apply downstream state.
	applyPowerDownstream(c, g, idx)
}

func tripSubstation(c *Ctx, g *Region, idx *PowerIndex, id world.SubstationID, load, capacity int32) {
	s := c.S
	if !s.Subs.Online[id] {
		return
	}
	s.Subs.Online[id] = false
	s.Subs.OverTicks[id] = 0
	s.Subs.Trips[id]++
	s.Metrics.SubstationTrips++
	// Restoration takes 12-40 simulated minutes: switching plus fault finding.
	gr := rng.Derive(s.Seed, rng.StreamPower, uint64(c.Tick), uint64(id))
	s.Subs.RestoreAt[id] = uint32(c.Tick) + uint32(gr.Range(12, 40))*units.TicksPerMinute
	g.emit(c.Tick, events.EvtSubstationTripped, events.SevCritical,
		int64(id), int64(load), int64(capacity), 0)
	g.emit(c.Tick, events.EvtBlackoutDistrict, events.SevCritical,
		int64(c.Map.Substations[id].District), int64(len(idx.POIsBySub[id])), 0, 0)
}

// ForceOutage is the operator-driven equivalent, used by the chaos lab and the
// cmd.power_failure command.
func ForceOutage(c *Ctx, g *Region, idx *PowerIndex, id world.SubstationID, durationTicks int64) {
	s := c.S
	if int(id) >= len(s.Subs.Online) {
		return
	}
	s.Subs.Online[id] = false
	s.Subs.OverTicks[id] = 0
	s.Subs.Trips[id]++
	s.Metrics.SubstationTrips++
	if durationTicks <= 0 {
		durationTicks = 20 * units.TicksPerMinute
	}
	s.Subs.RestoreAt[id] = uint32(int64(c.Tick) + durationTicks)
	g.emit(c.Tick, events.EvtSubstationTripped, events.SevCritical,
		int64(id), 0, int64(c.Map.Substations[id].CapacityKW), 0)
	applyPowerDownstream(c, g, idx)
}

func applyPowerDownstream(c *Ctx, g *Region, idx *PowerIndex) {
	m, s := c.Map, c.S
	var outageUnits int64
	for i := range m.Substations {
		up := s.Subs.Online[i]
		for _, si := range idx.SignalsBySub[i] {
			if s.Signals.Powered[si] != up {
				s.Signals.Powered[si] = up
				if !up {
					g.emit(c.Tick, events.EvtSignalPowerLost, events.SevWarning,
						int64(si), int64(i), 0, 0)
				}
			}
		}
		for _, e := range idx.EdgesBySub[i] {
			s.Edges.Lit[e] = up
		}
		for _, t := range idx.TowersBySub[i] {
			if up {
				s.Towers.Powered[t] = true
				// Batteries recharge slowly once mains returns.
				if s.Towers.BatteryMin[t] < m.Towers[t].BatteryMin {
					s.Towers.BatteryMin[t]++
				}
			} else if s.Towers.BatteryMin[t] > 0 {
				if uint64(c.Tick)%units.TicksPerMinute == 0 {
					s.Towers.BatteryMin[t]--
				}
			} else if s.Towers.Powered[t] {
				s.Towers.Powered[t] = false
				g.emit(c.Tick, events.EvtTowerDown, events.SevCritical, int64(t), 0, 0, 0)
			}
		}
		for _, h := range idx.HospsBySub[i] {
			if up {
				if s.Hosps.OnBackup[h] {
					s.Hosps.OnBackup[h] = false
				}
				if s.Hosps.BackupLeft[h] < m.Hospitals[h].BackupMinutes*units.TicksPerMinute {
					s.Hosps.BackupLeft[h] += 2 // refuelling
				}
			} else {
				if !s.Hosps.OnBackup[h] {
					s.Hosps.OnBackup[h] = true
					g.emit(c.Tick, events.EvtHospitalOnBackup, events.SevWarning, int64(h), 0, 0, 0)
				}
				if s.Hosps.BackupLeft[h] > 0 {
					s.Hosps.BackupLeft[h]--
				}
			}
		}
		if !up {
			outageUnits += int64(len(idx.POIsBySub[i]))
		}
	}
	s.Metrics.OutageNodeTicks += outageUnits
	unpowered := int64(0)
	for i := range s.Signals.Powered {
		if !s.Signals.Powered[i] {
			unpowered++
		}
	}
	s.Metrics.SignalsUnpowered = unpowered
}

// CommsSystem models cell-site loading.
//
// Demand is population-driven with a large multiplier during incidents: mass
// call attempts after an accident are the single most reliable way to knock a
// cell out, and modelling it means the comms layer degrades for a reason
// instead of on a timer.
func CommsSystem(c *Ctx, g *Region) {
	m, s := c.Map, c.S
	if uint64(c.Tick)%units.TicksPerSecond != 0 {
		return // once per simulated second is plenty for a slow-moving system
	}
	hour, _ := s.ClockHM()
	base := demandFactorP(hour, s.Weather)
	surge := int32(0)
	for i := range s.Incidents {
		if !s.Incidents[i].Resolved {
			surge += 220
		}
	}
	if surge > 1400 {
		surge = 1400
	}
	for i := range m.Towers {
		if !s.Towers.Powered[i] {
			s.Towers.LoadErl[i] = 0
			continue
		}
		load := units.MulP(int64(m.Towers[i].CapacityErl)*62/100, units.Permille(base+surge))
		s.Towers.LoadErl[i] = int32(load)
		if int32(load) > m.Towers[i].CapacityErl {
			dropped := int64(int32(load) - m.Towers[i].CapacityErl)
			s.Towers.Dropped[i] += dropped
			s.Metrics.CommsDropped += dropped
			if uint64(c.Tick)%(20*units.TicksPerSecond) == 0 {
				g.emit(c.Tick, events.EvtTowerOverloaded, events.SevWarning,
					int64(i), load, int64(m.Towers[i].CapacityErl), 0)
			}
		}
	}
}
