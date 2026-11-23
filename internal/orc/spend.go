package orc

import (
	"errors"
	"fmt"

	"github.com/MaryannGitonga/agent-orc/internal/adapter"
	"github.com/MaryannGitonga/agent-orc/internal/paths"
	"github.com/MaryannGitonga/agent-orc/internal/state"
	"github.com/MaryannGitonga/agent-orc/internal/task"
)

// ErrBudgetSpent means a task has used its whole budget, so nothing more runs.
var ErrBudgetSpent = errors.New("the task's budget is spent")

// taskSpend is what a task has cost so far, read from its logs: the worker
// session's latest result, which Claude totals across resumes, plus every
// review run, each a session of its own. Nil when nothing reports a cost; an
// error when a log that should say could not be read, so the total is unknown.
func taskSpend(layout paths.Layout, record state.Task) (*adapter.Usage, error) {
	var total adapter.Usage
	add := func(u adapter.Usage) {
		if u.CostUSD != nil {
			sum := *u.CostUSD
			if total.CostUSD != nil {
				sum += *total.CostUSD
			}
			total.CostUSD = &sum
		}
		if u.Tokens != nil {
			sum := *u.Tokens
			if total.Tokens != nil {
				sum += *total.Tokens
			}
			total.Tokens = &sum
		}
	}
	if a, err := adapter.For(record.CLI); err == nil {
		u, err := a.ParseUsage(record.LogPath)
		switch {
		case err == nil:
			add(*u)
		case !errors.Is(err, adapter.ErrNoUsage):
			return nil, err
		}
	}
	if record.Review.Enabled {
		if a, err := adapter.For(record.ReviewerCLI()); err == nil {
			runs, err := a.ParseUsages(layout.ReviewLogFile(record.ID))
			if err != nil && !errors.Is(err, adapter.ErrNoUsage) {
				return nil, err
			}
			for _, u := range runs {
				add(u)
			}
		}
	}
	if total.CostUSD == nil && total.Tokens == nil {
		return nil, nil
	}
	return &total, nil
}

// budgetLeft is what remains of b once spent is taken off, to cap the next
// run, or ErrBudgetSpent when a dollar budget is used up. Credits are not
// measured, so a credit budget is passed on whole.
func budgetLeft(b task.Budget, spent *adapter.Usage) (task.Budget, error) {
	if b.USD == nil || spent == nil || spent.CostUSD == nil {
		return b, nil
	}
	left := *b.USD - *spent.CostUSD
	if left <= 0 {
		return b, fmt.Errorf("%w: $%.2f of $%.2f used; the work so far is on its branch",
			ErrBudgetSpent, *spent.CostUSD, *b.USD)
	}
	b.USD = &left
	return b, nil
}

// recordSpend works out a task's spend and writes it to its record.
func recordSpend(scope runScope, layout paths.Layout, record state.Task) (*adapter.Usage, error) {
	spent, err := taskSpend(layout, record)
	if spent != nil {
		_ = scope.update(record.ID, func(k *state.Task) { k.SpentUSD, k.Tokens = spent.CostUSD, spent.Tokens })
	}
	return spent, err
}

// nextBudget records what the task has spent so far and returns the budget the
// next run may use, or ErrBudgetSpent. A dollar budget with a spend that cannot
// be read allows nothing more: an unknown total is not a total under the cap.
func nextBudget(scope runScope, layout paths.Layout, record state.Task) (task.Budget, error) {
	spent, err := recordSpend(scope, layout, record)
	if err != nil && record.Budget.USD != nil {
		return record.Budget, fmt.Errorf("%w: cannot read what it has spent: %w", ErrBudgetSpent, err)
	}
	return budgetLeft(record.Budget, spent)
}
