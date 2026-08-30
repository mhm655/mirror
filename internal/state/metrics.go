package state

import "github.com/mirror-sim/mirror/internal/units"

// HistBuckets covers 0..(HistBuckets*HistWidth) ticks; anything above lands in
// the last bucket. 256 buckets x 120 ticks = 51 simulated minutes, which spans
// every plausible urban commute including a badly congested one.
const (
	HistBuckets = 256
	HistWidth   = 120
)

// Histogram is a fixed-width integer histogram.
//
// Percentiles are computed from this rather than from a stored sample vector
// because a scenario running for a simulated week produces tens of millions of
// trips: keeping every sample would dominate both memory and checkpoint size,
// and sorting them would be the slowest thing in the process. The cost is
// bucket-resolution error (+/- 6 simulated seconds), which is far below the
// noise floor of the underlying traffic model.
type Histogram struct {
	Counts [HistBuckets]int64
	Total  int64
	Sum    int64
	Max    int64
}

func (h *Histogram) Observe(v int64) {
	if v < 0 {
		v = 0
	}
	b := v / HistWidth
	if b >= HistBuckets {
		b = HistBuckets - 1
	}
	h.Counts[b]++
	h.Total++
	h.Sum += v
	if v > h.Max {
		h.Max = v
	}
}

// Quantile returns the value at the given permille rank (500 = P50).
// Linear interpolation inside the bucket keeps the answer stable as counts
// grow, which matters when the UI polls this every second.
func (h *Histogram) Quantile(pMille int64) int64 {
	if h.Total == 0 {
		return 0
	}
	target := (h.Total*pMille + 999) / 1000
	if target < 1 {
		target = 1
	}
	var cum int64
	for i := 0; i < HistBuckets; i++ {
		c := h.Counts[i]
		if cum+c >= target {
			within := target - cum
			if c <= 0 {
				return int64(i) * HistWidth
			}
			return int64(i)*HistWidth + (within*HistWidth)/c
		}
		cum += c
	}
	return (HistBuckets - 1) * HistWidth
}

func (h *Histogram) Mean() int64 {
	if h.Total == 0 {
		return 0
	}
	return h.Sum / h.Total
}

// SeriesLen is the number of retained per-minute samples (24 simulated hours).
const SeriesLen = 1440

// Series is a fixed-capacity ring of per-simulated-minute samples. Bounded so
// that a long run cannot grow state without limit -- an unbounded time series
// inside forkable state would make every fork progressively more expensive.
type Series struct {
	Data  [SeriesLen]int32
	Head  int32
	Count int32
}

func (s *Series) Push(v int32) {
	s.Data[s.Head] = v
	s.Head = (s.Head + 1) % SeriesLen
	if s.Count < SeriesLen {
		s.Count++
	}
}

// Snapshot returns the samples oldest-first.
func (s *Series) Snapshot(out []int32) []int32 {
	out = out[:0]
	n := s.Count
	start := (s.Head - n + SeriesLen) % SeriesLen
	for i := int32(0); i < n; i++ {
		out = append(out, s.Data[(start+i)%SeriesLen])
	}
	return out
}

// Metrics are simulation outputs. They live inside State on purpose: a forked
// scenario must inherit its parent's metrics up to the branch point, and a
// replayed run must reproduce them exactly. Metrics computed outside state
// would silently break both properties.
type Metrics struct {
	TripsStarted   int64
	TripsCompleted int64
	TripsAbandoned int64

	// TravelTicks over completed trips.
	Travel Histogram
	// Delay = actual - free-flow travel time.
	Delay Histogram
	// EmergencyResponse from dispatch to on-scene arrival.
	EmergencyResponse Histogram

	VehicleTicks        int64
	StoppedVehicleTicks int64
	DistanceMM          int64
	// FuelMl and CO2G derived from distance and idling with fixed integer
	// coefficients (see systems.fuel).
	FuelMl int64
	CO2G   int64

	EmergencyDispatched int64
	EmergencyArrived    int64
	IncidentsOpened     int64
	IncidentsResolved   int64
	Casualties          int64

	HospitalAdmissions int64
	HospitalRejections int64
	HospitalDiversions int64
	PeakHospitalUtilP  int32

	OutageNodeTicks  int64
	SubstationTrips  int64
	SignalsUnpowered int64
	CommsDropped     int64

	TransitBoardings int64
	TransitDenied    int64

	RouteQueries  int64
	RouteFailures int64
	Reroutes      int64

	// Time series, one sample per simulated minute.
	SeriesActiveVehicles Series
	SeriesAvgSpeedKph    Series
	SeriesCongestionP    Series
	SeriesHospitalUtilP  Series
	SeriesPoweredP       Series
	SeriesOpenIncidents  Series
	// LastSeriesMinute guards one push per simulated minute.
	LastSeriesMinute int64
}

func (m *Metrics) init() { m.LastSeriesMinute = -1 }

// MaybeSample pushes one sample per simulated minute. Driven by tick count,
// never by wall clock, so a run at 100x speed produces exactly the same series
// as a run at 1x.
func (m *Metrics) MaybeSample(tick units.Tick, activeVeh, avgKph, congP, hospP, poweredP, openInc int32) bool {
	minute := int64(tick) / units.TicksPerMinute
	if minute == m.LastSeriesMinute {
		return false
	}
	m.LastSeriesMinute = minute
	m.SeriesActiveVehicles.Push(activeVeh)
	m.SeriesAvgSpeedKph.Push(avgKph)
	m.SeriesCongestionP.Push(congP)
	m.SeriesHospitalUtilP.Push(hospP)
	m.SeriesPoweredP.Push(poweredP)
	m.SeriesOpenIncidents.Push(openInc)
	return true
}
