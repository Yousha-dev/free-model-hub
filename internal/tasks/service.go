package tasks

// This file is the task engine instance entry: only one background task remains—periodic token keepalive.
// Check-in and growth/reward tasks have been removed (they were only used to farm upstream free quotas).

import (
	"context"
	"sync"
)

// Service is a background keepalive engine for an App instance.
type Service struct {
	deps Deps

	schedulerMu     sync.Mutex
	schedulerCancel context.CancelFunc
	schedulerWG     sync.WaitGroup
	running         bool
	pokeCh          chan string

	// refreshGate serializes credential refresh per account (see keepalive.go).
	refreshGate refreshGate
}

// New constructs a keepalive engine.
func New(deps Deps) *Service {
	return &Service{deps: deps, pokeCh: make(chan string, 4)}
}

// Start starts the background scheduling loop (idempotent).
func (s *Service) Start(ctx context.Context) {
	s.schedulerMu.Lock()
	defer s.schedulerMu.Unlock()
	if s.schedulerCancel != nil {
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.schedulerCancel = cancel
	s.schedulerWG.Add(1)
	go func() {
		defer s.schedulerWG.Done()
		s.runScheduler(runCtx)
	}()
}

// Stop stops the background scheduling loop and waits for exit.
func (s *Service) Stop() {
	s.schedulerMu.Lock()
	cancel := s.schedulerCancel
	s.schedulerCancel = nil
	s.schedulerMu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.schedulerWG.Wait()
}

// Poke wakes up the scheduling loop to recalculate the next execution time.
func (s *Service) Poke() {
	select {
	case s.pokeCh <- schedulerReconfigure:
	default:
	}
}

// Status returns the scheduler status (for management page display).
func (s *Service) Status() map[string]any { return s.schedulerStatus() }
