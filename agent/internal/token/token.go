// Package token parses Breakwater enrollment tokens.
// Format: BW1:<host:port>:<serverCertFP-sha256>:<secret>
// Server fingerprint travels inside the token (zero TOFU).
package token

import (
	"fmt"
	"strings"
)

// Prefix is the wire prefix for enrollment tokens.
const Prefix = "BW1"

// Token is a parsed enrollment token.
type Token struct {
	HostPort string // host:port of agent gRPC endpoint
	ServerFP string // SHA-256 hex of server cert (lowercase)
	Secret   string // single-use secret
	Raw      string
}

// Parse validates and splits a token string.
// host:port may contain colons (IPv6) — fields are split from the right.
func Parse(raw string) (*Token, error) {
	if !strings.HasPrefix(raw, Prefix+":") {
		return nil, fmt.Errorf("invalid token prefix")
	}
	rest := raw[len(Prefix)+1:]
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
