package proxy

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"kiro-go/logger"
)

// syncBuffer is a concurrency-safe io.Writer used to capture logger output
// that is written from the recovering goroutine launched by safeGo.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestSafeGoContainsPanicAndLogsStack validates that a background function launched
// via safeGo which panics with a sentinel value is recovered so the process stays
// alive, and that the panic value + stack trace are logged via the shared logger.
func TestSafeGoContainsPanicAndLogsStack(t *testing.T) {
	buf := &syncBuffer{}
	prevLevel := logger.GetLevel()
	logger.SetOutput(buf)
	logger.SetLevel(logger.LevelDebug)
	t.Cleanup(func() {
		logger.SetLevel(prevLevel)
	})

	sentinels := []struct {
		name     string
		panicArg interface{}
	}{
		{"string sentinel", "SENTINEL_PANIC_STRING_A1B2C3"},
		{"formatted sentinel", "nil map write SENTINEL_D4E5F6"},
		{"int sentinel", 424242},
	}

	for _, tc := range sentinels {
		panicArg := tc.panicArg
		safeGo(func() {
			panic(panicArg)
		})
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		out := buf.String()
		haveAll := strings.Contains(out, "SENTINEL_PANIC_STRING_A1B2C3") &&
			strings.Contains(out, "SENTINEL_D4E5F6") &&
			strings.Contains(out, "424242")
		haveStack := strings.Contains(out, "goroutine")
		if haveAll && haveStack {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected recovered panic values + stack trace to be logged; got log output:\n%s", out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
