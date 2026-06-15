package stats

import (
	"strings"
	"testing"
)

func TestFormatVerboseSanitizesStreamName(t *testing.T) {
	out := FormatVerbose(NewAggregate([]StreamSnapshot{{
		Name:       "bad\n\x1b[31m\x7fname",
		RemoteAddr: "127.0.0.1:47000",
	}}))

	if strings.Contains(out, "bad\n") {
		t.Fatalf("FormatVerbose output contains injected newline: %q", out)
	}
	if !strings.Contains(out, `bad\n\x1b[31m\x7fname`) {
		t.Fatalf("FormatVerbose output did not escape control bytes: %q", out)
	}
}
