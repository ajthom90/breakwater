// Package contentid computes Breakwater content IDs and splits file streams
// bit-identical to the server vault content layer (have/want contract).
//
// Standing-rule carve-out (PLAN / M2 stage 3): this package MAY import
// github.com/kopia/kopia/repo/hashing and .../repo/splitter (pure-Go). No other
// pkg/agent/cli package may import those modules; vault remains the only
// importer of repo/content/object/manifest/maintenance layers.
//
// Content ID string form matches the vault content ID for the empty prefix:
// lowercase hex of the truncated keyed hash (see R2-14).
package contentid

import (
	"encoding/hex"
	"fmt"
	"hash"
	"io"

	"github.com/kopia/kopia/repo/hashing"
	"github.com/kopia/kopia/repo/splitter"
	"golang.org/x/crypto/blake2b"
	"golang.org/x/crypto/blake2s"
)

// Default file splitter (PLAN: CDC for files).
const SplitterDynamic4M = "DYNAMIC-4M-BUZHASH"

// Default hashing algorithm when enrollment returns the vault default.
const DefaultAlgorithm = hashing.DefaultAlgorithm // BLAKE2B-256-128

// Hasher computes content IDs from (algorithm, secret) enrollment material.
type Hasher struct {
	algo   string
	secret []byte
	// newHash builds a fresh keyed hash; sum is truncated to hashLen bytes.
	newHash func() (hash.Hash, error)
	hashLen int
}

// New returns a Hasher for the given algorithm and HMAC/keying secret.
// Algorithm must be a supported hash name (e.g. BLAKE2B-256-128).
func New(algorithm string, secret []byte) (*Hasher, error) {
	if algorithm == "" {
		return nil, fmt.Errorf("contentid: algorithm required")
	}
	if len(secret) == 0 {
		return nil, fmt.Errorf("contentid: secret required")
	}
	// Validate algorithm is known (CreateHashFunc rejects unknowns).
	if _, err := hashing.CreateHashFunc(hashParams{algo: algorithm, secret: secret}); err != nil {
		return nil, fmt.Errorf("contentid: unknown algorithm %q: %w", algorithm, err)
	}

	h := &Hasher{algo: algorithm, secret: append([]byte(nil), secret...)}
	switch algorithm {
	case "BLAKE2B-256-128":
		h.hashLen = 16
		sec := h.secret
		h.newHash = func() (hash.Hash, error) { return blake2b.New256(sec) }
	case "BLAKE2B-256":
		h.hashLen = 32
		sec := h.secret
		h.newHash = func() (hash.Hash, error) { return blake2b.New256(sec) }
	case "BLAKE2S-128":
		h.hashLen = 16
		sec := h.secret
		h.newHash = func() (hash.Hash, error) { return blake2s.New128(sec) }
	case "BLAKE2S-256":
		h.hashLen = 32
		sec := h.secret
		h.newHash = func() (hash.Hash, error) { return blake2s.New256(sec) }
	default:
		// CreateHashFunc accepted it but we lack a pure-Go implementation that
		// avoids internal gather types. Fail clearly.
		return nil, fmt.Errorf("contentid: algorithm %q validated but not implemented in pkg (supported: BLAKE2B-256-128, BLAKE2B-256, BLAKE2S-128, BLAKE2S-256)", algorithm)
	}
	return h, nil
}

// Algorithm returns the hashing algorithm name.
func (h *Hasher) Algorithm() string { return h.algo }

// ContentID returns the vault-compatible content ID string for payload
// (empty-prefix: lowercase hex of the truncated keyed hash).
func (h *Hasher) ContentID(payload []byte) (string, error) {
	if h == nil || h.newHash == nil {
		return "", fmt.Errorf("contentid: nil hasher")
	}
	hh, err := h.newHash()
	if err != nil {
		return "", err
	}
	if _, err := hh.Write(payload); err != nil {
		return "", err
	}
	sum := hh.Sum(nil)
	if len(sum) < h.hashLen {
		return "", fmt.Errorf("contentid: hash too short")
	}
	return hex.EncodeToString(sum[:h.hashLen]), nil
}

// hashParams implements hashing.Parameters for CreateHashFunc validation only.
type hashParams struct {
	algo   string
	secret []byte
}

func (p hashParams) GetHashFunction() string { return p.algo }
func (p hashParams) GetHmacSecret() []byte   { return p.secret }

// ---------------------------------------------------------------------------
// Splitter
// ---------------------------------------------------------------------------

// Splitter splits a byte stream into content chunks using a named CDC/fixed splitter.
type Splitter struct {
	name string
	sp   splitter.Splitter
}

// NewSplitter returns a Splitter for the named algorithm (e.g. SplitterDynamic4M).
func NewSplitter(name string) (*Splitter, error) {
	if name == "" {
		name = SplitterDynamic4M
	}
	factory := splitter.GetFactory(name)
	if factory == nil {
		return nil, fmt.Errorf("contentid: unknown splitter %q", name)
	}
	return &Splitter{name: name, sp: factory()}, nil
}

// Name returns the splitter algorithm name.
func (s *Splitter) Name() string { return s.name }

// MaxSegmentSize is the largest chunk this splitter will emit.
func (s *Splitter) MaxSegmentSize() int {
	if s == nil || s.sp == nil {
		return 0
	}
	return s.sp.MaxSegmentSize()
}

// Close releases splitter resources.
func (s *Splitter) Close() {
	if s != nil && s.sp != nil {
		s.sp.Close()
	}
}

// SplitAll reads r fully and returns the chunks (copy of each segment).
// Prefer SplitAll for files that fit in memory; streaming helpers can be added later.
func (s *Splitter) SplitAll(r io.Reader) ([][]byte, error) {
	if s == nil || s.sp == nil {
		return nil, fmt.Errorf("contentid: nil splitter")
	}
	s.sp.Reset()
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return s.splitBytes(data), nil
}

// SplitBytes splits an in-memory buffer.
func (s *Splitter) SplitBytes(data []byte) [][]byte {
	if s == nil || s.sp == nil {
		return nil
	}
	s.sp.Reset()
	return s.splitBytes(data)
}

func (s *Splitter) splitBytes(data []byte) [][]byte {
	var chunks [][]byte
	var pending []byte
	for len(data) > 0 {
		n := s.sp.NextSplitPoint(data)
		if n < 0 {
			pending = append(pending, data...)
			break
		}
		pending = append(pending, data[:n]...)
		chunks = append(chunks, append([]byte(nil), pending...))
		pending = pending[:0]
		data = data[n:]
	}
	if len(pending) > 0 {
		chunks = append(chunks, append([]byte(nil), pending...))
	}
	// Empty input → one empty chunk (matches object writer flush of empty buffer).
	if len(chunks) == 0 {
		chunks = append(chunks, []byte{})
	}
	return chunks
}

// ChunkAndID splits data and returns (chunks, content IDs) using h.
func ChunkAndID(h *Hasher, s *Splitter, data []byte) (chunks [][]byte, ids []string, err error) {
	if h == nil || s == nil {
		return nil, nil, fmt.Errorf("contentid: nil hasher or splitter")
	}
	chunks = s.SplitBytes(data)
	ids = make([]string, len(chunks))
	for i, c := range chunks {
		id, err := h.ContentID(c)
		if err != nil {
			return nil, nil, err
		}
		ids[i] = id
	}
	return chunks, ids, nil
}
