package store

import (
	"testing"
)

func TestNormalizeAPIPath(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		// Already canonical — colon form unchanged.
		{
			name:  "colon_already_canonical",
			input: "GET /api/v1/users/:id",
			want:  "GET /api/v1/users/:id",
		},
		// Rails / OpenAPI curly-brace form.
		{
			name:  "curly_brace_single",
			input: "GET /api/v1/users/{id}",
			want:  "GET /api/v1/users/:id",
		},
		{
			name:  "curly_brace_multiple",
			input: "GET /api/v1/orgs/{org}/repos/{repo}",
			want:  "GET /api/v1/orgs/:org/repos/:repo",
		},
		// Template-literal / Spring dollar-brace form.
		{
			name:  "dollar_brace_single",
			input: "GET /api/v1/users/${id}",
			want:  "GET /api/v1/users/:id",
		},
		{
			name:  "dollar_brace_multiple",
			input: "POST /api/v1/${version}/items/${itemId}",
			want:  "POST /api/v1/:version/items/:itemId",
		},
		// Angle-bracket form.
		{
			name:  "angle_bracket_single",
			input: "DELETE /api/v1/users/<pk>",
			want:  "DELETE /api/v1/users/:pk",
		},
		{
			name:  "angle_bracket_multiple",
			input: "PATCH /api/<version>/items/<id>",
			want:  "PATCH /api/:version/items/:id",
		},
		// Trailing slash trimmed.
		{
			name:  "trailing_slash_trimmed",
			input: "GET /api/v1/users/",
			want:  "GET /api/v1/users",
		},
		{
			name:  "trailing_slash_with_param",
			input: "GET /api/v1/users/{id}/",
			want:  "GET /api/v1/users/:id",
		},
		// gRPC RPC name — no transformation.
		{
			name:  "grpc_rpc_unchanged",
			input: "service.UserService/GetUser",
			want:  "service.UserService/GetUser",
		},
		// Proto event — no transformation.
		{
			name:  "proto_event_unchanged",
			input: "events.UserCreated",
			want:  "events.UserCreated",
		},
		// No method prefix — plain path.
		{
			name:  "no_method_prefix",
			input: "/api/v1/users/{id}/posts/{postId}",
			want:  "/api/v1/users/:id/posts/:postId",
		},
		// Mixed forms in one path (unlikely but should still normalize).
		{
			name:  "mixed_forms",
			input: "GET /v1/{org}/${repo}/<branch>",
			want:  "GET /v1/:org/:repo/:branch",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeAPIPath(tc.input)
			if got != tc.want {
				t.Errorf("NormalizeAPIPath(%q)\n  got  %q\n  want %q", tc.input, got, tc.want)
			}
		})
	}
}
