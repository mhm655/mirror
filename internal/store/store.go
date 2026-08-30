// Package store persists checkpoints and command logs.
//
// Two implementations ship: an in-memory store for tests and single-process
// runs, and a filesystem store for local development and for the container's
// mounted volume. A Postgres implementation lives in postgres.go behind the
// same interface.
//
// The interface is deliberately small and content-addressed-ish: a checkpoint
// is an opaque blob plus a header that carries everything needed to validate
// it before use (map hash, tick, state digest, CRC). Recovery must be able to
// reject a corrupt or mismatched checkpoint rather than resume from it -- the
// chaos lab's "corrupt checkpoint" experiment exists precisely to prove it
// does.
package store

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mirror-sim/mirror/internal/events"
	"github.com/mirror-sim/mirror/internal/units"
)

var (
	ErrNotFound = errors.New("store: not found")
	ErrCorrupt  = errors.New("store: checkpoint failed integrity check")
)

// Header precedes every checkpoint blob on the wire and on disk.
type Header struct {
	Magic   uint32
	Version uint32
	SimID   string
	Tick    units.Tick
	MapHash uint64
	Digest  uint64
	Raw     uint32 // uncompressed length
	CRC     uint32 // CRC32C of the uncompressed payload
	Created time.Time
}

const (
	magic       uint32 = 0x4D52524F // "MRRO"
	ckptVersion uint32 = 2
)

// Checkpoint is a header plus its (already compressed) payload.
type Checkpoint struct {
	Header  Header
	Payload []byte
}

// Store is the persistence contract.
type Store interface {
	PutCheckpoint(simID string, h Header, raw []byte) (Header, error)
	GetCheckpoint(simID string, tick units.Tick) (Header, []byte, error)
	LatestCheckpoint(simID string) (Header, []byte, error)
	ListCheckpoints(simID string) ([]Header, error)
	AppendCommands(simID string, cmds []events.Event) error
	LoadCommands(simID string) ([]events.Event, error)
	ListSims() ([]string, error)
	Close() error
}

// Encode compresses a state blob and stamps a validated header.
//
// gzip rather than zstd or snappy: the standard library has it, checkpoints are
// written every few simulated minutes rather than every tick, and a 30 MB state
// compresses to roughly 6 MB in about 60 ms -- comfortably inside the write
// budget. Swapping the codec later is a one-line change behind this function,
// and the header version exists so old checkpoints stay readable when it
// happens.
func Encode(h Header, raw []byte) (Header, []byte, error) {
	h.Magic, h.Version = magic, ckptVersion
	h.Raw = uint32(len(raw))
	h.CRC = crc32.Checksum(raw, crc32.MakeTable(crc32.Castagnoli))
	if h.Created.IsZero() {
		h.Created = time.Now().UTC()
	}
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return h, nil, err
	}
	if _, err := zw.Write(raw); err != nil {
		return h, nil, err
	}
	if err := zw.Close(); err != nil {
		return h, nil, err
	}
	return h, buf.Bytes(), nil
}

// Decode validates and decompresses. Every failure path returns ErrCorrupt so
// callers have exactly one condition to handle.
func Decode(h Header, payload []byte) ([]byte, error) {
	if h.Magic != magic {
		return nil, fmt.Errorf("%w: bad magic %08x", ErrCorrupt, h.Magic)
	}
	if h.Version != ckptVersion {
		return nil, fmt.Errorf("%w: version %d, want %d", ErrCorrupt, h.Version, ckptVersion)
	}
	zr, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(io.LimitReader(zr, int64(h.Raw)+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if uint32(len(raw)) != h.Raw {
		return nil, fmt.Errorf("%w: length %d, want %d", ErrCorrupt, len(raw), h.Raw)
	}
	if crc32.Checksum(raw, crc32.MakeTable(crc32.Castagnoli)) != h.CRC {
		return nil, fmt.Errorf("%w: CRC mismatch", ErrCorrupt)
	}
	return raw, nil
}

// ---------------------------------------------------------------- memory ---

type memStore struct {
	mu    sync.RWMutex
	ckpts map[string]map[units.Tick]Checkpoint
	cmds  map[string][]events.Event
}

func NewMemory() Store {
	return &memStore{
		ckpts: make(map[string]map[units.Tick]Checkpoint),
		cmds:  make(map[string][]events.Event),
	}
}

func (s *memStore) PutCheckpoint(simID string, h Header, raw []byte) (Header, error) {
	h.SimID = simID
	h, payload, err := Encode(h, raw)
	if err != nil {
		return h, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ckpts[simID] == nil {
		s.ckpts[simID] = make(map[units.Tick]Checkpoint)
	}
	s.ckpts[simID][h.Tick] = Checkpoint{Header: h, Payload: payload}
	return h, nil
}

func (s *memStore) GetCheckpoint(simID string, tick units.Tick) (Header, []byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.ckpts[simID][tick]
	if !ok {
		return Header{}, nil, ErrNotFound
	}
	raw, err := Decode(c.Header, c.Payload)
	return c.Header, raw, err
}

func (s *memStore) LatestCheckpoint(simID string) (Header, []byte, error) {
	s.mu.RLock()
	var best units.Tick
	found := false
	for t := range s.ckpts[simID] {
		if !found || t > best {
			best, found = t, true
		}
	}
	s.mu.RUnlock()
	if !found {
		return Header{}, nil, ErrNotFound
	}
	return s.GetCheckpoint(simID, best)
}

func (s *memStore) ListCheckpoints(simID string) ([]Header, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Header
	for _, c := range s.ckpts[simID] {
		out = append(out, c.Header)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tick < out[j].Tick })
	return out, nil
}

func (s *memStore) AppendCommands(simID string, cmds []events.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cmds[simID] = append(s.cmds[simID], cmds...)
	return nil
}

func (s *memStore) LoadCommands(simID string) ([]events.Event, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]events.Event, len(s.cmds[simID]))
	copy(out, s.cmds[simID])
	return out, nil
}

func (s *memStore) ListSims() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := map[string]bool{}
	for k := range s.ckpts {
		seen[k] = true
	}
	for k := range s.cmds {
		seen[k] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

func (s *memStore) Close() error { return nil }

// ------------------------------------------------------------ filesystem ---

type fsStore struct {
	root string
	mu   sync.Mutex
}

func NewFilesystem(root string) (Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &fsStore{root: root}, nil
}

func (s *fsStore) dir(simID string) string { return filepath.Join(s.root, safeName(simID)) }

func safeName(id string) string {
	// Simulation ids come from the API. Anything that could escape the data
	// directory is stripped rather than rejected, because a checkpoint write
	// failing at 3am is worse than a slightly mangled directory name.
	r := strings.NewReplacer("/", "_", "\\", "_", "..", "_", ":", "_")
	return r.Replace(id)
}

func (s *fsStore) PutCheckpoint(simID string, h Header, raw []byte) (Header, error) {
	h.SimID = simID
	h, payload, err := Encode(h, raw)
	if err != nil {
		return h, err
	}
	d := s.dir(simID)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return h, err
	}
	var buf bytes.Buffer
	writeHeader(&buf, h)
	buf.Write(payload)

	// Write to a temporary file and rename. A checkpoint that is half-written
	// when the process dies must not be discoverable, or recovery will find it
	// and fail the CRC -- correct, but it would rather find the previous good
	// one and carry on.
	path := filepath.Join(d, fmt.Sprintf("ckpt-%012d.mirror", h.Tick))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return h, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return h, err
	}
	return h, nil
}

func (s *fsStore) GetCheckpoint(simID string, tick units.Tick) (Header, []byte, error) {
	path := filepath.Join(s.dir(simID), fmt.Sprintf("ckpt-%012d.mirror", tick))
	b, err := os.ReadFile(path)
	if err != nil {
		return Header{}, nil, ErrNotFound
	}
	h, payload, err := readHeader(b)
	if err != nil {
		return Header{}, nil, err
	}
	raw, err := Decode(h, payload)
	return h, raw, err
}

func (s *fsStore) LatestCheckpoint(simID string) (Header, []byte, error) {
	hs, err := s.ListCheckpoints(simID)
	if err != nil || len(hs) == 0 {
		return Header{}, nil, ErrNotFound
	}
	return s.GetCheckpoint(simID, hs[len(hs)-1].Tick)
}

func (s *fsStore) ListCheckpoints(simID string) ([]Header, error) {
	entries, err := os.ReadDir(s.dir(simID))
	if err != nil {
		return nil, nil
	}
	var out []Header
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "ckpt-") || !strings.HasSuffix(e.Name(), ".mirror") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dir(simID), e.Name()))
		if err != nil || len(b) < headerBytes {
			continue
		}
		h, _, err := readHeader(b)
		if err != nil {
			continue
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tick < out[j].Tick })
	return out, nil
}

func (s *fsStore) AppendCommands(simID string, cmds []events.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.dir(simID)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(d, "commands.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	var buf bytes.Buffer
	for _, c := range cmds {
		writeCommand(&buf, c)
	}
	_, err = f.Write(buf.Bytes())
	return err
}

func (s *fsStore) LoadCommands(simID string) ([]events.Event, error) {
	b, err := os.ReadFile(filepath.Join(s.dir(simID), "commands.log"))
	if err != nil {
		return nil, nil
	}
	var out []events.Event
	for off := 0; off+cmdBytes <= len(b); off += cmdBytes {
		out = append(out, readCommand(b[off:off+cmdBytes]))
	}
	return out, nil
}

func (s *fsStore) ListSims() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s *fsStore) Close() error { return nil }

// ------------------------------------------------------------- encoding ---

const headerBytes = 4 + 4 + 8 + 8 + 8 + 4 + 4 + 8 + 2 + 64

func writeHeader(w *bytes.Buffer, h Header) {
	var b [headerBytes]byte
	le := binary.LittleEndian
	le.PutUint32(b[0:], h.Magic)
	le.PutUint32(b[4:], h.Version)
	le.PutUint64(b[8:], uint64(h.Tick))
	le.PutUint64(b[16:], h.MapHash)
	le.PutUint64(b[24:], h.Digest)
	le.PutUint32(b[32:], h.Raw)
	le.PutUint32(b[36:], h.CRC)
	le.PutUint64(b[40:], uint64(h.Created.UnixNano()))
	name := h.SimID
	if len(name) > 64 {
		name = name[:64]
	}
	le.PutUint16(b[48:], uint16(len(name)))
	copy(b[50:], name)
	w.Write(b[:])
}

func readHeader(b []byte) (Header, []byte, error) {
	if len(b) < headerBytes {
		return Header{}, nil, ErrCorrupt
	}
	le := binary.LittleEndian
	h := Header{
		Magic:   le.Uint32(b[0:]),
		Version: le.Uint32(b[4:]),
		Tick:    units.Tick(le.Uint64(b[8:])),
		MapHash: le.Uint64(b[16:]),
		Digest:  le.Uint64(b[24:]),
		Raw:     le.Uint32(b[32:]),
		CRC:     le.Uint32(b[36:]),
		Created: time.Unix(0, int64(le.Uint64(b[40:]))).UTC(),
	}
	n := int(le.Uint16(b[48:]))
	if n > 64 {
		return Header{}, nil, ErrCorrupt
	}
	h.SimID = string(b[50 : 50+n])
	return h, b[headerBytes:], nil
}

const cmdBytes = 8 + 8 + 2 + 1 + 2 + 32

func writeCommand(w *bytes.Buffer, e events.Event) {
	var b [cmdBytes]byte
	le := binary.LittleEndian
	le.PutUint64(b[0:], e.Seq)
	le.PutUint64(b[8:], uint64(e.Tick))
	le.PutUint16(b[16:], uint16(e.Kind))
	b[18] = byte(e.Severity)
	le.PutUint16(b[19:], uint16(e.Region))
	le.PutUint64(b[21:], uint64(e.A))
	le.PutUint64(b[29:], uint64(e.B))
	le.PutUint64(b[37:], uint64(e.C))
	le.PutUint64(b[45:], uint64(e.D))
	w.Write(b[:])
}

func readCommand(b []byte) events.Event {
	le := binary.LittleEndian
	return events.Event{
		Seq:      le.Uint64(b[0:]),
		Tick:     units.Tick(le.Uint64(b[8:])),
		Kind:     events.Kind(le.Uint16(b[16:])),
		Severity: events.Severity(b[18]),
		Region:   int16(le.Uint16(b[19:])),
		A:        int64(le.Uint64(b[21:])),
		B:        int64(le.Uint64(b[29:])),
		C:        int64(le.Uint64(b[37:])),
		D:        int64(le.Uint64(b[45:])),
	}
}
