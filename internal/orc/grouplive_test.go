package orc

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestSignalGroupOutlastsAStubbornChild covers the difference between asking
// whether a process is alive and asking whether its group is. An agent's test
// runner and compiler sit in the same group as the leader, so a leader that
// exits on SIGTERM while one of those ignores it would end the wait early: the
// escalation never happens and the child runs on after the task reads stopped.
func TestSignalGroupOutlastsAStubbornChild(t *testing.T) {
	// A leader that ignores SIGTERM itself and exits after a beat, having
	// spawned a child in its group that ignores SIGTERM and never exits.
	cmd := exec.Command("sh", "-c",
		`trap "" TERM
		 sh -c 'trap "" TERM; while true; do sleep 0.2; done' &
		 echo $! > "$0"
		 sleep 0.4`,
		t.TempDir()+"/child.pid")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the group: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })

	// Let the leader spawn its child and exit, so only the child remains.
	go func() { _ = cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processAlive(pid) {
		t.Skip("the leader did not exit in time; nothing to assert")
	}
	if !groupAlive(pid) {
		t.Fatal("the group reads as gone while its child is still running")
	}

	// The child ignores SIGTERM, so this must sit out the grace period and
	// then kill it rather than returning as soon as the leader is gone.
	start := time.Now()
	if err := signalGroup(pid); err != nil {
		t.Fatalf("signalGroup() = %v", err)
	}
	if took := time.Since(start); took < stopGrace {
		t.Errorf("signalGroup returned after %s, before the %s grace: it stopped at the leader", took, stopGrace)
	}
	if groupAlive(pid) {
		t.Error("the group survived; the escalation never reached the child")
	}
}

// TestGroupAliveIgnoresAReusedPID covers the difference between "this group is
// gone" and "something else holds that number now". A pid is reused as soon as
// its process is reaped, so a record left stale by a missed reconcile names a
// pid that may belong to anyone. Consulting the bare pid there would have stop
// wait on a stranger and then SIGKILL it.
func TestGroupAliveIgnoresAReusedPID(t *testing.T) {
	// A live process that is not a group leader, which is what a reused pid
	// almost always names: it inherits this test binary's group, so no group
	// carries its own number.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	if !processAlive(pid) {
		t.Fatal("the stand-in process is not running, so there is nothing to confuse")
	}
	if syscall.Kill(-pid, 0) == nil {
		t.Skip("this pid happens to name a real group; nothing to assert")
	}
	if groupAlive(pid) {
		t.Error("a live process that leads no group was read as the task's group still running")
	}
}
