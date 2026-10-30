// This file provides a provider-agnostic login session registry.
//
// The session table is a SessionRegistry instance: one per App, isolated between tests and multi-instances.
// Polling states (PollMissing/PollThrottled/PollReady) and session ID generation are in core,
// and providers only depend on core, without needing to know about this package.

package auth

import (
	"strings"
	"sync"
	"time"

	"free-model-hub/internal/core"
)

// Session represents an ongoing login session.
type Session struct {
	VendorID   string
	ExpiresAt  time.Time
	PollMinGap time.Duration
	Data       any
	LastPoll   time.Time
}

// SessionRegistry is the login session registry.
type SessionRegistry struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[string]*Session
}

// NewSessionRegistry constructs a session registry.
func NewSessionRegistry() *SessionRegistry { return NewSessionRegistryWithClock(time.Now) }

// NewSessionRegistryWithClock constructs a session registry using the specified clock (for testing).
func NewSessionRegistryWithClock(now func() time.Time) *SessionRegistry {
	if now == nil {
		now = time.Now
	}
	return &SessionRegistry{now: now, m: map[string]*Session{}}
}

// Register registers a login session. ttl <= 0 defaults to 15 minutes.
func (r *SessionRegistry) Register(vendorID, sessionID string, ttl, pollMinGap time.Duration, data any) {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	r.mu.Lock()
	r.m[id] = &Session{VendorID: vendorID, ExpiresAt: r.now().Add(ttl), PollMinGap: pollMinGap, Data: data}
	r.mu.Unlock()
}

// Poll queries the session and performs expiration cleanup and polling throttling as needed.
func (r *SessionRegistry) Poll(sessionID string) (*core.LoginSession, core.PollState) {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return nil, core.PollMissing
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.m[id]
	if !ok {
		return nil, core.PollMissing
	}
	if !session.ExpiresAt.IsZero() && r.now().After(session.ExpiresAt) {
		delete(r.m, id)
		return nil, core.PollMissing
	}
	if session.PollMinGap > 0 && r.now().Sub(session.LastPoll) < session.PollMinGap {
		return &core.LoginSession{VendorID: session.VendorID, Data: session.Data}, core.PollThrottled
	}
	session.LastPoll = r.now()
	return &core.LoginSession{VendorID: session.VendorID, Data: session.Data}, core.PollReady
}

// Delete deletes a session.
func (r *SessionRegistry) Delete(sessionID string) {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return
	}
	r.mu.Lock()
	delete(r.m, id)
	r.mu.Unlock()
}

// Owns reports session ownership.
func (r *SessionRegistry) Owns(sessionID string) (string, bool) {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.m[id]
	if !ok {
		return "", false
	}
	if !session.ExpiresAt.IsZero() && r.now().After(session.ExpiresAt) {
		delete(r.m, id)
		return "", false
	}
	return session.VendorID, true
}
