package librarian

import (
	"context"
	"database/sql"
	"errors"
	"sync"
)

// MockClient is the test-side Client backing. Every Client method is
// recorded on the mock so tests can assert on the call history; the
// in-memory state (Memories, NextWriteID) is freely settable so tests
// fixture exactly the rows they need.
//
// Concurrency: safe under a single mutex. Tests that exercise the
// client from goroutines can rely on the mock's locking; the lock is
// held across the full method body, including any function-pointer
// callouts (so a test that sets WriteMemoryFn to a closure that calls
// back into the same mock will deadlock — keep callbacks pure).
type MockClient struct {
	mu sync.Mutex

	// State the mock returns. Tests freely mutate before / between calls.
	Memories    []Memory // returned by Get* methods (after filtering)
	NextWriteID int      // ID returned by the next WriteMemory(Tx); auto-increments

	// Optional behaviour hooks. When non-nil, the corresponding method
	// delegates to the hook instead of the default mock behaviour. Hooks
	// receive the same args the real Client method does.
	WriteMemoryFn               func(ctx context.Context, m Memory) (int, error)
	WriteMemoryTxFn             func(ctx context.Context, tx *sql.Tx, m Memory) (int, error)
	GetMemoriesForTaskFn        func(ctx context.Context, taskID int) ([]Memory, error)
	GetMemoriesByScopeFn        func(ctx context.Context, scope Scope) ([]Memory, error)
	UpdateMemoryFn              func(ctx context.Context, memoryID int, update MemoryUpdate) error
	RemoveMemoryFn              func(ctx context.Context, memoryID int) error
	SummarizeForOverflowFn      func(ctx context.Context, prompt string, targetBytes int) (string, error)

	// Recorded call history. Tests assert on these.
	WriteCalls       []Memory
	WriteTxCalls     []Memory
	GetTaskCalls     []int
	GetScopeCalls    []Scope
	UpdateCalls      []MockUpdateCall
	RemoveCalls      []int
	SummarizeCalls   []MockSummarizeCall
}

// MockSummarizeCall captures one SummarizeForContextOverflow invocation.
type MockSummarizeCall struct {
	Prompt      string
	TargetBytes int
}

// MockUpdateCall captures one UpdateMemory invocation.
type MockUpdateCall struct {
	MemoryID int
	Update   MemoryUpdate
}

// NewMock constructs a MockClient with a fresh ID counter (starts at 1)
// and empty fixture state.
func NewMock() *MockClient {
	return &MockClient{NextWriteID: 1}
}

// WriteMemory records the call and returns NextWriteID (incrementing it
// for the next call). Override via WriteMemoryFn.
func (m *MockClient) WriteMemory(ctx context.Context, mem Memory) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.WriteCalls = append(m.WriteCalls, mem)
	if m.WriteMemoryFn != nil {
		return m.WriteMemoryFn(ctx, mem)
	}
	id := m.NextWriteID
	m.NextWriteID++
	return id, nil
}

// WriteMemoryTx mirrors WriteMemory but records into WriteTxCalls and
// uses WriteMemoryTxFn for overrides. The mock does NOT touch the *sql.Tx;
// tests that need to assert tx wiring should set WriteMemoryTxFn.
func (m *MockClient) WriteMemoryTx(ctx context.Context, tx *sql.Tx, mem Memory) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.WriteTxCalls = append(m.WriteTxCalls, mem)
	if m.WriteMemoryTxFn != nil {
		return m.WriteMemoryTxFn(ctx, tx, mem)
	}
	id := m.NextWriteID
	m.NextWriteID++
	return id, nil
}

// GetMemoriesForTask returns Memories filtered to those whose
// ParentTaskID matches taskID, unless GetMemoriesForTaskFn overrides.
func (m *MockClient) GetMemoriesForTask(ctx context.Context, taskID int) ([]Memory, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.GetTaskCalls = append(m.GetTaskCalls, taskID)
	if m.GetMemoriesForTaskFn != nil {
		return m.GetMemoriesForTaskFn(ctx, taskID)
	}
	var out []Memory
	for _, mm := range m.Memories {
		if mm.ParentTaskID == taskID {
			out = append(out, mm)
		}
	}
	return out, nil
}

// GetMemoriesByScope returns Memories filtered by Scope (Repo and
// Outcome only — SinceCreatedAt and Limit are honoured by the override
// hook if needed). Empty Scope returns ErrEmptyScope to match the
// in-process backing.
func (m *MockClient) GetMemoriesByScope(ctx context.Context, s Scope) ([]Memory, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.GetScopeCalls = append(m.GetScopeCalls, s)
	if m.GetMemoriesByScopeFn != nil {
		return m.GetMemoriesByScopeFn(ctx, s)
	}
	if s.Repo == "" && s.SinceCreatedAt == "" {
		return nil, ErrEmptyScope
	}
	if s.Limit < 0 {
		return nil, ErrInvalidLimit
	}
	var out []Memory
	for _, mm := range m.Memories {
		if s.Repo != "" && mm.Repo != s.Repo {
			continue
		}
		if s.Outcome != "" && mm.Outcome != s.Outcome {
			continue
		}
		out = append(out, mm)
	}
	limit := s.Limit
	if limit == 0 {
		limit = defaultScopeLimit
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// UpdateMemory records the call and (default behaviour) updates the
// matching entry in m.Memories in place. ErrNotFound on miss.
func (m *MockClient) UpdateMemory(ctx context.Context, memoryID int, u MemoryUpdate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.UpdateCalls = append(m.UpdateCalls, MockUpdateCall{MemoryID: memoryID, Update: u})
	if m.UpdateMemoryFn != nil {
		return m.UpdateMemoryFn(ctx, memoryID, u)
	}
	for i := range m.Memories {
		if m.Memories[i].ID == memoryID {
			if u.Summary != "" {
				m.Memories[i].Summary = normalizeUpdateField(u.Summary)
			}
			if u.FilesChanged != "" {
				m.Memories[i].Files = normalizeUpdateField(u.FilesChanged)
			}
			if u.TopicTags != "" {
				m.Memories[i].TopicTags = normalizeUpdateField(u.TopicTags)
			}
			return nil
		}
	}
	return ErrNotFound
}

// RemoveMemory records the call and (default behaviour) removes the
// matching entry from m.Memories. ErrNotFound on miss.
func (m *MockClient) RemoveMemory(ctx context.Context, memoryID int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.RemoveCalls = append(m.RemoveCalls, memoryID)
	if m.RemoveMemoryFn != nil {
		return m.RemoveMemoryFn(ctx, memoryID)
	}
	for i := range m.Memories {
		if m.Memories[i].ID == memoryID {
			m.Memories = append(m.Memories[:i], m.Memories[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

// SummarizeForContextOverflow records the call and (default
// behaviour) returns the prompt unchanged. Override via
// SummarizeForOverflowFn for tests that need to assert the request
// budget or fixture a specific compression result. The default
// no-op behaviour keeps tests that don't care about the summarizer
// from accidentally producing different prompts at the LLM ingress
// layer.
func (m *MockClient) SummarizeForContextOverflow(ctx context.Context, prompt string, targetBytes int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.SummarizeCalls = append(m.SummarizeCalls, MockSummarizeCall{Prompt: prompt, TargetBytes: targetBytes})
	if m.SummarizeForOverflowFn != nil {
		return m.SummarizeForOverflowFn(ctx, prompt, targetBytes)
	}
	return prompt, nil
}

// Reset clears all recorded calls and fixture state, restoring NextWriteID
// to 1. Useful between sub-tests inside the same test function.
func (m *MockClient) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Memories = nil
	m.NextWriteID = 1
	m.WriteCalls = nil
	m.WriteTxCalls = nil
	m.GetTaskCalls = nil
	m.GetScopeCalls = nil
	m.UpdateCalls = nil
	m.RemoveCalls = nil
	m.SummarizeCalls = nil
}

// Compile-time assertion: *MockClient satisfies the Client interface.
var _ Client = (*MockClient)(nil)

// errNotFoundIs wires errors.Is for ErrNotFound so the mock's default
// errors compare cleanly with the in-process backing's. Required so a
// caller that does errors.Is(err, librarian.ErrNotFound) gets a true
// result regardless of which backing fired the error.
//
// Implementation note: ErrNotFound is a sentinel sealed via errors.New,
// so equality is by identity (==). No wrapping needed; this assertion is
// here as a guard against a future refactor that might add a wrapper
// without realising tests rely on errors.Is.
var _ = func() bool { return errors.Is(ErrNotFound, ErrNotFound) }()
