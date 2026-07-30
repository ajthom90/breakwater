package agentgw

import (
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
	// closeOnce guards close(done) and undelivered flush.
	closeOnce sync.Once

	// undelivered JobStart job IDs enqueued but not yet stream.Send-delivered (S2-F2).
	undelMu     sync.Mutex
	undelivered map[string]struct{}
}

func (s *session) noteJobStartQueued(jobID string) {
	if jobID == "" {
		return
	}
	s.undelMu.Lock()
	if s.undelivered == nil {
		s.undelivered = make(map[string]struct{})
	}
	s.undelivered[jobID] = struct{}{}
	s.undelMu.Unlock()
}

func (s *session) noteJobStartDelivered(jobID string) {
	if jobID == "" {
		return
	}
	s.undelMu.Lock()
	delete(s.undelivered, jobID)
	s.undelMu.Unlock()
}

func (s *session) takeUndeliveredJobStarts() []string {
	s.undelMu.Lock()
	defer s.undelMu.Unlock()
	out := make([]string, 0, len(s.undelivered))
	for id := range s.undelivered {
		out = append(out, id)
	}
	s.undelivered = nil
	return out
}

// close closes done and returns undelivered JobStart IDs (at most once).
func (s *session) close() []string {
	var undelivered []string
	s.closeOnce.Do(func() {
		close(s.done)
		undelivered = s.takeUndeliveredJobStarts()
	})
	return undelivered
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
	// OnUndelivered is invoked with JobStart job IDs that were queued but never
	// stream.Send-delivered when a session closes (supersede or death). Wired by
	// AttachControlPlane to Engine.RevertUndeliveredJobStarts (S2-F2).
	OnUndelivered func(jobIDs []string)
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

func (r *Registry) fireUndelivered(ids []string) {
	if len(ids) == 0 || r.OnUndelivered == nil {
		return
	}
	r.OnUndelivered(ids)
}

// Register installs a new session for machineID, superseding any previous one.
// The caller must call Unregister with the returned session when the stream ends
// (only if it is still the current session).
func (r *Registry) Register(machineID, sessionID string) *session {
	r.mu.Lock()
	var undelivered []string
	if old, ok := r.sessions[machineID]; ok {
		r.Log.Info("superseding control channel", "machine_id", machineID, "old_session", old.sessionID, "new_session", sessionID)
		undelivered = old.close()
	}
	s := &session{
		machineID:   machineID,
		sessionID:   sessionID,
		send:        make(chan *breakwaterv1.ServerToAgent, sendBuf),
		done:        make(chan struct{}),
		undelivered: make(map[string]struct{}),
	}
	r.sessions[machineID] = s
	r.mu.Unlock()

	// Outside the lock: revert undelivered JobStarts from the superseded session (S2-F2).
	r.fireUndelivered(undelivered)
	return s
}

// Unregister removes s if it is still the live session for its machine.
func (r *Registry) Unregister(s *session) {
	if s == nil {
		return
	}
	r.mu.Lock()
	cur, ok := r.sessions[s.machineID]
	if ok && cur == s {
		delete(r.sessions, s.machineID)
	}
	r.mu.Unlock()
	undelivered := s.close()
	r.fireUndelivered(undelivered)
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
// Returns (false, nil) when offline or queue-full (engine reverts to pending; S2-F2).
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
	return r.send(machineID, msg, jobID)
}

// SendJobCancel implements scheduler.Dispatcher (S2-F6).
func (r *Registry) SendJobCancel(machineID, jobID, reason string) (bool, error) {
	msg := &breakwaterv1.ServerToAgent{
		Msg: &breakwaterv1.ServerToAgent_JobCancel{
			JobCancel: &breakwaterv1.JobCancel{
				JobId:  jobID,
				Reason: reason,
			},
		},
	}
	return r.send(machineID, msg, "")
}

func (r *Registry) send(machineID string, msg *breakwaterv1.ServerToAgent, jobStartID string) (bool, error) {
	r.mu.Lock()
	s, ok := r.sessions[machineID]
	r.mu.Unlock()
	if !ok {
		return false, nil
	}
	// Track before enqueue so a concurrent writer cannot deliver-and-clear first (S2-F2).
	if jobStartID != "" {
		s.noteJobStartQueued(jobStartID)
	}
	select {
	case <-s.done:
		if jobStartID != "" {
			s.noteJobStartDelivered(jobStartID) // not actually queued
		}
		return false, nil
	case s.send <- msg:
		return true, nil
	default:
		// Queue full → pending (like offline), never hard-fail (S2-F2).
		if jobStartID != "" {
			s.noteJobStartDelivered(jobStartID) // not actually queued
		}
		return false, nil
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
