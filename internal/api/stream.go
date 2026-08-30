package api

import (
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/mirror-sim/mirror/internal/engine"
	"github.com/mirror-sim/mirror/internal/events"
	"github.com/mirror-sim/mirror/internal/simctl"
	"github.com/mirror-sim/mirror/internal/state"
	"github.com/mirror-sim/mirror/internal/units"
	"github.com/mirror-sim/mirror/internal/world"
)

// The live wire protocol.
//
// Three frame types, at three different rates, for three different reasons:
//
//	FrameVehicles (binary, 8 Hz)   viewport-culled positions. This is the only
//	                              high-rate payload, so it is the only one that
//	                              gets a hand-packed binary encoding.
//	FrameNetwork  (binary, 2 Hz)   a dense byte per edge carrying its speed as a
//	                              fraction of free flow, plus a bit per signal.
//	                              Dense because the client already holds the
//	                              static map, so no ids need to travel -- 40k
//	                              edges cost 40 KB instead of the 200 KB an
//	                              id-value stream would.
//	FrameJSON     (text, 2 Hz)     the snapshot, metrics and event feed, where
//	                              readability is worth more than bytes.
//
// Why viewport culling rather than sending everything: at 60k active vehicles a
// full positional frame is ~600 KB, which at 8 Hz is 4.8 MB/s per client. The
// client cannot draw 60k distinct dots on a 1600px canvas anyway. Culling to
// the viewport and capping the count bounds the frame at ~40 KB and loses
// nothing a human could have seen.
const (
	FrameVehicles uint8 = 1
	FrameNetwork  uint8 = 2

	streamMagic uint16 = 0x4D57 // "MW"
	streamVer   uint16 = 3
)

// Viewport is the client's current camera, in world millimetres.
type Viewport struct {
	X0, Y0, X1, Y1 units.MM
	MaxVehicles    int
}

func (v Viewport) valid() bool { return v.X1 > v.X0 && v.Y1 > v.Y0 }

// clientMsg is the JSON control channel from browser to server.
type clientMsg struct {
	Type        string  `json:"type"`
	Sim         string  `json:"sim,omitempty"`
	X0          float64 `json:"x0,omitempty"`
	Y0          float64 `json:"y0,omitempty"`
	X1          float64 `json:"x1,omitempty"`
	Y1          float64 `json:"y1,omitempty"`
	MaxVehicles int     `json:"maxVehicles,omitempty"`
	FromSeq     uint64  `json:"fromSeq,omitempty"`
}

type serverMsg struct {
	Type      string                `json:"type"`
	Sim       string                `json:"sim,omitempty"`
	Snapshot  *simctl.Snapshot      `json:"snapshot,omitempty"`
	Metrics   *simctl.Metrics       `json:"metrics,omitempty"`
	Series    *simctl.Series        `json:"series,omitempty"`
	Events    []EventView           `json:"events,omitempty"`
	Districts []simctl.DistrictStat `json:"districts,omitempty"`
	NextSeq   uint64                `json:"nextSeq,omitempty"`
	Missed    uint64                `json:"missed,omitempty"`
	Error     string                `json:"error,omitempty"`
}

// EventView is one effect event rendered for the UI.
type EventView struct {
	Seq      uint64 `json:"seq"`
	Tick     uint64 `json:"tick"`
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Region   int16  `json:"region"`
	Text     string `json:"text"`
	A        int64  `json:"a"`
	B        int64  `json:"b"`
}

// streamClient is one connected browser.
type streamClient struct {
	conn *Conn
	srv  *Server

	mu       sync.Mutex
	simID    string
	viewport Viewport
	eventSeq uint64

	vehBuf []byte
	netBuf []byte
}

func (s *Server) handleStream(c *Conn, initialSim string) {
	cl := &streamClient{
		conn: c, srv: s, simID: initialSim,
		vehBuf: make([]byte, 0, 128*1024),
		netBuf: make([]byte, 0, 128*1024),
	}
	defer c.Close()

	// Reader goroutine: control messages and liveness.
	go func() {
		for {
			_ = c.SetReadDeadline(time.Now().Add(90 * time.Second))
			msg, err := c.ReadMessage()
			if err != nil {
				c.Close()
				return
			}
			if msg.Binary {
				continue
			}
			var m clientMsg
			if err := json.Unmarshal(msg.Data, &m); err != nil {
				continue
			}
			cl.mu.Lock()
			switch m.Type {
			case "subscribe":
				if m.Sim != "" {
					cl.simID = m.Sim
					cl.eventSeq = 0
				}
			case "viewport":
				cl.viewport = Viewport{
					X0: units.MM(m.X0), Y0: units.MM(m.Y0),
					X1: units.MM(m.X1), Y1: units.MM(m.Y1),
					MaxVehicles: m.MaxVehicles,
				}
			case "events":
				cl.eventSeq = m.FromSeq
			}
			cl.mu.Unlock()
		}
	}()

	vehTick := time.NewTicker(125 * time.Millisecond)
	slowTick := time.NewTicker(500 * time.Millisecond)
	pingTick := time.NewTicker(30 * time.Second)
	defer vehTick.Stop()
	defer slowTick.Stop()
	defer pingTick.Stop()

	for {
		select {
		case <-c.Done():
			return
		case <-s.done:
			return
		case <-pingTick.C:
			if err := c.Ping(); err != nil {
				return
			}
		case <-vehTick.C:
			if err := cl.sendVehicles(); err != nil {
				return
			}
		case <-slowTick.C:
			if err := cl.sendNetwork(); err != nil {
				return
			}
			if err := cl.sendJSON(); err != nil {
				return
			}
		}
	}
}

func (cl *streamClient) sim() (*simctl.Sim, Viewport, bool) {
	cl.mu.Lock()
	id, vp := cl.simID, cl.viewport
	cl.mu.Unlock()
	if id == "" {
		return nil, vp, false
	}
	s, ok := cl.srv.mgr.Get(id)
	return s, vp, ok
}

// sendVehicles packs the viewport-culled vehicle frame.
//
// Layout, all little-endian:
//
//	u16 magic | u16 version | u8 frameType | u8 flags | u16 reserved
//	u64 tick  | i32 vx0 | i32 vy0 | i32 vx1 | i32 vy1
//	u32 total (before culling) | u32 sent
//	then `sent` records of 10 bytes:
//	  u32 id | u8 kindAndStatus | u8 speedKph | u16 nx | u16 ny
//
// nx/ny are the position normalised into the viewport as 16-bit fixed point.
// The client multiplies by its pixel width, which means the wire cost of a
// vehicle is independent of how large the city is -- a 16km map streams at the
// same byte rate as a 4km one.
func (cl *streamClient) sendVehicles() error {
	s, vp, ok := cl.sim()
	if !ok {
		return nil
	}
	buf := cl.vehBuf[:0]
	var hdr [40]byte
	le := binary.LittleEndian

	s.Read(func(e *engine.Engine) {
		m, st := e.Map, e.S
		if !vp.valid() {
			vp = Viewport{X0: 0, Y0: 0, X1: m.Width, Y1: m.Height}
		}
		maxV := vp.MaxVehicles
		if maxV <= 0 || maxV > 20000 {
			maxV = 6000
		}
		spanX := int64(vp.X1 - vp.X0)
		spanY := int64(vp.Y1 - vp.Y0)
		if spanX <= 0 {
			spanX = 1
		}
		if spanY <= 0 {
			spanY = 1
		}

		// First pass counts candidates so the sampling stride can be chosen
		// without a second allocation.
		total := 0
		for i := range st.Vehicles.Status {
			if st.Vehicles.Status[i] == state.VehIdle {
				continue
			}
			total++
		}
		stride := 1
		if total > maxV {
			stride = (total + maxV - 1) / maxV
		}

		le.PutUint16(hdr[0:], streamMagic)
		le.PutUint16(hdr[2:], streamVer)
		hdr[4] = FrameVehicles
		hdr[5] = 0
		le.PutUint16(hdr[6:], 0)
		le.PutUint64(hdr[8:], uint64(st.Tick))
		le.PutUint32(hdr[16:], uint32(int32(vp.X0)))
		le.PutUint32(hdr[20:], uint32(int32(vp.Y0)))
		le.PutUint32(hdr[24:], uint32(int32(vp.X1)))
		le.PutUint32(hdr[28:], uint32(int32(vp.Y1)))
		le.PutUint32(hdr[32:], uint32(total))
		buf = append(buf, hdr[:]...)

		sent := 0
		var rec [10]byte
		for i := range st.Vehicles.Status {
			if st.Vehicles.Status[i] == state.VehIdle {
				continue
			}
			// Emergency and transit vehicles are never sampled out: they are
			// what an operator is actually watching, and there are never many.
			kind := st.Vehicles.Kind[i]
			important := kind.IsEmergency() || kind == state.VehBus || kind == state.VehMetro
			if !important && stride > 1 && i%stride != 0 {
				continue
			}
			ed := st.Vehicles.Edge[i]
			if ed < 0 {
				continue
			}
			x, y := vehiclePos(m, st, int32(i))
			if x < vp.X0 || x > vp.X1 || y < vp.Y0 || y > vp.Y1 {
				continue
			}
			if sent >= maxV+512 {
				break
			}
			nx := uint16(int64(x-vp.X0) * 65535 / spanX)
			ny := uint16(int64(y-vp.Y0) * 65535 / spanY)
			kph := units.MMPerTickToKmh(st.Vehicles.Speed[i])
			if kph > 255 {
				kph = 255
			}
			if kph < 0 {
				kph = 0
			}
			le.PutUint32(rec[0:], uint32(i))
			rec[4] = byte(kind)&0x0F | byte(st.Vehicles.Status[i])<<4
			rec[5] = byte(kph)
			le.PutUint16(rec[6:], nx)
			le.PutUint16(rec[8:], ny)
			buf = append(buf, rec[:]...)
			sent++
		}
		le.PutUint32(buf[36:], uint32(sent))
	})
	cl.vehBuf = buf
	return cl.conn.WriteBinary(buf)
}

// vehiclePos interpolates a vehicle's position along its current edge.
func vehiclePos(m *world.Map, st *state.State, id int32) (units.MM, units.MM) {
	e := st.Vehicles.Edge[id]
	ed := &m.Edges[e]
	a, b := &m.Nodes[ed.From], &m.Nodes[ed.To]
	if ed.Length <= 0 {
		return a.X, a.Y
	}
	p := int64(st.Vehicles.Pos[id])
	if p > int64(ed.Length) {
		p = int64(ed.Length)
	}
	x := int64(a.X) + (int64(b.X-a.X)*p)/int64(ed.Length)
	y := int64(a.Y) + (int64(b.Y-a.Y)*p)/int64(ed.Length)
	return units.MM(x), units.MM(y)
}

// sendNetwork packs the dense congestion and signal frame.
//
//	u16 magic | u16 version | u8 frameType | u8 flags | u16 reserved
//	u64 tick | u32 edgeCount | u32 signalCount
//	edgeCount bytes: 0..200 = speed as a percentage of free flow,
//	                 254 = closed, 255 = blocked by an incident
//	ceil(signalCount/4) bytes: 2 bits per signal (phase, powered)
func (cl *streamClient) sendNetwork() error {
	s, _, ok := cl.sim()
	if !ok {
		return nil
	}
	buf := cl.netBuf[:0]
	var hdr [24]byte
	le := binary.LittleEndian

	s.Read(func(e *engine.Engine) {
		m, st := e.Map, e.S
		le.PutUint16(hdr[0:], streamMagic)
		le.PutUint16(hdr[2:], streamVer)
		hdr[4] = FrameNetwork
		le.PutUint64(hdr[8:], uint64(st.Tick))
		le.PutUint32(hdr[16:], uint32(len(m.Edges)))
		le.PutUint32(hdr[20:], uint32(len(m.Signals)))
		buf = append(buf, hdr[:]...)

		for i := range m.Edges {
			var v byte
			switch {
			case st.Edges.ClosedUntil[i] > uint32(st.Tick):
				v = 254
			case st.Edges.BlockedLanes[i] > 0:
				v = 255
			default:
				fs := m.Edges[i].FreeSpeed
				if fs <= 0 {
					v = 100
				} else {
					r := int64(st.Edges.Speed[i]) * 100 / int64(fs)
					if r > 200 {
						r = 200
					}
					if r < 0 {
						r = 0
					}
					v = byte(r)
				}
			}
			buf = append(buf, v)
		}
		packed := make([]byte, (len(m.Signals)+3)/4)
		for i := range m.Signals {
			var bits byte
			if st.Signals.Phase[i] != 0 {
				bits |= 1
			}
			if st.Signals.Powered[i] {
				bits |= 2
			}
			packed[i/4] |= bits << ((i % 4) * 2)
		}
		buf = append(buf, packed...)
	})
	cl.netBuf = buf
	return cl.conn.WriteBinary(buf)
}

func (cl *streamClient) sendJSON() error {
	s, _, ok := cl.sim()
	if !ok {
		return nil
	}
	cl.mu.Lock()
	from := cl.eventSeq
	cl.mu.Unlock()

	var out serverMsg
	out.Type = "tick"
	out.Sim = s.ID
	var evs []events.Event
	var next, missed uint64
	s.Read(func(e *engine.Engine) {
		snap := simctl.BuildSnapshot(s, e)
		met := simctl.BuildMetrics(e)
		ser := simctl.BuildSeries(e)
		out.Snapshot = &snap
		out.Metrics = &met
		out.Series = &ser
		out.Districts = simctl.DistrictStats(e)
		evs, next, missed = e.Ring.ReadFrom(from, evs, 120)
		for _, ev := range evs {
			out.Events = append(out.Events, EventView{
				Seq: ev.Seq, Tick: uint64(ev.Tick), Kind: ev.Kind.String(),
				Severity: ev.Severity.String(), Region: ev.Region,
				Text: events.Describe(e.Map, ev), A: ev.A, B: ev.B,
			})
		}
	})
	out.NextSeq, out.Missed = next, missed
	cl.mu.Lock()
	cl.eventSeq = next
	cl.mu.Unlock()

	b, err := json.Marshal(out)
	if err != nil {
		slog.Error("stream: marshal failed", "err", err)
		return nil
	}
	return cl.conn.WriteText(b)
}
