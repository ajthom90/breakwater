package state_test

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/ajthom90/breakwater/agent/internal/identity"
	"github.com/ajthom90/breakwater/agent/internal/state"
)

func TestSaveLoadIdentity_Atomic(t *testing.T) {
	dir, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if dir.IsEnrolled() {
		t.Fatal("expected not enrolled")
	}

	creds, err := identity.Generate("test-host", 0)
	if err != nil {
		t.Fatal(err)
	}
	meta := &state.Identity{
		MachineID:        "01MACHINE000000000000000000",
		ServerAddr:       "127.0.0.1:9443",
		ServerFP:         "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		HashingAlgorithm: "BLAKE2B-256-128",
		HashingKeyB64:    base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
		Hostname:         "test-host",
	}
	if err := dir.SaveEnrolled(meta, creds); err != nil {
		t.Fatal(err)
	}
	if !dir.IsEnrolled() {
		t.Fatal("expected enrolled")
	}
	got, id, err := dir.LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if got.MachineID != meta.MachineID {
		t.Fatalf("machine_id=%s", got.MachineID)
	}
	if id.Fingerprint() != creds.Fingerprint() {
		t.Fatal("cert fingerprint mismatch")
	}
	key, err := got.HashingKey()
	if err != nil || len(key) != 32 {
		t.Fatalf("hashing key: %v len=%d", err, len(key))
	}
}

func TestIncompleteIdentityNotLoadable(t *testing.T) {
	// Only cert present — LoadIdentity must fail.
	tmp := t.TempDir()
	dir, err := state.Open(tmp)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := identity.Generate("x", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.Save(tmp, creds); err != nil {
		t.Fatal(err)
	}
	// No identity.json
	if _, _, err := dir.LoadIdentity(); err == nil {
		t.Fatal("expected error without identity.json")
	}
	// identity.json without certs
	_ = os.WriteFile(filepath.Join(tmp, "identity.json"), []byte(`{
		"machine_id":"m","server_addr":"a","server_fp":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"hashing_algorithm":"BLAKE2B-256-128","hashing_key_b64":"YQ=="
	}`), 0o600)
	_ = os.Remove(filepath.Join(tmp, "cert.pem"))
	_ = os.Remove(filepath.Join(tmp, "key.pem"))
	if _, _, err := dir.LoadIdentity(); err == nil {
		t.Fatal("expected error without certs")
	}
}

func TestCompletedJobs_Idempotency(t *testing.T) {
	dir, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if dir.HasCompleted("job-1") {
		t.Fatal("unexpected")
	}
	if err := dir.MarkCompleted("job-1"); err != nil {
		t.Fatal(err)
	}
	if !dir.HasCompleted("job-1") {
		t.Fatal("expected completed")
	}
	// Persist across reopen.
	dir2, err := state.Open(dir.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !dir2.HasCompleted("job-1") {
		t.Fatal("completed job lost after reopen")
	}
}

func TestCompletedJobs_RingBound(t *testing.T) {
	dir, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < state.MaxCompletedJobs+10; i++ {
		if err := dir.MarkCompleted(string(rune('a'+(i%26))) + "-" + itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	if dir.CompletedCount() > state.MaxCompletedJobs {
		t.Fatalf("count=%d > max", dir.CompletedCount())
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}
