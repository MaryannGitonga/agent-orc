package orc

import (
	"errors"
	"fmt"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/state"
)

// runScope is a task store tied to one dispatch of a task.
//
// A task id becomes reusable the moment its task is cleaned up, and the work
// that happens after an agent exits, the test loop, the review loop and the
// publish chain, can outlive that. Everything in this package that keeps
// working on a task's behalf holds one of these, so a write only lands while
// the id still names the run it started on.
//
// A zero since is unscoped, which is what a command a human typed gets: it acts
// on whatever the id names now.
type runScope struct {
	store *state.Store
	since time.Time
}

// scopeTo returns a scope over store, tied to the dispatch that began at since.
// A zero since is unscoped.
func scopeTo(store *state.Store, since time.Time) runScope {
	return runScope{store: store, since: since}
}

// owns reports whether a record is still the run this scope belongs to.
func (s runScope) owns(rec state.Task) bool {
	return s.since.IsZero() || rec.StartedAt.Equal(s.since)
}

// update applies mutate unless the id has moved on to a later run.
//
// The check and the write are one locked step, so a task cleaned up and
// dispatched again cannot slip between them. Declining writes nothing at all:
// saving an unchanged copy would still stamp this run's snapshot over whatever
// the newer one had written.
func (s runScope) update(id string, mutate func(*state.Task)) error {
	_, err := s.updateOwned(id, mutate)
	return err
}

// updateOwned is update, reporting whether the record was still this run's. It
// is for the caller that has something to undo when the write does not land,
// rather than only a line to log.
func (s runScope) updateOwned(id string, mutate func(*state.Task)) (bool, error) {
	owned := false
	err := s.store.UpdateIf(id, func(k *state.Task) bool {
		if !s.owns(*k) {
			return false
		}
		owned = true
		mutate(k)
		return true
	})
	return owned, err
}

// ownsID reads the record and reports whether it is still this run's. A record
// that cannot be read is not, which is the safe answer for a guard on a write:
// it is the same as the task having gone. Callers that report why they stopped
// want halted instead, which says what it found.
func (s runScope) ownsID(id string) bool {
	rec, err := s.store.Load(id)
	return err == nil && s.owns(rec)
}

// halted reports why this run must go no further, or nil to carry on.
//
// Between commands there is no child to be killed, so a stop shows up only on
// the record. So do the other endings: a task that was cleaned up, or an id
// taken by a later run. They are different things and say so, because a run
// that lost its id is not one a human stopped, and a store that cannot be read
// is neither and must not be reported as either.
//
// All of it comes from one read. Asking twice would let the answer be
// assembled from two different snapshots, with the id cleaned up in between.
//
// That read is retried briefly before a failure counts as an ending. A record
// is written to a temporary file and renamed, so a half-read one is not
// something that happens; a momentary failure to open one is, and ending a loop
// that has been running for an hour over a single one of those would be its own
// kind of bug. A failure that persists is still reported rather than guessed at.
func (s runScope) halted(id string) error {
	var rec state.Task
	var err error
	for attempt := 0; ; attempt++ {
		if rec, err = s.store.Load(id); err == nil || errors.Is(err, state.ErrNotFound) || attempt == 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	switch {
	case errors.Is(err, state.ErrNotFound):
		return fmt.Errorf("task %q no longer exists", id)
	case err != nil:
		// Returned as it stands: the store's error already names the task and
		// what it was doing, and wrapping it again only says so twice.
		return err
	case !s.owns(rec):
		return fmt.Errorf("task %q now belongs to a later run", id)
	case rec.Status == state.StatusStopped:
		return fmt.Errorf("task %q was stopped", id)
	}
	return nil
}
