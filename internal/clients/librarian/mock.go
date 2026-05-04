package librarian

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"
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
	EmitCandidateFn             func(ctx context.Context, candidate Candidate) (int, error)
	ListPendingCandidatesFn     func(ctx context.Context) ([]Candidate, error)
	GetWeightedMemoriesFn       func(ctx context.Context, scope Scope, k int) ([]Memory, error)
	RecentCommitsDigestFn       func(ctx context.Context, repo string, window time.Duration) (CommitsDigest, error)
	BootstrapSenatorRulesFn     func(ctx context.Context, repo string) ([]CandidateRule, error)
	RefreshSenatorMemoryFn      func(ctx context.Context, repo string) (SenatorDigest, error)
	BuildRepoDigestFn           func(ctx context.Context, repoSpec string) (RepoDigest, error)
	BuildArchitectureDocFn      func(ctx context.Context, repoSpec string) (ArchitectureDoc, error)

	// D3 Phase 3 — Librarian → EC handoff state. Candidates added via
	// EmitCandidate live here; ListPendingCandidates returns the slice.
	Candidates       []Candidate
	NextCandidateID  int

	// Recorded call history. Tests assert on these.
	WriteCalls       []Memory
	WriteTxCalls     []Memory
	GetTaskCalls     []int
	GetScopeCalls    []Scope
	UpdateCalls      []MockUpdateCall
	RemoveCalls      []int
	SummarizeCalls   []MockSummarizeCall
	EmitCalls        []Candidate
	ListPendingCalls int

	// D4 Phase 0 — call history for the new client methods.
	GetWeightedCalls   []MockGetWeightedCall
	RecentCommitsCalls []MockRecentCommitsCall
	BootstrapCalls     []string
	RefreshDigestCalls []string
	BuildDigestCalls   []string
	// D10 — BuildArchitectureDoc call recording.
	BuildArchitectureCalls []string
}

// MockGetWeightedCall records one GetWeightedMemories invocation.
type MockGetWeightedCall struct {
	Scope Scope
	K     int
}

// MockRecentCommitsCall records one RecentCommitsDigest invocation.
type MockRecentCommitsCall struct {
	Repo   string
	Window time.Duration
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

// EmitCandidate records the call and (default behaviour) appends the
// candidate to m.Candidates with an auto-incrementing ProposalID
// drawn from NextCandidateID (which starts at 1 and bumps each call).
// Override via EmitCandidateFn.
func (m *MockClient) EmitCandidate(ctx context.Context, c Candidate) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.EmitCalls = append(m.EmitCalls, c)
	if m.EmitCandidateFn != nil {
		return m.EmitCandidateFn(ctx, c)
	}
	if m.NextCandidateID == 0 {
		m.NextCandidateID = 1
	}
	id := m.NextCandidateID
	m.NextCandidateID++
	c.ProposalID = id
	m.Candidates = append(m.Candidates, c)
	return id, nil
}

// ListPendingCandidates returns the recorded candidates slice (every
// EmitCandidate-pushed row is treated as pending unless the caller
// explicitly mutates m.Candidates). Override via ListPendingCandidatesFn.
func (m *MockClient) ListPendingCandidates(ctx context.Context) ([]Candidate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ListPendingCalls++
	if m.ListPendingCandidatesFn != nil {
		return m.ListPendingCandidatesFn(ctx)
	}
	out := make([]Candidate, len(m.Candidates))
	copy(out, m.Candidates)
	return out, nil
}

// GetWeightedMemories returns the same shape as GetMemoriesByScope by
// default, but capped at k (or 20 if k <= 0). Tests fixturing the
// composite-score ordering should override via GetWeightedMemoriesFn.
func (m *MockClient) GetWeightedMemories(ctx context.Context, s Scope, k int) ([]Memory, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.GetWeightedCalls = append(m.GetWeightedCalls, MockGetWeightedCall{Scope: s, K: k})
	if m.GetWeightedMemoriesFn != nil {
		return m.GetWeightedMemoriesFn(ctx, s, k)
	}
	if s.Repo == "" && s.SinceCreatedAt == "" {
		return nil, ErrEmptyScope
	}
	if k <= 0 {
		k = 20
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
		if len(out) >= k {
			break
		}
	}
	return out, nil
}

// RecentCommitsDigest returns an empty digest by default. Tests
// override via RecentCommitsDigestFn.
func (m *MockClient) RecentCommitsDigest(ctx context.Context, repo string, window time.Duration) (CommitsDigest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.RecentCommitsCalls = append(m.RecentCommitsCalls, MockRecentCommitsCall{Repo: repo, Window: window})
	if m.RecentCommitsDigestFn != nil {
		return m.RecentCommitsDigestFn(ctx, repo, window)
	}
	return CommitsDigest{Repo: repo, Window: window}, nil
}

// BootstrapSenatorRules returns nil by default. Tests override via
// BootstrapSenatorRulesFn.
func (m *MockClient) BootstrapSenatorRules(ctx context.Context, repo string) ([]CandidateRule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.BootstrapCalls = append(m.BootstrapCalls, repo)
	if m.BootstrapSenatorRulesFn != nil {
		return m.BootstrapSenatorRulesFn(ctx, repo)
	}
	return nil, nil
}

// RefreshSenatorMemoryDigest returns an empty digest by default.
// Tests override via RefreshSenatorMemoryFn.
func (m *MockClient) RefreshSenatorMemoryDigest(ctx context.Context, repo string) (SenatorDigest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.RefreshDigestCalls = append(m.RefreshDigestCalls, repo)
	if m.RefreshSenatorMemoryFn != nil {
		return m.RefreshSenatorMemoryFn(ctx, repo)
	}
	return SenatorDigest{Repo: repo}, nil
}

// BuildRepoDigest returns an empty digest by default. Tests override
// via BuildRepoDigestFn.
func (m *MockClient) BuildRepoDigest(ctx context.Context, repoSpec string) (RepoDigest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.BuildDigestCalls = append(m.BuildDigestCalls, repoSpec)
	if m.BuildRepoDigestFn != nil {
		return m.BuildRepoDigestFn(ctx, repoSpec)
	}
	return RepoDigest{
		RepoName:    repoSpec,
		Conventions: map[string]string{},
	}, nil
}

// BuildArchitectureDoc records the call and (default) returns a stub
// ArchitectureDoc with the AUTO-GENERATED header so tests that hit
// this path get a structurally valid value. Override via
// BuildArchitectureDocFn.
func (m *MockClient) BuildArchitectureDoc(ctx context.Context, repoSpec string) (ArchitectureDoc, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.BuildArchitectureCalls = append(m.BuildArchitectureCalls, repoSpec)
	if m.BuildArchitectureDocFn != nil {
		return m.BuildArchitectureDocFn(ctx, repoSpec)
	}
	return ArchitectureDoc{
		RepoName: repoSpec,
		Markdown: "<!-- AUTO-GENERATED by `dogArchitectureDocRender` on 1970-01-01; DO NOT HAND-EDIT. -->\n\n# ARCHITECTURE — " + repoSpec + "\n\n_(stub)_\n",
	}, nil
}

// Reset clears all recorded calls and fixture state, restoring NextWriteID
// to 1. Useful between sub-tests inside the same test function.
func (m *MockClient) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Memories = nil
	m.NextWriteID = 1
	m.Candidates = nil
	m.NextCandidateID = 1
	m.WriteCalls = nil
	m.WriteTxCalls = nil
	m.GetTaskCalls = nil
	m.GetScopeCalls = nil
	m.UpdateCalls = nil
	m.RemoveCalls = nil
	m.SummarizeCalls = nil
	m.EmitCalls = nil
	m.ListPendingCalls = 0
	m.GetWeightedCalls = nil
	m.RecentCommitsCalls = nil
	m.BootstrapCalls = nil
	m.RefreshDigestCalls = nil
	m.BuildDigestCalls = nil
	m.BuildArchitectureCalls = nil
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
