package service

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// ─── ParseLevel ────────────────────────────────────────────────────────────

func TestParseLevel_KnownLevels(t *testing.T) {
	cases := []struct {
		input string
		want  Level
	}{
		{"emergency", LevelEmergency},
		{"EMERGENCY", LevelEmergency},
		{"Emergency", LevelEmergency},
		{"critical", LevelCritical},
		{"CRITICAL", LevelCritical},
		{"error", LevelError},
		{"ERROR", LevelError},
		{"warning", LevelWarning},
		{"WARNING", LevelWarning},
		{"warn", LevelWarning}, // alias
		{"WARN", LevelWarning}, // alias upper
		{"info", LevelInfo},
		{"INFO", LevelInfo},
		{"debug", LevelDebug},
		{"DEBUG", LevelDebug},
		{"trace", LevelTrace},
		{"TRACE", LevelTrace},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, ok := ParseLevel(tc.input)
			if !ok {
				t.Errorf("ParseLevel(%q): expected ok=true, got false", tc.input)
			}
			if got != tc.want {
				t.Errorf("ParseLevel(%q): got %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

func TestParseLevel_UnknownReturnsInfoFalse(t *testing.T) {
	cases := []string{"", "verbose", "fatal", "notice", "unknown", "123"}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			got, ok := ParseLevel(input)
			if ok {
				t.Errorf("ParseLevel(%q): expected ok=false, got true", input)
			}
			if got != LevelInfo {
				t.Errorf("ParseLevel(%q): expected LevelInfo fallback, got %d", input, got)
			}
		})
	}
}

func TestParseLevel_WhitespaceStripped(t *testing.T) {
	got, ok := ParseLevel("  debug  ")
	if !ok {
		t.Error("ParseLevel with surrounding spaces: expected ok=true")
	}
	if got != LevelDebug {
		t.Errorf("ParseLevel with surrounding spaces: got %d, want LevelDebug", got)
	}
}

// ─── Level ordering ────────────────────────────────────────────────────────

func TestLevelOrdering(t *testing.T) {
	if LevelEmergency >= LevelCritical {
		t.Error("Emergency should be lower (more severe) than Critical")
	}
	if LevelCritical >= LevelError {
		t.Error("Critical should be lower than Error")
	}
	if LevelError >= LevelWarning {
		t.Error("Error should be lower than Warning")
	}
	if LevelWarning >= LevelInfo {
		t.Error("Warning should be lower than Info")
	}
	if LevelInfo >= LevelDebug {
		t.Error("Info should be lower than Debug")
	}
	if LevelDebug >= LevelTrace {
		t.Error("Debug should be lower than Trace")
	}
}

// ─── SetLevel filtering ────────────────────────────────────────────────────

func TestSetLevel_SuppressesLowerSeverity(t *testing.T) {
	var buf bytes.Buffer
	SetOutput(&buf)
	defer SetOutput(os.Stderr)
	SetLevel(LevelError)
	defer SetLevel(LevelInfo)

	Infof("info should be hidden")
	Debugf("debug should be hidden")
	Tracef("trace should be hidden")
	Warningf("warning should be hidden")

	if buf.Len() != 0 {
		t.Errorf("expected no output at LevelError, got: %s", buf.String())
	}
}

func TestSetLevel_AllowsHigherSeverity(t *testing.T) {
	var buf bytes.Buffer
	SetOutput(&buf)
	defer SetOutput(os.Stderr)
	SetLevel(LevelError)
	defer SetLevel(LevelInfo)

	Errorf("error visible")
	Criticalf("critical visible")
	Emergencyf("emergency visible")

	out := buf.String()
	for _, want := range []string{`"level":"error"`, `"level":"critical"`, `"level":"emergency"`} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output, got: %s", want, out)
		}
	}
}

func TestSetLevel_TraceShowsEverything(t *testing.T) {
	var buf bytes.Buffer
	SetOutput(&buf)
	defer SetOutput(os.Stderr)
	SetLevel(LevelTrace)
	defer SetLevel(LevelInfo)

	Emergencyf("em")
	Criticalf("cr")
	Errorf("er")
	Warningf("wa")
	Infof("in")
	Debugf("de")
	Tracef("tr")

	out := buf.String()
	for _, want := range []string{"emergency", "critical", "error", "warning", "info", "debug", "trace"} {
		if !strings.Contains(out, `"level":"`+want+`"`) {
			t.Errorf("expected level %q in output, got: %s", want, out)
		}
	}
}

// ─── JSON log format ───────────────────────────────────────────────────────

func TestLogFormat_EmitsValidJSONPerLine(t *testing.T) {
	var buf bytes.Buffer
	SetOutput(&buf)
	defer SetOutput(os.Stderr)
	SetLevel(LevelInfo)
	defer SetLevel(LevelInfo)

	Infof("hello %s", "world")
	Warningf("watch out: %q", `it's "tricky"`)

	for i, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		var rec map[string]string
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Errorf("line %d not valid JSON (%v): %s", i, err, line)
			continue
		}
		if rec["level"] == "" || rec["msg"] == "" || rec["time"] == "" {
			t.Errorf("line %d missing required field(s): %s", i, line)
		}
		// RFC3339-shaped: at minimum contains 'T'.
		if !strings.Contains(rec["time"], "T") {
			t.Errorf("line %d time field not RFC3339-shaped: %q", i, rec["time"])
		}
	}
}

func TestLogFormat_KeysAreAlphabetical(t *testing.T) {
	// Pins key POSITION, not just presence: level < msg < time (the
	// alphabetical-JSON invariant, via logRecord's declaration order).
	var buf bytes.Buffer
	SetOutput(&buf)
	defer SetOutput(os.Stderr)
	SetLevel(LevelInfo)
	defer SetLevel(LevelInfo)

	Infof("ordering check")
	line := strings.TrimRight(buf.String(), "\n")
	iLevel := strings.Index(line, `"level":`)
	iMsg := strings.Index(line, `"msg":`)
	iTime := strings.Index(line, `"time":`)
	if !(iLevel < iMsg && iMsg < iTime) {
		t.Errorf("expected alphabetical key order level<msg<time in %q; got positions level=%d msg=%d time=%d",
			line, iLevel, iMsg, iTime)
	}
}

func TestLogFormat_EscapesQuotesAndNewlinesInMessage(t *testing.T) {
	// Quotes and newlines in msg must be escaped so jq still parses the line.
	var buf bytes.Buffer
	SetOutput(&buf)
	defer SetOutput(os.Stderr)
	SetLevel(LevelInfo)
	defer SetLevel(LevelInfo)

	Infof("multi\nline %s", `with "quotes"`)
	line := strings.TrimRight(buf.String(), "\n")
	var rec map[string]string
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("invalid JSON: %v\nline: %s", err, line)
	}
	if rec["msg"] != "multi\nline with \"quotes\"" {
		t.Errorf("msg not preserved through JSON round-trip: %q", rec["msg"])
	}
}

func TestLogFormat_FormattingApplied(t *testing.T) {
	var buf bytes.Buffer
	SetOutput(&buf)
	defer SetOutput(os.Stderr)
	SetLevel(LevelTrace)
	defer SetLevel(LevelInfo)

	Debugf("value=%d name=%s flag=%t", 42, "test", true)
	out := buf.String()
	if !strings.Contains(out, "value=42 name=test flag=true") {
		t.Errorf("expected formatted message in output: %s", out)
	}
}

// ─── SetOutput ─────────────────────────────────────────────────────────────

func TestSetOutput_RedirectsLog(t *testing.T) {
	var buf1, buf2 bytes.Buffer
	SetOutput(&buf1)
	Infof("to buf1")
	SetOutput(&buf2)
	Infof("to buf2")
	defer SetOutput(os.Stderr)

	if !strings.Contains(buf1.String(), "to buf1") {
		t.Errorf("expected 'to buf1' in buf1, got: %s", buf1.String())
	}
	if strings.Contains(buf1.String(), "to buf2") {
		t.Errorf("did not expect 'to buf2' in buf1")
	}
	if !strings.Contains(buf2.String(), "to buf2") {
		t.Errorf("expected 'to buf2' in buf2, got: %s", buf2.String())
	}
}
