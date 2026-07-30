// Package enroll implements one-time enrollment tokens and the enroll RPC logic.
//
// Token format: BW1:<host:port>:<serverCertFP-sha256>:<secret>
// Server fingerprint travels INSIDE the token (zero TOFU).
// Tokens are single-use, 24h TTL.
package enroll

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

const (
	// TokenPrefix is the wire prefix for enrollment tokens.
	TokenPrefix = "BW1"
	// DefaultTTL is the default enrollment token lifetime.
	DefaultTTL = 24 * time.Hour
)

// Token is a parsed enrollment token.
type Token struct {
	HostPort  string // host:port of agent gRPC endpoint
	ServerFP  string // SHA-256 hex of server cert
	Secret    string // single-use secret
	Raw       string
}

// Mint creates a new token string and returns (rawToken, secret, error).
// The secret alone is what gets hashed into the catalog; the full raw form
// is given to the operator / installer.
func Mint(hostPort, serverFP string) (raw string, secret string, err error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	secret = base64.RawURLEncoding.EncodeToString(b[:])
	serverFP = strings.ToLower(strings.TrimSpace(serverFP))
	raw = fmt.Sprintf("%s:%s:%s:%s", TokenPrefix, hostPort, serverFP, secret)
	return raw, secret, nil
}

// Parse validates and splits a token string.
func Parse(raw string) (*Token, error) {
	// Format: BW1:<host:port>:<fp>:<secret>
	// host:port may contain colons (IPv6) — parse carefully from the right.
	if !strings.HasPrefix(raw, TokenPrefix+":") {
		return nil, fmt.Errorf("invalid token prefix")
	}
	rest := raw[len(TokenPrefix)+1:]
	// Find last two colons for fp and secret.
	// secret is last field; fp is second-to-last; hostPort is the remainder.
	last := strings.LastIndex(rest, ":")
	if last < 0 {
		return nil, fmt.Errorf("invalid token format")
	}
	secret := rest[last+1:]
	rest = rest[:last]
	last = strings.LastIndex(rest, ":")
	if last < 0 {
		return nil, fmt.Errorf("invalid token format")
	}
	fp := rest[last+1:]
	hostPort := rest[:last]
	if hostPort == "" || fp == "" || secret == "" {
		return nil, fmt.Errorf("invalid token fields")
	}
	if len(fp) != 64 {
		return nil, fmt.Errorf("invalid server fingerprint length")
	}
	return &Token{
		HostPort: hostPort,
		ServerFP: strings.ToLower(fp),
		Secret:   secret,
		Raw:      raw,
	}, nil
}
