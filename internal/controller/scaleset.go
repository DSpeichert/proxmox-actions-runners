package controller

import (
	"context"
	"sync"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/github"
)

// scaleSetState is one scale set's view of what GitHub wants. It implements github.Handler: the listener reports
// into it, and the reconcile loop reads it. Handler methods only record and wake the loop, because the listener
// doesn't fetch the next message until they return.
type scaleSetState struct {
	key ssKey
	// targetURL is the organization or repository the scale set serves.
	targetURL string
	cfg       config.ScaleSet
	// gh serves the scale set: the client for the App installation it is registered with.
	gh   GitHub
	wake func()

	mu sync.Mutex
	// id is GitHub's ID for the scale set, 0 until it is registered.
	id int
	// desired is how many workers GitHub's last message calls for, once known is true. Until the first message the
	// controller neither adds nor retires workers, so a restart doesn't disturb running ones.
	desired int
	known   bool
	// busy holds the runners running a job, from JobStarted (or GitHub refusing to remove the runner) until
	// JobCompleted.
	busy map[string]bool
	// retryAfter holds off new workers after a failed creation.
	retryAfter time.Time
	// wantSince is when the scale set last went from having the workers it wants to wanting more; zero while it
	// wants none. Scale sets that have waited longest create first when maxWorkers leaves too little room.
	wantSince time.Time

	// For the status: the message session, and GitHub's last statistics.
	sessionOpen  bool
	sessionSince time.Time
	sessionErr   string
	stats        *github.Stats
	now          func() time.Time
}

func newScaleSetState(key ssKey, targetURL string, cfg config.ScaleSet, gh GitHub, wake func(),
	now func() time.Time) *scaleSetState {
	return &scaleSetState{key: key, targetURL: targetURL, cfg: cfg, gh: gh, wake: wake, busy: map[string]bool{},
		now: now}
}

// description is the VM description of the scale set's workers.
func (s *scaleSetState) description() string {
	of := ""
	if s.targetURL != "" {
		of = " of " + s.targetURL
	}
	return "Runner for scale set " + s.cfg.Name + of + ". Managed by proxmox-actions-runners; destroyed after one job."
}

// SessionOpened implements github.SessionObserver.
func (s *scaleSetState) SessionOpened() {
	s.mu.Lock()
	s.sessionOpen, s.sessionSince, s.sessionErr = true, s.now(), ""
	s.mu.Unlock()
}

// SessionEnded implements github.SessionObserver.
func (s *scaleSetState) SessionEnded(err error) {
	s.mu.Lock()
	s.sessionOpen, s.sessionSince, s.sessionErr = false, s.now(), err.Error()
	s.mu.Unlock()
}

// RecordStats implements github.StatsRecorder.
func (s *scaleSetState) RecordStats(st github.Stats) {
	s.mu.Lock()
	s.stats = &st
	s.mu.Unlock()
}

// DesiredRunners implements github.Handler. The target is minRunners idle workers on top of one per assigned job,
// capped at maxRunners.
func (s *scaleSetState) DesiredRunners(_ context.Context, assignedJobs int) (int, error) {
	target := min(s.cfg.MaxRunners, s.cfg.MinRunners+assignedJobs)
	s.mu.Lock()
	changed := !s.known || s.desired != target
	s.desired, s.known = target, true
	s.mu.Unlock()
	if changed {
		s.wake()
	}
	return target, nil
}

// JobStarted implements github.Handler.
func (s *scaleSetState) JobStarted(_ context.Context, job github.Job) error {
	s.mu.Lock()
	s.busy[job.RunnerName] = true
	s.mu.Unlock()
	return nil
}

// JobCompleted implements github.Handler. The worker powers itself off after its job, and the next pass
// destroys it.
func (s *scaleSetState) JobCompleted(_ context.Context, job github.Job) error {
	s.mu.Lock()
	delete(s.busy, job.RunnerName)
	s.mu.Unlock()
	s.wake()
	return nil
}

// scaleSetID returns GitHub's ID for the scale set, or 0 if it isn't registered yet.
func (s *scaleSetState) scaleSetID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

func (s *scaleSetState) setScaleSetID(id int) {
	s.mu.Lock()
	s.id = id
	s.mu.Unlock()
}

// target returns the desired worker count and whether it is known yet.
func (s *scaleSetState) target() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.desired, s.known
}

// hasJob reports whether the controller knows the runner is running a job.
func (s *scaleSetState) hasJob(runner string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy[runner]
}

// markRunning records that a runner is in a job the controller didn't hear about: GitHub refused to remove it.
func (s *scaleSetState) markRunning(runner string) {
	s.mu.Lock()
	s.busy[runner] = true
	s.mu.Unlock()
}

// forget drops what the scale set knows about a runner whose worker is gone.
func (s *scaleSetState) forget(runner string) {
	s.mu.Lock()
	delete(s.busy, runner)
	s.mu.Unlock()
}

// wanting records how many more workers the scale set wants.
func (s *scaleSetState) wanting(n int, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case n == 0:
		s.wantSince = time.Time{}
	case s.wantSince.IsZero():
		s.wantSince = now
	}
}

func (s *scaleSetState) waitingSince() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wantSince
}

func (s *scaleSetState) backOff(until time.Time) {
	s.mu.Lock()
	s.retryAfter = until
	s.mu.Unlock()
}

func (s *scaleSetState) canCreate(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !now.Before(s.retryAfter)
}

var (
	_ github.Handler         = (*scaleSetState)(nil)
	_ github.SessionObserver = (*scaleSetState)(nil)
	_ github.StatsRecorder   = (*scaleSetState)(nil)
)
