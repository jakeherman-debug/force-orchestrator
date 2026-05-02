package databricks

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"force-orchestrator/internal/store"
)

// newServer returns an httptest.Server whose handler invokes fn. The
// returned client has its base URL pinned to the server.
func newServer(t *testing.T, fn http.HandlerFunc) (*httptest.Server, Client) {
	t.Helper()
	srv := httptest.NewServer(fn)
	t.Cleanup(srv.Close)
	hc := &http.Client{Timeout: 5 * time.Second}
	c := newInProcessFromHTTP(srv.URL, "test-token", hc)
	return srv, c
}

// requireBearer asserts that the request carries the expected bearer
// token. Tests that exercise auth failure paths skip this check.
func requireBearer(t *testing.T, r *http.Request, want string) {
	t.Helper()
	got := r.Header.Get("Authorization")
	if got != "Bearer "+want {
		t.Errorf("Authorization header: got %q, want %q", got, "Bearer "+want)
	}
}

// ─────────────────────────────────────────────────────────────────────
// ExecuteQuery — happy path
// ─────────────────────────────────────────────────────────────────────

func TestExecuteQuery_HappyPath_SingleScalar(t *testing.T) {
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r, "test-token")
		if r.Method != http.MethodPost {
			t.Errorf("method: got %s, want POST", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/api/2.0/sql/statements") {
			t.Errorf("path: got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"statement_id": "abc-123",
			"status": {"state": "SUCCEEDED"},
			"result": {"data_array": [["42.5"]], "row_count": 1}
		}`)
	})

	v, err := c.ExecuteQuery(context.Background(), "wh-1", "SELECT 42.5", 5*time.Second)
	if err != nil {
		t.Fatalf("ExecuteQuery: unexpected error: %v", err)
	}
	if v != 42.5 {
		t.Errorf("scalar: got %v, want 42.5", v)
	}
}

// ─────────────────────────────────────────────────────────────────────
// ExecuteQuery — async/poll completion
// ─────────────────────────────────────────────────────────────────────

func TestExecuteQuery_AsyncCompletion(t *testing.T) {
	var calls int32
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost:
			// First call: still RUNNING.
			fmt.Fprint(w, `{
				"statement_id": "stmt-1",
				"status": {"state": "RUNNING"}
			}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/sql/statements/stmt-1"):
			if n == 2 {
				// First poll: still RUNNING.
				fmt.Fprint(w, `{
					"statement_id": "stmt-1",
					"status": {"state": "RUNNING"}
				}`)
				return
			}
			// Second poll: SUCCEEDED with data.
			fmt.Fprint(w, `{
				"statement_id": "stmt-1",
				"status": {"state": "SUCCEEDED"},
				"result": {"data_array": [["99"]]}
			}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})

	// Generous timeout so the polling loop can run twice (pollInterval
	// is 2s; we need ~5s of headroom).
	v, err := c.ExecuteQuery(context.Background(), "wh-1", "SELECT 99", 10*time.Second)
	if err != nil {
		t.Fatalf("ExecuteQuery: unexpected error: %v", err)
	}
	if v != 99 {
		t.Errorf("scalar: got %v, want 99", v)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("expected 3 HTTP calls (1 POST + 2 GET), got %d", got)
	}
}

func TestExecuteQuery_AsyncFailed_ErrTransient(t *testing.T) {
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			fmt.Fprint(w, `{
				"statement_id": "stmt-fail",
				"status": {"state": "RUNNING"}
			}`)
		case http.MethodGet:
			fmt.Fprint(w, `{
				"statement_id": "stmt-fail",
				"status": {"state": "FAILED", "error": {"message": "syntax", "error_code": "BAD_SQL"}}
			}`)
		}
	})

	_, err := c.ExecuteQuery(context.Background(), "wh-1", "SELECT bogus", 10*time.Second)
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("expected ErrTransient, got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────
// ExecuteQuery — shape errors
// ─────────────────────────────────────────────────────────────────────

func TestExecuteQuery_MultiRow_ErrShapeUnexpected(t *testing.T) {
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{
			"status": {"state": "SUCCEEDED"},
			"result": {"data_array": [["1"], ["2"]]}
		}`)
	})

	_, err := c.ExecuteQuery(context.Background(), "wh-1", "SELECT *", 5*time.Second)
	if !errors.Is(err, ErrShapeUnexpected) {
		t.Fatalf("expected ErrShapeUnexpected, got %v", err)
	}
}

func TestExecuteQuery_MultiColumn_ErrShapeUnexpected(t *testing.T) {
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{
			"status": {"state": "SUCCEEDED"},
			"result": {"data_array": [["1", "2"]]}
		}`)
	})

	_, err := c.ExecuteQuery(context.Background(), "wh-1", "SELECT 1,2", 5*time.Second)
	if !errors.Is(err, ErrShapeUnexpected) {
		t.Fatalf("expected ErrShapeUnexpected, got %v", err)
	}
}

func TestExecuteQuery_NonNumericResult_ErrShapeUnexpected(t *testing.T) {
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{
			"status": {"state": "SUCCEEDED"},
			"result": {"data_array": [["hello"]]}
		}`)
	})

	_, err := c.ExecuteQuery(context.Background(), "wh-1", "SELECT 'hello'", 5*time.Second)
	if !errors.Is(err, ErrShapeUnexpected) {
		t.Fatalf("expected ErrShapeUnexpected, got %v", err)
	}
}

func TestExecuteQuery_EmptyResult_ErrShapeUnexpected(t *testing.T) {
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{
			"status": {"state": "SUCCEEDED"},
			"result": {"data_array": []}
		}`)
	})

	_, err := c.ExecuteQuery(context.Background(), "wh-1", "SELECT *", 5*time.Second)
	if !errors.Is(err, ErrShapeUnexpected) {
		t.Fatalf("expected ErrShapeUnexpected on empty result, got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────
// ExecuteQuery — timeout / transport
// ─────────────────────────────────────────────────────────────────────

func TestExecuteQuery_Timeout_ErrTimeout(t *testing.T) {
	// Server holds the response open longer than the client's timeout.
	// We bound the handler at 2s so httptest.Server.Close() doesn't
	// block waiting for the connection after the test.
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})

	_, err := c.ExecuteQuery(context.Background(), "wh-1", "SELECT 1", 200*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected ErrTimeout, got %v", err)
	}
}

func TestExecuteQuery_5xx_ErrTransient(t *testing.T) {
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	_, err := c.ExecuteQuery(context.Background(), "wh-1", "SELECT 1", 5*time.Second)
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("expected ErrTransient, got %v", err)
	}
}

func TestExecuteQuery_401_ErrAuthFailure(t *testing.T) {
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	})

	_, err := c.ExecuteQuery(context.Background(), "wh-1", "SELECT 1", 5*time.Second)
	if !errors.Is(err, ErrAuthFailure) {
		t.Fatalf("expected ErrAuthFailure, got %v", err)
	}
}

func TestExecuteQuery_403_ErrAuthFailure(t *testing.T) {
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	})

	_, err := c.ExecuteQuery(context.Background(), "wh-1", "SELECT 1", 5*time.Second)
	if !errors.Is(err, ErrAuthFailure) {
		t.Fatalf("expected ErrAuthFailure, got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────
// ExecuteQuery — argument validation
// ─────────────────────────────────────────────────────────────────────

func TestExecuteQuery_EmptyWarehouse_Error(t *testing.T) {
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server should not be called when warehouse_id is empty")
	})
	_, err := c.ExecuteQuery(context.Background(), "", "SELECT 1", 5*time.Second)
	if err == nil {
		t.Fatal("expected error for empty warehouse_id")
	}
}

func TestExecuteQuery_EmptySQL_Error(t *testing.T) {
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server should not be called when sql_query is empty")
	})
	_, err := c.ExecuteQuery(context.Background(), "wh-1", "", 5*time.Second)
	if err == nil {
		t.Fatal("expected error for empty sql_query")
	}
}

// ─────────────────────────────────────────────────────────────────────
// Health
// ─────────────────────────────────────────────────────────────────────

func TestHealth_200_Nil(t *testing.T) {
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		requireBearer(t, r, "test-token")
		if r.Method != http.MethodGet {
			t.Errorf("method: got %s, want GET", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/api/2.0/preview/scim/v2/Me") {
			t.Errorf("path: got %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"id": "user-1", "userName": "test@example.com"}`)
	})

	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health: unexpected error: %v", err)
	}
}

func TestHealth_401_ErrAuthFailure(t *testing.T) {
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	})
	err := c.Health(context.Background())
	if !errors.Is(err, ErrAuthFailure) {
		t.Fatalf("expected ErrAuthFailure, got %v", err)
	}
}

func TestHealth_5xx_ErrTransient(t *testing.T) {
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	})
	err := c.Health(context.Background())
	if !errors.Is(err, ErrTransient) {
		t.Fatalf("expected ErrTransient, got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────
// NewInProcess — config validation
// ─────────────────────────────────────────────────────────────────────

func TestNewInProcess_MissingWorkspaceURL_ErrConfig(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SetConfig(db, ConfigKeyToken, "some-token")
	// Workspace URL deliberately unset.

	_, err := NewInProcess(context.Background(), db)
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("expected ErrConfig, got %v", err)
	}
}

func TestNewInProcess_MissingToken_ErrConfig(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SetConfig(db, ConfigKeyWorkspaceURL, "https://example.cloud.databricks.com")
	// Token deliberately unset.

	_, err := NewInProcess(context.Background(), db)
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("expected ErrConfig, got %v", err)
	}
}

func TestNewInProcess_BothSet_OK(t *testing.T) {
	db := store.InitHolocronDSN(":memory:")
	defer db.Close()

	store.SetConfig(db, ConfigKeyWorkspaceURL, "https://example.cloud.databricks.com/")
	store.SetConfig(db, ConfigKeyToken, "pat-xyz")

	c, err := NewInProcess(context.Background(), db)
	if err != nil {
		t.Fatalf("NewInProcess: unexpected error: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil Client")
	}
	// Sanity-check that the URL trim happened: hammer the unexported
	// field via the package-internal type assertion.
	ipc, ok := c.(*inProcessClient)
	if !ok {
		t.Fatalf("unexpected concrete type %T", c)
	}
	if strings.HasSuffix(ipc.workspaceURL, "/") {
		t.Errorf("workspace URL should be trimmed of trailing slash, got %q", ipc.workspaceURL)
	}
}

func TestNewInProcess_NilDB_ErrConfig(t *testing.T) {
	_, err := NewInProcess(context.Background(), nil)
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("expected ErrConfig on nil db, got %v", err)
	}
}
