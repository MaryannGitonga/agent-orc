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
func (s runScope) halted(id string) error {
	rec, err := s.store.Load(id)
	switch {
	case errors.Is(err, state.ErrNotFound):
		return fmt.Errorf("task %q no longer exists", id)
	case err != nil:
		return fmt.Errorf("reading task %q: %w", id, err)
	case !s.owns(rec):
		return fmt.Errorf("task %q now belongs to a later run", id)
	case rec.Status == state.StatusStopped:
		return fmt.Errorf("task %q was stopped", id)
	}
	return nil
}
