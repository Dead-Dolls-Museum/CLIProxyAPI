package claude

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtractTier(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		profile map[string]any
		want    string
	}{
		{
			name:    "nil profile",
			profile: nil,
			want:    "unknown",
		},
		{
			name:    "empty profile",
			profile: map[string]any{},
			want:    "unknown",
		},
		{
			name:    "top-level subscription_tier",
			profile: map[string]any{"subscription_tier": "claude_max_5x"},
			want:    "claude_max_5x",
		},
		{
			name:    "top-level tier alias",
			profile: map[string]any{"tier": "claude_pro"},
			want:    "claude_pro",
		},
		{
			name: "nested under organization",
			profile: map[string]any{
				"organization": map[string]any{"plan": "claude_max"},
			},
			want: "claude_max",
		},
		{
			name: "scope array of strings",
			profile: map[string]any{
				"scope": []any{"openid", "claude_max_20x", "email"},
			},
			want: "claude_max_20x",
		},
		{
			name: "scope as space-separated string",
			profile: map[string]any{
				"scope": "openid claude_pro email",
			},
			want: "claude_pro",
		},
		{
			name: "longest match wins (max_20x before max)",
			profile: map[string]any{
				"scopes": []string{"claude_max_20x", "claude_max"},
			},
			want: "claude_max_20x",
		},
		{
			name: "unrecognised values",
			profile: map[string]any{
				"tier":  "enterprise",
				"plan":  "",
				"scope": []any{"openid"},
			},
			want: "enterprise",
		},
		{
			name: "case-insensitive scope match",
			profile: map[string]any{
				"scope": []any{"CLAUDE_PRO"},
			},
			want: "claude_pro",
		},
		{
			name: "non-string nested value ignored",
			profile: map[string]any{
				"organization": map[string]any{"tier": 42},
			},
			want: "unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ExtractTier(tc.profile); got != tc.want {
				t.Fatalf("ExtractTier = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFetchProfileSendsExpectedHeaders(t *testing.T) {
	t.Parallel()
	var gotAuth, gotBeta, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotBeta = r.Header.Get("anthropic-beta")
		gotCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"subscription_tier":"claude_pro","email":"a@b.c"}`))
	}))
	defer srv.Close()

	client := srv.Client()
	profile, err := fetchProfileFromURL(context.Background(), "tok123", client, srv.URL)
	if err != nil {
		t.Fatalf("FetchProfile error: %v", err)
	}
	if got := profile["subscription_tier"]; got != "claude_pro" {
		t.Fatalf("subscription_tier = %v, want claude_pro", got)
	}
	if gotAuth != "Bearer tok123" {
		t.Fatalf("Authorization header = %q", gotAuth)
	}
	if gotBeta != AnthropicOAuthBetaHeader {
		t.Fatalf("anthropic-beta = %q", gotBeta)
	}
	if gotCT != "application/json" {
		t.Fatalf("Content-Type = %q", gotCT)
	}
}

func TestFetchProfileEmptyToken(t *testing.T) {
	t.Parallel()
	if _, err := FetchProfile(context.Background(), "  ", nil); err == nil {
		t.Fatal("expected error for empty access token")
	}
}

func TestFetchProfileNon2xx(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	_, err := fetchProfileFromURL(context.Background(), "tok", srv.Client(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 error, got %v", err)
	}
}

func TestFetchProfileMalformedJSON(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer srv.Close()

	_, err := fetchProfileFromURL(context.Background(), "tok", srv.Client(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("expected decode error, got %v", err)
	}
}

// realProfilePayload is the confirmed production Anthropic /api/oauth/profile
// response shape used to drive the helper extraction tests.
var realProfilePayload = map[string]any{
	"account": map[string]any{
		"created_at":     "2024-01-01T00:00:00Z",
		"display_name":   "Vlad",
		"email":          "user@example.com",
		"full_name":      "Vlad Doe",
		"has_claude_max": false,
		"has_claude_pro": true,
		"uuid":           "acct-uuid-123",
	},
	"application": map[string]any{
		"name": "Claude Code",
		"slug": "claude-code",
		"uuid": "app-uuid-456",
	},
	"organization": map[string]any{
		"billing_type":            "stripe_subscription",
		"has_extra_usage_enabled": false,
		"organization_type":       "claude_pro",
		"rate_limit_tier":         "default_claude_ai",
		"subscription_status":     "active",
		"subscription_created_at": "2024-01-15T00:00:00Z",
		"uuid":                    "org-uuid-789",
		"name":                    "Vlad's org",
	},
}

var realProfilePayloadMax = map[string]any{
	"account": map[string]any{
		"display_name":   "Vlad",
		"email":          "vlad@example.com",
		"full_name":      "Vlad Max",
		"has_claude_max": true,
		"has_claude_pro": false,
	},
	"organization": map[string]any{
		"organization_type":   "claude_max",
		"rate_limit_tier":     "default_claude_max_20x",
		"subscription_status": "active",
	},
}

func TestExtractPlan(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		profile map[string]any
		want    string
	}{
		{"nil", nil, "unknown"},
		{"empty", map[string]any{}, "unknown"},
		{
			"claude_pro from organization_type",
			realProfilePayload,
			"claude_pro",
		},
		{
			"claude_max from organization_type",
			realProfilePayloadMax,
			"claude_max",
		},
		{
			"fallback has_claude_max",
			map[string]any{
				"account": map[string]any{"has_claude_max": true, "has_claude_pro": false},
			},
			"claude_max",
		},
		{
			"fallback has_claude_pro",
			map[string]any{
				"account": map[string]any{"has_claude_max": false, "has_claude_pro": true},
			},
			"claude_pro",
		},
		{
			"no organization_type and no flags",
			map[string]any{"account": map[string]any{}},
			"unknown",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ExtractPlan(tc.profile); got != tc.want {
				t.Errorf("ExtractPlan = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractRateLimitTier(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		profile map[string]any
		want    string
	}{
		{"nil", nil, ""},
		{"empty", map[string]any{}, ""},
		{
			"strips default_ prefix for claude_ai",
			realProfilePayload,
			"claude_ai",
		},
		{
			"strips default_ prefix for claude_max_20x",
			realProfilePayloadMax,
			"claude_max_20x",
		},
		{
			"no prefix present",
			map[string]any{
				"organization": map[string]any{"rate_limit_tier": "claude_pro"},
			},
			"claude_pro",
		},
		{
			"empty tier field",
			map[string]any{
				"organization": map[string]any{"rate_limit_tier": ""},
			},
			"",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ExtractRateLimitTier(tc.profile); got != tc.want {
				t.Errorf("ExtractRateLimitTier = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractSubscriptionStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		profile map[string]any
		want    string
	}{
		{"nil", nil, ""},
		{"empty", map[string]any{}, ""},
		{"active from real payload", realProfilePayload, "active"},
		{
			"missing field",
			map[string]any{"organization": map[string]any{}},
			"",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ExtractSubscriptionStatus(tc.profile); got != tc.want {
				t.Errorf("ExtractSubscriptionStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractDisplayName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		profile map[string]any
		want    string
	}{
		{"nil", nil, ""},
		{"empty", map[string]any{}, ""},
		{"display_name present", realProfilePayload, "Vlad"},
		{
			"falls back to full_name",
			map[string]any{
				"account": map[string]any{"full_name": "Full Name"},
			},
			"Full Name",
		},
		{
			"display_name wins over full_name",
			map[string]any{
				"account": map[string]any{"display_name": "Display", "full_name": "Full"},
			},
			"Display",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ExtractDisplayName(tc.profile); got != tc.want {
				t.Errorf("ExtractDisplayName = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractEmail(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		profile map[string]any
		want    string
	}{
		{"nil", nil, ""},
		{"empty", map[string]any{}, ""},
		{"email from real payload", realProfilePayload, "user@example.com"},
		{
			"missing account",
			map[string]any{"organization": map[string]any{}},
			"",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ExtractEmail(tc.profile); got != tc.want {
				t.Errorf("ExtractEmail = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestExtractTier_DelegatesToRateLimitTier verifies that ExtractTier now returns
// the normalised rate_limit_tier when present.
func TestExtractTier_DelegatesToRateLimitTier(t *testing.T) {
	t.Parallel()
	profile := map[string]any{
		"organization": map[string]any{
			"rate_limit_tier": "default_claude_max_20x",
		},
	}
	if got := ExtractTier(profile); got != "claude_max_20x" {
		t.Errorf("ExtractTier = %q, want claude_max_20x", got)
	}
}
