package service

// panicguard.go: the per-cycle panic boundary for the background loops.
// A recovered panic is a bug: logged at Error with a stack and counted, never
// hidden. See ADR-0014.

import (
	"runtime/debug"
)

// safeCycle runs one background-loop iteration behind a panic boundary: a panic
// is recovered, logged at Error with debug.Stack() and counted in
// cystemd_panic_recovered_total{loop}; the next tick retries.
// loop is the metric label and log name; keep it constant per loop so the
// series does not fragment.
func safeCycle(loop string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			recordPanicRecovered(loop)
			Errorf("panic recovered in %s loop — cycle skipped, next tick will retry: %v\n%s",
				loop, r, debug.Stack())
		}
	}()
	fn()
}

// safeCycleBool is safeCycle for a cycle whose outcome drives scheduling. A
// panicking cycle reports false, so the caller's normal failure backoff applies.
func safeCycleBool(loop string, fn func() bool) bool {
	ok := false
	safeCycle(loop, func() { ok = fn() })
	return ok
}
