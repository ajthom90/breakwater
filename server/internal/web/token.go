package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// APITokenFileName is the basename under dataDir for the dev-only API token.
const APITokenFileName = "api-token"

// LoadOrCreateAPIToken returns the contents of <dataDir>/api-token, creating a
// cryptographically random 32-byte hex token (0600) on first boot.
//
// The full token is NEVER logged. Callers should log only TokenPreview.
func LoadOrCreateAPIToken(dataDir string, log *slog.Logger) (string, error) {
	if log == nil {
		log = slog.Default()
	}
	path := filepath.Join(dataDir, APITokenFileName)
	b, err := os.ReadFile(path)
	if err == nil {
		tok := strings.TrimSpace(string(b))
		if tok == "" {
			return "", fmt.Errorf("api-token file is empty: %s", path)
		}
		log.Info("api token loaded", "path", path, "preview", TokenPreview(tok))
		return tok, nil
	}
	if !os.IsNotExist(err) {
		return "", fmt.Errorf("read api-token: %w", err)
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate api-token: %w", err)
	}
	tok := hex.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write api-token: %w", err)
	}
	// First-boot setup: print preview like PLAN's admin-setup token.
	// Operators read the full token from the file on the host.
	log.Info("dev API token generated (M2 placeholder; real sessions in M6)",
		"path", path,
		"preview", TokenPreview(tok),
		"hint", "read full token from file; Authorization: Bearer <token>")
	return tok, nil
}

// TokenPreview returns a log-safe truncation (first 8 hex chars + "…").
// Never pass the full token to log fields.
func TokenPreview(tok string) string {
	if len(tok) <= 8 {
		return "…"
	}
	return tok[:8] + "…"
}

// ConstantTimeTokenEqual compares bearer tokens without leaking length via timing
// of the comparison itself (still rejects empty).
func ConstantTimeTokenEqual(got, want string) bool {
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
