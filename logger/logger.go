// Package logger provides a lightweight leveled logger for Kiro-Go.
//
// Levels (from most to least verbose):
//
//	DEBUG < INFO < WARN < ERROR
//
// The active level is configured via logger.Init at startup.
// Priority: LOG_LEVEL environment variable > provided fallback (usually
// taken from config.json "logLevel"). If neither is set or the value is
// unrecognized, the level defaults to INFO.
package logger

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Level represents a log severity.
type Level int32

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// Entry is a single captured log line, broadcast to live subscribers and
// retained in the ring buffer for backfill.
type Entry struct {
	Time  time.Time
	Level Level
	Text  string
}

// levelWriter wraps the real destination (stdout/stderr). It exists so tests
// can redirect output via SetOutput; the broadcast hub is fed separately from
// the Debugf/Infof/... helpers with the clean (un-prefixed) message text.
type levelWriter struct {
	level Level
	mu    sync.Mutex
	dest  io.Writer
}

func (lw *levelWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	dest := lw.dest
	lw.mu.Unlock()
	return dest.Write(p)
}

func (lw *levelWriter) setDest(w io.Writer) {
	lw.mu.Lock()
	lw.dest = w
	lw.mu.Unlock()
}

var (
	currentLevel atomic.Int32

	debugWriter = &levelWriter{level: LevelDebug, dest: os.Stdout}
	infoWriter  = &levelWriter{level: LevelInfo, dest: os.Stdout}
	warnWriter  = &levelWriter{level: LevelWarn, dest: os.Stderr}
	errorWriter = &levelWriter{level: LevelError, dest: os.Stderr}

	debugLog = log.New(debugWriter, "DEBUG ", log.LstdFlags)
	infoLog  = log.New(infoWriter, "INFO  ", log.LstdFlags)
	warnLog  = log.New(warnWriter, "WARN  ", log.LstdFlags)
	errorLog = log.New(errorWriter, "ERROR ", log.LstdFlags)
)

func init() {
	currentLevel.Store(int32(LevelInfo))
}

// ParseLevel converts a textual level ("debug", "info", "warn", "error")
// to a Level. The ok flag is false when the input is empty or unknown.
func ParseLevel(s string) (Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug", "trace":
		return LevelDebug, true
	case "info":
		return LevelInfo, true
	case "warn", "warning":
		return LevelWarn, true
	case "error", "err":
		return LevelError, true
	}
	return LevelInfo, false
}

// LevelName returns the canonical lowercase name of a Level.
func LevelName(l Level) string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	}
	return "info"
}

// SetLevel sets the active log level.
func SetLevel(l Level) {
	currentLevel.Store(int32(l))
}

// GetLevel returns the active log level.
func GetLevel() Level {
	return Level(currentLevel.Load())
}

// SetOutput redirects all level outputs to w. Useful for tests.
func SetOutput(w io.Writer) {
	debugWriter.setDest(w)
	infoWriter.setDest(w)
	warnWriter.setDest(w)
	errorWriter.setDest(w)
}

// Init configures the logger. The LOG_LEVEL environment variable, if set,
// overrides the supplied fallback (typically config.GetLogLevel()).
func Init(fallback string) {
	value := fallback
	if env := os.Getenv("LOG_LEVEL"); env != "" {
		value = env
	}
	if l, ok := ParseLevel(value); ok {
		SetLevel(l)
	}
}

func enabled(l Level) bool {
	return Level(currentLevel.Load()) <= l
}

// emit formats the message once, writes it to the level's log.Logger (which
// adds the prefix + timestamp for stdout/stderr), and publishes the clean
// (un-prefixed) text to the broadcast hub so the web console renders its own
// timestamp/badge without duplication.
func emit(l Level, dst *log.Logger, format string, v ...interface{}) {
	msg := fmt.Sprintf(format, v...)
	dst.Output(3, msg)
	hub.publish(Entry{Time: time.Now(), Level: l, Text: msg})
}

// Debugf logs a formatted message at DEBUG level.
func Debugf(format string, v ...interface{}) {
	if enabled(LevelDebug) {
		emit(LevelDebug, debugLog, format, v...)
	}
}

// Infof logs a formatted message at INFO level.
func Infof(format string, v ...interface{}) {
	if enabled(LevelInfo) {
		emit(LevelInfo, infoLog, format, v...)
	}
}

// Warnf logs a formatted message at WARN level.
func Warnf(format string, v ...interface{}) {
	if enabled(LevelWarn) {
		emit(LevelWarn, warnLog, format, v...)
	}
}

// Errorf logs a formatted message at ERROR level.
func Errorf(format string, v ...interface{}) {
	if enabled(LevelError) {
		emit(LevelError, errorLog, format, v...)
	}
}

// Fatalf logs a formatted message at ERROR level and terminates the process.
func Fatalf(format string, v ...interface{}) {
	errorLog.Printf(format, v...)
	os.Exit(1)
}

// ringCapacity is the number of recent entries retained for backfill when a
// new subscriber connects.
const ringCapacity = 500

// broadcastHub fans every captured log line out to live subscribers and keeps
// a bounded ring buffer of recent entries.
type broadcastHub struct {
	mu          sync.Mutex
	ring        []Entry
	next        int
	full        bool
	subscribers map[chan Entry]struct{}
}

var hub = &broadcastHub{
	ring:        make([]Entry, ringCapacity),
	subscribers: make(map[chan Entry]struct{}),
}

func (h *broadcastHub) publish(e Entry) {
	h.mu.Lock()
	h.ring[h.next] = e
	h.next = (h.next + 1) % ringCapacity
	if h.next == 0 {
		h.full = true
	}
	for ch := range h.subscribers {
		// Non-blocking: a slow consumer drops lines rather than stalling
		// the logging path.
		select {
		case ch <- e:
		default:
		}
	}
	h.mu.Unlock()
}

// History returns a snapshot of retained entries in chronological order.
func History() []Entry {
	h := hub
	h.mu.Lock()
	defer h.mu.Unlock()

	if !h.full {
		out := make([]Entry, h.next)
		copy(out, h.ring[:h.next])
		return out
	}
	out := make([]Entry, 0, ringCapacity)
	out = append(out, h.ring[h.next:]...)
	out = append(out, h.ring[:h.next]...)
	return out
}

// Subscribe registers a live subscriber and returns its channel plus a cancel
// function that must be called to release resources.
func Subscribe() (<-chan Entry, func()) {
	ch := make(chan Entry, 256)
	h := hub
	h.mu.Lock()
	h.subscribers[ch] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subscribers, ch)
			h.mu.Unlock()
			close(ch)
		})
	}
	return ch, cancel
}
