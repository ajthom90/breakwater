package enroll_test

import (
	"strings"
	"testing"

	"github.com/ajthom90/breakwater/server/internal/enroll"
)

func TestTokenMintParse(t *testing.T) {
	fp := strings.Repeat("ab", 32)
	raw, secret, err := enroll.Mint("192.168.1.10:9443", fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, "BW1:") {
		t.Fatalf("prefix: %s", raw)
	}
	tok, err := enroll.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if tok.HostPort != "192.168.1.10:9443" {
		t.Fatalf("host: %s", tok.HostPort)
	}
	if tok.ServerFP != fp {
		t.Fatalf("fp: %s", tok.ServerFP)
	}
	if tok.Secret != secret {
		t.Fatalf("secret mismatch")
	}
}

func TestTokenIPv6Host(t *testing.T) {
	fp := strings.Repeat("cd", 32)
	raw, _, err := enroll.Mint("[::1]:9443", fp)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := enroll.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if tok.HostPort != "[::1]:9443" {
		t.Fatalf("host: %s", tok.HostPort)
	}
}
