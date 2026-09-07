package orc

import (
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
func (s runScope) update(id string, mutate func(*state.Task)) error {
	return s.store.Update(id, func(k *state.Task) {
		if s.owns(*k) {
			mutate(k)
		}
	})
}

// ownsID reads the record and reports whether it is still this run's.
func (s runScope) ownsID(id string) bool {
	_, ok := s.mine(id)
	return ok
}

// mine reads the record and reports whether it is still this run's. A record
// that cannot be read is not, which is the safe answer: it is the same as the
// task having gone.
func (s runScope) mine(id string) (state.Task, bool) {
	rec, err := s.store.Load(id)
	if err != nil {
		return state.Task{}, false
	}
	return rec, s.owns(rec)
}

// stopped reports whether a human has stopped the task. Between commands there
// is no child to be killed, so the record is the only place a stop shows.
func (s runScope) stopped(id string) bool {
	rec, err := s.store.Load(id)
	return err == nil && rec.Status == state.StatusStopped
}
