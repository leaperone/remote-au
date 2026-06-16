package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestNewRejectsInvalidLevelAndFormat(t *testing.T) {
	if _, err := New(&bytes.Buffer{}, "trace", "text"); err == nil || !strings.Contains(err.Error(), "log-level") {
		t.Fatalf("New invalid level err=%v, want log-level error", err)
	}
	if _, err := New(&bytes.Buffer{}, "info", "xml"); err == nil || !strings.Contains(err.Error(), "log-format") {
		t.Fatalf("New invalid format err=%v, want log-format error", err)
	}
}

func TestTextLoggerFiltersByLevel(t *testing.T) {
	var out bytes.Buffer
	logger, err := New(&out, "warn", "text")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	logger.Infof("hidden %d", 1)
	logger.Warnf("shown %d", 2)

	got := out.String()
	if strings.Contains(got, "hidden") {
		t.Fatalf("info log was not filtered: %q", got)
	}
	if !strings.Contains(got, "level=WARN") || !strings.Contains(got, "shown 2") {
		t.Fatalf("warn log missing expected text: %q", got)
	}
}

func TestJSONLoggerEmitsJSON(t *testing.T) {
	var out bytes.Buffer
	logger, err := New(&out, "debug", "json")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	logger.Debugf("debug %s", "detail")

	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &record); err != nil {
		t.Fatalf("json log did not parse: %v\n%s", err, out.String())
	}
	if record["level"] != "DEBUG" || record["msg"] != "debug detail" {
		t.Fatalf("record=%v, want DEBUG debug detail", record)
	}
}

func TestEffectiveLevelVerboseAlias(t *testing.T) {
	if got := EffectiveLevel("info", true, false); got != "debug" {
		t.Fatalf("EffectiveLevel verbose implicit=%q, want debug", got)
	}
	if got := EffectiveLevel("warn", true, true); got != "warn" {
		t.Fatalf("EffectiveLevel verbose explicit=%q, want warn", got)
	}
	if got := EffectiveLevel("info", false, false); got != "info" {
		t.Fatalf("EffectiveLevel default=%q, want info", got)
	}
}
