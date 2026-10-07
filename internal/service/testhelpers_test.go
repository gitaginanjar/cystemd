package service

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
)

// safeBuffer is a goroutine-safe bytes.Buffer for captureLogs: background loops
// (CD, reloads, Vault) log while the test reads, so a plain buffer races under -race.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Bytes returns a copy, never the slice a concurrent Write mutates.
func (b *safeBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

func (b *safeBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

func (b *safeBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

// captureLogs sends the package logger to a buffer at the minimum level for the
// test and returns it; cleanup restores stderr and LevelInfo.
func captureLogs(t *testing.T, min Level) *safeBuffer {
	t.Helper()
	buf := &safeBuffer{}
	SetOutput(buf)
	SetLevel(min)
	t.Cleanup(func() {
		SetOutput(os.Stderr)
		SetLevel(LevelInfo)
	})
	return buf
}

// decodeLogMsgs returns the unescaped `msg` fields of buf's NDJSON lines, joined by
// newlines. Use it to substring-match text with quotes or newlines, which the JSON
// encoding escapes in buf. Empty lines are skipped; malformed lines fail the test.
func decodeLogMsgs(t *testing.T, buf *safeBuffer) string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec struct {
			Msg string `json:"msg"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decodeLogMsgs: line %q is not valid JSON: %v", line, err)
		}
		out = append(out, rec.Msg)
	}
	return strings.Join(out, "\n")
}

// uint32Ptr returns &v for UnitStatusJSON.NRestarts, a pointer because a read zero
// is a measurement (DOCS/DESIGN-N-RESTARTS-ZERO-VS-ABSENT.md).
func uint32Ptr(v uint32) *uint32 { return &v }
