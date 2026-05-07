package claude

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func newTestHandler(auths []*coreauth.Auth) *ClaudeCodeAPIHandler {
	mgr := coreauth.NewManager(nil, nil, nil)
	for _, a := range auths {
		if a == nil {
			continue
		}
		_, _ = mgr.Register(context.Background(), a)
	}
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, mgr)
	return NewClaudeCodeAPIHandler(base)
}

func TestClaudeLimits_EmptyCredentials(t *testing.T) {
	t.Parallel()
	h := newTestHandler(nil)

	engine := gin.New()
	engine.GET("/v1/claude/limits", h.ClaudeLimits)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/claude/limits", nil)
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusOK)
	}
	var resp claudeLimitsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Credentials) != 0 {
		t.Errorf("credentials: got %d, want 0", len(resp.Credentials))
	}
}

func TestClaudeLimits_FiltersNonClaude(t *testing.T) {
	t.Parallel()
	auths := []*coreauth.Auth{
		{ID: "gemini-1", Provider: "gemini", Metadata: map[string]any{"email": "g@example.com"}},
		{ID: "codex-1", Provider: "codex", Metadata: map[string]any{"email": "c@example.com"}},
	}
	h := newTestHandler(auths)

	engine := gin.New()
	engine.GET("/v1/claude/limits", h.ClaudeLimits)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/claude/limits", nil)
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusOK)
	}
	var resp claudeLimitsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Credentials) != 0 {
		t.Errorf("credentials: got %d, want 0 (non-claude filtered out)", len(resp.Credentials))
	}
}

func TestClaudeLimits_NoObservationYet(t *testing.T) {
	t.Parallel()
	auths := []*coreauth.Auth{
		{
			ID:       "claude-1",
			Provider: "claude",
			Metadata: map[string]any{
				"email":             "user@example.com",
				"subscription_tier": "claude_max_5x",
			},
		},
	}
	h := newTestHandler(auths)

	engine := gin.New()
	engine.GET("/v1/claude/limits", h.ClaudeLimits)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/claude/limits", nil)
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusOK)
	}
	var resp claudeLimitsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Credentials) != 1 {
		t.Fatalf("credentials: got %d, want 1", len(resp.Credentials))
	}
	cred := resp.Credentials[0]
	if cred.SubscriptionTier != "claude_max_5x" {
		t.Errorf("subscription_tier: got %q, want %q", cred.SubscriptionTier, "claude_max_5x")
	}
	if cred.Email != "user@example.com" {
		t.Errorf("email: got %q, want %q", cred.Email, "user@example.com")
	}
	// No observation yet — rate_limits and last_observed_at should be null.
	if cred.RateLimits != nil {
		t.Errorf("rate_limits: expected nil, got %+v", cred.RateLimits)
	}
	if cred.LastObservedAt != nil {
		t.Errorf("last_observed_at: expected nil, got %v", cred.LastObservedAt)
	}
}

func TestClaudeLimits_WithObservation(t *testing.T) {
	t.Parallel()

	remaining := int64(32145)
	resetAt := int64(1746630000)
	now := time.Now().UTC()

	snap := coreauth.ClaudeLimitsSnapshot{
		FiveHour: &coreauth.ClaudeRateWindow{
			Status:    "allowed",
			Remaining: &remaining,
			ResetAt:   &resetAt,
		},
		LastObservedAt: now,
	}

	meta := map[string]any{
		"email":             "pro@example.com",
		"subscription_tier": "claude_pro",
		"claude_limits":     snap,
	}
	auths := []*coreauth.Auth{
		{ID: "claude-2", Provider: "claude", Metadata: meta},
	}
	h := newTestHandler(auths)

	engine := gin.New()
	engine.GET("/v1/claude/limits", h.ClaudeLimits)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/claude/limits", nil)
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusOK)
	}
	var resp claudeLimitsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Credentials) != 1 {
		t.Fatalf("credentials: got %d, want 1", len(resp.Credentials))
	}
	cred := resp.Credentials[0]
	if cred.SubscriptionTier != "claude_pro" {
		t.Errorf("subscription_tier: got %q, want %q", cred.SubscriptionTier, "claude_pro")
	}
	if cred.RateLimits == nil {
		t.Fatal("rate_limits: expected non-nil")
	}
	if cred.RateLimits.FiveHour == nil {
		t.Fatal("rate_limits.five_hour: expected non-nil")
	}
	if cred.RateLimits.FiveHour.Status != "allowed" {
		t.Errorf("five_hour.status: got %q, want %q", cred.RateLimits.FiveHour.Status, "allowed")
	}
	if cred.RateLimits.FiveHour.Remaining == nil || *cred.RateLimits.FiveHour.Remaining != 32145 {
		t.Errorf("five_hour.remaining: got %v, want 32145", cred.RateLimits.FiveHour.Remaining)
	}
	if cred.LastObservedAt == nil {
		t.Error("last_observed_at: expected non-nil")
	}
}

func TestClaudeLimits_QuotaExceeded(t *testing.T) {
	t.Parallel()

	recover := time.Now().Add(5 * time.Hour).UTC()
	auths := []*coreauth.Auth{
		{
			ID:       "claude-quota",
			Provider: "claude",
			Metadata: map[string]any{"subscription_tier": "claude_max_5x"},
			Quota: coreauth.QuotaState{
				Exceeded:      true,
				NextRecoverAt: recover,
			},
		},
	}
	h := newTestHandler(auths)

	engine := gin.New()
	engine.GET("/v1/claude/limits", h.ClaudeLimits)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/claude/limits", nil)
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusOK)
	}
	var resp claudeLimitsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Credentials) != 1 {
		t.Fatalf("credentials: got %d, want 1", len(resp.Credentials))
	}
	cred := resp.Credentials[0]
	if !cred.Quota.Exceeded {
		t.Error("quota.exceeded: expected true")
	}
	if cred.Quota.NextRecoverAt == nil {
		t.Error("quota.next_recover_at: expected non-nil")
	}
}

func TestClaudeLimits_UnknownTierWhenNoMetadata(t *testing.T) {
	t.Parallel()
	auths := []*coreauth.Auth{
		{ID: "claude-bare", Provider: "claude"},
	}
	h := newTestHandler(auths)

	engine := gin.New()
	engine.GET("/v1/claude/limits", h.ClaudeLimits)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/claude/limits", nil)
	engine.ServeHTTP(w, req)

	var resp claudeLimitsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Credentials) != 1 {
		t.Fatalf("credentials: got %d, want 1", len(resp.Credentials))
	}
	if resp.Credentials[0].SubscriptionTier != "unknown" {
		t.Errorf("subscription_tier: got %q, want %q", resp.Credentials[0].SubscriptionTier, "unknown")
	}
}
