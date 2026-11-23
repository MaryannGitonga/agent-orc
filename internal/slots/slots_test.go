package slots

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func init() { pollEvery = 10 * time.Millisecond }

func never() error { return nil }

func TestNoCapTakesNoSlot(t *testing.T) {
	release, err := Pool{Dir: t.TempDir(), Size: 0}.Acquire("t", never, func() { t.Error("waited with no cap") })
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestAFullPoolWaitsForASlotToFree(t *testing.T) {
	pool := Pool{Dir: t.TempDir(), Size: 1}
	first, err := pool.Acquire("a", never, func() {})
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{}, 1)
	got := make(chan func(), 1)
	go func() {
		release, _ := pool.Acquire("b", never, func() { waited <- struct{}{} })
		got <- release
	}()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("the second run never reported waiting")
	}
	select {
	case <-got:
		t.Fatal("the second run took a slot while the only one was held")
	case <-time.After(100 * time.Millisecond):
	}
	first()
	select {
	case release := <-got:
		release()
	case <-time.After(5 * time.Second):
		t.Fatal("the second run never got the freed slot")
	}
}

// TestSlotsGoInDispatchOrder covers fairness: the run dispatched first gets
// the next slot, even when a later one started waiting before it.
func TestSlotsGoInDispatchOrder(t *testing.T) {
	pool := Pool{Dir: t.TempDir(), Size: 1}
	held, err := pool.Acquire("0", never, func() {})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	for _, ticket := range []string{"2-later", "1-earlier"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := pool.Acquire(ticket, never, func() {})
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			order = append(order, ticket)
			mu.Unlock()
			time.Sleep(50 * time.Millisecond)
			release()
		}()
		time.Sleep(50 * time.Millisecond) // the later one joins first
	}
	held()
	wg.Wait()
	if len(order) != 2 || order[0] != "1-earlier" {
		t.Errorf("slots went to %v, want 1-earlier first", order)
	}
}

func TestADeadWaiterDoesNotBlockTheQueue(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "queue"), 0o755); err != nil {
		t.Fatal(err)
	}
	// An older ticket nobody holds: its waiter died.
	dead := filepath.Join(dir, "queue", "0-dead.wait")
	if err := os.WriteFile(dead, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	release, err := Pool{Dir: dir, Size: 1}.Acquire("1-alive", never, func() {})
	if err != nil {
		t.Fatalf("Acquire() = %v", err)
	}
	release()
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Error("the dead waiter's ticket was left in the queue")
	}
}

func TestHaltedEndsTheWaitAndLeavesTheQueue(t *testing.T) {
	dir := t.TempDir()
	pool := Pool{Dir: dir, Size: 1}
	held, err := pool.Acquire("0", never, func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	stopped := errors.New("task was stopped")
	calls := 0
	_, err = pool.Acquire("1", func() error {
		if calls++; calls > 3 {
			return stopped
		}
		return nil
	}, func() {})
	if !errors.Is(err, stopped) {
		t.Fatalf("Acquire() = %v, want the halt reason", err)
	}
	if tickets, _ := filepath.Glob(filepath.Join(dir, "queue", "*.wait")); len(tickets) != 0 {
		t.Errorf("tickets left after a halted wait: %v", tickets)
	}
}

func TestTicketsSortInDispatchOrder(t *testing.T) {
	early := Ticket(time.Unix(1, 0), "z")
	late := Ticket(time.Unix(10, 0), "a")
	if !(early < late) {
		t.Errorf("Ticket order: %q sorts after %q", early, late)
	}
}

// TestAnUnusableSlotIsAnErrorNotAWait covers a damaged slots directory: a
// slot that cannot be locked must fail the run, not stall the whole queue.
func TestAnUnusableSlotIsAnErrorNotAWait(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "slot-0.lock"), 0o755); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Pool{Dir: dir, Size: 1}.Acquire("t", never, func() {})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "slot-0.lock") {
			t.Errorf("Acquire() = %v, want an error naming the slot", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Acquire() is still waiting on a slot that can never be taken")
	}
}
