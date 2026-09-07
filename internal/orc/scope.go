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
//
// The check and the write are one locked step, so a task cleaned up and
// dispatched again cannot slip between them. Declining writes nothing at all:
// saving an unchanged copy would still stamp this run's snapshot over whatever
// the newer one had written.
func (s runScope) update(id string, mutate func(*state.Task)) error {
	return s.store.UpdateIf(id, func(k *state.Task) bool {
		if !s.owns(*k) {
			return false
		}
		mutate(k)
		return true
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

// stopped reports whether this run should go no further. Between commands there
// is no child to be killed, so the record is the only place a stop shows.
//
// A record that has gone or moved to a later run counts as stopped: this run is
// over either way, and carrying on would mean working in a worktree that now
// belongs to someone else. It comes from the same read as the ownership check
// so the two cannot disagree about which record they saw.
func (s runScope) stopped(id string) bool {
	rec, ok := s.mine(id)
	return !ok || rec.Status == state.StatusStopped
}
