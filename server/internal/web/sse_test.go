package web_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/ajthom90/breakwater/server/internal/web"
)

// TestSSE_UnsubscribeOnDisconnect proves client disconnect unsubscribes
// (no leaked hub subscription / channel).
func TestSSE_UnsubscribeOnDisconnect(t *testing.T) {
	hub := scheduler.NewEventHub()
	db := mustDB(t)
	token := "test-dev-token-bbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	handler := web.NewHandler(web.Config{
		DB: db, Events: hub, APIToken: token, Version: "test",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	if hub.SubscriberCount() != 0 {
		t.Fatalf("subs=%d", hub.SubscriberCount())
	}

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// Do not close body until after cancel — disconnect is cancel+Close.

	waitSubs(t, hub, 1, 2*time.Second)

	// Publish and read one event from the stream (single reader — no race).
	hub.Publish(scheduler.JobEvent{JobID: "j1", State: "running", Type: "noop"})

	got := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "data: ") {
				got <- line
				return
			}
		}
	}()

	select {
	case line := <-got:
		if !strings.Contains(line, `"job_id":"j1"`) {
			t.Fatalf("unexpected payload: %s", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for SSE data")
	}

	cancel()
	_ = resp.Body.Close()

	waitSubs(t, hub, 0, 2*time.Second)
}

// TestSSE_ManyConnectDisconnect no leak under churn.
func TestSSE_ManyConnectDisconnect(t *testing.T) {
	hub := scheduler.NewEventHub()
	db := mustDB(t)
	token := "test-dev-token-bbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	handler := web.NewHandler(web.Config{
		DB: db, Events: hub, APIToken: token, Version: "test",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/events", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
			cancel()
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()
	// Brief settle for unsub
	waitSubs(t, hub, 0, 2*time.Second)
}

func TestSSE_JobEventJSON(t *testing.T) {
	hub := scheduler.NewEventHub()
	db := mustDB(t)
	token := "test-dev-token-cccccccccccccccccccccccccccc"
	handler := web.NewHandler(web.Config{
		DB: db, Events: hub, APIToken: token, Version: "test",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	waitSubs(t, hub, 1, 2*time.Second)
	hub.Publish(scheduler.JobEvent{
		JobID: "job-x", MachineID: "m1", Type: "file", State: "success",
		BytesStored: 42,
	})

	deadline := time.After(2 * time.Second)
	sc := bufio.NewScanner(resp.Body)
	for {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for job-x")
		default:
		}
		// Scanner blocks; use a short helper channel
		lineCh := make(chan string, 1)
		errCh := make(chan error, 1)
		go func() {
			if sc.Scan() {
				lineCh <- sc.Text()
			} else {
				errCh <- sc.Err()
			}
		}()
		select {
		case <-deadline:
			t.Fatal("timeout waiting for job-x")
		case err := <-errCh:
			if err != nil {
				t.Fatal(err)
			}
			t.Fatal("stream ended without job-x")
		case line := <-lineCh:
			if strings.HasPrefix(line, "data: ") {
				var ev scheduler.JobEvent
				if err := json.Unmarshal([]byte(line[6:]), &ev); err != nil {
					continue
				}
				if ev.JobID == "job-x" {
					if ev.BytesStored != 42 {
						t.Fatalf("bytes_stored=%d", ev.BytesStored)
					}
					return
				}
			}
		}
	}
}

// TestEventHub_SubscribeLeak unit-level (no HTTP) — disconnect unsub frees map entry.
func TestEventHub_SubscribeLeak(t *testing.T) {
	hub := scheduler.NewEventHub()
	ch, unsub := hub.Subscribe()
	if hub.SubscriberCount() != 1 {
		t.Fatal("expected 1")
	}
	hub.Publish(scheduler.JobEvent{JobID: "a", State: "pending"})
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
	unsub()
	if hub.SubscriberCount() != 0 {
		t.Fatal("leak after unsub")
	}
	// double unsub safe
	unsub()
	if hub.SubscriberCount() != 0 {
		t.Fatal("double unsub leak")
	}
}

func waitSubs(t *testing.T, hub *scheduler.EventHub, want int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if hub.SubscriberCount() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("subscriber count want %d got %d", want, hub.SubscriberCount())
}

func mustDB(t *testing.T) *catalog.DB {
	t.Helper()
	db, err := catalog.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
