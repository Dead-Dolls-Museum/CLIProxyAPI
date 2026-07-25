package claude

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claude"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestClaudeLimits_ProfileFetchPopulatesPlan(t *testing.T) {
	t.Parallel()
	auths := []*coreauth.Auth{{
		ID:       "claude-1",
		Provider: "claude",
		Metadata: map[string]any{"access_token": "live-tok"},
	}}
	h := newTestHandlerWithFetchers(auths,
		func(ctx context.Context, accessToken string) (map[string]any, error) {
			if accessToken != "live-tok" {
				t.Errorf("unexpected token: %q", accessToken)
			}
			return map[string]any{
				"account": map[string]any{
					"email":        "u@example.com",
					"display_name": "Test User",
				},
				"organization": map[string]any{
					"organization_type":   "claude_max",
					"rate_limit_tier":     "default_claude_max_20x",
					"subscription_status": "active",
				},
			}, nil
		},
		noopUsageFetcher,
	)

	engine := gin.New()
	engine.GET("/v1/claude/limits", h.ClaudeLimits)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/claude/limits", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	var resp claudeLimitsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Credentials) != 1 {
		t.Fatalf("credentials: got %d", len(resp.Credentials))
	}
	cred := resp.Credentials[0]
	if cred.Plan != "claude_max" {
		t.Errorf("plan = %q, want claude_max", cred.Plan)
	}
	if cred.RateLimitTier != "claude_max_20x" {
		t.Errorf("rate_limit_tier = %q, want claude_max_20x", cred.RateLimitTier)
	}
	if cred.SubscriptionStatus != "active" {
		t.Errorf("subscription_status = %q, want active", cred.SubscriptionStatus)
	}
	if cred.FetchErrors != nil {
		t.Errorf("fetch_errors unexpectedly set: %v", cred.FetchErrors)
	}
}

func TestClaudeLimits_ProfileFetchErrorFallsBack(t *testing.T) {
	t.Parallel()
	auths := []*coreauth.Auth{{
		ID:       "claude-stale",
		Provider: "claude",
		Metadata: map[string]any{"access_token": "expired"},
	}}
	h := newTestHandlerWithFetchers(auths,
		func(ctx context.Context, accessToken string) (map[string]any, error) {
			return nil, errors.New("401 Unauthorized")
		},
		noopUsageFetcher,
	)

	engine := gin.New()
	engine.GET("/v1/claude/limits", h.ClaudeLimits)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/claude/limits", nil))

	var resp claudeLimitsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cred := resp.Credentials[0]
	// Plan falls back to "unknown" since no profile data could be retrieved.
	if cred.Plan != "unknown" {
		t.Errorf("plan = %q, want unknown", cred.Plan)
	}
	if cred.FetchErrors == nil || cred.FetchErrors["profile"] == "" {
		t.Errorf("fetch_errors.profile should be populated on error")
	}
}

func TestClaudeLimits_NoAccessTokenSkipsFetch(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	auths := []*coreauth.Auth{{
		ID:       "claude-empty",
		Provider: "claude",
		Metadata: map[string]any{"email": "x@y"},
	}}
	h := newTestHandlerWithFetchers(auths,
		func(ctx context.Context, accessToken string) (map[string]any, error) {
			calls.Add(1)
			return nil, nil
		},
		func(ctx context.Context, accessToken string) (*claudeauth.Usage, error) {
			calls.Add(1)
			return nil, nil
		},
	)

	engine := gin.New()
	engine.GET("/v1/claude/limits", h.ClaudeLimits)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/claude/limits", nil))

	if calls.Load() != 0 {
		t.Errorf("fetcher called %d times, want 0", calls.Load())
	}
	var resp claudeLimitsResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Credentials[0].FetchErrors == nil {
		t.Errorf("fetch_errors: expected non-nil for missing token")
	}
}

func TestClaudeLimits_ProfileCacheReusesEntry(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	auths := []*coreauth.Auth{{
		ID:       "claude-cached",
		Provider: "claude",
		Metadata: map[string]any{"access_token": "tok"},
	}}
	h := newTestHandlerWithFetchers(auths,
		func(ctx context.Context, accessToken string) (map[string]any, error) {
			calls.Add(1)
			return map[string]any{
				"organization": map[string]any{"organization_type": "claude_pro"},
			}, nil
		},
		func(ctx context.Context, accessToken string) (*claudeauth.Usage, error) {
			// usage fetcher counts separately but the important thing is
			// profile cache is used after first call.
			return &claudeauth.Usage{}, nil
		},
	)

	engine := gin.New()
	engine.GET("/v1/claude/limits", h.ClaudeLimits)

	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/claude/limits", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("call %d: status %d", i, w.Code)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("profile fetcher calls = %d, want 1 (cache should serve calls 2 and 3)", got)
	}
}
