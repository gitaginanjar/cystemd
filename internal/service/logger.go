package service

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Level is a logging severity. Lower numeric value = higher severity.
type Level int

const (
	LevelEmergency Level = iota
	LevelCritical
	LevelError
	LevelWarning
	LevelInfo
	LevelDebug
	LevelTrace
)

// levelNames maps a Level to its canonical lowercase name (the "level" field).
var levelNames = map[Level]string{
	LevelEmergency: "emergency",
	LevelCritical:  "critical",
	LevelError:     "error",
	LevelWarning:   "warning",
	LevelInfo:      "info",
	LevelDebug:     "debug",
	LevelTrace:     "trace",
}

// logRecord is the JSON shape of one log line. Keep fields in JSON-tag
// alphabetical order: encoding/json emits keys in declaration order.
type logRecord struct {
	Level string `json:"level"`
	Msg   string `json:"msg"`
	Time  string `json:"time"`
}

// ParseLevel maps a level name to a Level. Accepts any case and treats "warn"
// as an alias for "warning". Returns (LevelInfo, false) on unknown input.
func ParseLevel(s string) (Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "emergency":
		return LevelEmergency, true
	case "critical":
		return LevelCritical, true
	case "error":
		return LevelError, true
	case "warning", "warn":
		return LevelWarning, true
	case "info":
		return LevelInfo, true
	case "debug":
		return LevelDebug, true
	case "trace":
		return LevelTrace, true
	}
	return LevelInfo, false
}

type logger struct {
	mu    sync.Mutex
	level Level
	out   io.Writer
}

var defaultLogger = &logger{level: LevelInfo, out: os.Stderr}

// SetLevel updates the global logger threshold. Messages with severity above
// this level (numerically greater) are suppressed.
func SetLevel(l Level) {
	defaultLogger.mu.Lock()
	defer defaultLogger.mu.Unlock()
	defaultLogger.level = l
}

// SetOutput replaces the global logger output (default: os.Stderr).
func SetOutput(w io.Writer) {
	defaultLogger.mu.Lock()
	defer defaultLogger.mu.Unlock()
	defaultLogger.out = w
}

func (l *logger) logf(lvl Level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if lvl > l.level {
		return
	}
	rec := logRecord{
		Level: levelNames[lvl],
		Msg:   fmt.Sprintf(format, args...),
		Time:  time.Now().Format(time.RFC3339),
	}
	// json.Marshal escapes quotes, newlines and control characters: one
	// parseable NDJSON line per call.
	data, err := json.Marshal(rec)
	if err != nil {
		// Cannot fail for three strings; never drop the entry, and keep the
		// keys alphabetical.
		fmt.Fprintf(l.out, "{\"level\":%q,\"msg\":%q,\"time\":%q}\n",
			rec.Level, rec.Msg, rec.Time)
		return
	}
	data = append(data, '\n')
	_, _ = l.out.Write(data)
}

func Emergencyf(f string, a ...any) { defaultLogger.logf(LevelEmergency, f, a...) }
func Criticalf(f string, a ...any)  { defaultLogger.logf(LevelCritical, f, a...) }
func Errorf(f string, a ...any)     { defaultLogger.logf(LevelError, f, a...) }
func Warningf(f string, a ...any)   { defaultLogger.logf(LevelWarning, f, a...) }
func Infof(f string, a ...any)      { defaultLogger.logf(LevelInfo, f, a...) }
func Debugf(f string, a ...any)     { defaultLogger.logf(LevelDebug, f, a...) }
func Tracef(f string, a ...any)     { defaultLogger.logf(LevelTrace, f, a...) }
