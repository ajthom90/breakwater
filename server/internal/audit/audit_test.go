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
	// Frozen shape: seven newline-terminated fields after prev_hash.
	got := audit.CanonicalEncoding("id1", "ts1", "act", "agent", "machine.enroll", "tgt", `{"a":1}`)
	want := "id1\nts1\nact\nagent\nmachine.enroll\ntgt\n{\"a\":1}\n"
	if got != want {
		t.Fatalf("canonical encoding changed (compatibility break):\n got %q\nwant %q", got, want)
	}
	h := audit.ComputeRowHash("", "id1", "ts1", "act", "agent", "machine.enroll", "tgt", `{"a":1}`)
	if len(h) != 64 {
		t.Fatalf("row_hash hex len=%d want 64", len(h))
	}
}
