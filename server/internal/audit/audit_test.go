package audit_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ajthom90/breakwater/server/internal/audit"
	"github.com/ajthom90/breakwater/server/internal/catalog"
)

func TestAppendVerifyTamperDetect(t *testing.T) {
	ctx := context.Background()
	db, err := catalog.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	w := audit.NewWriter(db)

	for i := 0; i < 5; i++ {
		if err := w.Append(ctx, audit.Event{
			Actor:     fmt.Sprintf("actor-%d", i),
			ActorType: audit.ActorAgent,
			Action:    audit.ActionMachineEnroll,
			Target:    fmt.Sprintf("machine-%d", i),
			Detail:    map[string]any{"i": i, "outcome": "success"},
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	if err := w.VerifyChain(ctx); err != nil {
		t.Fatalf("fresh chain must verify: %v", err)
	}

	// Tamper with middle row via raw SQL.
	var midID string
	err = db.SQL().QueryRow(`SELECT id FROM audit_events ORDER BY rowid ASC LIMIT 1 OFFSET 2`).Scan(&midID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.SQL().Exec(`UPDATE audit_events SET detail_json = '{"tampered":true}' WHERE id = ?`, midID)
	if err != nil {
		t.Fatal(err)
	}

	verr := w.VerifyChain(ctx)
	if verr == nil {
		t.Fatal("expected chain break after tamper")
	}
	br, ok := verr.(*audit.ChainBreak)
	if !ok {
		t.Fatalf("expected *ChainBreak, got %T: %v", verr, verr)
	}
	if br.ID != midID {
		t.Fatalf("break id=%s want %s", br.ID, midID)
	}
	if br.Index != 2 {
		t.Fatalf("break index=%d want 2", br.Index)
	}
	if br.Reason != "row_hash mismatch" {
		t.Fatalf("reason=%q", br.Reason)
	}
	t.Logf("tamper detected: %v", br)
}

func TestChainConcurrentAppends(t *testing.T) {
	ctx := context.Background()
	db, err := catalog.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	w := audit.NewWriter(db)
	const n = 32
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := w.Append(ctx, audit.Event{
				Actor:     fmt.Sprintf("fp-%d", i),
				ActorType: audit.ActorAgent,
				Action:    audit.ActionMachineEnroll,
				Target:    fmt.Sprintf("m-%d", i),
				Detail:    map[string]any{"n": i},
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent append: %v", err)
	}

	if err := w.VerifyChain(ctx); err != nil {
		t.Fatalf("chain invalid after concurrent appends: %v", err)
	}

	var count int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM audit_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != n {
		t.Fatalf("count=%d want %d", count, n)
	}
	t.Logf("concurrent %d appends: chain OK", n)
}

func TestCanonicalEncodingSurface(t *testing.T) {
	// Length-prefixed fields (S1-F2). Format: <decimal-len>:<bytes> per field, in order.
	got := audit.CanonicalEncoding("id1", "ts1", "act", "agent", "machine.enroll", "tgt", `{"a":1}`)
	// len("machine.enroll") == 14
	want := "3:id1" + "3:ts1" + "3:act" + "5:agent" + "14:machine.enroll" + "3:tgt" + "7:{\"a\":1}"
	if got != want {
		t.Fatalf("canonical encoding changed (compatibility break):\n got %q\nwant %q", got, want)
	}
	h := audit.ComputeRowHash("", "id1", "ts1", "act", "agent", "machine.enroll", "tgt", `{"a":1}`)
	if len(h) != 64 {
		t.Fatalf("row_hash hex len=%d want 64", len(h))
	}
}

// oldNewlineCanonical is the bc65f8a newline-delimited encoding (S1-F2).
// Kept only to prove the ambiguity the length-prefix encoding closes.
func oldNewlineCanonical(id, ts, actor, actorType, action, target, detailJSON string) string {
	return id + "\n" +
		ts + "\n" +
		actor + "\n" +
		actorType + "\n" +
		action + "\n" +
		target + "\n" +
		detailJSON + "\n"
}

// TestCanonicalEncoding_NoAmbiguity is S1-F2: two distinct field tuples that
// collide under the old newline-delimited encoding must produce different
// row hashes under the length-prefixed encoding.
func TestCanonicalEncoding_NoAmbiguity(t *testing.T) {
	// Collision under old encoding: action="x\ny", target="z" vs action="x", target="y\nz"
	// both yield: id\nts\nactor\ntype\nx\ny\nz\n{}\n
	aID, aTS, aActor, aType := "id", "ts", "actor", "type"
	aDetail := "{}"
	old1 := oldNewlineCanonical(aID, aTS, aActor, aType, "x\ny", "z", aDetail)
	old2 := oldNewlineCanonical(aID, aTS, aActor, aType, "x", "y\nz", aDetail)
	if old1 != old2 {
		t.Fatalf("test fixture broken: old encodings must collide\n 1=%q\n 2=%q", old1, old2)
	}
	t.Logf("old encoding collides as expected (len=%d)", len(old1))

	// New encoding must distinguish them.
	new1 := audit.CanonicalEncoding(aID, aTS, aActor, aType, "x\ny", "z", aDetail)
	new2 := audit.CanonicalEncoding(aID, aTS, aActor, aType, "x", "y\nz", aDetail)
	if new1 == new2 {
		t.Fatalf("length-prefixed encoding still collides:\n 1=%q\n 2=%q", new1, new2)
	}
	h1 := audit.ComputeRowHash("", aID, aTS, aActor, aType, "x\ny", "z", aDetail)
	h2 := audit.ComputeRowHash("", aID, aTS, aActor, aType, "x", "y\nz", aDetail)
	if h1 == h2 {
		t.Fatalf("row hashes collide under new encoding: %s", h1)
	}
	t.Logf("new encoding distinguishes: h1=%s… h2=%s…", h1[:16], h2[:16])
}
