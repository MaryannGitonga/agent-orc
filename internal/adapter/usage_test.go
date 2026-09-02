package adapter

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// logWith writes a log file holding body and returns its path.
func logWith(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "task.log")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClaudeParsesCostAndTokens(t *testing.T) {
	path := logWith(t, `starting up
{"type":"progress"}
{"type":"result","total_cost_usd":0.4213,"usage":{"input_tokens":120,"output_tokens":80,"cache_read_input_tokens":400}}
`)
	got, err := Claude{}.ParseUsage(path)
	if err != nil {
		t.Fatalf("ParseUsage() = %v", err)
	}
	if got.CostUSD == nil || *got.CostUSD != 0.4213 {
		t.Errorf("CostUSD = %v, want 0.4213", got.CostUSD)
	}
	if got.Tokens == nil || *got.Tokens != 600 {
		t.Errorf("Tokens = %v, want 600 (every token field summed)", got.Tokens)
	}
}

func TestClaudeTakesTheLastResultInTheLog(t *testing.T) {
	// Progress output is interleaved with the final result, so the last valid
	// object wins rather than the first.
	path := logWith(t, `{"total_cost_usd":0.1}
not json at all
{"total_cost_usd":0.9}
`)
	got, err := Claude{}.ParseUsage(path)
	if err != nil {
		t.Fatalf("ParseUsage() = %v", err)
	}
	if got.CostUSD == nil || *got.CostUSD != 0.9 {
		t.Errorf("CostUSD = %v, want the last result 0.9", got.CostUSD)
	}
}

func TestClaudeReportsNoUsageRatherThanGuessing(t *testing.T) {
	tests := map[string]string{
		"no json":       "just some prose\n",
		"json but bare": "{\"type\":\"result\"}\n",
		"empty":         "",
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Claude{}.ParseUsage(logWith(t, body))
			if !errors.Is(err, ErrNoUsage) {
				t.Errorf("ParseUsage() = %v, want ErrNoUsage", err)
			}
		})
	}
}

func TestParseUsageOnAMissingLog(t *testing.T) {
	_, err := Claude{}.ParseUsage(filepath.Join(t.TempDir(), "absent.log"))
	if !errors.Is(err, ErrNoUsage) {
		t.Errorf("ParseUsage() = %v, want ErrNoUsage", err)
	}
}

func TestCLIsWithoutUsageReportingSaySo(t *testing.T) {
	path := logWith(t, `{"total_cost_usd":0.5}`)
	for name, a := range map[string]Adapter{"copilot": Copilot{}, "codex": Codex{}} {
		t.Run(name, func(t *testing.T) {
			if _, err := a.ParseUsage(path); !errors.Is(err, ErrNoUsage) {
				t.Errorf("ParseUsage() = %v, want ErrNoUsage", err)
			}
		})
	}
}
