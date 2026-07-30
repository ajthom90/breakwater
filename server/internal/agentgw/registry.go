package agentgw

import (
	"fmt"
	"log/slog"
	"sync"

	breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
)

// sendBuf is the per-session outbound queue depth for ServerToAgent messages.
const sendBuf = 32

// session is one live ControlService.Channel for a machine.
type session struct {
	machineID string
	sessionID string
	send      chan *breakwaterv1.ServerToAgent
	// done is closed when the session is superseded or the stream ends.
	done chan struct{}
	// closeOnce guards close(done).
	closeOnce sync.Once
}

func (s *session) close() {
	s.closeOnce.Do(func() {
		close(s.done)
	})
}

// Registry tracks at most one live control channel per machine.
// A NEW channel from the same machine supersedes the old one so agent
// restarts/reconnects cannot wedge the control plane.
//
// Implements scheduler.Dispatcher.
type Registry struct {
	mu       sync.Mutex
	sessions map[string]*session // machineID → live session
	Log      *slog.Logger
}

// NewRegistry constructs an empty connection registry.
func NewRegistry(log *slog.Logger) *Registry {
	if log == nil {
		log = slog.Default()
	}
	return &Registry{
		sessions: make(map[string]*session),
		Log:      log,
	}
}

// Register installs a new session for machineID, superseding any previous one.
// The caller must call Unregister with the returned session when the stream ends
// (only if it is still the current session).
func (r *Registry) Register(machineID, sessionID string) *session {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.sessions[machineID]; ok {
		r.Log.Info("superseding control channel", "machine_id", machineID, "old_session", old.sessionID, "new_session", sessionID)
		old.close()
		// Do not close old.send — the old writer exits on done and the channel
		// is GC'd; sends after close would panic. Writers select on done first.
	}
	s := &session{
		machineID: machineID,
		sessionID: sessionID,
		send:      make(chan *breakwaterv1.ServerToAgent, sendBuf),
		done:      make(chan struct{}),
	}
	r.sessions[machineID] = s
	return s
}

// Unregister removes s if it is still the live session for its machine.
func (r *Registry) Unregister(s *session) {
	if s == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.sessions[s.machineID]
	if ok && cur == s {
		delete(r.sessions, s.machineID)
	}
	s.close()
}

// IsOnline reports whether a control channel is live for machineID.
func (r *Registry) IsOnline(machineID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[machineID]
	if !ok {
		return false
	}
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// SendJobStart implements scheduler.Dispatcher.
func (r *Registry) SendJobStart(machineID, jobID, jobType string, paramsJSON []byte) (bool, error) {
	msg := &breakwaterv1.ServerToAgent{
		Msg: &breakwaterv1.ServerToAgent_JobStart{
			JobStart: &breakwaterv1.JobStart{
				JobId:      jobID,
				Type:       scheduler.WireJobType(jobType),
				ParamsJson: paramsJSON,
			},
		},
	}
	return r.send(machineID, msg)
}

// SendJobCancel delivers a cancel to the live channel (best-effort).
func (r *Registry) SendJobCancel(machineID, jobID, reason string) (bool, error) {
	msg := &breakwaterv1.ServerToAgent{
		Msg: &breakwaterv1.ServerToAgent_JobCancel{
			JobCancel: &breakwaterv1.JobCancel{
				JobId:  jobID,
				Reason: reason,
			},
		},
	}
	return r.send(machineID, msg)
}

func (r *Registry) send(machineID string, msg *breakwaterv1.ServerToAgent) (bool, error) {
	r.mu.Lock()
	s, ok := r.sessions[machineID]
	r.mu.Unlock()
	if !ok {
		return false, nil
	}
	select {
	case <-s.done:
		return false, nil
	case s.send <- msg:
		return true, nil
	default:
		// Queue full — treat as transient failure so the engine can fail/retry.
		return false, fmt.Errorf("control send queue full for machine %s", machineID)
	}
}

// LiveSessionID returns the current session id if online (tests).
func (r *Registry) LiveSessionID(machineID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[machineID]
	if !ok {
		return "", false
	}
	select {
	case <-s.done:
		return "", false
	default:
		return s.sessionID, true
	}
}

// Ensure Registry implements Dispatcher at compile time.
var _ scheduler.Dispatcher = (*Registry)(nil)
