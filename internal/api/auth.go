package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Role is a coarse capability level. Three roles, not a permission matrix:
// a matrix nobody can hold in their head gets misconfigured, and this system
// genuinely only has three kinds of user.
type Role uint8

const (
	RoleViewer   Role = iota // read state, subscribe to the stream
	RoleOperator             // + inject events, fork, control playback
	RoleAdmin                // + chaos lab, restore, key management
)

var roleName = [...]string{"viewer", "operator", "admin"}

func (r Role) String() string { return roleName[r] }

func ParseRole(s string) (Role, error) {
	for i, n := range roleName {
		if n == s {
			return Role(i), nil
		}
	}
	return 0, errors.New("api: unknown role " + s)
}

// Key is an API credential. Only the hash is retained: a process that can be
// core-dumped should not be holding plaintext credentials for its own users,
// and there is never a reason to display a key after it is issued.
type Key struct {
	ID      string
	Name    string
	Role    Role
	hash    [32]byte
	Created time.Time
	// Prefix is the first 8 characters, kept for display and for log
	// correlation without ever revealing the secret.
	Prefix string
}

type Authenticator struct {
	mu   sync.RWMutex
	keys map[string]*Key // keyed by hex(hash) for O(1) lookup

	// DevKey is the plaintext of an auto-generated development credential.
	// Non-empty only when the server is running in dev mode.
	DevKey string
	Mode   string
}

// NewAuthenticator builds the credential store.
//
// Configuration precedence: MIRROR_API_KEYS ("name:role:secret" entries,
// comma-separated) wins; otherwise, in dev mode, one operator key is generated
// and printed once. In production mode with no keys configured, the server
// refuses to start rather than defaulting to open -- an authentication system
// whose failure mode is "allow everything" is not an authentication system.
func NewAuthenticator(mode string) (*Authenticator, error) {
	a := &Authenticator{keys: make(map[string]*Key), Mode: mode}
	if raw := os.Getenv("MIRROR_API_KEYS"); raw != "" {
		for _, entry := range strings.Split(raw, ",") {
			parts := strings.SplitN(strings.TrimSpace(entry), ":", 3)
			if len(parts) != 3 {
				return nil, errors.New("api: MIRROR_API_KEYS entries must be name:role:secret")
			}
			role, err := ParseRole(parts[1])
			if err != nil {
				return nil, err
			}
			a.Add(parts[0], role, parts[2])
		}
		return a, nil
	}
	if mode == "production" {
		return nil, errors.New("api: MIRROR_API_KEYS is required when MIRROR_AUTH_MODE=production")
	}
	secret, err := randomSecret()
	if err != nil {
		return nil, err
	}
	a.Add("local-dev", RoleAdmin, secret)
	a.DevKey = secret
	return a, nil
}

func randomSecret() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "mrr_" + hex.EncodeToString(b[:]), nil
}

func (a *Authenticator) Add(name string, role Role, secret string) *Key {
	h := sha256.Sum256([]byte(secret))
	k := &Key{
		ID: hex.EncodeToString(h[:6]), Name: name, Role: role, hash: h,
		Created: time.Now().UTC(), Prefix: safePrefix(secret),
	}
	a.mu.Lock()
	a.keys[hex.EncodeToString(h[:])] = k
	a.mu.Unlock()
	return k
}

func safePrefix(secret string) string {
	if len(secret) <= 8 {
		return "…"
	}
	return secret[:8] + "…"
}

// Lookup resolves a presented secret.
//
// The map lookup is on the full SHA-256, so it is already constant-time with
// respect to the secret's content; the extra ConstantTimeCompare guards the
// (theoretical) case of a hash collision being used to probe. Belt and braces
// is cheap here and the alternative -- a linear scan with ==  -- is a textbook
// timing oracle.
func (a *Authenticator) Lookup(secret string) (*Key, bool) {
	h := sha256.Sum256([]byte(secret))
	a.mu.RLock()
	k, ok := a.keys[hex.EncodeToString(h[:])]
	a.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if subtle.ConstantTimeCompare(k.hash[:], h[:]) != 1 {
		return nil, false
	}
	return k, true
}

func (a *Authenticator) List() []*Key {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]*Key, 0, len(a.keys))
	for _, k := range a.keys {
		out = append(out, k)
	}
	return out
}

// credential extracts the presented secret from a request.
//
// Three carriers are accepted, in decreasing order of preference. The query
// parameter exists only because the browser WebSocket API cannot set headers;
// it is accepted ONLY on the WebSocket route and the value is never logged.
func credential(r *http.Request, allowQuery bool) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	if h := r.Header.Get("X-API-Key"); h != "" {
		return strings.TrimSpace(h)
	}
	if allowQuery {
		return r.URL.Query().Get("key")
	}
	return ""
}

// ------------------------------------------------------------ rate limit ---

// bucket is a token bucket. Refill is computed lazily from elapsed time rather
// than by a background goroutine: one goroutine per client is a memory leak
// waiting for a client that never disconnects cleanly.
type bucket struct {
	tokens float64
	last   time.Time
}

type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens per second
	burst   float64
	lastGC  time.Time
}

func NewRateLimiter(ratePerSec, burst float64) *RateLimiter {
	return &RateLimiter{
		buckets: make(map[string]*bucket), rate: ratePerSec, burst: burst,
		lastGC: time.Now(),
	}
}

// Allow consumes a token for the given identity.
func (l *RateLimiter) Allow(id string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastGC) > 5*time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.lastGC = now
	}

	b, ok := l.buckets[id]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[id] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func clientIP(r *http.Request) string {
	// X-Forwarded-For is honoured only for its first entry, and only because
	// this service is expected to run behind an ingress. It is used for rate
	// limiting, never for authorisation.
	if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
		if i := strings.IndexByte(xf, ','); i > 0 {
			return strings.TrimSpace(xf[:i])
		}
		return strings.TrimSpace(xf)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------------------------------------------------------------- audit ----

// AuditEntry records a state-changing action.
type AuditEntry struct {
	At      time.Time `json:"at"`
	Actor   string    `json:"actor"`
	Role    string    `json:"role"`
	IP      string    `json:"ip"`
	Method  string    `json:"method"`
	Path    string    `json:"path"`
	Target  string    `json:"target,omitempty"`
	Detail  string    `json:"detail,omitempty"`
	Status  int       `json:"status"`
	Latency float64   `json:"latencyMs"`
}

// AuditLog is a bounded in-memory ring plus a structured log line per entry.
//
// Bounded because an audit trail that can exhaust memory is a denial of
// service; the structured line is the durable copy, shipped to Loki or
// whatever the deployment actually retains. Keeping a short in-memory window
// as well means the admin UI can show recent activity without a log query.
type AuditLog struct {
	mu   sync.Mutex
	buf  []AuditEntry
	head int
	n    int
}

func NewAuditLog(capacity int) *AuditLog {
	if capacity < 16 {
		capacity = 16
	}
	return &AuditLog{buf: make([]AuditEntry, capacity)}
}

func (a *AuditLog) Add(e AuditEntry) {
	a.mu.Lock()
	a.buf[a.head] = e
	a.head = (a.head + 1) % len(a.buf)
	if a.n < len(a.buf) {
		a.n++
	}
	a.mu.Unlock()
}

// Recent returns entries newest first.
func (a *AuditLog) Recent(limit int) []AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	if limit <= 0 || limit > a.n {
		limit = a.n
	}
	out := make([]AuditEntry, 0, limit)
	for i := 0; i < limit; i++ {
		idx := (a.head - 1 - i + len(a.buf)*2) % len(a.buf)
		out = append(out, a.buf[idx])
	}
	return out
}
