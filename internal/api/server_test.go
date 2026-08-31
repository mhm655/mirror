package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mirror-sim/mirror/internal/engine"
	"github.com/mirror-sim/mirror/internal/simctl"
)

// testHarness wires a real Server to a real (small, fast) Manager, exposed
// through httptest.NewServer so requests exercise the actual routing,
// auth and audit chain.
type testHarness struct {
	t      *testing.T
	server *httptest.Server
	srv    *Server
	mgr    *simctl.Manager
	devKey string
}

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()
	mgr := simctl.NewManager(simctl.DefaultOptions())
	srv, err := NewServer(Config{AuthMode: "dev", RateLimit: 1000, Burst: 1000}, mgr)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.mux)
	h := &testHarness{t: t, server: ts, srv: srv, mgr: mgr, devKey: srv.DevKey()}
	t.Cleanup(func() {
		ts.Close()
		mgr.Shutdown()
	})
	return h
}

// smallCfg keeps simulations fast enough for a test suite: the same
// preset/population combination the engine package's own determinism tests
// use (see internal/engine/determinism_test.go).
func smallCfg() engine.Config {
	cfg := engine.DefaultConfig()
	cfg.Preset = "small"
	cfg.Population = 6000
	cfg.Seed = 42
	return cfg
}

func (h *testHarness) do(method, path, key string, body any) *http.Response {
	h.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, h.server.URL+path, rdr)
	if err != nil {
		h.t.Fatalf("NewRequest: %v", err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.server.Client().Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func decodeJSON[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer resp.Body.Close()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return v
}

func TestHealthzAndReadyzUnauthenticated(t *testing.T) {
	h := newTestHarness(t)

	resp := h.do("GET", "/healthz", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("/healthz: status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.do("GET", "/readyz", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("/readyz: status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestDevSessionEndpoint(t *testing.T) {
	h := newTestHarness(t)
	resp := h.do("POST", "/api/v1/auth/dev-session", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("dev-session: status = %d", resp.StatusCode)
	}
	body := decodeJSON[map[string]string](t, resp)
	if body["key"] != h.devKey || body["role"] != "admin" {
		t.Errorf("dev-session body = %+v", body)
	}
}

func TestMissingCredentialsRejected(t *testing.T) {
	h := newTestHarness(t)
	resp := h.do("GET", "/api/v1/simulations", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no credentials: status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.do("GET", "/api/v1/simulations", "not-a-real-key", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad credentials: status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestRoleGateForbidsInsufficientRole(t *testing.T) {
	h := newTestHarness(t)
	h.srv.auth.Add("viewer-key", RoleViewer, "viewer-secret")

	// Creating a simulation requires RoleOperator; a viewer must be refused.
	resp := h.do("POST", "/api/v1/simulations", "viewer-secret", createReq{Preset: "small", Population: 6000})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer creating a sim: status = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// Reading is allowed for a viewer.
	resp = h.do("GET", "/api/v1/simulations", "viewer-secret", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("viewer listing sims: status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestCreateGetAndDeleteSimulation(t *testing.T) {
	h := newTestHarness(t)

	resp := h.do("POST", "/api/v1/simulations", h.devKey, createReq{
		Name: "test-sim", Preset: "small", Population: 6000, Seed: 42, StartHour: 7,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201", resp.StatusCode)
	}
	snap := decodeJSON[simctl.Snapshot](t, resp)
	if snap.Name != "test-sim" || snap.ID == "" {
		t.Fatalf("create response = %+v", snap)
	}

	resp = h.do("GET", "/api/v1/simulations/"+snap.ID, h.devKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: status = %d, want 200", resp.StatusCode)
	}
	got := decodeJSON[simctl.Snapshot](t, resp)
	if got.ID != snap.ID {
		t.Fatalf("get returned id %q, want %q", got.ID, snap.ID)
	}

	resp = h.do("GET", "/api/v1/simulations/does-not-exist", h.devKey, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get missing sim: status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// Deleting requires admin; the dev key is admin, so this should succeed.
	resp = h.do("DELETE", "/api/v1/simulations/"+snap.ID, h.devKey, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.do("GET", "/api/v1/simulations/"+snap.ID, h.devKey, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestCreateSimulationValidation(t *testing.T) {
	h := newTestHarness(t)

	resp := h.do("POST", "/api/v1/simulations", h.devKey, createReq{Preset: "gigantic"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad preset: status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.do("POST", "/api/v1/simulations", h.devKey, createReq{Preset: "small", Population: 500_000})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("population over cap: status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestDeleteRequiresAdmin(t *testing.T) {
	h := newTestHarness(t)
	h.srv.auth.Add("operator-key", RoleOperator, "operator-secret")

	resp := h.do("POST", "/api/v1/simulations", h.devKey, createReq{Preset: "small", Population: 6000})
	snap := decodeJSON[simctl.Snapshot](t, resp)

	resp = h.do("DELETE", "/api/v1/simulations/"+snap.ID, "operator-secret", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("operator delete: status = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestInjectEventRejectsUnknownKind(t *testing.T) {
	h := newTestHarness(t)
	resp := h.do("POST", "/api/v1/simulations", h.devKey, createReq{Preset: "small", Population: 6000})
	snap := decodeJSON[simctl.Snapshot](t, resp)

	resp = h.do("POST", "/api/v1/simulations/"+snap.ID+"/events", h.devKey, injectReq{Kind: "meteor_strike"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown event kind: status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestReadJSONRejectsUnknownFields(t *testing.T) {
	h := newTestHarness(t)
	req, _ := http.NewRequest("POST", h.server.URL+"/api/v1/simulations",
		bytes.NewReader([]byte(`{"preset":"small","totallyBogusField":1}`)))
	req.Header.Set("Authorization", "Bearer "+h.devKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field: status = %d, want 400", resp.StatusCode)
	}
}

func TestCompareRequiresAtLeastOneID(t *testing.T) {
	h := newTestHarness(t)
	resp := h.do("POST", "/api/v1/compare", h.devKey, compareReq{IDs: nil})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("compare with no ids: status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}
