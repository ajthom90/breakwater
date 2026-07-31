// Package notify implements SMTP alerts, missed-backup watchdog, and the daily
// digest (PLAN Notifier). Sending is non-blocking to job processing: events are
// enqueued and drained by a background worker with bounded retry. Past the
// bound, events are logged and dropped visibly.
//
// Credentials must never appear in logs. Tests inject a fake Sender — no network.
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ajthom90/breakwater/server/internal/clock"
)

// SMTPConfig holds outbound mail settings (from catalog settings / config).
// Password is never logged.
type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string // secret — never log
	From     string
	// To is the default recipient list for alerts/digest.
	To []string
	// TLS mode: "starttls" (default), "tls", "none".
	TLSMode string
}

// Redacted returns a log-safe copy (password blanked).
func (c SMTPConfig) Redacted() SMTPConfig {
	out := c
	if out.Password != "" {
		out.Password = "***"
	}
	return out
}

// Message is one outbound notification.
type Message struct {
	To      []string
	Subject string
	Body    string
	// Kind for metrics/logging: failure|watchdog|digest
	Kind string
}

// Sender delivers a message. Production uses go-mail SMTP; tests use FakeSender.
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// FakeSender records messages for tests (no network).
type FakeSender struct {
	mu   sync.Mutex
	Msgs []Message
	// FailN makes the next N sends fail.
	FailN int
}

// Send implements Sender.
func (f *FakeSender) Send(_ context.Context, msg Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailN > 0 {
		f.FailN--
		return fmt.Errorf("fake smtp down")
	}
	f.Msgs = append(f.Msgs, msg)
	return nil
}

// Messages returns a copy of recorded messages.
func (f *FakeSender) Messages() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Message, len(f.Msgs))
	copy(out, f.Msgs)
	return out
}

// Queue bounds and retry.
const (
	DefaultQueueSize   = 256
	DefaultMaxAttempts = 5
	DefaultBaseDelay   = 2 * time.Second
)

// Notifier is the non-blocking alert pipeline.
type Notifier struct {
	Sender      Sender
	Clock       clock.Clock
	Log         *slog.Logger
	DefaultTo   []string
	QueueSize   int
	MaxAttempts int
	BaseDelay   time.Duration

	ch     chan queued
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
	// dropped is visible for tests / metrics.
	Dropped int
}

type queued struct {
	msg       Message
	attempt   int
	notBefore time.Time
}

// New constructs a notifier. Call Start to begin the worker.
func New(sender Sender, clk clock.Clock, log *slog.Logger) *Notifier {
	if log == nil {
		log = slog.Default()
	}
	if clk == nil {
		clk = clock.System()
	}
	return &Notifier{
		Sender: sender, Clock: clk, Log: log,
		QueueSize: DefaultQueueSize, MaxAttempts: DefaultMaxAttempts,
		BaseDelay: DefaultBaseDelay,
	}
}

// Start launches the background drain worker.
func (n *Notifier) Start(ctx context.Context) {
	n.mu.Lock()
	if n.ch != nil {
		n.mu.Unlock()
		return
	}
	size := n.QueueSize
	if size <= 0 {
		size = DefaultQueueSize
	}
	n.ch = make(chan queued, size)
	n.mu.Unlock()
	n.wg.Add(1)
	go n.loop(ctx)
}

// Close stops accepting and waits for the worker (best-effort drain).
func (n *Notifier) Close() {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return
	}
	n.closed = true
	if n.ch != nil {
		close(n.ch)
	}
	n.mu.Unlock()
	n.wg.Wait()
}

// Enqueue is non-blocking. If the queue is full, the message is dropped and
// logged visibly (Dropped increments).
func (n *Notifier) Enqueue(msg Message) {
	if len(msg.To) == 0 {
		msg.To = n.DefaultTo
	}
	if len(msg.To) == 0 {
		n.Log.Warn("notify drop: no recipients", "kind", msg.Kind, "subject", msg.Subject)
		n.mu.Lock()
		n.Dropped++
		n.mu.Unlock()
		return
	}
	n.mu.Lock()
	ch := n.ch
	closed := n.closed
	n.mu.Unlock()
	if ch == nil || closed {
		n.Log.Warn("notify drop: not started", "kind", msg.Kind)
		n.mu.Lock()
		n.Dropped++
		n.mu.Unlock()
		return
	}
	select {
	case ch <- queued{msg: msg, attempt: 0}:
	default:
		n.Log.Error("notify queue full; dropping event",
			"kind", msg.Kind, "subject", msg.Subject, "to_count", len(msg.To))
		n.mu.Lock()
		n.Dropped++
		n.mu.Unlock()
	}
}

func (n *Notifier) loop(ctx context.Context) {
	defer n.wg.Done()
	// Simple retry: re-enqueue with delay via timer goroutine when send fails.
	for {
		select {
		case <-ctx.Done():
			return
		case q, ok := <-n.ch:
			if !ok {
				return
			}
			if !q.notBefore.IsZero() && n.Clock.Now().Before(q.notBefore) {
				// Not yet — reschedule without counting as drop.
				delay := time.Until(q.notBefore)
				if delay < 0 {
					delay = 0
				}
				if delay > time.Second {
					// Avoid tight spin in tests with fake clock: sleep wall or requeue.
					time.Sleep(min(delay, 50*time.Millisecond))
				}
				n.requeue(q)
				continue
			}
			if n.Sender == nil {
				n.Log.Error("notify: no sender configured; dropping", "kind", q.msg.Kind)
				n.mu.Lock()
				n.Dropped++
				n.mu.Unlock()
				continue
			}
			err := n.Sender.Send(ctx, q.msg)
			if err == nil {
				n.Log.Info("notify sent", "kind", q.msg.Kind, "subject", q.msg.Subject, "to_count", len(q.msg.To))
				continue
			}
			// Never log credentials — only host-free error text.
			q.attempt++
			max := n.MaxAttempts
			if max <= 0 {
				max = DefaultMaxAttempts
			}
			if q.attempt >= max {
				n.Log.Error("notify give up after retries",
					"kind", q.msg.Kind, "subject", q.msg.Subject,
					"attempts", q.attempt, "err", err.Error())
				n.mu.Lock()
				n.Dropped++
				n.mu.Unlock()
				continue
			}
			base := n.BaseDelay
			if base <= 0 {
				base = DefaultBaseDelay
			}
			delay := base * time.Duration(1<<uint(min(q.attempt-1, 6)))
			q.notBefore = n.Clock.Now().Add(delay)
			n.Log.Warn("notify send failed; will retry",
				"kind", q.msg.Kind, "attempt", q.attempt, "err", err.Error())
			n.requeue(q)
		}
	}
}

func (n *Notifier) requeue(q queued) {
	n.mu.Lock()
	ch := n.ch
	closed := n.closed
	n.mu.Unlock()
	if ch == nil || closed {
		n.mu.Lock()
		n.Dropped++
		n.mu.Unlock()
		return
	}
	select {
	case ch <- q:
	default:
		n.Log.Error("notify requeue full; dropping", "kind", q.msg.Kind)
		n.mu.Lock()
		n.Dropped++
		n.mu.Unlock()
	}
}

// AlertFailure enqueues a backup/job failure alert.
func (n *Notifier) AlertFailure(machine, jobID, errMsg string) {
	n.Enqueue(Message{
		Kind:    "failure",
		Subject: fmt.Sprintf("[Breakwater] Backup failed: %s", machine),
		Body:    fmt.Sprintf("Machine: %s\nJob: %s\nError: %s\n", machine, jobID, errMsg),
	})
}

// AlertMissedBackup enqueues a watchdog alert for a silent machine.
func (n *Notifier) AlertMissedBackup(machine string, lastSuccess time.Time, expectedWindow string) {
	last := "never"
	if !lastSuccess.IsZero() {
		last = lastSuccess.UTC().Format(time.RFC3339)
	}
	n.Enqueue(Message{
		Kind:    "watchdog",
		Subject: fmt.Sprintf("[Breakwater] Missed backup: %s", machine),
		Body: fmt.Sprintf(
			"Machine %s has not completed a backup in the expected window (%s).\nLast success: %s\n",
			machine, expectedWindow, last),
	})
}

// DigestRow is one machine line in the daily digest table.
type DigestRow struct {
	Machine     string
	LastSuccess time.Time
	SizeBytes   int64
	Duration    time.Duration
	Status      string // success|failed|missed|none
}

// SendDigest enqueues the daily fleet health table.
func (n *Notifier) SendDigest(rows []DigestRow) {
	var b strings.Builder
	b.WriteString("Breakwater daily digest\n")
	b.WriteString("Machine | Last success | Size | Duration | Status\n")
	b.WriteString("--------|--------------|------|----------|-------\n")
	for _, r := range rows {
		last := "-"
		if !r.LastSuccess.IsZero() {
			last = r.LastSuccess.UTC().Format("2006-01-02 15:04")
		}
		dur := "-"
		if r.Duration > 0 {
			dur = r.Duration.Round(time.Second).String()
		}
		fmt.Fprintf(&b, "%s | %s | %d | %s | %s\n",
			r.Machine, last, r.SizeBytes, dur, r.Status)
	}
	n.Enqueue(Message{
		Kind:    "digest",
		Subject: "[Breakwater] Daily backup digest",
		Body:    b.String(),
	})
}

// Watchdog evaluates machines that missed their expected backup window.
// expectedSilence is how long past last success (or enrollment) before alert.
func (n *Notifier) Watchdog(machines []WatchMachine, expectedSilence time.Duration) {
	if expectedSilence <= 0 {
		expectedSilence = 36 * time.Hour // nightly + slack
	}
	now := n.Clock.Now()
	for _, m := range machines {
		ref := m.LastSuccess
		if ref.IsZero() {
			ref = m.EnrolledAt
		}
		if ref.IsZero() {
			continue
		}
		if now.Sub(ref) > expectedSilence {
			n.AlertMissedBackup(m.Hostname, m.LastSuccess, expectedSilence.String())
		}
	}
}

// WatchMachine is watchdog input.
type WatchMachine struct {
	Hostname    string
	LastSuccess time.Time
	EnrolledAt  time.Time
}
