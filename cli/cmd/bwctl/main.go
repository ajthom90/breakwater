// Command bwctl is the Breakwater CLI.
//
// Transport choice (M4): REST over HTTPS :8443 with the server API token
// (Authorization: Bearer <token> or X-API-Token). Direct agent gRPC (:9443)
// remains agent-only (mTLS enrollment certs). bwctl is an operator tool.
//
// Commands:
//
//	bwctl restore     — POST /api/v1/jobs type=restore (submits to target agent)
//	bwctl rescan      — POST /api/v1/rescan (rebuild snapshot index from vaults)
//	bwctl token mint  — POST /api/v1/enroll-tokens (one-time agent enrollment)
//	bwctl token list  — GET  /api/v1/enroll-tokens (metadata only)
//	bwctl version     — print version
//
// Offline repo extract (recovery kit) is a later phase.
package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

var version = "0.0.1-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "-version", "--version", "version":
		fmt.Println(version)
	case "restore":
		os.Exit(cmdRestore(os.Args[2:]))
	case "rescan":
		os.Exit(cmdRescan(os.Args[2:]))
	case "token":
		os.Exit(cmdToken(os.Args[2:]))
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "bwctl: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `bwctl — Breakwater operator CLI

Usage:
  bwctl version
  bwctl restore --server https://host:8443 --token TOKEN \
      --machine TARGET_MACHINE_ID --snapshot SNAPSHOT_ID \
      --target /path/on/agent [--source-machine SOURCE_ID] \
      [--conflict overwrite|rename|skip]
  bwctl rescan --server https://host:8443 --token TOKEN
  bwctl token mint --server https://host:8443 --token TOKEN \
      --advertise 10.0.0.5:9443 [--ttl 24h] [--note "ws2022 vm"]
  bwctl token list --server https://host:8443 --token TOKEN

Transport: REST + API token on :8443 (not agent mTLS).
API token: from <dataDir>/api-token on the server host (or BW_API_TOKEN).
`)
}

func commonFlags(fs *flag.FlagSet) (server, token *string, insecure *bool) {
	server = fs.String("server", envOr("BW_SERVER", "https://127.0.0.1:8443"), "breakwaterd web base URL")
	token = fs.String("token", envOr("BW_API_TOKEN", ""), "API token (or BW_API_TOKEN)")
	insecure = fs.Bool("insecure", false, "skip TLS verify (dev only)")
	return
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func cmdRestore(args []string) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	server, token, insecure := commonFlags(fs)
	machine := fs.String("machine", "", "target machine id (agent that writes files)")
	snapshot := fs.String("snapshot", "", "source snapshot id")
	sourceMachine := fs.String("source-machine", "", "source machine id (default: same as --machine)")
	target := fs.String("target", "", "target path on the agent")
	conflict := fs.String("conflict", "overwrite", "overwrite|rename|skip")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *token == "" || *machine == "" || *snapshot == "" || *target == "" {
		fmt.Fprintln(os.Stderr, "bwctl restore: --token, --machine, --snapshot, and --target required")
		return 2
	}
	params := map[string]string{
		"source_snapshot_id": *snapshot,
		"target_path":        *target,
		"conflict_policy":    *conflict,
	}
	if *sourceMachine != "" {
		params["source_machine_id"] = *sourceMachine
	}
	body, _ := json.Marshal(map[string]any{
		"machine_id": *machine,
		"type":       "restore",
		"params":     params,
		"initiator":  "bwctl",
	})
	resp, err := doJSON(http.MethodPost, strings.TrimRight(*server, "/")+"/api/v1/jobs", *token, *insecure, body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bwctl restore:", err)
		return 1
	}
	fmt.Println(string(resp))
	return 0
}

func cmdRescan(args []string) int {
	fs := flag.NewFlagSet("rescan", flag.ContinueOnError)
	server, token, insecure := commonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *token == "" {
		fmt.Fprintln(os.Stderr, "bwctl rescan: --token required (or BW_API_TOKEN)")
		return 2
	}
	resp, err := doJSON(http.MethodPost, strings.TrimRight(*server, "/")+"/api/v1/rescan", *token, *insecure, []byte("{}"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "bwctl rescan:", err)
		return 1
	}
	fmt.Println(string(resp))
	return 0
}

func cmdToken(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "bwctl token: subcommand required (mint|list)")
		return 2
	}
	switch args[0] {
	case "mint":
		return cmdTokenMint(args[1:])
	case "list":
		return cmdTokenList(args[1:])
	case "help", "-h", "--help":
		fmt.Fprintln(os.Stderr, "bwctl token mint|list — see bwctl help")
		return 0
	default:
		fmt.Fprintf(os.Stderr, "bwctl token: unknown subcommand %q\n", args[0])
		return 2
	}
}

func cmdTokenMint(args []string) int {
	fs := flag.NewFlagSet("token mint", flag.ContinueOnError)
	server, token, insecure := commonFlags(fs)
	advertise := fs.String("advertise", "", "host:port the agent will dial (e.g. 10.0.0.5:9443)")
	ttl := fs.String("ttl", "24h", "token lifetime (Go duration, default 24h per PLAN)")
	note := fs.String("note", "", "optional label for audit (e.g. ws2022 vm)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *token == "" {
		fmt.Fprintln(os.Stderr, "bwctl token mint: --token required (or BW_API_TOKEN)")
		return 2
	}
	if *advertise == "" {
		fmt.Fprintln(os.Stderr, "bwctl token mint: --advertise host:port required (address the agent dials, not the server bind)")
		return 2
	}
	d, err := time.ParseDuration(*ttl)
	if err != nil || d <= 0 {
		fmt.Fprintf(os.Stderr, "bwctl token mint: invalid --ttl %q\n", *ttl)
		return 2
	}
	body, _ := json.Marshal(map[string]any{
		"advertise_addr": *advertise,
		"ttl_seconds":    int(d.Seconds()),
		"note":           *note,
		"created_by":     "bwctl",
	})
	resp, err := doJSON(http.MethodPost, strings.TrimRight(*server, "/")+"/api/v1/enroll-tokens", *token, *insecure, body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bwctl token mint:", err)
		return 1
	}
	var out struct {
		ID        string `json:"id"`
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
		Advertise string `json:"advertise"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		fmt.Fprintln(os.Stderr, "bwctl token mint: bad response:", err)
		return 1
	}
	if out.Token == "" {
		fmt.Fprintln(os.Stderr, "bwctl token mint: empty token in response")
		return 1
	}
	// Human context on stderr; token alone on stdout (pipe/copy friendly).
	fmt.Fprintf(os.Stderr, "enrollment token id=%s expires_at=%s advertise=%s\n", out.ID, out.ExpiresAt, out.Advertise)
	fmt.Fprintf(os.Stderr, "shown once — copy now. MSI install line:\n")
	fmt.Fprintf(os.Stderr, "  msiexec /i breakwater-agent.msi /qn BWTOKEN=%s\n", out.Token)
	fmt.Println(out.Token)
	return 0
}

func cmdTokenList(args []string) int {
	fs := flag.NewFlagSet("token list", flag.ContinueOnError)
	server, token, insecure := commonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *token == "" {
		fmt.Fprintln(os.Stderr, "bwctl token list: --token required (or BW_API_TOKEN)")
		return 2
	}
	resp, err := doJSON(http.MethodGet, strings.TrimRight(*server, "/")+"/api/v1/enroll-tokens", *token, *insecure, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bwctl token list:", err)
		return 1
	}
	fmt.Println(string(resp))
	return 0
}

func doJSON(method, url, token string, insecure bool, body []byte) ([]byte, error) {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}, //nolint:gosec // explicit --insecure
	}
	client := &http.Client{Timeout: 120 * time.Second, Transport: tr}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-API-Token", token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return b, nil
}
