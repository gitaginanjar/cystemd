package service

import (
	"strings"
	"testing"
)

// TestSafeCycle_RecoversAndContinues pins the core guarantee: a panicking cycle
// does not propagate, and the Error line names the loop, the value and a stack.
// See ADR-0014.
func TestSafeCycle_RecoversAndContinues(t *testing.T) {
	resetMetricsForTest()
	buf := captureLogs(t, LevelError)

	reached := false
	safeCycle("test-loop", func() { panic("boom") })
	reached = true // unreachable if safeCycle re-panicked

	if !reached {
		t.Fatal("safeCycle did not contain the panic")
	}
	if !strings.Contains(buf.String(), "panic recovered in test-loop loop") {
		t.Errorf("expected an error log naming the loop; got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "boom") {
		t.Errorf("expected the panic value in the log; got:\n%s", buf.String())
	}
	// The stack is mandatory: without it a recovered panic is undiagnosable.
	if !strings.Contains(buf.String(), "panicguard.go") && !strings.Contains(buf.String(), "goroutine") {
		t.Errorf("expected a stack trace in the log; got:\n%s", buf.String())
	}
}

// TestSafeCycle_RecoveredPanicIsCounted pins that a recovered panic (a bug) is
// counted in cystemd_panic_recovered_total{loop}, alertable on any non-zero value.
func TestSafeCycle_RecoveredPanicIsCounted(t *testing.T) {
	resetMetricsForTest()
	captureLogs(t, LevelError)

	safeCycle("counted-loop", func() { panic("kaboom") })

	var sb strings.Builder
	writeMetrics(&sb, nil)
	want := `cystemd_panic_recovered_total{loop="counted-loop"} 1`
	if !strings.Contains(sb.String(), want) {
		t.Errorf("expected %q in the exposition; got:\n%s", want, sb.String())
	}
}

// TestSafeCycle_HappyPathIsSilent pins that a normal cycle logs and counts nothing.
func TestSafeCycle_HappyPathIsSilent(t *testing.T) {
	resetMetricsForTest()
	buf := captureLogs(t, LevelDebug)

	ran := false
	safeCycle("quiet-loop", func() { ran = true })

	if !ran {
		t.Fatal("safeCycle did not run the cycle")
	}
	if strings.Contains(buf.String(), "panic recovered") {
		t.Errorf("a healthy cycle logged a panic; got:\n%s", buf.String())
	}
	var sb strings.Builder
	writeMetrics(&sb, nil)
	if strings.Contains(sb.String(), `cystemd_panic_recovered_total{loop="quiet-loop"}`) {
		t.Error("a healthy cycle emitted a panic-recovered series")
	}
}

// TestSafeCycleBool_PanicReportsFailure pins the renewal loop's contract: a
// panicking cycle reports false, so the normal backoff applies (no zero-delay
// spin); other results pass through.
func TestSafeCycleBool_PanicReportsFailure(t *testing.T) {
	resetMetricsForTest()
	captureLogs(t, LevelError)

	if got := safeCycleBool("bool-loop", func() bool { panic("nope") }); got != false {
		t.Errorf("safeCycleBool on panic = %t, want false", got)
	}
	if got := safeCycleBool("bool-loop", func() bool { return true }); got != true {
		t.Errorf("safeCycleBool passthrough = %t, want true", got)
	}
	if got := safeCycleBool("bool-loop", func() bool { return false }); got != false {
		t.Errorf("safeCycleBool passthrough = %t, want false", got)
	}
}
