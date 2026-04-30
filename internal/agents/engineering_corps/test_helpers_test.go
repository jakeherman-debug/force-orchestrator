package engineering_corps

import (
	"bytes"
	"log"
	"strings"
	"sync"
)

// testLogger captures log output during dispatcher tests. Tests assert
// against the captured stream when the dispatcher writes diagnostic
// lines (e.g. "unknown task type"). Real production runs go through
// agents.NewLogger which writes to fleet.log.
type testLogger struct {
	mu  sync.Mutex
	buf bytes.Buffer
	lg  *log.Logger
}

func newTestLogger() *testLogger {
	tl := &testLogger{}
	tl.lg = log.New(&tl.buf, "[ec-test] ", 0)
	return tl
}

func (tl *testLogger) std() *log.Logger { return tl.lg }

func (tl *testLogger) dump() string {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return tl.buf.String()
}

func (tl *testLogger) containsAny(needles []string) bool {
	out := tl.dump()
	for _, n := range needles {
		if !strings.Contains(out, n) {
			return false
		}
	}
	return true
}
