// Command bwctl is the Breakwater CLI.
//
// Transport choice (M4): REST over HTTPS :8443 with the server API token
// (Authorization: Bearer <token> or X-API-Token). Direct agent gRPC (:9443)
// remains agent-only (mTLS enrollment certs). bwctl is an operator tool.
//
// Commands:
//
//	bwctl restore  — POST /api/v1/jobs type=restore (submits to target agent)
//	bwctl rescan   — POST /api/v1/rescan (rebuild snapshot index from vaults)
//	bwctl version  — print version
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

Transport: REST + API token on :8443 (not agent mTLS).
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

func doJSON(method, url, token string, insecure bool, body []byte) ([]byte, error) {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}, //nolint:gosec // explicit --insecure
	}
	client := &http.Client{Timeout: 120 * time.Second, Transport: tr}
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
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
