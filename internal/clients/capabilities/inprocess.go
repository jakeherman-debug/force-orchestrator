package capabilities

import "context"

// inProcessClient is the placeholder D0 backing. Every method returns
// ErrNotImplemented so callers receive a real error if they reach a
// capability-profile path before D1 lands its implementation.
//
// D1 (capability profiles deliverable, T0-1) replaces this stub with
// a real implementation that reads `agent_profiles/<agent>.toml` (or
// equivalent) and serves the four interface methods.
type inProcessClient struct{}

// NewInProcess returns the placeholder Client. Callers receive
// ErrNotImplemented from every method until D1 replaces this body.
// Constructor signature matches the D0 standard so D1 only changes
// the body, not the call sites.
func NewInProcess() Client { return &inProcessClient{} }

func (*inProcessClient) LoadProfile(ctx context.Context, agentName string) (*Profile, error) {
	return nil, ErrNotImplemented
}

func (*inProcessClient) AllowedTools(ctx context.Context, agentName string) ([]string, error) {
	return nil, ErrNotImplemented
}

func (*inProcessClient) DisallowedTools(ctx context.Context, agentName string) ([]string, error) {
	return nil, ErrNotImplemented
}

func (*inProcessClient) MCPConfigPath(ctx context.Context, agentName string) (string, error) {
	return "", ErrNotImplemented
}
