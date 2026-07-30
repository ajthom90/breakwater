package contentid_test

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestKopiaConfinement enforces the M2 stage-3 carve-out across the workspace:
// only pkg/contentid may import kopia, and only repo/hashing + repo/splitter.
// Scans pkg, agent, and cli modules (S3-F7).
func TestKopiaConfinement(t *testing.T) {
	root := findWorkspaceRoot(t)
	var violations []string
	for _, mod := range []string{"pkg", "agent", "cli"} {
		modRoot := filepath.Join(root, mod)
		if _, err := os.Stat(modRoot); err != nil {
			continue
		}
		_ = filepath.Walk(modRoot, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			// Skip generated protobuf (comments may mention the engine).
			if strings.HasSuffix(path, ".pb.go") {
				return nil
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			sc := bufio.NewScanner(f)
			inImport := false
			rel, _ := filepath.Rel(root, path)
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
				isImportLine := inImport || strings.HasPrefix(line, "import ")
				if !isImportLine {
					continue
				}
				if !strings.Contains(line, "github.com/kopia/kopia") {
					continue
				}
				// Only pkg/contentid may import, and only hashing/splitter.
				if strings.HasPrefix(rel, filepath.Join("pkg", "contentid")) {
					if strings.Contains(line, "repo/hashing") || strings.Contains(line, "repo/splitter") {
						continue
					}
				}
				violations = append(violations, rel+": "+line)
			}
			return sc.Err()
		})
	}
	if len(violations) > 0 {
		t.Fatalf("kopia confinement violations:\n  %s", strings.Join(violations, "\n  "))
	}
}

func findWorkspaceRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := wd
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		if _, err := os.Stat(filepath.Join(dir, "pkg", "go.mod")); err == nil {
			// contentid test cwd is pkg/contentid → parent pkg → parent root
			if filepath.Base(dir) == "pkg" {
				return filepath.Dir(dir)
			}
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("workspace root not found from %s", wd)
	return ""
}
