package orc

import (
	"strings"
	"testing"
)

// format runs the whole input through a formatter, in one write.
func format(t *testing.T, in string) string {
	t.Helper()
	var b strings.Builder
	f := &logFormatter{out: &b}
	if _, err := f.Write([]byte(in)); err != nil {
		t.Fatalf("writing: %v", err)
	}
	if err := f.Flush(); err != nil {
		t.Fatalf("flushing: %v", err)
	}
	return b.String()
}

func TestLogFormatter(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
		skip []string
	}{{
		name: "prose is passed through untouched",
		in:   "Reading main.go\nEditing main.go\n",
		want: []string{"Reading main.go\nEditing main.go\n"},
	}, {
		name: "a result object becomes its answer and a footer",
		in: `{"type":"result","subtype":"success","is_error":false,"num_turns":10,` +
			`"duration_ms":30679,"total_cost_usd":0.11469460000000001,"stop_reason":"end_turn",` +
			`"session_id":"6fb7fb30","permission_denials":[],"result":"Added the greeting flag.",` +
			`"usage":{"input_tokens":20,"output_tokens":1171}}` + "\n",
		want: []string{
			"Added the greeting flag.\n\n",
			"status   success, 10 turns, 30.7s\n",
			"cost     $0.1147\n",
			"session  6fb7fb30\n",
		},
		// The token accounting is the noise the summary exists to drop.
		skip: []string{"input_tokens", "usage", "duration_ms"},
	}, {
		name: "a failed run says so, and names a stop reason that is not end_turn",
		in: `{"is_error":true,"stop_reason":"max_budget_exceeded","num_turns":1,` +
			`"result":"Budget exhausted."}` + "\n",
		want: []string{
			"Budget exhausted.\n\n",
			"status   error, 1 turn, stopped: max_budget_exceeded\n",
		},
	}, {
		name: "denied tool calls get their own line",
		in:   `{"subtype":"success","permission_denials":[{"tool":"Bash"},{"tool":"Edit"}],"result":"Nothing to do."}` + "\n",
		want: []string{"denied   2 tool calls\n"},
	}, {
		name: "an object with no result is indented rather than summarized",
		in:   `{"type":"assistant","message":"thinking"}` + "\n",
		want: []string{"{\n  \"message\": \"thinking\",\n  \"type\": \"assistant\"\n}\n"},
	}, {
		name: "a line that only looks like json is left alone",
		in:   "{not json at all\n",
		want: []string{"{not json at all\n"},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := format(t, tt.in)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("output is missing %q\ngot:\n%s", want, got)
				}
			}
			for _, skip := range tt.skip {
				if strings.Contains(got, skip) {
					t.Errorf("output still holds %q\ngot:\n%s", skip, got)
				}
			}
		})
	}
}

// TestLogFormatterReassemblesSplitLines covers the streaming case: a result is
// one long line, and the copy feeding the formatter reads fixed-size chunks,
// so almost every real run splits that line across several writes.
func TestLogFormatterReassemblesSplitLines(t *testing.T) {
	line := `{"subtype":"success","result":"Done.","session_id":"abc"}` + "\n"
	var b strings.Builder
	f := &logFormatter{out: &b}
	for _, chunk := range []string{line[:9], line[9:30], line[30:]} {
		if _, err := f.Write([]byte(chunk)); err != nil {
			t.Fatalf("writing: %v", err)
		}
	}
	if err := f.Flush(); err != nil {
		t.Fatalf("flushing: %v", err)
	}
	if got := b.String(); !strings.Contains(got, "Done.") || !strings.Contains(got, "session  abc") {
		t.Errorf("split writes were not reassembled, got:\n%s", got)
	}
}

// TestLogFormatterHoldsAnUnterminatedLine checks that a partial line is not
// printed early. Following a running task writes whatever the agent has
// flushed so far, and rendering half a JSON object as prose would be wrong.
func TestLogFormatterHoldsAnUnterminatedLine(t *testing.T) {
	var b strings.Builder
	f := &logFormatter{out: &b}
	if _, err := f.Write([]byte(`{"subtype":"success","resu`)); err != nil {
		t.Fatalf("writing: %v", err)
	}
	if b.Len() != 0 {
		t.Errorf("wrote %q before the line was complete", b.String())
	}
	if _, err := f.Write([]byte(`lt":"Done."}` + "\n")); err != nil {
		t.Fatalf("writing: %v", err)
	}
	if got := b.String(); !strings.Contains(got, "Done.") {
		t.Errorf("the completed line was not rendered, got:\n%s", got)
	}
}
