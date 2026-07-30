package token_test

import (
	"testing"

	"github.com/ajthom90/breakwater/agent/internal/token"
)

func TestParse(t *testing.T) {
	raw := "BW1:127.0.0.1:9443:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef:secret-xyz"
	tok, err := token.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if tok.HostPort != "127.0.0.1:9443" {
		t.Fatalf("hostPort=%q", tok.HostPort)
	}
	if len(tok.ServerFP) != 64 {
		t.Fatalf("fp len=%d", len(tok.ServerFP))
	}
	if tok.Secret != "secret-xyz" {
		t.Fatalf("secret=%q", tok.Secret)
	}
}

func TestParse_IPv6(t *testing.T) {
	fp := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	raw := "BW1:[::1]:9443:" + fp + ":s"
	tok, err := token.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if tok.HostPort != "[::1]:9443" {
		t.Fatalf("hostPort=%q", tok.HostPort)
	}
}

func TestParse_Rejects(t *testing.T) {
	for _, raw := range []string{"", "XX1:a:b:c", "BW1:only", "BW1:h:shortfp:s"} {
		if _, err := token.Parse(raw); err == nil {
			t.Fatalf("expected error for %q", raw)
		}
	}
}
