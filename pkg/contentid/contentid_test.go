package contentid_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/ajthom90/breakwater/pkg/contentid"
	"github.com/kopia/kopia/repo/hashing"
	"golang.org/x/crypto/blake2b"
)

func TestHasher_BLAKE2B256128_MatchesKeyedBlake2b(t *testing.T) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	h, err := contentid.New(contentid.DefaultAlgorithm, secret)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("breakwater-contentid-unit-payload")
	got, err := h.ContentID(payload)
	if err != nil {
		t.Fatal(err)
	}
	// Independent reference: keyed blake2b-256 truncated to 16 → hex.
	ref, err := blake2b.New256(secret)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = ref.Write(payload)
	want := hex.EncodeToString(ref.Sum(nil)[:16])
	if got != want {
		t.Fatalf("ContentID=%s want %s", got, want)
	}
	if h.Algorithm() != hashing.DefaultAlgorithm {
		t.Fatalf("algo=%s", h.Algorithm())
	}
}

func TestHasher_RejectsEmpty(t *testing.T) {
	if _, err := contentid.New("", []byte("x")); err == nil {
		t.Fatal("expected error for empty algo")
	}
	if _, err := contentid.New(contentid.DefaultAlgorithm, nil); err == nil {
		t.Fatal("expected error for empty secret")
	}
	if _, err := contentid.New("NOT-A-REAL-HASH", []byte("secret-32-bytes-long-enough!!")); err == nil {
		t.Fatal("expected error for unknown algo")
	}
}

func TestSplitter_Dynamic4M_EmptyAndSmall(t *testing.T) {
	sp, err := contentid.NewSplitter(contentid.SplitterDynamic4M)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	if sp.MaxSegmentSize() != 8<<20 {
		t.Fatalf("MaxSegmentSize=%d want 8MiB", sp.MaxSegmentSize())
	}
	chunks := sp.SplitBytes(nil)
	if len(chunks) != 1 || len(chunks[0]) != 0 {
		t.Fatalf("empty: %+v", chunks)
	}
	small := []byte("hello")
	chunks = sp.SplitBytes(small)
	if len(chunks) != 1 || !bytes.Equal(chunks[0], small) {
		t.Fatalf("small: %v", chunks)
	}
}

func TestSplitter_Unknown(t *testing.T) {
	if _, err := contentid.NewSplitter("NO-SUCH-SPLITTER"); err == nil {
		t.Fatal("expected error")
	}
}

func TestChunkAndID(t *testing.T) {
	secret := bytes.Repeat([]byte{0xab}, 32)
	h, err := contentid.New(contentid.DefaultAlgorithm, secret)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := contentid.NewSplitter(contentid.SplitterDynamic4M)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	data := bytes.Repeat([]byte("chunk-me-please-"), 100)
	chunks, ids, err := contentid.ChunkAndID(h, sp, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != len(ids) || len(chunks) < 1 {
		t.Fatalf("chunks=%d ids=%d", len(chunks), len(ids))
	}
	for i, c := range chunks {
		want, _ := h.ContentID(c)
		if ids[i] != want {
			t.Fatalf("id[%d]=%s want %s", i, ids[i], want)
		}
	}
}
