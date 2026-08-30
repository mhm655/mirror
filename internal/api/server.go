package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mirror-sim/mirror/internal/agent"
	"github.com/mirror-sim/mirror/internal/engine"
	"github.com/mirror-sim/mirror/internal/events"
	"github.com/mirror-sim/mirror/internal/simctl"
	"github.com/mirror-sim/mirror/internal/state"
	"github.com/mirror-sim/mirror/internal/store"
	"github.com/mirror-sim/mirror/internal/telemetry"
	"github.com/mirror-sim/mirror/internal/units"
	"github.com/mirror-sim/mirror/internal/world"
)

// Config for the HTTP server.
type Config struct {
	Addr      string
	WebDir    string
	AuthMode  string
	RateLimit float64
	Burst     float64
	// AllowOrigin for CORS. Empty disables cross-origin entirely, which is
	// correct when the UI is served from this same process.
	AllowOrigin string
}

func DefaultConfig() Config {
	return Config{
		Addr: ":8080", WebDir: envOr("MIRROR_WEB_DIR", "web/dist"),
		AuthMode:  envOr("MIRROR_AUTH_MODE", "dev"),
		RateLimit: 40, Burst: 120,
		AllowOrigin: os.Getenv("MIRROR_ALLOW_ORIGIN"),
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// Server ties the manager, the AI agent and the transport together.
type Server struct {
	cfg    Config
	mgr    *simctl.Manager
	auth   *Authenticator
	rl     *RateLimiter
	audit  *AuditLog
	agent  *agent.Agent
	metric *telemetry.Registry
	mux    *http.ServeMux
	http   *http.Server
	done   chan struct{}

	wsConns  atomic.Int64
	reqTotal atomic.Int64
	reqErr   atomic.Int64
}

func NewServer(cfg Config, mgr *simctl.Manager) (*Server, error) {
	auth, err := NewAuthenticator(cfg.AuthMode)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg: cfg, mgr: mgr, auth: auth,
		rl:     NewRateLimiter(cfg.RateLimit, cfg.Burst),
		audit:  NewAuditLog(2048),
		metric: telemetry.NewRegistry(),
		done:   make(chan struct{}),
	}
	s.agent = agent.New(mgr)
	s.routes()
	s.http = &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: the WebSocket route hijacks the connection and a
		// write deadline set by net/http would kill long-lived streams. The
		// per-frame deadline in Conn.write bounds a stuck socket instead.
		IdleTimeout: 120 * time.Second,
	}
	return s, nil
}

func (s *Server) DevKey() string { return s.auth.DevKey }

func (s *Server) ListenAndServe() error {
	slog.Info("mirror api listening", "addr", s.cfg.Addr, "authMode", s.cfg.AuthMode, "webDir", s.cfg.WebDir)
	err := s.http.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error {
	close(s.done)
	return s.http.Shutdown(ctx)
}

// ---------------------------------------------------------------- routes ---

func (s *Server) routes() {
	mux := http.NewServeMux()
	s.mux = mux

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		n := len(s.mgr.List())
		writeJSON(w, 200, map[string]any{"status": "ready", "simulations": n})
	})
	mux.HandleFunc("GET /metrics", s.handlePrometheus)

	// Dev bootstrap. Only ever returns a credential in dev mode; in production
	// it 404s so that its very existence is not a hint.
	mux.HandleFunc("POST /api/v1/auth/dev-session", func(w http.ResponseWriter, r *http.Request) {
		if s.auth.Mode == "production" || s.auth.DevKey == "" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, 200, map[string]string{"key": s.auth.DevKey, "role": "admin"})
	})

	get := func(pattern string, role Role, h http.HandlerFunc) {
		mux.Handle("GET "+pattern, s.guard(role, h))
	}
	post := func(pattern string, role Role, h http.HandlerFunc) {
		mux.Handle("POST "+pattern, s.guard(role, h))
	}

	get("/api/v1/simulations", RoleViewer, s.listSims)
	post("/api/v1/simulations", RoleOperator, s.createSim)
	get("/api/v1/simulations/{id}", RoleViewer, s.getSim)
	mux.Handle("DELETE /api/v1/simulations/{id}", s.guard(RoleAdmin, s.deleteSim))
	get("/api/v1/simulations/{id}/map", RoleViewer, s.getMap)
	get("/api/v1/simulations/{id}/metrics", RoleViewer, s.getMetrics)
	get("/api/v1/simulations/{id}/series", RoleViewer, s.getSeries)
	get("/api/v1/simulations/{id}/events", RoleViewer, s.getEvents)
	get("/api/v1/simulations/{id}/commands", RoleViewer, s.getCommands)
	get("/api/v1/simulations/{id}/districts", RoleViewer, s.getDistricts)
	get("/api/v1/simulations/{id}/incidents", RoleViewer, s.getIncidents)
	get("/api/v1/simulations/{id}/entity", RoleViewer, s.getEntity)
	get("/api/v1/simulations/{id}/checkpoints", RoleViewer, s.getCheckpoints)
	post("/api/v1/simulations/{id}/control", RoleOperator, s.control)
	post("/api/v1/simulations/{id}/events", RoleOperator, s.injectEvent)
	post("/api/v1/simulations/{id}/policy", RoleOperator, s.setPolicy)
	post("/api/v1/simulations/{id}/fork", RoleOperator, s.fork)
	post("/api/v1/simulations/{id}/restore", RoleAdmin, s.restore)
	post("/api/v1/compare", RoleViewer, s.compare)
	post("/api/v1/chaos", RoleAdmin, s.chaos)
	get("/api/v1/agent/tools", RoleViewer, s.agentTools)
	post("/api/v1/agent/chat", RoleOperator, s.agentChat)
	get("/api/v1/audit", RoleAdmin, s.getAudit)

	mux.HandleFunc("GET /ws", s.handleWS)
	mux.HandleFunc("/", s.serveUI)
}

// guard is the authentication, authorisation, rate limiting and audit chain.
func (s *Server) guard(min Role, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		s.reqTotal.Add(1)
		s.cors(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		secret := credential(r, false)
		key, ok := s.auth.Lookup(secret)
		if !ok {
			s.reqErr.Add(1)
			// A generic message: distinguishing "no key" from "bad key" tells
			// a prober whether they are close.
			writeErr(w, http.StatusUnauthorized, "invalid or missing credentials")
			return
		}
		if key.Role < min {
			s.reqErr.Add(1)
			s.audit.Add(AuditEntry{
				At: time.Now().UTC(), Actor: key.Name, Role: key.Role.String(),
				IP: clientIP(r), Method: r.Method, Path: r.URL.Path,
				Status: http.StatusForbidden, Detail: "requires " + min.String(),
			})
			writeErr(w, http.StatusForbidden, "this action requires the "+min.String()+" role")
			return
		}
		if !s.rl.Allow(key.ID) {
			s.reqErr.Add(1)
			w.Header().Set("Retry-After", "1")
			writeErr(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}

		rec := &statusRecorder{ResponseWriter: w, code: 200}
		h(rec, r.WithContext(context.WithValue(r.Context(), ctxKeyRole{}, key)))

		if r.Method != http.MethodGet {
			s.audit.Add(AuditEntry{
				At: start.UTC(), Actor: key.Name, Role: key.Role.String(),
				IP: clientIP(r), Method: r.Method, Path: r.URL.Path,
				Target: r.PathValue("id"), Status: rec.code,
				Latency: float64(time.Since(start).Microseconds()) / 1000,
			})
		}
		if rec.code >= 400 {
			s.reqErr.Add(1)
		}
		s.metric.ObserveAPI(r.URL.Path, rec.code, time.Since(start))
	})
}

type ctxKeyRole struct{}

func keyOf(r *http.Request) *Key {
	k, _ := r.Context().Value(ctxKeyRole{}).(*Key)
	return k
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(c int) { s.code = c; s.ResponseWriter.WriteHeader(c) }

func (s *Server) cors(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AllowOrigin == "" {
		return
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	// Exact match only. Reflecting an arbitrary Origin with credentials is the
	// classic CORS mistake; a wildcard here would let any page drive an
	// operator's simulations.
	if origin != s.cfg.AllowOrigin {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
}

// -------------------------------------------------------------- handlers ---

func (s *Server) listSims(w http.ResponseWriter, r *http.Request) {
	sims := s.mgr.List()
	out := make([]simctl.Snapshot, 0, len(sims))
	for _, sim := range sims {
		sim.Read(func(e *engine.Engine) { out = append(out, simctl.BuildSnapshot(sim, e)) })
	}
	writeJSON(w, 200, map[string]any{"simulations": out})
}

type createReq struct {
	Name       string `json:"name"`
	Preset     string `json:"preset"`
	Seed       uint64 `json:"seed"`
	Population int    `json:"population"`
	StartHour  int32  `json:"startHour"`
	Regions    int    `json:"regions"`
	Workers    int    `json:"workers"`
}

func (s *Server) createSim(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if !readJSON(w, r, &req) {
		return
	}
	cfg := engine.DefaultConfig()
	if req.Preset != "" {
		cfg.Preset = req.Preset
	}
	if req.Seed != 0 {
		cfg.Seed = req.Seed
	}
	if req.Population > 0 {
		cfg.Population = req.Population
	}
	if req.StartHour >= 0 && req.StartHour < 24 {
		cfg.StartHour = req.StartHour
	}
	cfg.Regions, cfg.Workers = req.Regions, req.Workers

	// Bound the request. An unbounded population is a trivial way to OOM the
	// process from an authenticated but careless client.
	if cfg.Population > 400_000 {
		writeErr(w, http.StatusBadRequest, "population is capped at 400000")
		return
	}
	switch cfg.Preset {
	case "small", "medium", "large", "huge":
	default:
		writeErr(w, http.StatusBadRequest, "preset must be small, medium, large or huge")
		return
	}

	sim, err := s.mgr.Create(req.Name, cfg)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	var snap simctl.Snapshot
	sim.Read(func(e *engine.Engine) { snap = simctl.BuildSnapshot(sim, e) })
	writeJSON(w, 201, snap)
}

func (s *Server) simOr404(w http.ResponseWriter, r *http.Request) (*simctl.Sim, bool) {
	sim, ok := s.mgr.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no such simulation")
		return nil, false
	}
	return sim, true
}

func (s *Server) getSim(w http.ResponseWriter, r *http.Request) {
	sim, ok := s.simOr404(w, r)
	if !ok {
		return
	}
	var snap simctl.Snapshot
	sim.Read(func(e *engine.Engine) { snap = simctl.BuildSnapshot(sim, e) })
	writeJSON(w, 200, snap)
}

func (s *Server) deleteSim(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.Delete(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getMetrics(w http.ResponseWriter, r *http.Request) {
	sim, ok := s.simOr404(w, r)
	if !ok {
		return
	}
	var m simctl.Metrics
	sim.Read(func(e *engine.Engine) { m = simctl.BuildMetrics(e) })
	writeJSON(w, 200, m)
}

func (s *Server) getSeries(w http.ResponseWriter, r *http.Request) {
	sim, ok := s.simOr404(w, r)
	if !ok {
		return
	}
	var ser simctl.Series
	sim.Read(func(e *engine.Engine) { ser = simctl.BuildSeries(e) })
	writeJSON(w, 200, ser)
}

func (s *Server) getDistricts(w http.ResponseWriter, r *http.Request) {
	sim, ok := s.simOr404(w, r)
	if !ok {
		return
	}
	var d []simctl.DistrictStat
	sim.Read(func(e *engine.Engine) { d = simctl.DistrictStats(e) })
	writeJSON(w, 200, map[string]any{"districts": d})
}

func (s *Server) getEvents(w http.ResponseWriter, r *http.Request) {
	sim, ok := s.simOr404(w, r)
	if !ok {
		return
	}
	from, _ := strconv.ParseUint(r.URL.Query().Get("from"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	var out []EventView
	var next, missed uint64
	sim.Read(func(e *engine.Engine) {
		var buf []events.Event
		buf, next, missed = e.Ring.ReadFrom(from, buf, limit)
		for _, ev := range buf {
			out = append(out, EventView{
				Seq: ev.Seq, Tick: uint64(ev.Tick), Kind: ev.Kind.String(),
				Severity: ev.Severity.String(), Region: ev.Region,
				Text: events.Describe(e.Map, ev), A: ev.A, B: ev.B,
			})
		}
	})
	writeJSON(w, 200, map[string]any{"events": out, "nextSeq": next, "missed": missed})
}

func (s *Server) getCommands(w http.ResponseWriter, r *http.Request) {
	sim, ok := s.simOr404(w, r)
	if !ok {
		return
	}
	type cmdView struct {
		Seq  uint64 `json:"seq"`
		Tick uint64 `json:"tick"`
		Kind string `json:"kind"`
		Text string `json:"text"`
	}
	var out []cmdView
	sim.Read(func(e *engine.Engine) {
		for _, c := range e.Log.Cmds {
			out = append(out, cmdView{
				Seq: c.Seq, Tick: uint64(c.Tick), Kind: c.Kind.String(),
				Text: events.Describe(e.Map, c),
			})
		}
	})
	writeJSON(w, 200, map[string]any{"commands": out})
}

func (s *Server) getIncidents(w http.ResponseWriter, r *http.Request) {
	sim, ok := s.simOr404(w, r)
	if !ok {
		return
	}
	type incView struct {
		ID            int64  `json:"id"`
		Kind          string `json:"kind"`
		Edge          int32  `json:"edge"`
		District      string `json:"district"`
		StartTick     uint32 `json:"startTick"`
		Severity      int32  `json:"severity"`
		Casualties    int32  `json:"casualties"`
		ResponseSec   int32  `json:"responseSec"`
		Resolved      bool   `json:"resolved"`
		AwaitingUnits int32  `json:"awaitingUnits"`
		X             int64  `json:"x"`
		Y             int64  `json:"y"`
	}
	var out []incView
	sim.Read(func(e *engine.Engine) {
		for i := range e.S.Incidents {
			in := &e.S.Incidents[i]
			v := incView{
				ID: in.ID, Kind: state.IncidentKindName(in.Kind), Edge: int32(in.Edge),
				StartTick: in.StartTick, Severity: in.Severity, Casualties: in.Casualties,
				Resolved:      in.Resolved,
				AwaitingUnits: in.NeedAmbulance + in.NeedFire + in.NeedPolice,
			}
			if int(in.District) < len(e.Map.Districts) {
				v.District = e.Map.Districts[in.District].Name
			}
			if in.FirstResponseTick > 0 {
				v.ResponseSec = int32(in.FirstResponseTick-in.StartTick) / units.TicksPerSecond
			}
			if in.Node >= 0 && int(in.Node) < len(e.Map.Nodes) {
				v.X, v.Y = int64(e.Map.Nodes[in.Node].X), int64(e.Map.Nodes[in.Node].Y)
			}
			out = append(out, v)
		}
	})
	writeJSON(w, 200, map[string]any{"incidents": out})
}

func (s *Server) getEntity(w http.ResponseWriter, r *http.Request) {
	sim, ok := s.simOr404(w, r)
	if !ok {
		return
	}
	kind := r.URL.Query().Get("kind")
	id64, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	var out any
	var err error
	sim.Read(func(e *engine.Engine) { out, err = inspectEntity(e, kind, int32(id64)) })
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, 200, out)
}

type controlReq struct {
	Action    string `json:"action"`
	Speed     int32  `json:"speed"`
	UntilTick uint64 `json:"untilTick"`
}

func (s *Server) control(w http.ResponseWriter, r *http.Request) {
	var req controlReq
	if !readJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	var err error
	switch req.Action {
	case "play":
		err = s.mgr.Play(id)
	case "pause":
		err = s.mgr.Pause(id)
	case "speed":
		err = s.mgr.SetSpeed(id, req.Speed)
	case "runUntil":
		err = s.mgr.RunUntil(id, units.Tick(req.UntilTick))
	default:
		writeErr(w, http.StatusBadRequest, "action must be play, pause, speed or runUntil")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

type injectReq struct {
	Kind    string `json:"kind"`
	Tick    uint64 `json:"tick"`
	A       int64  `json:"a"`
	B       int64  `json:"b"`
	C       int64  `json:"c"`
	D       int64  `json:"d"`
	Weather string `json:"weather,omitempty"`
}

// injectableKinds is an allow-list.
//
// The API deliberately does not accept a raw numeric event kind. Commands are
// the replay contract; letting a client post an arbitrary integer means any
// future renumbering silently changes what stored logs mean, and it also opens
// the chaos commands to any operator. Naming them explicitly costs one map and
// removes both problems.
var injectableKinds = map[string]events.Kind{
	"accident":        events.CmdInjectAccident,
	"close_road":      events.CmdCloseRoad,
	"reopen_road":     events.CmdReopenRoad,
	"power_failure":   events.CmdPowerFailure,
	"power_restore":   events.CmdPowerRestore,
	"weather":         events.CmdSetWeather,
	"hospital_surge":  events.CmdHospitalSurge,
	"transit_failure": events.CmdTransitFailure,
	"flood":           events.CmdFloodDistrict,
	"earthquake":      events.CmdEarthquake,
	"comms_outage":    events.CmdCommsOutage,
	"spawn_traffic":   events.CmdSpawnTraffic,
}

func (s *Server) injectEvent(w http.ResponseWriter, r *http.Request) {
	var req injectReq
	if !readJSON(w, r, &req) {
		return
	}
	kind, ok := injectableKinds[req.Kind]
	if !ok {
		names := make([]string, 0, len(injectableKinds))
		for k := range injectableKinds {
			names = append(names, k)
		}
		writeErr(w, http.StatusBadRequest, "unknown event kind; expected one of: "+strings.Join(names, ", "))
		return
	}
	if kind == events.CmdSetWeather && req.Weather != "" {
		code, ok := events.WeatherCode(req.Weather)
		if !ok {
			writeErr(w, http.StatusBadRequest, "unknown weather condition")
			return
		}
		req.A = int64(code)
	}
	ev, err := s.mgr.Inject(r.PathValue("id"), events.C(units.Tick(req.Tick), kind, req.A, req.B, req.C, req.D))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, 202, map[string]any{"seq": ev.Seq, "tick": ev.Tick, "kind": ev.Kind.String()})
}

type policyReq struct {
	AdaptiveSignals      *bool  `json:"adaptiveSignals,omitempty"`
	AdaptiveMaxExtendSec *int32 `json:"adaptiveMaxExtendSec,omitempty"`
	EmergencyPreemption  *bool  `json:"emergencyPreemption,omitempty"`
	TransitVehiclesPct   *int32 `json:"transitVehiclesPct,omitempty"`
	RerouteAwarenessPct  *int32 `json:"rerouteAwarenessPct,omitempty"`
	SpeedLimitPct        *int32 `json:"speedLimitPct,omitempty"`
	CongestionCharge     *bool  `json:"congestionCharge,omitempty"`
	AmbulanceSurgePct    *int32 `json:"ambulanceSurgePct,omitempty"`
}

func (s *Server) setPolicy(w http.ResponseWriter, r *http.Request) {
	var req policyReq
	if !readJSON(w, r, &req) {
		return
	}
	writeJSON(w, 202, map[string]int{"applied": s.applyPolicy(r.PathValue("id"), req)})
}

// applyPolicy turns a partial policy patch into individual policy commands.
//
// One command per field rather than one command carrying a whole policy blob.
// The command log is the replay contract and it outlives this binary: a
// per-field command means an older reader that does not recognise field 8
// ignores it, whereas a blob means an older reader misparses the entire
// policy. It also makes the log readable -- "set adaptive signals on at 08:42"
// rather than an opaque struct.
func (s *Server) applyPolicy(id string, req policyReq) int {
	applied := 0
	send := func(field, v int64) {
		if _, err := s.mgr.Inject(id, events.C(0, events.CmdSetPolicy, field, v, 0, 0)); err == nil {
			applied++
		}
	}
	b2i := func(b bool) int64 {
		if b {
			return 1
		}
		return 0
	}
	if req.AdaptiveSignals != nil {
		send(0, b2i(*req.AdaptiveSignals))
	}
	if req.AdaptiveMaxExtendSec != nil {
		send(1, int64(*req.AdaptiveMaxExtendSec)*units.TicksPerSecond)
	}
	if req.EmergencyPreemption != nil {
		send(2, b2i(*req.EmergencyPreemption))
	}
	if req.TransitVehiclesPct != nil {
		send(3, int64(*req.TransitVehiclesPct)*10)
	}
	if req.RerouteAwarenessPct != nil {
		send(4, int64(*req.RerouteAwarenessPct)*10)
	}
	if req.SpeedLimitPct != nil {
		send(5, int64(*req.SpeedLimitPct)*10)
	}
	if req.CongestionCharge != nil {
		send(6, b2i(*req.CongestionCharge))
	}
	if req.AmbulanceSurgePct != nil {
		send(7, int64(*req.AmbulanceSurgePct)*10)
	}
	return applied
}

type forkReq struct {
	Name   string     `json:"name"`
	Policy *policyReq `json:"policy,omitempty"`
	Play   bool       `json:"play"`
	Speed  int32      `json:"speed"`
}

func (s *Server) fork(w http.ResponseWriter, r *http.Request) {
	var req forkReq
	if !readJSON(w, r, &req) {
		return
	}
	child, err := s.mgr.Fork(r.PathValue("id"), req.Name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Policy != nil {
		s.applyPolicy(child.ID, *req.Policy)
	}
	if req.Speed != 0 {
		_ = s.mgr.SetSpeed(child.ID, req.Speed)
	}
	if req.Play {
		_ = s.mgr.Play(child.ID)
	}
	var snap simctl.Snapshot
	child.Read(func(e *engine.Engine) { snap = simctl.BuildSnapshot(child, e) })
	writeJSON(w, 201, snap)
}

func (s *Server) getCheckpoints(w http.ResponseWriter, r *http.Request) {
	hs, err := s.mgr.StoreRef().ListCheckpoints(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type view struct {
		Tick    uint64 `json:"tick"`
		Digest  string `json:"digest"`
		Bytes   uint32 `json:"uncompressedBytes"`
		Created string `json:"created"`
	}
	out := make([]view, 0, len(hs))
	for _, h := range hs {
		out = append(out, view{
			Tick: uint64(h.Tick), Digest: fmt.Sprintf("%016x", h.Digest),
			Bytes: h.Raw, Created: h.Created.Format(time.RFC3339),
		})
	}
	writeJSON(w, 200, map[string]any{"checkpoints": out})
}

type restoreReq struct {
	Tick uint64 `json:"tick"`
}

func (s *Server) restore(w http.ResponseWriter, r *http.Request) {
	var req restoreReq
	if !readJSON(w, r, &req) {
		return
	}
	start := time.Now()
	from, replayed, err := s.mgr.Restore(r.PathValue("id"), units.Tick(req.Tick))
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, store.ErrNotFound) {
			code = http.StatusNotFound
		}
		if errors.Is(err, store.ErrCorrupt) {
			code = http.StatusUnprocessableEntity
		}
		writeErr(w, code, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"restoredFromTick": uint64(from),
		"ticksReplayed":    replayed,
		"recoveryMillis":   time.Since(start).Milliseconds(),
	})
}

type compareReq struct {
	IDs []string `json:"ids"`
}

func (s *Server) compare(w http.ResponseWriter, r *http.Request) {
	var req compareReq
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.IDs) < 1 || len(req.IDs) > 8 {
		writeErr(w, http.StatusBadRequest, "compare between 1 and 8 scenarios")
		return
	}
	cmp, err := s.mgr.Compare(req.IDs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, 200, cmp)
}

func (s *Server) getAudit(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	writeJSON(w, 200, map[string]any{"entries": s.audit.Recent(limit)})
}

func (s *Server) agentTools(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"tools": s.agent.ToolSpecs()})
}

type agentReq struct {
	Sim     string `json:"sim"`
	Message string `json:"message"`
	// Approve authorises the agent to run tools that mutate a simulation.
	// Absent, the agent is restricted to read-only tools.
	Approve bool `json:"approveMutations"`
}

func (s *Server) agentChat(w http.ResponseWriter, r *http.Request) {
	var req agentReq
	if !readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeErr(w, http.StatusBadRequest, "message is required")
		return
	}
	k := keyOf(r)
	// Mutation authority is the *intersection* of what the caller asked for
	// and what their role permits. An operator can let the agent act; a viewer
	// cannot grant an agent powers they do not have themselves.
	allowMutate := req.Approve && k != nil && k.Role >= RoleOperator
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	resp, err := s.agent.Chat(ctx, agent.Request{
		SimID: req.Sim, Message: req.Message, AllowMutations: allowMutate,
		Actor: actorName(k),
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, 200, resp)
}

func actorName(k *Key) string {
	if k == nil {
		return "anonymous"
	}
	return k.Name
}

type chaosReq struct {
	Sim        string `json:"sim"`
	Experiment string `json:"experiment"`
	Param      int64  `json:"param"`
}

func (s *Server) chaos(w http.ResponseWriter, r *http.Request) {
	var req chaosReq
	if !readJSON(w, r, &req) {
		return
	}
	res, err := s.runChaos(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, 200, res)
}

// ------------------------------------------------------------------ map ----

// mapView is columnar on purpose.
//
// A 45x45 city has ~11,000 edges. As an array of objects with named fields that
// is roughly 3 MB of JSON; as parallel arrays of numbers it is under 700 KB,
// and the browser can load it straight into typed arrays without a per-edge
// object allocation. The map is fetched once per session, so this is a
// one-time cost -- but it is a one-time cost the user waits for.
type mapView struct {
	Name       string         `json:"name"`
	Hash       string         `json:"hash"`
	Width      int64          `json:"width"`
	Height     int64          `json:"height"`
	NodeX      []int32        `json:"nodeX"`
	NodeY      []int32        `json:"nodeY"`
	NodeSignal []int32        `json:"nodeSignal"`
	EdgeFrom   []int32        `json:"edgeFrom"`
	EdgeTo     []int32        `json:"edgeTo"`
	EdgeClass  []uint8        `json:"edgeClass"`
	EdgeDist   []int32        `json:"edgeDistrict"`
	Districts  []districtView `json:"districts"`
	POIs       []poiView      `json:"pois"`
	Signals    []int32        `json:"signalNodes"`
	Routes     []routeView    `json:"routes"`
}

type districtView struct {
	ID     int32  `json:"id"`
	Name   string `json:"name"`
	MinX   int64  `json:"minX"`
	MinY   int64  `json:"minY"`
	MaxX   int64  `json:"maxX"`
	MaxY   int64  `json:"maxY"`
	Region int32  `json:"region"`
}

type poiView struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	X    int64  `json:"x"`
	Y    int64  `json:"y"`
	ID   int32  `json:"id"`
	Cap  int32  `json:"capacity"`
}

type routeView struct {
	ID    int32   `json:"id"`
	Name  string  `json:"name"`
	Mode  uint8   `json:"mode"`
	Stops []int32 `json:"stops"`
}

func (s *Server) getMap(w http.ResponseWriter, r *http.Request) {
	sim, ok := s.simOr404(w, r)
	if !ok {
		return
	}
	var out mapView
	sim.Read(func(e *engine.Engine) {
		m := e.Map
		out = mapView{
			Name: m.Name, Hash: fmt.Sprintf("%016x", m.Hash),
			Width: int64(m.Width), Height: int64(m.Height),
			NodeX: make([]int32, len(m.Nodes)), NodeY: make([]int32, len(m.Nodes)),
			NodeSignal: make([]int32, len(m.Nodes)),
			EdgeFrom:   make([]int32, len(m.Edges)), EdgeTo: make([]int32, len(m.Edges)),
			EdgeClass: make([]uint8, len(m.Edges)), EdgeDist: make([]int32, len(m.Edges)),
		}
		for i := range m.Nodes {
			out.NodeX[i] = int32(m.Nodes[i].X)
			out.NodeY[i] = int32(m.Nodes[i].Y)
			out.NodeSignal[i] = m.Nodes[i].Signal
		}
		for i := range m.Edges {
			out.EdgeFrom[i] = int32(m.Edges[i].From)
			out.EdgeTo[i] = int32(m.Edges[i].To)
			out.EdgeClass[i] = uint8(m.Edges[i].Class)
			out.EdgeDist[i] = int32(m.Edges[i].District)
		}
		for i := range m.Districts {
			d := &m.Districts[i]
			out.Districts = append(out.Districts, districtView{
				ID: int32(d.ID), Name: d.Name,
				MinX: int64(d.MinX), MinY: int64(d.MinY),
				MaxX: int64(d.MaxX), MaxY: int64(d.MaxY),
				Region: e.RegionOfDistrict(d.ID),
			})
		}
		// Only the landmarks are shipped. Sending 12,000 residence blocks
		// would double the payload to draw dots nobody looks at.
		for i := range m.POIs {
			p := &m.POIs[i]
			switch p.Kind {
			case world.POIHospital, world.POISchool:
				out.POIs = append(out.POIs, poiView{
					Kind: p.Kind.String(), Name: p.Name,
					X: int64(p.X), Y: int64(p.Y), ID: int32(p.ID), Cap: p.Capacity,
				})
			}
		}
		for i := range m.Substations {
			ss := &m.Substations[i]
			out.POIs = append(out.POIs, poiView{
				Kind: "substation", Name: ss.Name,
				X: int64(ss.X), Y: int64(ss.Y), ID: int32(ss.ID), Cap: ss.CapacityKW,
			})
		}
		for i := range m.Depots {
			d := &m.Depots[i]
			out.POIs = append(out.POIs, poiView{
				Kind: "depot", Name: d.Name, X: int64(d.X), Y: int64(d.Y),
				ID: int32(d.ID), Cap: d.Units,
			})
		}
		for i := range m.Signals {
			out.Signals = append(out.Signals, int32(m.Signals[i].Node))
		}
		for i := range m.Routes {
			rt := &m.Routes[i]
			rv := routeView{ID: int32(rt.ID), Name: rt.Name, Mode: rt.Mode}
			for _, sn := range rt.Stops {
				rv.Stops = append(rv.Stops, int32(sn))
			}
			out.Routes = append(out.Routes, rv)
		}
	})
	w.Header().Set("Cache-Control", "public, max-age=3600")
	writeJSON(w, 200, out)
}

// ------------------------------------------------------------ websocket ----

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	// The browser WebSocket API cannot set an Authorization header, so the
	// credential arrives as a query parameter here and nowhere else. It is
	// never written to the access log.
	key, ok := s.auth.Lookup(credential(r, true))
	if !ok {
		writeErr(w, http.StatusUnauthorized, "invalid or missing credentials")
		return
	}
	if !s.rl.Allow("ws:" + key.ID) {
		writeErr(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}
	conn, err := Upgrade(w, r, "mirror.v1")
	if err != nil {
		if !errors.Is(err, errNotWebsocket) {
			slog.Warn("websocket upgrade failed", "err", err, "ip", clientIP(r))
		}
		return
	}
	s.wsConns.Add(1)
	defer s.wsConns.Add(-1)
	s.handleStream(conn, r.URL.Query().Get("sim"))
}

// --------------------------------------------------------------- static ----

func (s *Server) serveUI(w http.ResponseWriter, r *http.Request) {
	dir := s.cfg.WebDir
	if dir == "" {
		http.NotFound(w, r)
		return
	}
	clean := filepath.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
	path := filepath.Join(dir, clean)
	if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
		setStaticHeaders(w, path)
		http.ServeFile(w, r, path)
		return
	}
	// SPA fallback.
	index := filepath.Join(dir, "index.html")
	if _, err := os.Stat(index); err != nil {
		writeJSON(w, 200, map[string]string{
			"service": "mirror",
			"note":    "UI bundle not found; run `make ui` or set MIRROR_WEB_DIR",
			"api":     "/api/v1/simulations",
		})
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	setSecurityHeaders(w)
	http.ServeFile(w, r, index)
}

func setStaticHeaders(w http.ResponseWriter, path string) {
	setSecurityHeaders(w)
	// Vite emits content-hashed asset names, so assets are immutable and
	// index.html is not.
	if strings.Contains(path, string(filepath.Separator)+"assets"+string(filepath.Separator)) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
}

func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	// The UI ships as a self-contained bundle with no third-party origins, so
	// the policy can be strict without breaking anything.
	h.Set("Content-Security-Policy",
		"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
			"script-src 'self'; connect-src 'self' ws: wss:; object-src 'none'; base-uri 'none'")
}

// ------------------------------------------------------------- plumbing ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	if err := enc.Encode(v); err != nil {
		slog.Error("api: response encode failed", "err", err)
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg, "status": code})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}
	return true
}
