package claude

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claude"
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

// newTestHandlerWithFetchers creates a handler whose limitsState uses the
// supplied fetchers instead of the production ones. Each call returns a fresh,
// isolated state so tests never share cache entries.
func newTestHandlerWithFetchers(auths []*coreauth.Auth, pf profileFetcher, uf usageFetcher) *ClaudeCodeAPIHandler {
	h := newTestHandler(auths)
	h.limitsState = newClaudeLimitsState(pf, uf)
	return h
}

// noopUsageFetcher returns an empty Usage so tests that do not care about
// usage data still compile and run cleanly.
func noopUsageFetcher(_ context.Context, _ string) (*claudeauth.Usage, error) {
	return &claudeauth.Usage{}, nil
}

// serveAndDecode is a helper that runs one GET /v1/claude/limits request and
// decodes the response.
func serveAndDecode(t *testing.T, h *ClaudeCodeAPIHandler) claudeLimitsResponse {
	t.Helper()
	engine := gin.New()
	engine.GET("/v1/claude/limits", h.ClaudeLimits)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/claude/limits", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp claudeLimitsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return resp
}

func TestClaudeLimits_EmptyCredentials(t *testing.T) {
	t.Parallel()
	h := newTestHandler(nil)
	resp := serveAndDecode(t, h)
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
	resp := serveAndDecode(t, h)
	if len(resp.Credentials) != 0 {
		t.Errorf("credentials: got %d, want 0 (non-claude filtered out)", len(resp.Credentials))
	}
}

func TestClaudeLimits_NoAccessToken(t *testing.T) {
	t.Parallel()
	auths := []*coreauth.Auth{{
		ID:       "claude-bare",
		Provider: "claude",
		Metadata: map[string]any{"email": "x@y"},
	}}
	h := newTestHandlerWithFetchers(auths,
		func(_ context.Context, _ string) (map[string]any, error) {
			t.Error("profile fetcher should not be called when no token")
			return nil, nil
		},
		func(_ context.Context, _ string) (*claudeauth.Usage, error) {
			t.Error("usage fetcher should not be called when no token")
			return nil, nil
		},
	)
	resp := serveAndDecode(t, h)
	if len(resp.Credentials) != 1 {
		t.Fatalf("credentials: got %d, want 1", len(resp.Credentials))
	}
	cred := resp.Credentials[0]
	if cred.FetchErrors == nil {
		t.Error("fetch_errors: expected non-nil for missing token")
	}
	if _, ok := cred.FetchErrors["profile"]; !ok {
		t.Error("fetch_errors.profile: expected to be populated")
	}
}

func TestClaudeLimits_PlanFromProfile(t *testing.T) {
	t.Parallel()
	auths := []*coreauth.Auth{{
		ID:       "claude-1",
		Provider: "claude",
		Metadata: map[string]any{"access_token": "live-tok"},
	}}
	h := newTestHandlerWithFetchers(auths,
		func(_ context.Context, _ string) (map[string]any, error) {
			return map[string]any{
				"account": map[string]any{
					"email":        "u@example.com",
					"display_name": "Vlad",
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
	resp := serveAndDecode(t, h)
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
	if cred.Email != "u@example.com" {
		t.Errorf("email = %q, want u@example.com", cred.Email)
	}
	if cred.DisplayName != "Vlad" {
		t.Errorf("display_name = %q, want Vlad", cred.DisplayName)
	}
	if cred.FetchErrors != nil {
		t.Errorf("fetch_errors: expected nil on success, got %v", cred.FetchErrors)
	}
}

func TestClaudeLimits_UsagePopulatesLimits(t *testing.T) {
	t.Parallel()

	resetsAt := time.Date(2026, 5, 7, 21, 0, 1, 0, time.UTC)
	auths := []*coreauth.Auth{{
		ID:       "claude-2",
		Provider: "claude",
		Metadata: map[string]any{"access_token": "tok"},
	}}
	h := newTestHandlerWithFetchers(auths,
		func(_ context.Context, _ string) (map[string]any, error) {
			return map[string]any{
				"account":      map[string]any{"email": "u@x.com"},
				"organization": map[string]any{"organization_type": "claude_pro"},
			}, nil
		},
		func(_ context.Context, _ string) (*claudeauth.Usage, error) {
			return &claudeauth.Usage{
				FiveHour: &claudeauth.UsageWindow{Utilization: 3.0, ResetsAt: &resetsAt},
				SevenDay: &claudeauth.UsageWindow{Utilization: 6.0, ResetsAt: &resetsAt},
				ExtraUsage: &claudeauth.ExtraUsage{
					IsEnabled:      true,
					MonthlyLimit:   5000,
					UsedCredits:    566,
					UtilizationPct: 11.32,
					Currency:       "USD",
				},
			}, nil
		},
	)

	resp := serveAndDecode(t, h)
	if len(resp.Credentials) != 1 {
		t.Fatalf("credentials: got %d", len(resp.Credentials))
	}
	cred := resp.Credentials[0]

	if cred.Limits.FiveHour == nil {
		t.Fatal("limits.five_hour: expected non-nil")
	}
	if cred.Limits.FiveHour.UtilizationPct != 3.0 {
		t.Errorf("five_hour.utilization_pct = %v, want 3.0", cred.Limits.FiveHour.UtilizationPct)
	}
	if cred.Limits.FiveHour.RemainingPct != 97.0 {
		t.Errorf("five_hour.remaining_pct = %v, want 97.0", cred.Limits.FiveHour.RemainingPct)
	}
	if cred.Limits.SevenDay == nil {
		t.Fatal("limits.seven_day: expected non-nil")
	}

	// SevenDaySonnet and SevenDayOpus are nil from the fetcher — they must
	// serialise as explicit null, not be omitted.
	raw := serveRaw(t, h)
	var rawMap map[string]any
	if err := json.Unmarshal(raw, &rawMap); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	creds := rawMap["credentials"].([]any)
	limits := creds[0].(map[string]any)["limits"].(map[string]any)

	for _, key := range []string{"seven_day_sonnet", "seven_day_opus"} {
		v, exists := limits[key]
		if !exists {
			t.Errorf("limits.%s: key must be present in JSON (explicit null)", key)
			continue
		}
		if v != nil {
			t.Errorf("limits.%s: expected JSON null, got %v", key, v)
		}
	}

	if cred.ExtraUsage == nil {
		t.Fatal("extra_usage: expected non-nil")
	}
	if cred.ExtraUsage.RemainingCredits != 4434 {
		t.Errorf("extra_usage.remaining_credits = %v, want 4434", cred.ExtraUsage.RemainingCredits)
	}
}

func TestClaudeLimits_QuotaExceeded(t *testing.T) {
	t.Parallel()
	recover := time.Now().Add(5 * time.Hour).UTC()
	auths := []*coreauth.Auth{{
		ID:       "claude-quota",
		Provider: "claude",
		Metadata: map[string]any{"access_token": "tok"},
		Quota: coreauth.QuotaState{
			Exceeded:      true,
			NextRecoverAt: recover,
		},
	}}
	h := newTestHandlerWithFetchers(auths,
		func(_ context.Context, _ string) (map[string]any, error) {
			return map[string]any{"organization": map[string]any{"organization_type": "claude_max"}}, nil
		},
		noopUsageFetcher,
	)
	resp := serveAndDecode(t, h)
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

func TestClaudeLimits_ProfileFetchError(t *testing.T) {
	t.Parallel()
	auths := []*coreauth.Auth{{
		ID:       "claude-err",
		Provider: "claude",
		Metadata: map[string]any{"access_token": "expired"},
	}}
	h := newTestHandlerWithFetchers(auths,
		func(_ context.Context, _ string) (map[string]any, error) {
			return nil, &statusErr{401, "Unauthorized"}
		},
		noopUsageFetcher,
	)
	resp := serveAndDecode(t, h)
	if len(resp.Credentials) != 1 {
		t.Fatalf("credentials: got %d", len(resp.Credentials))
	}
	cred := resp.Credentials[0]
	if cred.FetchErrors == nil {
		t.Fatal("fetch_errors: expected non-nil on profile error")
	}
	if cred.FetchErrors["profile"] == "" {
		t.Error("fetch_errors.profile: expected non-empty")
	}
	// Usage succeeded so usage key should not be present.
	if _, ok := cred.FetchErrors["usage"]; ok {
		t.Error("fetch_errors.usage: should be absent when usage succeeded")
	}
}

func TestClaudeLimits_UsageFetchError(t *testing.T) {
	t.Parallel()
	auths := []*coreauth.Auth{{
		ID:       "claude-usg-err",
		Provider: "claude",
		Metadata: map[string]any{"access_token": "tok"},
	}}
	h := newTestHandlerWithFetchers(auths,
		func(_ context.Context, _ string) (map[string]any, error) {
			return map[string]any{"organization": map[string]any{"organization_type": "claude_pro"}}, nil
		},
		func(_ context.Context, _ string) (*claudeauth.Usage, error) {
			return nil, &statusErr{503, "Service Unavailable"}
		},
	)
	resp := serveAndDecode(t, h)
	if len(resp.Credentials) != 1 {
		t.Fatalf("credentials: got %d", len(resp.Credentials))
	}
	cred := resp.Credentials[0]
	if cred.FetchErrors == nil {
		t.Fatal("fetch_errors: expected non-nil on usage error")
	}
	if cred.FetchErrors["usage"] == "" {
		t.Error("fetch_errors.usage: expected non-empty")
	}
	if _, ok := cred.FetchErrors["profile"]; ok {
		t.Error("fetch_errors.profile: should be absent when profile succeeded")
	}
}

func TestClaudeLimits_BothFetchErrors(t *testing.T) {
	t.Parallel()
	auths := []*coreauth.Auth{{
		ID:       "claude-both-err",
		Provider: "claude",
		Metadata: map[string]any{"access_token": "bad"},
	}}
	h := newTestHandlerWithFetchers(auths,
		func(_ context.Context, _ string) (map[string]any, error) {
			return nil, &statusErr{401, "Unauthorized"}
		},
		func(_ context.Context, _ string) (*claudeauth.Usage, error) {
			return nil, &statusErr{401, "Unauthorized"}
		},
	)
	resp := serveAndDecode(t, h)
	cred := resp.Credentials[0]
	if cred.FetchErrors == nil {
		t.Fatal("fetch_errors: expected non-nil")
	}
	if cred.FetchErrors["profile"] == "" {
		t.Error("fetch_errors.profile: expected non-empty")
	}
	if cred.FetchErrors["usage"] == "" {
		t.Error("fetch_errors.usage: expected non-empty")
	}
}

func TestClaudeLimits_UnknownPlanWhenNoMetadata(t *testing.T) {
	t.Parallel()
	// No access token — fetchers never called.
	auths := []*coreauth.Auth{
		{ID: "claude-bare", Provider: "claude"},
	}
	h := newTestHandler(auths)
	resp := serveAndDecode(t, h)
	if len(resp.Credentials) != 1 {
		t.Fatalf("credentials: got %d, want 1", len(resp.Credentials))
	}
	if resp.Credentials[0].Plan != "unknown" {
		t.Errorf("plan = %q, want unknown", resp.Credentials[0].Plan)
	}
}

func TestCacheTTLConstants(t *testing.T) {
	t.Parallel()
	if profileCacheTTL < 30*time.Second {
		t.Errorf("profileCacheTTL too short: %v", profileCacheTTL)
	}
	if usageCacheTTL < 30*time.Second {
		t.Errorf("usageCacheTTL too short: %v", usageCacheTTL)
	}
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// statusErr is a lightweight error type used in tests.
type statusErr struct {
	code int
	msg  string
}

func (e *statusErr) Error() string {
	return e.msg
}

// serveRaw fires a request and returns the raw body bytes for JSON path
// inspection.
func serveRaw(t *testing.T, h *ClaudeCodeAPIHandler) []byte {
	t.Helper()
	engine := gin.New()
	engine.GET("/v1/claude/limits", h.ClaudeLimits)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/claude/limits", nil))
	return w.Body.Bytes()
}
