package bounded

import (
	"strings"
	"testing"
	"time"
)

func TestACommandWithinItsLimitRunsNormally(t *testing.T) {
	out, err := Within(time.Minute, "echo", "hello").Output()
	if err != nil || strings.TrimSpace(string(out)) != "hello" {
		t.Errorf("Output() = %q, %v, want hello", out, err)
	}
}

func TestAHungCommandIsKilledAndSaysSo(t *testing.T) {
	start := time.Now()
	err := Within(100*time.Millisecond, "sleep", "30").Run()
	if err == nil || !strings.Contains(err.Error(), "sleep timed out after 100ms") {
		t.Errorf("Run() = %v, want it to say sleep timed out", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("Run() took %s, want it ended near its limit", took)
	}
}

// TestAChildHoldingTheOutputDoesNotHangWait covers git push killed while its
// ssh child still holds the output pipe open.
func TestAChildHoldingTheOutputDoesNotHangWait(t *testing.T) {
	start := time.Now()
	_, err := Within(100*time.Millisecond, "sh", "-c", "sleep 30 & sleep 30").CombinedOutput()
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("CombinedOutput() = %v, want a timeout", err)
	}
	if took := time.Since(start); took > waitDelay+5*time.Second {
		t.Errorf("CombinedOutput() took %s, want it bounded by the limit and waitDelay", took)
	}
}

func TestAFailureWithinTheLimitIsNotCalledATimeout(t *testing.T) {
	err := Within(time.Minute, "false").Run()
	if err == nil || strings.Contains(err.Error(), "timed out") {
		t.Errorf("Run() = %v, want the plain exit error", err)
	}
}
