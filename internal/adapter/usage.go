package adapter

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// ErrNoUsage means the log held nothing this adapter could read a cost out of.
var ErrNoUsage = errors.New("the cli reported no usable usage")

// lastJSONObject returns the last line of a log that parses as a JSON object.
//
// Agentic CLIs interleave progress output with their final result, and the
// result is what carries the cost, so the file is scanned from the top and the
// last valid object wins.
func lastJSONObject(logPath string) (map[string]any, error) {
	f, err := os.Open(logPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", logPath, ErrNoUsage)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", logPath, err)
	}
	defer f.Close()

	var found map[string]any
	scanner := bufio.NewScanner(f)
	// Agent logs hold whole JSON results on one line, which can be long.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err == nil {
			found = obj
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", logPath, err)
	}
	if found == nil {
		return nil, fmt.Errorf("%s: %w", logPath, ErrNoUsage)
	}
	return found, nil
}

// floatField reads a number from a decoded JSON object.
func floatField(obj map[string]any, key string) *float64 {
	if v, ok := obj[key].(float64); ok {
		return &v
	}
	return nil
}

// intField reads a number from a decoded JSON object as an int.
func intField(obj map[string]any, key string) *int {
	if v, ok := obj[key].(float64); ok {
		n := int(v)
		return &n
	}
	return nil
}
