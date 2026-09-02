package orc

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/MaryannGitonga/agent-orc/internal/state"
)

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
// the reporting surface — anything richer is a dashboard, and there is nothing
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
	fmt.Fprintln(w, "ID\tCLI\tMODEL\tSTATUS\tSPEND\tBRANCH\tELAPSED")
	var notes []string
	for _, t := range tasks {
		t = reconcile(r.store, t)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			t.ID, t.CLI, orDash(t.Model), t.Status, spend(t), t.Branch, elapsed(t))
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
// is gone — a machine reboot, or a supervisor killed outright. Without this,
// `status` would report a task as running forever.
func reconcile(store *state.Store, t state.Task) state.Task {
	if !t.Status.Active() || t.PID == 0 || processAlive(t.PID) {
		return t
	}
	now := time.Now().UTC()
	t.Status = state.StatusFailed
	t.PID = 0
	t.FinishedAt = &now
	t.Error = "the agent process is no longer running; it was killed or the machine restarted"
	if err := store.Save(t); err != nil {
		// Reporting is best-effort: show the corrected row even if the
		// correction could not be written back.
		return t
	}
	return t
}

// processAlive reports whether a pid still refers to a live process. Signal 0
// performs the permission and existence checks without delivering anything.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// spend renders "$spent / $budget" in whichever units are known.
func spend(t state.Task) string {
	spent := "—"
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

// elapsed renders how long a task ran, or has been running.
func elapsed(t state.Task) string {
	if t.StartedAt.IsZero() {
		return "—"
	}
	end := time.Now().UTC()
	if t.FinishedAt != nil {
		end = *t.FinishedAt
	}
	d := end.Sub(t.StartedAt).Round(time.Second)
	if d < 0 {
		return "—"
	}
	return strings.TrimSuffix(d.String(), "0s0ms")
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
