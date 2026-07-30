package contentid_test

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestKopiaConfinement_PkgOnlyContentID enforces the M2 stage-3 standing-rule
// carve-out: only pkg/contentid may import kopia, and only repo/hashing +
// repo/splitter (not repo/content, repo/object, etc.).
func TestKopiaConfinement_PkgOnlyContentID(t *testing.T) {
	pkgRoot := findPkgRoot(t)
	var violations []string
	err := filepath.Walk(pkgRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Tests in contentid may mention kopia for documentation; still scan imports.
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		inImport := false
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "import (") {
				inImport = true
				continue
			}
			if inImport && line == ")" {
				inImport = false
				continue
			}
			if !inImport && !strings.HasPrefix(line, "import ") {
				continue
			}
			if !strings.Contains(line, "github.com/kopia/kopia") {
				continue
			}
			rel, _ := filepath.Rel(pkgRoot, path)
			// Only contentid package may import kopia, and only hashing/splitter.
			if !strings.HasPrefix(rel, "contentid"+string(filepath.Separator)) && rel != "contentid" {
				violations = append(violations, rel+": "+line)
				continue
			}
			if strings.Contains(line, "repo/hashing") || strings.Contains(line, "repo/splitter") {
				continue
			}
			// contentid tests may import nothing else; production contentid only hashing+splitter.
			if strings.HasSuffix(rel, "_test.go") {
				// Allow test-only mentions if they stay on hashing (already covered).
				violations = append(violations, rel+": non-carve-out kopia import: "+line)
				continue
			}
			violations = append(violations, rel+": non-carve-out kopia import: "+line)
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Fatalf("kopia confinement violations in pkg/:\n  %s", strings.Join(violations, "\n  "))
	}
}

func findPkgRoot(t *testing.T) string {
	t.Helper()
	// contentid_test runs with cwd = pkg/contentid or via go test from pkg.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Walk up looking for pkg/go.mod
	dir := wd
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			// If we're in contentid, parent is pkg.
			if filepath.Base(dir) == "contentid" {
				return filepath.Dir(dir)
			}
			// If go.mod module is breakwater/pkg
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("could not find pkg root from %s", wd)
	return ""
}
