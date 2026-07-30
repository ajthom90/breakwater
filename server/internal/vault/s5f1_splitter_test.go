package vault_test

import (
	"bytes"
	"context"
	"math/rand"
	"strconv"
	"testing"

	"github.com/ajthom90/breakwater/pkg/contentid"
	"github.com/ajthom90/breakwater/server/internal/vault"
)

// Known seeds from REVIEW-M2-S5 where TestS3F8's ordered VerifyObject compare
// diverged (map-iteration order, not true splitter mismatch).
var s5f1KnownOrderSeeds = []int64{11, 16, 26, 28, 36, 39}

// TestS5F1_SeededSplitterSequenceIdentity locks the PLAN have/want contract:
// pkg/contentid ChunkAndID sequence must equal server WriteObject(DYNAMIC)
// data-content sequence (stream order), and no chunk may exceed MaxPutContentBytes.
//
// S5-F1 root cause (not a splitter bug): kopia's VerifyObject returns backing
// content IDs from a map (non-deterministic order). The old TestS3F8 compared
// pkg sequence to VerifyObject order and failed ~1/6 of random runs. This test
// uses Vault.ObjectDataContentIDs (stream order via indirect index) instead.
//
// Red-first (against 9ab2831 VerifyObject-ordered compare): seeds 11,16,26,28,36,39
// failed intermittently / reliably depending on map iteration; captured in PROGRESS.
func TestS5F1_SeededSplitterSequenceIdentity(t *testing.T) {
	ctx := context.Background()
	mgr := vault.NewManager(t.TempDir(), t.TempDir())
	defer mgr.CloseAll(ctx)
	v, err := mgr.Create(ctx, "s5f1-split", "pw-s5f1")
	if err != nil {
		t.Fatal(err)
	}
	secret, algo, err := v.HashingKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h, err := contentid.New(algo, secret)
	if err != nil {
		t.Fatal(err)
	}

	// Seeds: known-bad-from-review + a denser band so flakiness cannot hide.
	seeds := make([]int64, 0, 40+len(s5f1KnownOrderSeeds))
	seen := map[int64]bool{}
	for _, s := range s5f1KnownOrderSeeds {
		seeds = append(seeds, s)
		seen[s] = true
	}
	for s := int64(1); s <= 40; s++ {
		if !seen[s] {
			seeds = append(seeds, s)
		}
	}

	const payloadSize = 10 << 20 // 10 MiB — multi-chunk for DYNAMIC-4M
	for _, seed := range seeds {
		seed := seed
		t.Run(strconv.FormatInt(seed, 10), func(t *testing.T) {
			payload := make([]byte, payloadSize)
			rng := rand.New(rand.NewSource(seed))
			if _, err := rng.Read(payload); err != nil {
				t.Fatal(err)
			}

			sp, err := contentid.NewSplitter(contentid.SplitterDynamic4M)
			if err != nil {
				t.Fatal(err)
			}
			defer sp.Close()

			chunks, pkgIDs, err := contentid.ChunkAndID(h, sp, payload)
			if err != nil {
				t.Fatal(err)
			}
			if len(pkgIDs) < 1 {
				t.Fatal("no chunks")
			}
			if len(pkgIDs) <= 1 {
				t.Fatalf("need multi-chunk for 10MiB, got %d", len(pkgIDs))
			}

			// MaxPutContentBytes headroom: DYNAMIC-4M max segment is 8 MiB;
			// every agent chunk must be uploadable via PutContents.
			for i, c := range chunks {
				if len(c) > vault.MaxPutContentBytes {
					t.Fatalf("seed %d chunk[%d] len=%d exceeds MaxPutContentBytes=%d",
						seed, i, len(c), vault.MaxPutContentBytes)
				}
				// PutContent path: agent-computed ID must match server hash of same bytes.
				sid, err := v.PutContent(ctx, c)
				if err != nil {
					t.Fatalf("seed %d PutContent chunk %d (len=%d): %v", seed, i, len(c), err)
				}
				if string(sid) != pkgIDs[i] {
					t.Fatalf("seed %d chunk %d hash diverge: pkg=%s put=%s", seed, i, pkgIDs[i], sid)
				}
			}

			oid, err := v.WriteObject(ctx, vault.SplitterDynamic, bytes.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}

			// Stream-order data content IDs (not VerifyObject map order).
			serverIDs, err := v.ObjectDataContentIDs(ctx, oid)
			if err != nil {
				t.Fatalf("ObjectDataContentIDs: %v", err)
			}
			if len(serverIDs) != len(pkgIDs) {
				t.Fatalf("seed %d content count pkg=%d server=%d pkgIDs=%v serverIDs=%v",
					seed, len(pkgIDs), len(serverIDs), pkgIDs, serverIDs)
			}
			for i := range pkgIDs {
				if pkgIDs[i] != string(serverIDs[i]) {
					t.Fatalf("seed %d SEQUENCE DIVERGENCE at chunk %d: pkg=%s server=%s (pkgN=%d serverN=%d)",
						seed, i, pkgIDs[i], serverIDs[i], len(pkgIDs), len(serverIDs))
				}
			}

			// VerifyObject set must match (unordered integrity check still holds).
			verifyIDs, err := v.VerifyObject(ctx, oid)
			if err != nil {
				t.Fatal(err)
			}
			dataSet := map[string]int{}
			for _, id := range verifyIDs {
				s := string(id)
				if len(s) > 0 && s[0] >= 'g' && s[0] <= 'z' {
					continue // index/metadata prefix
				}
				dataSet[s]++
			}
			for _, id := range pkgIDs {
				if dataSet[id] == 0 {
					t.Fatalf("seed %d pkg id %s missing from VerifyObject set", seed, id)
				}
				dataSet[id]--
			}
		})
	}
}

// TestS5F1_MaxSegmentEqualsMaxPutContentBytes documents that DYNAMIC-4M max
// segment is exactly MaxPutContentBytes (8 MiB) — PutContent accepts equality
// (guard is `>` not `>=`). Seed 16 of the sequence test produces an 8 MiB chunk.
func TestS5F1_MaxSegmentEqualsMaxPutContentBytes(t *testing.T) {
	sp, err := contentid.NewSplitter(contentid.SplitterDynamic4M)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	maxSeg := sp.MaxSegmentSize()
	if maxSeg != vault.MaxPutContentBytes {
		t.Fatalf("MaxSegmentSize=%d MaxPutContentBytes=%d — agent chunks at max would be rejected or oversized",
			maxSeg, vault.MaxPutContentBytes)
	}
}

// TestS5F1_VerifyObjectOrderIsNotStreamOrder documents why the old S3-F8
// ordered compare was flaky: VerifyObject uses a map tracker.
func TestS5F1_VerifyObjectOrderIsNotStreamOrder(t *testing.T) {
	ctx := context.Background()
	mgr := vault.NewManager(t.TempDir(), t.TempDir())
	defer mgr.CloseAll(ctx)
	v, err := mgr.Create(ctx, "order-doc", "pw")
	if err != nil {
		t.Fatal(err)
	}
	// Multi-chunk WriteObject so VerifyObject returns ≥2 data IDs + index.
	payload := make([]byte, 10<<20)
	rand.New(rand.NewSource(7)).Read(payload)
	oid, err := v.WriteObject(ctx, vault.SplitterDynamic, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	ordered, err := v.ObjectDataContentIDs(ctx, oid)
	if err != nil {
		t.Fatal(err)
	}
	if len(ordered) < 2 {
		t.Fatalf("need multi-chunk, got %d", len(ordered))
	}
	// Across several VerifyObject calls, order relative to stream may differ.
	// We only assert that the *set* matches and that at least one of a few
	// calls would have failed a naïve ordered compare (map iteration).
	mismatchSeen := false
	for attempt := 0; attempt < 20; attempt++ {
		verify, err := v.VerifyObject(ctx, oid)
		if err != nil {
			t.Fatal(err)
		}
		var data []string
		for _, id := range verify {
			s := string(id)
			if len(s) > 0 && s[0] >= 'g' && s[0] <= 'z' {
				continue
			}
			data = append(data, s)
		}
		if len(data) != len(ordered) {
			t.Fatalf("set size mismatch attempt %d", attempt)
		}
		// set equality
		set := map[string]int{}
		for _, s := range data {
			set[s]++
		}
		for _, id := range ordered {
			if set[string(id)] == 0 {
				t.Fatalf("ordered id missing from VerifyObject set: %s", id)
			}
			set[string(id)]--
		}
		for i := range ordered {
			if string(ordered[i]) != data[i] {
				mismatchSeen = true
				break
			}
		}
		if mismatchSeen {
			break
		}
	}
	if !mismatchSeen {
		// Map iteration can match stream order by chance for many runs; do not
		// fail the suite — the flaky S3-F8 failure rate was ~15%, so 20 tries
		// is usually enough. Log and skip if unlucky.
		t.Log("VerifyObject order happened to match stream order for 20 calls; set-equality still verified")
	} else {
		t.Log("confirmed: VerifyObject data ID order differs from stream order (S5-F1)")
	}
}
