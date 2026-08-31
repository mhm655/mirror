package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestParseRole(t *testing.T) {
	cases := []struct {
		in      string
		want    Role
		wantErr bool
	}{
		{"viewer", RoleViewer, false},
		{"operator", RoleOperator, false},
		{"admin", RoleAdmin, false},
		{"superadmin", 0, true},
		{"", 0, true},
	}
	for _, c := range cases {
		got, err := ParseRole(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseRole(%q): expected error, got nil", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRole(%q): unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ParseRole(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestRoleString(t *testing.T) {
	if RoleViewer.String() != "viewer" || RoleOperator.String() != "operator" || RoleAdmin.String() != "admin" {
		t.Fatalf("unexpected role names: %q %q %q", RoleViewer, RoleOperator, RoleAdmin)
	}
}

func TestAuthenticatorAddLookup(t *testing.T) {
	a := &Authenticator{keys: make(map[string]*Key)}
	a.Add("alice", RoleOperator, "s3cr3t")

	k, ok := a.Lookup("s3cr3t")
	if !ok {
		t.Fatal("Lookup: expected the key to be found")
	}
	if k.Name != "alice" || k.Role != RoleOperator {
		t.Errorf("Lookup returned %+v, want name=alice role=operator", k)
	}

	if _, ok := a.Lookup("wrong"); ok {
		t.Error("Lookup: expected an incorrect secret to fail")
	}
	if _, ok := a.Lookup(""); ok {
		t.Error("Lookup: expected an empty secret to fail")
	}
}

func TestNewAuthenticatorDevMode(t *testing.T) {
	os.Unsetenv("MIRROR_API_KEYS")
	a, err := NewAuthenticator("dev")
	if err != nil {
		t.Fatalf("NewAuthenticator(dev): %v", err)
	}
	if a.DevKey == "" {
		t.Fatal("expected a dev key to be generated")
	}
	k, ok := a.Lookup(a.DevKey)
	if !ok {
		t.Fatal("expected the generated dev key to be a valid credential")
	}
	if k.Role != RoleAdmin {
		t.Errorf("dev key role = %v, want admin", k.Role)
	}
}

func TestNewAuthenticatorProductionRequiresKeys(t *testing.T) {
	os.Unsetenv("MIRROR_API_KEYS")
	if _, err := NewAuthenticator("production"); err == nil {
		t.Fatal("expected production mode with no MIRROR_API_KEYS to refuse to start")
	}
}

func TestNewAuthenticatorFromEnv(t *testing.T) {
	os.Setenv("MIRROR_API_KEYS", "bob:viewer:bobsecret,carol:admin:carolsecret")
	defer os.Unsetenv("MIRROR_API_KEYS")

	a, err := NewAuthenticator("production")
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	if a.DevKey != "" {
		t.Error("expected no dev key when MIRROR_API_KEYS is set")
	}
	bob, ok := a.Lookup("bobsecret")
	if !ok || bob.Role != RoleViewer {
		t.Errorf("bob: got %+v, ok=%v", bob, ok)
	}
	carol, ok := a.Lookup("carolsecret")
	if !ok || carol.Role != RoleAdmin {
		t.Errorf("carol: got %+v, ok=%v", carol, ok)
	}
}

func TestNewAuthenticatorFromEnvMalformed(t *testing.T) {
	os.Setenv("MIRROR_API_KEYS", "not-enough-parts")
	defer os.Unsetenv("MIRROR_API_KEYS")
	if _, err := NewAuthenticator("dev"); err == nil {
		t.Fatal("expected a malformed MIRROR_API_KEYS entry to error")
	}
}

func TestCredential(t *testing.T) {
	mk := func(auth, apiKey, query string) *http.Request {
		r := httptest.NewRequest("GET", "/x?key="+query, nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		if apiKey != "" {
			r.Header.Set("X-API-Key", apiKey)
		}
		return r
	}

	if got := credential(mk("Bearer abc", "", ""), false); got != "abc" {
		t.Errorf("Bearer: got %q", got)
	}
	if got := credential(mk("", "xyz", ""), false); got != "xyz" {
		t.Errorf("X-API-Key: got %q", got)
	}
	// Bearer takes precedence over X-API-Key.
	if got := credential(mk("Bearer abc", "xyz", ""), false); got != "abc" {
		t.Errorf("precedence: got %q, want abc", got)
	}
	// Query param ignored unless allowQuery.
	if got := credential(mk("", "", "qqq"), false); got != "" {
		t.Errorf("query without allowQuery: got %q, want empty", got)
	}
	if got := credential(mk("", "", "qqq"), true); got != "qqq" {
		t.Errorf("query with allowQuery: got %q, want qqq", got)
	}
	if got := credential(mk("", "", ""), false); got != "" {
		t.Errorf("no credential: got %q, want empty", got)
	}
}

func TestRateLimiterBurstThenBlocked(t *testing.T) {
	rl := NewRateLimiter(1, 3) // 1 token/sec, burst of 3
	for i := 0; i < 3; i++ {
		if !rl.Allow("client") {
			t.Fatalf("call %d: expected burst capacity to allow this request", i)
		}
	}
	if rl.Allow("client") {
		t.Fatal("expected the 4th immediate request to be rate limited")
	}
}

func TestRateLimiterRefillsOverTime(t *testing.T) {
	rl := NewRateLimiter(1000, 1) // fast refill so the test doesn't sleep long
	if !rl.Allow("client") {
		t.Fatal("first request should be allowed")
	}
	if rl.Allow("client") {
		t.Fatal("second immediate request should be blocked (burst exhausted)")
	}
	time.Sleep(5 * time.Millisecond) // ~5 tokens at 1000/sec
	if !rl.Allow("client") {
		t.Fatal("expected a token to have refilled after sleeping")
	}
}

func TestRateLimiterIndependentBuckets(t *testing.T) {
	rl := NewRateLimiter(1, 1)
	if !rl.Allow("a") {
		t.Fatal("client a should get its own bucket")
	}
	if !rl.Allow("b") {
		t.Fatal("client b should get its own bucket, independent of a")
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:4321"
	if got := clientIP(r); got != "10.0.0.5" {
		t.Errorf("RemoteAddr fallback: got %q", got)
	}

	r2 := httptest.NewRequest("GET", "/", nil)
	r2.RemoteAddr = "10.0.0.5:4321"
	r2.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	if got := clientIP(r2); got != "203.0.113.9" {
		t.Errorf("X-Forwarded-For: got %q, want first entry", got)
	}

	r3 := httptest.NewRequest("GET", "/", nil)
	r3.RemoteAddr = "not-a-host-port"
	if got := clientIP(r3); got != "not-a-host-port" {
		t.Errorf("malformed RemoteAddr: got %q, want raw passthrough", got)
	}
}

func TestAuditLogRecent(t *testing.T) {
	log := NewAuditLog(4) // below the 16 floor
	if cap := len(log.buf); cap != 16 {
		t.Fatalf("NewAuditLog(4): buffer capacity = %d, want floor of 16", cap)
	}

	for i := 0; i < 3; i++ {
		log.Add(AuditEntry{Actor: string(rune('a' + i))})
	}
	recent := log.Recent(0)
	if len(recent) != 3 {
		t.Fatalf("Recent(0): got %d entries, want 3", len(recent))
	}
	// Newest first.
	if recent[0].Actor != "c" || recent[1].Actor != "b" || recent[2].Actor != "a" {
		t.Errorf("Recent order = %+v, want newest first (c, b, a)", recent)
	}

	if got := log.Recent(2); len(got) != 2 || got[0].Actor != "c" {
		t.Errorf("Recent(2) = %+v", got)
	}
}

func TestAuditLogWraps(t *testing.T) {
	log := NewAuditLog(16)
	for i := 0; i < 20; i++ {
		log.Add(AuditEntry{Path: string(rune('a' + i))})
	}
	recent := log.Recent(0)
	if len(recent) != 16 {
		t.Fatalf("Recent(0) after wrap = %d entries, want capacity 16", len(recent))
	}
	// The newest entry is the 20th one added ('a'+19 = 't').
	if recent[0].Path != string(rune('a'+19)) {
		t.Errorf("newest entry = %q, want %q", recent[0].Path, string(rune('a'+19)))
	}
}
