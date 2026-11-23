// Package slots caps how many tasks run at once: each slot is a lock file, and
// waiters queue in dispatch order. A dead process's locks free themselves.
package slots

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// pollEvery is how often a waiting supervisor looks for a free slot.
var pollEvery = time.Second

// Pool is a machine's slots. A Size of zero or less means no cap.
type Pool struct {
	Dir  string
	Size int
}

// Acquire waits for a slot, earliest ticket first, and returns its release. An
// error from halted ends the wait; onWait is called once if it has to wait.
func (p Pool) Acquire(ticket string, halted func() error, onWait func()) (func(), error) {
	if p.Size <= 0 {
		return func() {}, nil
	}
	queue := filepath.Join(p.Dir, "queue")
	if err := os.MkdirAll(queue, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", queue, err)
	}
	mine, leave, err := join(queue, ticket)
	if err != nil {
		return nil, err
	}
	defer leave()

	for waited := false; ; waited = true {
		if err := halted(); err != nil {
			return nil, err
		}
		isFirst, err := first(queue, mine)
		if err != nil {
			return nil, err
		}
		if isFirst {
			release, err := p.take()
			if err != nil || release != nil {
				return release, err
			}
		}
		if !waited {
			onWait()
		}
		time.Sleep(pollEvery)
	}
}

// join adds a held ticket to the queue. It is locked before it is renamed into
// place, so no one ever sees it unheld and takes it for a dead waiter's.
func join(queue, ticket string) (name string, leave func(), err error) {
	tmp, err := os.CreateTemp(queue, "join-*.tmp")
	if err != nil {
		return "", nil, fmt.Errorf("joining the queue: %w", err)
	}
	if err := syscall.Flock(int(tmp.Fd()), syscall.LOCK_EX); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", nil, fmt.Errorf("joining the queue: %w", err)
	}
	name = ticket + ".wait"
	path := filepath.Join(queue, name)
	if err := os.Rename(tmp.Name(), path); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", nil, fmt.Errorf("joining the queue: %w", err)
	}
	return name, func() { os.Remove(path); tmp.Close() }, nil
}

// first reports whether mine is the oldest ticket whose waiter is alive,
// removing any dead waiter's ticket it passes on the way.
func first(queue, mine string) (bool, error) {
	entries, err := os.ReadDir(queue)
	if err != nil {
		return false, fmt.Errorf("reading the slot queue: %w", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".wait") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if name == mine {
			return true, nil
		}
		path := filepath.Join(queue, name)
		if held(path) {
			return false, nil
		}
		_ = os.Remove(path)
	}
	return false, fmt.Errorf("this run's ticket %s left the slot queue", mine)
}

// held reports whether some process holds the lock on path.
func held(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true
	}
	if err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}
	return false
}

// take holds the first free slot, or returns nil when all are in use. A slot
// that cannot be used is an error, not a busy slot nobody will ever free.
func (p Pool) take() (func(), error) {
	for k := 0; k < p.Size; k++ {
		path := filepath.Join(p.Dir, fmt.Sprintf("slot-%d.lock", k))
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return nil, fmt.Errorf("opening slot %s: %w", path, err)
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { f.Close() }, nil
		}
		f.Close()
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("locking slot %s: %w", path, err)
		}
	}
	return nil, nil
}

// Ticket names a run's place in the queue: its dispatch time first, so the
// queue keeps dispatch order, then something unique to the run.
func Ticket(started time.Time, unique string) string {
	return fmt.Sprintf("%020d-%s", started.UnixNano(), unique)
}
