package claude

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// TestClaudeLimits_ResponseOmitsIdentityAndUpstreamErrors guards that the
// /v1/claude/limits endpoint, which any client API key can call, never exposes
// account identity (auth id, email, display name) or raw upstream error text.
func TestClaudeLimits_ResponseOmitsIdentityAndUpstreamErrors(t *testing.T) {
	t.Parallel()
	const (
		authID      = "claude-secret-auth-id.json"
		email       = "owner@example.com"
		displayName = "Account Owner"
		upstreamMsg = "upstream-secret-detail"
	)
	auths := []*coreauth.Auth{
		{
			ID:       authID,
			Provider: "claude",
			Metadata: map[string]any{"access_token": "tok", "email": email, "display_name": displayName},
		},
		{
			ID:       "claude-no-token",
			Provider: "claude",
			Metadata: map[string]any{"email": "other@example.com"},
		},
	}
	h := newTestHandlerWithFetchers(auths,
		func(_ context.Context, _ string) (map[string]any, error) {
			return map[string]any{
				"account":      map[string]any{"email": email, "display_name": displayName},
				"organization": map[string]any{"organization_type": "claude_max"},
			}, nil
		},
		func(_ context.Context, _ string) (*claudeauth.Usage, error) {
			return nil, &statusErr{500, upstreamMsg}
		},
	)

	engine := gin.New()
	engine.GET("/v1/claude/limits", h.ClaudeLimits)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/claude/limits", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", w.Code, http.StatusOK)
	}
	body := w.Body.String()

	forbidden := []string{
		authID, "claude-no-token", email, "other@example.com", displayName, upstreamMsg,
		"no access token", `"auth_id"`, `"email"`, `"display_name"`,
	}
	for _, s := range forbidden {
		if strings.Contains(body, s) {
			t.Errorf("response leaks %q: %s", s, body)
		}
	}
	if !strings.Contains(body, `"plan":"claude_max"`) {
		t.Errorf("response should still carry plan data: %s", body)
	}
}
