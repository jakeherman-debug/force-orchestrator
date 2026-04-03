package agents

import (
	"fmt"
	"io"
	"log"
	"os"
	"sync"
)

// lockedWriter is a synchronized wrapper around a shared log file, opened once
// for the entire process. All loggers write through this to avoid multiple FDs
// pointing at fleet.log and to prevent interleaved log lines.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (lw *lockedWriter) Write(p []byte) (n int, err error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.w.Write(p)
}

var (
	sharedLog     = &lockedWriter{w: os.Stderr}
	sharedLogOnce sync.Once
)

func NewLogger(name string) *log.Logger {
	sharedLogOnce.Do(func() {
		f, err := os.OpenFile("fleet.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			sharedLog.w = f
		}
	})
	return log.New(sharedLog, fmt.Sprintf("[%s] ", name), log.LstdFlags)
}
