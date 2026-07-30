package agentgw_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/ajthom90/breakwater/server/internal/web"
)

// TestM2S5_UIDemo is the provable Linux/darwin subset of the PLAN M2 demo:
//
//	fake agent enroll → appears via GET /api/v1/machines →
//	file-backup job → second run shows reduced upload (dedup).
//
// Windows-only parts of the full demo remain unproven (see PROGRESS.md):
// MSI install, service start as LocalSystem, appears-in-UI-in-10s after msiexec.
func TestM2S5_UIDemo(t *testing.T) {
	env := startDataEnv(t)
	hub := scheduler.NewEventHub()
	env.Engine.Events = hub

	apiToken := "m2s5-demo-token-aaaaaaaaaaaaaaaaaaaaaaaa"
	handler := web.NewHandler(web.Config{
		DB: env.DB, Auditor: env.Auditor, Events: hub,
		APIToken: apiToken, Version: "0.0.1-m2s5-test",
	})
	// httptest is sufficient; production uses HTTPS leaf (M11).
	ts := httptest.NewTLSServer(handler)
	t.Cleanup(ts.Close)
	client := ts.Client()
	// Accept test server cert
	if tr, ok := client.Transport.(*http.Transport); ok {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test only
	}

	apiGet := func(path string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+apiToken)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Unauthenticated rejected
	resp, err := client.Get(ts.URL + "/api/v1/machines")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth: %d", resp.StatusCode)
	}

	// --- enroll fake agent ---
	machineID, hashingKey, hashingAlgo, _, conn := env.mintAndEnroll("m2s5-ui-demo")
	dataClient := breakwaterv1.NewDataServiceClient(conn)
	agent := openBackupAgent(t, conn, machineID, hashingKey, hashingAlgo, dataClient)
	defer agent.close()
	waitOnline(t, env.DB, machineID, 5*time.Second)

	// Appears in REST API
	resp = apiGet("/api/v1/machines")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("machines: %d %s", resp.StatusCode, body)
	}
	var mlist struct {
		Machines []struct {
			ID       string `json:"id"`
			Hostname string `json:"hostname"`
			Status   string `json:"status"`
		} `json:"machines"`
	}
	if err := json.Unmarshal(body, &mlist); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range mlist.Machines {
		if m.ID == machineID {
			found = true
			if m.Hostname != "m2s5-ui-demo" {
				t.Fatalf("hostname=%v", m.Hostname)
			}
			if m.Status != catalog.MachineStatusActive {
				t.Fatalf("status=%v want active", m.Status)
			}
		}
	}
	if !found {
		t.Fatalf("machine %s not in API response: %s", machineID, body)
	}
	t.Logf("machine %s appeared in /api/v1/machines (status=active)", machineID)

	// Detail + inventory path
	resp = apiGet("/api/v1/machines/" + machineID)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("machine detail: %d %s", resp.StatusCode, body)
	}

	// Summary
	resp = apiGet("/api/v1/summary")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var sum map[string]any
	if err := json.Unmarshal(body, &sum); err != nil {
		t.Fatal(err)
	}
	if int(sum["machines_online"].(float64)) < 1 {
		t.Fatalf("summary online=%v", sum["machines_online"])
	}

	// Source tree for backup
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("hello m2s5"), 0o644); err != nil {
		t.Fatal(err)
	}
	// ~1 MiB for measurable dedup
	big := make([]byte, 1<<20)
	for i := range big {
		big[i] = byte(i % 251)
	}
	if err := os.WriteFile(filepath.Join(src, "blob.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(map[string]string{"source": src})

	ctx := context.Background()
	job1, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypeFileBackup,
		Initiator: "m2s5-demo", ParamsJSON: string(params),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitJobState(t, env.Engine, job1, catalog.JobStateSuccess, 60*time.Second)
	j1, _ := env.Engine.Job(ctx, job1)
	t.Logf("run1 job=%s bytes_stored=%d bytes_read=%d", job1, j1.BytesStored, j1.BytesRead)
	if j1.BytesStored <= 0 {
		t.Fatalf("run1 bytes_stored=%d", j1.BytesStored)
	}

	// Mutate slightly + second run
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("hello m2s5 changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "new.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	job2, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypeFileBackup,
		Initiator: "m2s5-demo", ParamsJSON: string(params),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitJobState(t, env.Engine, job2, catalog.JobStateSuccess, 60*time.Second)
	j2, _ := env.Engine.Job(ctx, job2)
	t.Logf("run2 job=%s bytes_stored=%d bytes_read=%d", job2, j2.BytesStored, j2.BytesRead)
	if j2.BytesStored >= j1.BytesStored {
		t.Fatalf("dedup not observed: run2 bytes_stored=%d >= run1=%d", j2.BytesStored, j1.BytesStored)
	}
	ratio := float64(j2.BytesStored) / float64(j1.BytesStored)
	t.Logf("dedup ratio run2/run1 = %.4f", ratio)

	// Jobs visible via API
	resp = apiGet(fmt.Sprintf("/api/v1/jobs?machine_id=%s&limit=10", machineID))
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var jlist struct {
		Jobs []struct {
			ID, State string
			Stored    int64 `json:"bytes_stored"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(body, &jlist); err != nil {
		t.Fatal(err)
	}
	if len(jlist.Jobs) < 2 {
		t.Fatalf("want ≥2 jobs via API, got %d: %s", len(jlist.Jobs), body)
	}

	// Snapshots via API
	resp = apiGet(fmt.Sprintf("/api/v1/snapshots?machine_id=%s", machineID))
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var slist struct {
		Snapshots []map[string]any `json:"snapshots"`
	}
	if err := json.Unmarshal(body, &slist); err != nil {
		t.Fatal(err)
	}
	if len(slist.Snapshots) < 2 {
		t.Fatalf("want ≥2 snapshots, got %d: %s", len(slist.Snapshots), body)
	}

	// Audit API + chain
	resp = apiGet("/api/v1/audit?limit=20")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	var alist struct {
		ChainOK bool  `json:"chain_ok"`
		Events  []any `json:"events"`
	}
	if err := json.Unmarshal(body, &alist); err != nil {
		t.Fatal(err)
	}
	if !alist.ChainOK {
		t.Fatalf("audit chain_ok=false: %s", body)
	}

	// healthz open
	resp, err = client.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz %d", resp.StatusCode)
	}

	t.Logf("M2S5 demo OK: enroll→API→backup×2 dedup ratio=%.4f snapshots=%d", ratio, len(slist.Snapshots))
}
