package orc

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/state"
)

// unknown is what a table cell shows when there is no value to show.
const unknown = "-"

// Reporter prints what agent-orc knows about the tasks it has dispatched.
type Reporter struct {
	store *state.Store
	out   io.Writer
}

// NewReporter returns a reporter reading from the layout's state directory.
func NewReporter(stateDir string, out io.Writer) *Reporter {
	return &Reporter{store: state.NewStore(stateDir), out: out}
}

// Status prints one row per task. A plain table is deliberately the whole of
// the reporting surface: anything richer is a dashboard, and there is nothing
// yet to justify one.
func (r *Reporter) Status() error {
	tasks, err := r.store.List()
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		fmt.Fprintln(r.out, "no tasks; dispatch one with 'agent-orc run'")
		return nil
	}

	w := tabwriter.NewWriter(r.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tCLI\tMODEL\tSTATUS\tSPEND\tROUNDS\tBRANCH\tELAPSED")
	var notes []string
	for _, t := range tasks {
		t = reconcile(r.store, t)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			t.ID, t.CLI, orUnknown(t.Model), t.Status, spend(t), rounds(t), t.Branch, elapsed(t))
		if t.BudgetNote != "" {
			notes = append(notes, fmt.Sprintf("%s: %s", t.ID, t.BudgetNote))
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("writing the status table: %w", err)
	}
	for _, n := range notes {
		fmt.Fprintf(r.out, "\nnote  %s\n", n)
	}
	return nil
}

// reconcile corrects a record that claims to be running behind a process that
// is gone after a machine reboot, or a supervisor killed outright. Without it,
// `status` would report a task as running forever.
func reconcile(store *state.Store, t state.Task) state.Task {
	if !t.Status.HasProcess() || t.PID == 0 || processAlive(t.PID) {
		return t
	}
	now := time.Now().UTC()
	// Decide again inside the update, against the record as it is on disk
	// rather than the copy this listing read. The supervisor may have written
	// its own outcome in between, and saving the stale copy would discard that
	// along with the spend and session it recorded. A window still remains
	// between that load and its save; for a reporting command in a single-user
	// tool that is an acceptable trade rather than a lock.
	_ = store.Update(t.ID, func(k *state.Task) {
		if !k.Status.Active() || k.PID == 0 || processAlive(k.PID) {
			return
		}
		k.Status = state.StatusFailed
		k.PID = 0
		k.FinishedAt = &now
		k.Error = processGoneNote
	})
	// Reporting is best-effort: fall back to a locally corrected row if the
	// record cannot be read back.
	fresh, err := store.Load(t.ID)
	if err != nil {
		t.Status = state.StatusFailed
		t.PID = 0
		t.FinishedAt = &now
		t.Error = processGoneNote
		return t
	}
	return fresh
}

// processGoneNote explains a task reconciled from running to failed.
const processGoneNote = "the agent process is no longer running; it was killed or the machine restarted"

// processAlive reports whether a pid still refers to a live process. Signal 0
// runs the existence and permission checks without delivering anything, so
// only ESRCH means gone: EPERM is a process that is very much alive, just
// owned by someone else.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// spend renders "$spent / $budget" in whichever units are known.
func spend(t state.Task) string {
	spent := unknown
	if t.SpentUSD != nil {
		spent = "$" + strconv.FormatFloat(*t.SpentUSD, 'f', 2, 64)
	} else if t.Tokens != nil {
		spent = strconv.Itoa(*t.Tokens) + " tok"
	}
	switch {
	case t.Budget.USD != nil:
		return spent + " / $" + strconv.FormatFloat(*t.Budget.USD, 'f', 2, 64)
	case t.Budget.Credits != nil:
		return spent + " / " + strconv.FormatFloat(*t.Budget.Credits, 'f', -1, 64) + " cr"
	default:
		return spent
	}
}

// rounds renders review progress against the task's cap.
func rounds(t state.Task) string {
	if !t.Review.Enabled {
		return unknown
	}
	return strconv.Itoa(t.ReviewRound) + "/" + strconv.Itoa(t.Review.Rounds())
}

// elapsed renders how long a task ran, or has been running.
func elapsed(t state.Task) string {
	if t.StartedAt.IsZero() {
		return unknown
	}
	end := time.Now().UTC()
	if t.FinishedAt != nil {
		end = *t.FinishedAt
	}
	d := end.Sub(t.StartedAt).Round(time.Second)
	if d < 0 {
		return unknown
	}
	return trimZeroTail(d.String())
}

// trimZeroTail shortens "1m0s" to "1m" and "2h0m0s" to "2h". Duration.String()
// always spells out every unit, so a whole number of minutes carries a "0s"
// that says nothing. A zero component only ever follows another unit's letter,
// which is what makes stripping it safe: "40s" keeps its seconds, and a bare
// "0s" is left alone rather than trimmed away to nothing.
func trimZeroTail(s string) string {
	for _, zero := range []string{"0s", "0m"} {
		trimmed := strings.TrimSuffix(s, zero)
		if trimmed == s || trimmed == "" {
			continue
		}
		if last := trimmed[len(trimmed)-1]; last == 'm' || last == 'h' {
			s = trimmed
		}
	}
	return s
}

// orUnknown renders an empty value as the table's placeholder.
func orUnknown(s string) string {
	if s == "" {
		return unknown
	}
	return s
}
