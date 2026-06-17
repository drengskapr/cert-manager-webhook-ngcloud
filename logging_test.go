package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	logsapi "k8s.io/component-base/logs/api/v1"
)

// TestRFC3339JSONFactoryEncodesTimestamp verifies the custom json-rfc3339 log
// format emits the "ts" field as an RFC3339Nano string rather than the epoch
// milliseconds float used by component-base's built-in json format.
func TestRFC3339JSONFactoryEncodesTimestamp(t *testing.T) {
	var buf bytes.Buffer
	logger, ctrl := rfc3339JSONFactory{}.Create(
		logsapi.LoggingConfiguration{Verbosity: 1},
		logsapi.LoggingOptions{ErrorStream: &buf},
	)

	logger.Info("test message", "key", "value")
	if ctrl.Flush != nil {
		ctrl.Flush()
	}

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("log output is not valid JSON: %v\noutput: %q", err, buf.String())
	}

	ts, ok := entry["ts"].(string)
	if !ok {
		t.Fatalf("ts field is not a string (got %T: %v) — epoch encoder still in use", entry["ts"], entry["ts"])
	}
	if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
		t.Errorf("ts %q is not RFC3339Nano: %v", ts, err)
	}
	if msg, _ := entry["msg"].(string); msg != "test message" {
		t.Errorf("msg = %q, want %q", msg, "test message")
	}
}
