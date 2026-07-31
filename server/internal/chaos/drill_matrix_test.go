package chaos_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestChaosMatrix_Index documents the full PLAN chaos matrix in one place and
// wires drills #6 and #7 by reference (already proven elsewhere) so the matrix
// is complete without duplicating tests.
func TestChaosMatrix_Index(t *testing.T) {
	// #6 — token reuse / unknown cert / server-cert-swap (M1)
	// Evidence: server/internal/agentgw/enroll_mtls_test.go
	//   TestM1_EnrollmentAndWrongCertRejection
	m1 := findRepoFile(t, "server/internal/agentgw/enroll_mtls_test.go")
	raw, err := os.ReadFile(m1)
	if err != nil {
		t.Fatal(err)
	}
	m1src := string(raw)
	for _, need := range []string{
		"TestM1_EnrollmentAndWrongCertRejection",
		"wrong-cert",
		"token reuse",
		"server cert pin",
	} {
		if !strings.Contains(m1src, need) {
			t.Errorf("drill #6 reference missing evidence %q in %s", need, m1)
		}
	}
	t.Log("chaos#6 BY REFERENCE: TestM1_EnrollmentAndWrongCertRejection (enroll + wrong-cert + token reuse + pin mismatch)")

	// #7 — compromised agent cannot delete/prune
	// Evidence: server/internal/agentgw/m5_boundary_test.go
	m5 := findRepoFile(t, "server/internal/agentgw/m5_boundary_test.go")
	raw, err = os.ReadFile(m5)
	if err != nil {
		t.Fatal(err)
	}
	m5src := string(raw)
	if !strings.Contains(m5src, "TestM5_AgentHasNoForgetOrPrunePath") {
		t.Error("drill #7 reference missing TestM5_AgentHasNoForgetOrPrunePath")
	}
	t.Log("chaos#7 BY REFERENCE: TestM5_AgentHasNoForgetOrPrunePath (append-only :9443)")

	// Matrix coverage table (for human readers of -v output).
	type row struct {
		n, name, test, platform string
	}
	rows := []row{
		{"#1", "kill agent mid-upload", "TestChaos01_AgentKilledMidUpload", "Linux half; VSS leak → Windows/M3"},
		{"#2", "server killed mid-upload", "TestChaos02_ServerKilledMidUpload", "Linux"},
		{"#3", "network partition mid-backup", "TestChaos03_NetworkPartitionMidBackup", "Linux"},
		{"#4", "server ENOSPC", "TestChaos04_ENOSPC", "Linux/darwin tiny FS"},
		{"#5", "agent clock ±3d", "TestChaos05_AgentClockSkew", "Linux"},
		{"#6", "token/cert pinning", "TestM1_EnrollmentAndWrongCertRejection", "by ref"},
		{"#7", "agent cannot prune", "TestM5_AgentHasNoForgetOrPrunePath", "by ref"},
		{"#8", "bit-flip pack → scrub", "TestChaos08_BitFlipPack", "Linux"},
		{"#9", "missed backup watchdog", "TestChaos09_MissedBackupWatchdog", "Linux"},
		{"#10", "kill -9 fuzz backup+prune", "TestChaos10_Kill9Fuzz + ProcessKill9", "Linux"},
	}
	for _, r := range rows {
		t.Logf("MATRIX %s %-32s → %s (%s)", r.n, r.name, r.test, r.platform)
	}
}

func findRepoFile(t *testing.T, rel string) string {
	t.Helper()
	// Walk up from this source file to repo root.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	for i := 0; i < 8; i++ {
		cand := filepath.Join(dir, rel)
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
		// Also try when cwd is server/
		cand2 := filepath.Join(dir, "..", "..", "..", rel)
		if _, err := os.Stat(cand2); err == nil {
			return cand2
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	// Fallback: relative to cwd (go test from server/)
	if _, err := os.Stat(rel); err == nil {
		return rel
	}
	if _, err := os.Stat(filepath.Join("..", "..", rel)); err == nil {
		return filepath.Join("..", "..", rel)
	}
	t.Fatalf("cannot find %s", rel)
	return ""
}
