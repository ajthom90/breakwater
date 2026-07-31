package notify

import (
	"context"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/clock"
)

func TestFakeSender_NoNetwork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &FakeSender{}
	n := New(fake, clock.System(), nil)
	n.DefaultTo = []string{"ops@example.com"}
	n.Start(ctx)
	defer n.Close()

	n.AlertFailure("host1", "job1", "disk full")
	// Wait for worker
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(fake.Messages()) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	msgs := fake.Messages()
	if len(msgs) != 1 {
		t.Fatalf("msgs=%d", len(msgs))
	}
	if msgs[0].Kind != "failure" {
		t.Fatalf("kind %s", msgs[0].Kind)
	}
}

func TestQueueDropWhenFull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &FakeSender{}
	// Block sender so queue fills.
	block := make(chan struct{})
	blocking := &blockSender{ch: block}
	n := New(blocking, clock.System(), nil)
	n.DefaultTo = []string{"a@b.c"}
	n.QueueSize = 2
	n.Start(ctx)
	defer func() {
		close(block)
		n.Close()
		_ = fake
	}()

	// Fill queue + in-flight
	for i := 0; i < 10; i++ {
		n.Enqueue(Message{Kind: "failure", Subject: "x", Body: "y", To: []string{"a@b.c"}})
	}
	if n.Dropped == 0 {
		// May race; give a moment
		time.Sleep(50 * time.Millisecond)
	}
	if n.Dropped == 0 {
		t.Fatal("expected visible drops when queue full")
	}
}

type blockSender struct{ ch chan struct{} }

func (b *blockSender) Send(ctx context.Context, _ Message) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.ch:
		return nil
	}
}

func TestWatchdog(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &FakeSender{}
	clk := clock.NewFake(time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC))
	n := New(fake, clk, nil)
	n.DefaultTo = []string{"ops@example.com"}
	n.Start(ctx)
	defer n.Close()

	n.Watchdog([]WatchMachine{{
		Hostname:    "silent",
		LastSuccess: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}}, 36*time.Hour)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(fake.Messages()) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(fake.Messages()) < 1 {
		t.Fatal("expected watchdog email")
	}
	if fake.Messages()[0].Kind != "watchdog" {
		t.Fatalf("kind %s", fake.Messages()[0].Kind)
	}
}

func TestDigest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &FakeSender{}
	n := New(fake, clock.System(), nil)
	n.DefaultTo = []string{"ops@example.com"}
	n.Start(ctx)
	defer n.Close()

	n.SendDigest([]DigestRow{
		{Machine: "a", Status: "success", SizeBytes: 100, Duration: time.Minute},
		{Machine: "b", Status: "missed"},
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(fake.Messages()) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	msgs := fake.Messages()
	if len(msgs) != 1 || msgs[0].Kind != "digest" {
		t.Fatalf("%+v", msgs)
	}
	if !contains(msgs[0].Body, "Machine |") {
		t.Fatalf("body missing table: %s", msgs[0].Body)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(len(s) > 0 && (s[:len(sub)] == sub || contains(s[1:], sub))))
}

func TestSMTPConfigRedacted(t *testing.T) {
	c := SMTPConfig{Password: "secret", Host: "mail.example"}
	r := c.Redacted()
	if r.Password == "secret" {
		t.Fatal("password must not appear in redacted config")
	}
	if c.Password != "secret" {
		t.Fatal("original mutated")
	}
}
