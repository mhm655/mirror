package state

import (
	"encoding/binary"
	"errors"
	"hash/fnv"

	"github.com/mirror-sim/mirror/internal/units"
	"github.com/mirror-sim/mirror/internal/world"
)

// The codec serves two jobs with one canonical byte layout:
//
//  1. Checkpoints. A checkpoint is the encoded bytes plus a header.
//  2. State digests. The digest is FNV-1a over exactly these bytes.
//
// Deriving the digest from the checkpoint encoding rather than from a separate
// hand-written hash function removes an entire class of bug: it is impossible
// for a field to be checkpointed but not hashed, or hashed but not
// checkpointed. If a new field is added and the encoder is not updated, the
// determinism test does not catch it -- but the checkpoint round-trip test
// does, and vice versa. Together they are airtight.
//
// Values are zigzag varints. Most simulation quantities are small (edge ids,
// tick counters, permille factors) so varints cut checkpoint size roughly 3x
// versus fixed 64-bit fields, and varint encoding is exactly deterministic.

const codecVersion uint32 = 3

type enc struct{ b []byte }

func (e *enc) i(v int64)  { e.b = binary.AppendVarint(e.b, v) }
func (e *enc) u(v uint64) { e.b = binary.AppendUvarint(e.b, v) }
func (e *enc) bl(v bool) {
	if v {
		e.b = append(e.b, 1)
	} else {
		e.b = append(e.b, 0)
	}
}
func (e *enc) str(s string) {
	e.u(uint64(len(s)))
	e.b = append(e.b, s...)
}

type num interface {
	~int8 | ~int16 | ~int32 | ~int64 | ~uint8 | ~uint16 | ~uint32 | ~uint64
}

func encSlice[T num](e *enc, s []T) {
	e.u(uint64(len(s)))
	for _, v := range s {
		e.i(int64(v))
	}
}

func encBools(e *enc, s []bool) {
	e.u(uint64(len(s)))
	// Bit-packed: a 20k-edge Lit array costs 2.5KB instead of 20KB, and the
	// packing order is fixed so it stays canonical.
	var cur byte
	for i, v := range s {
		if v {
			cur |= 1 << (i % 8)
		}
		if i%8 == 7 {
			e.b = append(e.b, cur)
			cur = 0
		}
	}
	if len(s)%8 != 0 {
		e.b = append(e.b, cur)
	}
}

type dec struct {
	b   []byte
	off int
	err error
}

func (d *dec) i() int64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Varint(d.b[d.off:])
	if n <= 0 {
		d.err = errors.New("state: truncated varint")
		return 0
	}
	d.off += n
	return v
}

func (d *dec) u() uint64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.b[d.off:])
	if n <= 0 {
		d.err = errors.New("state: truncated uvarint")
		return 0
	}
	d.off += n
	return v
}

func (d *dec) bl() bool {
	if d.err != nil || d.off >= len(d.b) {
		d.err = errors.New("state: truncated bool")
		return false
	}
	v := d.b[d.off] != 0
	d.off++
	return v
}

func (d *dec) str() string {
	n := int(d.u())
	if d.err != nil || d.off+n > len(d.b) {
		d.err = errors.New("state: truncated string")
		return ""
	}
	s := string(d.b[d.off : d.off+n])
	d.off += n
	return s
}

func decSlice[T num](d *dec, dst *[]T) {
	n := int(d.u())
	if d.err != nil {
		return
	}
	if n < 0 || n > 1<<30 {
		d.err = errors.New("state: implausible slice length")
		return
	}
	s := make([]T, n)
	for i := 0; i < n; i++ {
		s[i] = T(d.i())
	}
	*dst = s
}

func decBools(d *dec, dst *[]bool) {
	n := int(d.u())
	if d.err != nil {
		return
	}
	nb := (n + 7) / 8
	if d.off+nb > len(d.b) {
		d.err = errors.New("state: truncated bool slice")
		return
	}
	s := make([]bool, n)
	for i := 0; i < n; i++ {
		s[i] = d.b[d.off+i/8]&(1<<(i%8)) != 0
	}
	d.off += nb
	*dst = s
}

// Encode serialises the whole state canonically.
func (s *State) Encode() []byte {
	e := &enc{b: make([]byte, 0, 1<<20)}
	e.u(uint64(codecVersion))
	e.u(uint64(s.Tick))
	e.u(s.Seed)
	e.u(s.MapHash)
	e.i(int64(s.StartHour))
	e.i(int64(s.SpawnCursor))
	e.i(int64(s.NextIncID))
	e.i(int64(s.RouteLive))

	// policy
	p := &s.Policy
	e.bl(p.AdaptiveSignals)
	e.i(int64(p.AdaptiveMaxExtendTicks))
	e.bl(p.EmergencyPreemption)
	e.i(int64(p.TransitExtraVehiclesP))
	e.i(int64(p.RerouteAwarenessP))
	e.i(int64(p.SpeedLimitP))
	e.bl(p.CongestionCharge)
	e.i(int64(p.AmbulanceSurgeP))
	e.str(p.Name)

	// agents
	a := &s.Agents
	encSlice(e, a.HomeNode)
	encSlice(e, a.WorkNode)
	encSlice(e, a.HomePOI)
	encSlice(e, a.WorkPOI)
	encSlice(e, a.District)
	encSlice(e, a.DepartOut)
	encSlice(e, a.DepartRet)
	encSlice(e, a.Mode)
	encSlice(e, a.Status)
	encSlice(e, a.Vehicle)
	encSlice(e, a.TransitRide)
	encSlice(e, a.Health)
	encSlice(e, a.PatienceP)
	encSlice(e, a.RiskP)
	encSlice(e, a.TripStart)
	encSlice(e, a.TripsDone)
	encSlice(e, a.LastTravel)
	encSlice(e, a.WaitingTicks)
	encSlice(e, a.CurrentTarget)
	encSlice(e, a.TRoute)
	encSlice(e, a.TBoard)
	encSlice(e, a.TAlight)
	encSlice(e, a.WalkTicks)
	encSlice(e, a.FreeRefTicks)
	encSlice(e, a.NextInList)

	// vehicles
	v := &s.Vehicles
	encSlice(e, v.Kind)
	encSlice(e, v.Status)
	encSlice(e, v.Agent)
	encSlice(e, v.Edge)
	encSlice(e, v.Pos)
	encSlice(e, v.Speed)
	encSlice(e, v.RouteStart)
	encSlice(e, v.RouteLen)
	encSlice(e, v.RouteIdx)
	encSlice(e, v.Dest)
	encSlice(e, v.SpawnTick)
	encSlice(e, v.StopTicks)
	encSlice(e, v.DistanceMM)
	encSlice(e, v.ReplanAt)
	encSlice(e, v.Payload)
	encSlice(e, v.TransitRoute)
	encSlice(e, v.TransitLeg)
	encSlice(e, v.Occupancy)
	encSlice(e, v.RiderHead)
	encSlice(e, v.DwellTicks)
	encSlice(e, v.TransitDir)
	encSlice(e, v.free)

	encSlice(e, s.RouteBuf)
	encSlice(e, s.StopHead)
	encSlice(e, s.NextDepart)

	// edges
	ed := &s.Edges
	encSlice(e, ed.Count)
	encSlice(e, ed.BlockedLanes)
	encSlice(e, ed.ClosedUntil)
	encSlice(e, ed.Speed)
	encSlice(e, ed.TravelTicks)
	encBools(e, ed.Lit)
	encSlice(e, ed.EnteredTotal)

	// signals
	sg := &s.Signals
	encSlice(e, sg.Phase)
	encSlice(e, sg.PhaseTick)
	encBools(e, sg.Powered)
	encSlice(e, sg.Preempt)
	encSlice(e, sg.Queue0)
	encSlice(e, sg.Queue1)
	encSlice(e, sg.Green0)
	encSlice(e, sg.Green1)

	// substations
	sb := &s.Subs
	encBools(e, sb.Online)
	encSlice(e, sb.LoadKW)
	encSlice(e, sb.OverTicks)
	encSlice(e, sb.RestoreAt)
	encSlice(e, sb.Trips)

	// hospitals
	h := &s.Hosps
	encSlice(e, h.BedsUsed)
	encSlice(e, h.ERUsed)
	encSlice(e, h.Waiting)
	encSlice(e, h.AmbAvail)
	encBools(e, h.OnBackup)
	encSlice(e, h.BackupLeft)
	encSlice(e, h.Admissions)
	encSlice(e, h.Rejections)
	encSlice(e, h.Diverted)

	// towers
	t := &s.Towers
	encBools(e, t.Powered)
	encSlice(e, t.LoadErl)
	encSlice(e, t.BatteryMin)
	encSlice(e, t.Dropped)

	// depots
	encSlice(e, s.Depots.Available)
	encSlice(e, s.Depots.Dispatched)

	// weather
	e.i(int64(s.Weather.Condition))
	e.i(int64(s.Weather.TempC))
	e.i(int64(s.Weather.WindKph))
	e.i(int64(s.Weather.VisibilityM))
	e.u(uint64(s.Weather.UntilTick))

	// incidents
	e.u(uint64(len(s.Incidents)))
	for i := range s.Incidents {
		in := &s.Incidents[i]
		e.i(in.ID)
		e.i(int64(in.Kind))
		e.i(int64(in.Edge))
		e.i(int64(in.Node))
		e.i(int64(in.District))
		e.u(uint64(in.StartTick))
		e.u(uint64(in.EndTick))
		e.i(int64(in.Severity))
		e.i(int64(in.Casualties))
		e.i(int64(in.NeedAmbulance))
		e.i(int64(in.NeedFire))
		e.i(int64(in.NeedPolice))
		e.u(uint64(in.FirstResponseTick))
		e.u(uint64(in.NextConsiderTick))
		e.bl(in.Resolved)
	}

	encMetrics(e, &s.Metrics)
	return e.b
}

func encHist(e *enc, h *Histogram) {
	for i := range h.Counts {
		e.i(h.Counts[i])
	}
	e.i(h.Total)
	e.i(h.Sum)
	e.i(h.Max)
}

func decHist(d *dec, h *Histogram) {
	for i := range h.Counts {
		h.Counts[i] = d.i()
	}
	h.Total, h.Sum, h.Max = d.i(), d.i(), d.i()
}

func encSeries(e *enc, s *Series) {
	e.i(int64(s.Head))
	e.i(int64(s.Count))
	for i := int32(0); i < s.Count; i++ {
		idx := (s.Head - s.Count + i + SeriesLen) % SeriesLen
		e.i(int64(s.Data[idx]))
	}
}

func decSeries(d *dec, s *Series) {
	s.Head = int32(d.i())
	s.Count = int32(d.i())
	if s.Count < 0 || s.Count > SeriesLen {
		d.err = errors.New("state: bad series count")
		return
	}
	for i := int32(0); i < s.Count; i++ {
		idx := (s.Head - s.Count + i + SeriesLen) % SeriesLen
		s.Data[idx] = int32(d.i())
	}
}

func encMetrics(e *enc, m *Metrics) {
	for _, v := range []int64{
		m.TripsStarted, m.TripsCompleted, m.TripsAbandoned,
		m.VehicleTicks, m.StoppedVehicleTicks, m.DistanceMM, m.FuelMl, m.CO2G,
		m.EmergencyDispatched, m.EmergencyArrived, m.IncidentsOpened,
		m.IncidentsResolved, m.Casualties, m.HospitalAdmissions,
		m.HospitalRejections, m.HospitalDiversions, m.OutageNodeTicks,
		m.SubstationTrips, m.SignalsUnpowered, m.CommsDropped,
		m.TransitBoardings, m.TransitDenied, m.RouteQueries, m.RouteFailures,
		m.Reroutes, m.LastSeriesMinute,
	} {
		e.i(v)
	}
	e.i(int64(m.PeakHospitalUtilP))
	encHist(e, &m.Travel)
	encHist(e, &m.Delay)
	encHist(e, &m.EmergencyResponse)
	encSeries(e, &m.SeriesActiveVehicles)
	encSeries(e, &m.SeriesAvgSpeedKph)
	encSeries(e, &m.SeriesCongestionP)
	encSeries(e, &m.SeriesHospitalUtilP)
	encSeries(e, &m.SeriesPoweredP)
	encSeries(e, &m.SeriesOpenIncidents)
}

func decMetrics(d *dec, m *Metrics) {
	ptrs := []*int64{
		&m.TripsStarted, &m.TripsCompleted, &m.TripsAbandoned,
		&m.VehicleTicks, &m.StoppedVehicleTicks, &m.DistanceMM, &m.FuelMl, &m.CO2G,
		&m.EmergencyDispatched, &m.EmergencyArrived, &m.IncidentsOpened,
		&m.IncidentsResolved, &m.Casualties, &m.HospitalAdmissions,
		&m.HospitalRejections, &m.HospitalDiversions, &m.OutageNodeTicks,
		&m.SubstationTrips, &m.SignalsUnpowered, &m.CommsDropped,
		&m.TransitBoardings, &m.TransitDenied, &m.RouteQueries, &m.RouteFailures,
		&m.Reroutes, &m.LastSeriesMinute,
	}
	for _, p := range ptrs {
		*p = d.i()
	}
	m.PeakHospitalUtilP = int32(d.i())
	decHist(d, &m.Travel)
	decHist(d, &m.Delay)
	decHist(d, &m.EmergencyResponse)
	decSeries(d, &m.SeriesActiveVehicles)
	decSeries(d, &m.SeriesAvgSpeedKph)
	decSeries(d, &m.SeriesCongestionP)
	decSeries(d, &m.SeriesHospitalUtilP)
	decSeries(d, &m.SeriesPoweredP)
	decSeries(d, &m.SeriesOpenIncidents)
}

// Decode reconstructs a state. It does NOT restore the map; the caller must
// supply a map whose Hash equals State.MapHash (Engine.Restore enforces this).
func Decode(b []byte) (*State, error) {
	d := &dec{b: b}
	if v := uint32(d.u()); v != codecVersion {
		return nil, errors.New("state: checkpoint version mismatch")
	}
	s := &State{}
	s.Tick = units.Tick(d.u())
	s.Seed = d.u()
	s.MapHash = d.u()
	s.StartHour = int32(d.i())
	s.SpawnCursor = int32(d.i())
	s.NextIncID = d.i()
	s.RouteLive = int32(d.i())

	p := &s.Policy
	p.AdaptiveSignals = d.bl()
	p.AdaptiveMaxExtendTicks = int32(d.i())
	p.EmergencyPreemption = d.bl()
	p.TransitExtraVehiclesP = int32(d.i())
	p.RerouteAwarenessP = int32(d.i())
	p.SpeedLimitP = int32(d.i())
	p.CongestionCharge = d.bl()
	p.AmbulanceSurgeP = int32(d.i())
	p.Name = d.str()

	a := &s.Agents
	decSlice(d, &a.HomeNode)
	decSlice(d, &a.WorkNode)
	decSlice(d, &a.HomePOI)
	decSlice(d, &a.WorkPOI)
	decSlice(d, &a.District)
	decSlice(d, &a.DepartOut)
	decSlice(d, &a.DepartRet)
	decSlice(d, &a.Mode)
	decSlice(d, &a.Status)
	decSlice(d, &a.Vehicle)
	decSlice(d, &a.TransitRide)
	decSlice(d, &a.Health)
	decSlice(d, &a.PatienceP)
	decSlice(d, &a.RiskP)
	decSlice(d, &a.TripStart)
	decSlice(d, &a.TripsDone)
	decSlice(d, &a.LastTravel)
	decSlice(d, &a.WaitingTicks)
	decSlice(d, &a.CurrentTarget)
	decSlice(d, &a.TRoute)
	decSlice(d, &a.TBoard)
	decSlice(d, &a.TAlight)
	decSlice(d, &a.WalkTicks)
	decSlice(d, &a.FreeRefTicks)
	decSlice(d, &a.NextInList)

	v := &s.Vehicles
	decSlice(d, &v.Kind)
	decSlice(d, &v.Status)
	decSlice(d, &v.Agent)
	decSlice(d, &v.Edge)
	decSlice(d, &v.Pos)
	decSlice(d, &v.Speed)
	decSlice(d, &v.RouteStart)
	decSlice(d, &v.RouteLen)
	decSlice(d, &v.RouteIdx)
	decSlice(d, &v.Dest)
	decSlice(d, &v.SpawnTick)
	decSlice(d, &v.StopTicks)
	decSlice(d, &v.DistanceMM)
	decSlice(d, &v.ReplanAt)
	decSlice(d, &v.Payload)
	decSlice(d, &v.TransitRoute)
	decSlice(d, &v.TransitLeg)
	decSlice(d, &v.Occupancy)
	decSlice(d, &v.RiderHead)
	decSlice(d, &v.DwellTicks)
	decSlice(d, &v.TransitDir)
	decSlice(d, &v.free)

	decSlice(d, &s.RouteBuf)
	decSlice(d, &s.StopHead)
	decSlice(d, &s.NextDepart)

	ed := &s.Edges
	decSlice(d, &ed.Count)
	decSlice(d, &ed.BlockedLanes)
	decSlice(d, &ed.ClosedUntil)
	decSlice(d, &ed.Speed)
	decSlice(d, &ed.TravelTicks)
	decBools(d, &ed.Lit)
	decSlice(d, &ed.EnteredTotal)

	sg := &s.Signals
	decSlice(d, &sg.Phase)
	decSlice(d, &sg.PhaseTick)
	decBools(d, &sg.Powered)
	decSlice(d, &sg.Preempt)
	decSlice(d, &sg.Queue0)
	decSlice(d, &sg.Queue1)
	decSlice(d, &sg.Green0)
	decSlice(d, &sg.Green1)

	sb := &s.Subs
	decBools(d, &sb.Online)
	decSlice(d, &sb.LoadKW)
	decSlice(d, &sb.OverTicks)
	decSlice(d, &sb.RestoreAt)
	decSlice(d, &sb.Trips)

	h := &s.Hosps
	decSlice(d, &h.BedsUsed)
	decSlice(d, &h.ERUsed)
	decSlice(d, &h.Waiting)
	decSlice(d, &h.AmbAvail)
	decBools(d, &h.OnBackup)
	decSlice(d, &h.BackupLeft)
	decSlice(d, &h.Admissions)
	decSlice(d, &h.Rejections)
	decSlice(d, &h.Diverted)

	t := &s.Towers
	decBools(d, &t.Powered)
	decSlice(d, &t.LoadErl)
	decSlice(d, &t.BatteryMin)
	decSlice(d, &t.Dropped)

	decSlice(d, &s.Depots.Available)
	decSlice(d, &s.Depots.Dispatched)

	s.Weather.Condition = int32(d.i())
	s.Weather.TempC = int32(d.i())
	s.Weather.WindKph = int32(d.i())
	s.Weather.VisibilityM = int32(d.i())
	s.Weather.UntilTick = uint32(d.u())

	n := int(d.u())
	if d.err != nil || n < 0 || n > 1<<24 {
		return nil, errors.New("state: bad incident count")
	}
	s.Incidents = make([]Incident, n)
	for i := 0; i < n; i++ {
		in := &s.Incidents[i]
		in.ID = d.i()
		in.Kind = uint8(d.i())
		in.Edge = world.EdgeID(d.i())
		in.Node = world.NodeID(d.i())
		in.District = world.DistrictID(d.i())
		in.StartTick = uint32(d.u())
		in.EndTick = uint32(d.u())
		in.Severity = int32(d.i())
		in.Casualties = int32(d.i())
		in.NeedAmbulance = int32(d.i())
		in.NeedFire = int32(d.i())
		in.NeedPolice = int32(d.i())
		in.FirstResponseTick = uint32(d.u())
		in.NextConsiderTick = uint32(d.u())
		in.Resolved = d.bl()
	}

	decMetrics(d, &s.Metrics)
	if d.err != nil {
		return nil, d.err
	}
	return s, nil
}

// Digest is the canonical fingerprint of a simulation state.
//
// Two runs are identical iff their digests match at every tick. The
// determinism test suite compares digests rather than states because a digest
// mismatch localises to a tick, and because comparing 60MB of state per tick
// would make the test unusably slow.
func (s *State) Digest() uint64 {
	h := fnv.New64a()
	h.Write(s.Encode())
	return h.Sum64()
}

// Clone deep-copies the state. The map is NOT copied -- it is immutable and
// shared. This is the whole counterfactual story: forking a 100k-agent
// scenario copies ~30MB of dynamic state and zero bytes of the ~40MB world.
func (s *State) Clone() *State {
	n := *s // copies scalars, Policy, Weather, Metrics (arrays are values)
	a := &s.Agents
	n.Agents = Agents{
		HomeNode: cp(a.HomeNode), WorkNode: cp(a.WorkNode),
		HomePOI: cp(a.HomePOI), WorkPOI: cp(a.WorkPOI), District: cp(a.District),
		DepartOut: cp(a.DepartOut), DepartRet: cp(a.DepartRet),
		Mode: cp(a.Mode), Status: cp(a.Status), Vehicle: cp(a.Vehicle),
		TransitRide: cp(a.TransitRide), Health: cp(a.Health),
		PatienceP: cp(a.PatienceP), RiskP: cp(a.RiskP),
		TripStart: cp(a.TripStart), TripsDone: cp(a.TripsDone),
		LastTravel: cp(a.LastTravel), WaitingTicks: cp(a.WaitingTicks),
		CurrentTarget: cp(a.CurrentTarget), TRoute: cp(a.TRoute),
		TBoard: cp(a.TBoard), TAlight: cp(a.TAlight),
		WalkTicks: cp(a.WalkTicks), FreeRefTicks: cp(a.FreeRefTicks),
		NextInList: cp(a.NextInList),
	}
	v := &s.Vehicles
	n.Vehicles = Vehicles{
		Kind: cp(v.Kind), Status: cp(v.Status), Agent: cp(v.Agent),
		Edge: cp(v.Edge), Pos: cp(v.Pos), Speed: cp(v.Speed),
		RouteStart: cp(v.RouteStart), RouteLen: cp(v.RouteLen), RouteIdx: cp(v.RouteIdx),
		Dest: cp(v.Dest), SpawnTick: cp(v.SpawnTick), StopTicks: cp(v.StopTicks),
		DistanceMM: cp(v.DistanceMM), ReplanAt: cp(v.ReplanAt), Payload: cp(v.Payload),
		TransitRoute: cp(v.TransitRoute), TransitLeg: cp(v.TransitLeg),
		Occupancy: cp(v.Occupancy), RiderHead: cp(v.RiderHead),
		DwellTicks: cp(v.DwellTicks), TransitDir: cp(v.TransitDir),
		free: cp(v.free),
	}
	e := &s.Edges
	n.Edges = EdgeState{
		Count: cp(e.Count), BlockedLanes: cp(e.BlockedLanes),
		ClosedUntil: cp(e.ClosedUntil), Speed: cp(e.Speed),
		TravelTicks: cp(e.TravelTicks), Lit: cp(e.Lit), EnteredTotal: cp(e.EnteredTotal),
	}
	g := &s.Signals
	n.Signals = SignalState{
		Phase: cp(g.Phase), PhaseTick: cp(g.PhaseTick), Powered: cp(g.Powered),
		Preempt: cp(g.Preempt), Queue0: cp(g.Queue0), Queue1: cp(g.Queue1),
		Green0: cp(g.Green0), Green1: cp(g.Green1),
	}
	b := &s.Subs
	n.Subs = SubstationState{
		Online: cp(b.Online), LoadKW: cp(b.LoadKW), OverTicks: cp(b.OverTicks),
		RestoreAt: cp(b.RestoreAt), Trips: cp(b.Trips),
	}
	hh := &s.Hosps
	n.Hosps = HospitalState{
		BedsUsed: cp(hh.BedsUsed), ERUsed: cp(hh.ERUsed), Waiting: cp(hh.Waiting),
		AmbAvail: cp(hh.AmbAvail), OnBackup: cp(hh.OnBackup), BackupLeft: cp(hh.BackupLeft),
		Admissions: cp(hh.Admissions), Rejections: cp(hh.Rejections), Diverted: cp(hh.Diverted),
	}
	t := &s.Towers
	n.Towers = TowerState{
		Powered: cp(t.Powered), LoadErl: cp(t.LoadErl),
		BatteryMin: cp(t.BatteryMin), Dropped: cp(t.Dropped),
	}
	n.Depots = DepotState{Available: cp(s.Depots.Available), Dispatched: cp(s.Depots.Dispatched)}
	n.Incidents = cp(s.Incidents)
	n.RouteBuf = cp(s.RouteBuf)
	n.StopHead = cp(s.StopHead)
	n.NextDepart = cp(s.NextDepart)
	return &n
}

func cp[T any](s []T) []T {
	if s == nil {
		return nil
	}
	d := make([]T, len(s))
	copy(d, s)
	return d
}
