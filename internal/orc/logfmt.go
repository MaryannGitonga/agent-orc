package orc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// maxLineBuffer caps how much of an unterminated line is held while waiting
// for its newline. An agent's final result arrives as one long JSON line, so
// the buffer has to be generous; past the cap the bytes are written through
// unformatted rather than grown without limit.
const maxLineBuffer = 8 << 20

// logFormatter renders an agent's log as it streams. The CLIs that report
// machine-readable results write them as a single JSON object per line, which
// is unreadable in a terminal, so those lines become a short summary. Every
// other line is passed through untouched, which is the whole of what the CLIs
// that only write prose produce.
//
// It is an io.Writer so it can sit in front of the same copy loop the raw path
// uses, including while following a running task. Bytes arrive in whatever
// chunks the copy happens to read, so a line split across two writes is held
// until its newline turns up; Flush writes whatever is left at the end.
type logFormatter struct {
	out io.Writer
	buf []byte
}

// Write consumes a chunk of log, rendering every complete line in it.
//
// It reports len(p) on the way out even when it failed, because p is appended
// to the buffer before any of it is rendered: by the time anything can go
// wrong the bytes have been taken, and saying none were would invite a caller
// to send them again.
func (f *logFormatter) Write(p []byte) (int, error) {
	f.buf = append(f.buf, p...)
	for {
		i := bytes.IndexByte(f.buf, '\n')
		if i < 0 {
			break
		}
		line := string(f.buf[:i])
		f.buf = f.buf[i+1:]
		if err := f.line(line); err != nil {
			return len(p), err
		}
	}
	if len(f.buf) > maxLineBuffer {
		if _, err := f.out.Write(f.buf); err != nil {
			return len(p), err
		}
		f.buf = f.buf[:0]
	}
	return len(p), nil
}

// Flush writes any trailing bytes that never got a newline. Call it once, when
// no more output is coming: a log whose last line is still being written has no
// terminator, and dropping it would hide the newest output.
func (f *logFormatter) Flush() error {
	if len(f.buf) == 0 {
		return nil
	}
	line := string(f.buf)
	f.buf = f.buf[:0]
	if obj, ok := decodeObject(line); ok {
		return f.object(obj)
	}
	// Verbatim, and with no newline added. This is the end of the log, so a
	// line the agent left unterminated is the last thing it wrote, and passing
	// prose through untouched has to mean untouched.
	_, err := io.WriteString(f.out, line)
	return err
}

// line renders one complete line of log.
func (f *logFormatter) line(line string) error {
	obj, ok := decodeObject(line)
	if !ok {
		_, err := fmt.Fprintln(f.out, line)
		return err
	}
	return f.object(obj)
}

// decodeObject reports whether a line is a whole JSON object, and returns it.
func decodeObject(line string) (map[string]any, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "{") {
		return nil, false
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		return nil, false
	}
	return obj, true
}

// object renders one decoded JSON line. An object carrying a result is the
// agent's answer, and becomes that answer followed by a short footer. Any
// other object is indented instead, which is not a summary but is at least
// readable, and keeps a CLI whose output we do not recognize legible.
func (f *logFormatter) object(obj map[string]any) error {
	text, ok := obj["result"].(string)
	if !ok {
		pretty, err := json.MarshalIndent(obj, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(f.out, "%s\n", pretty)
		return err
	}

	var b strings.Builder
	if text = strings.TrimSpace(text); text != "" {
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	for _, field := range summarize(obj) {
		fmt.Fprintf(&b, "%-8s %s\n", field.label, field.value)
	}
	_, err := io.WriteString(f.out, b.String())
	return err
}

// logField is one label and value in a result's footer.
type logField struct{ label, value string }

// summarize picks the few facts worth keeping out of a result object: how the
// run ended, what it cost, whether it was refused anything, and the session ID
// needed to resume it. The rest is token accounting nobody reads in a terminal,
// and it is still in the log file for anyone who wants it.
func summarize(obj map[string]any) []logField {
	fields := []logField{{"status", runStatus(obj)}}
	if cost, ok := obj["total_cost_usd"].(float64); ok {
		fields = append(fields, logField{"cost", fmt.Sprintf("$%.4f", cost)})
	}
	// Worth a line of its own: a run that was denied tool calls can exit
	// successfully having changed nothing, which is otherwise invisible.
	if denials, ok := obj["permission_denials"].([]any); ok && len(denials) > 0 {
		fields = append(fields, logField{
			"denied", fmt.Sprintf("%d tool %s", len(denials), plural(len(denials), "call")),
		})
	}
	if id, ok := obj["session_id"].(string); ok && id != "" {
		fields = append(fields, logField{"session", id})
	}
	return fields
}

// runStatus describes how the run ended in one line.
func runStatus(obj map[string]any) string {
	parts := []string{outcome(obj)}
	if turns, ok := obj["num_turns"].(float64); ok {
		n := int(turns)
		parts = append(parts, fmt.Sprintf("%d %s", n, plural(n, "turn")))
	}
	if ms, ok := obj["duration_ms"].(float64); ok {
		parts = append(parts, (time.Duration(ms) * time.Millisecond).
			Round(100*time.Millisecond).String())
	}
	// end_turn is the agent deciding it is finished, which the outcome already
	// says. Any other reason is the interesting case, a budget cap among them.
	if reason, ok := obj["stop_reason"].(string); ok && reason != "" && reason != "end_turn" {
		parts = append(parts, "stopped: "+reason)
	}
	return strings.Join(parts, ", ")
}

// outcome is the single word for how the run ended.
func outcome(obj map[string]any) string {
	if failed, ok := obj["is_error"].(bool); ok && failed {
		return "error"
	}
	for _, key := range []string{"subtype", "terminal_reason"} {
		if v, ok := obj[key].(string); ok && v != "" {
			return v
		}
	}
	return "done"
}

// plural returns word, with an s when n is not 1.
func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
