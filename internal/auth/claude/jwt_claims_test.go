package claude

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// buildJWT constructs a minimal unsigned JWT with the given payload map.
func buildJWT(t *testing.T, payload map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("buildJWT: marshal payload: %v", err)
	}
	payloadEncoded := base64.RawURLEncoding.EncodeToString(payloadBytes)
	return header + "." + payloadEncoded + ".fakesig"
}

func TestParseSubscriptionTier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		idToken    string
		wantTier   string
		wantClaims bool // true when we expect non-nil rawClaims
	}{
		{
			name:       "valid JWT with subscription_tier claim",
			idToken:    buildJWT(t, map[string]any{"subscription_tier": "claude_max_5x", "sub": "u123"}),
			wantTier:   "claude_max_5x",
			wantClaims: true,
		},
		{
			name:       "valid JWT with tier claim",
			idToken:    buildJWT(t, map[string]any{"tier": "claude_pro"}),
			wantTier:   "claude_pro",
			wantClaims: true,
		},
		{
			name:       "valid JWT with plan claim",
			idToken:    buildJWT(t, map[string]any{"plan": "claude_max_20x"}),
			wantTier:   "claude_max_20x",
			wantClaims: true,
		},
		{
			name: "valid JWT with subscription_tier nested under organization",
			idToken: buildJWT(t, map[string]any{
				"organization": map[string]any{"subscription_tier": "claude_max_5x"},
			}),
			wantTier:   "claude_max_5x",
			wantClaims: true,
		},
		{
			name: "valid JWT with subscription_tier nested under account",
			idToken: buildJWT(t, map[string]any{
				"account": map[string]any{"tier": "claude_pro"},
			}),
			wantTier:   "claude_pro",
			wantClaims: true,
		},
		{
			name: "valid JWT with claude_max_5x in scope array",
			idToken: buildJWT(t, map[string]any{
				"scope": []string{"openid", "profile", "claude_max_5x"},
			}),
			wantTier:   "claude_max_5x",
			wantClaims: true,
		},
		{
			name: "valid JWT with claude_pro in scope array",
			idToken: buildJWT(t, map[string]any{
				"scope": []string{"openid", "claude_pro"},
			}),
			wantTier:   "claude_pro",
			wantClaims: true,
		},
		{
			name: "valid JWT with claude_max_20x in scopes string field",
			idToken: buildJWT(t, map[string]any{
				"scopes": "openid email claude_max_20x",
			}),
			wantTier:   "claude_max_20x",
			wantClaims: true,
		},
		{
			name: "valid JWT with permissions []any containing tier",
			idToken: buildJWT(t, map[string]any{
				"permissions": []any{"read", "claude_pro", "write"},
			}),
			wantTier:   "claude_pro",
			wantClaims: true,
		},
		{
			name:       "valid JWT with no recognisable tier → unknown",
			idToken:    buildJWT(t, map[string]any{"sub": "u123", "email": "user@example.com"}),
			wantTier:   "unknown",
			wantClaims: true,
		},
		{
			name:       "empty string → unknown, nil claims",
			idToken:    "",
			wantTier:   "unknown",
			wantClaims: false,
		},
		{
			name:       "one segment → unknown",
			idToken:    "onlyone",
			wantTier:   "unknown",
			wantClaims: false,
		},
		{
			name:       "two segments → unknown",
			idToken:    "header.payload",
			wantTier:   "unknown",
			wantClaims: false,
		},
		{
			name:       "four segments → unknown",
			idToken:    "a.b.c.d",
			wantTier:   "unknown",
			wantClaims: false,
		},
		{
			name:       "garbage base64 payload → unknown",
			idToken:    "header.!!not_valid_base64!!.sig",
			wantTier:   "unknown",
			wantClaims: false,
		},
		{
			name: "case-insensitive scope matching: CLAUDE_PRO",
			idToken: buildJWT(t, map[string]any{
				"scope": []string{"CLAUDE_PRO"},
			}),
			wantTier:   "claude_pro",
			wantClaims: true,
		},
		{
			name: "most-specific scope wins: claude_max_20x beats claude_max",
			idToken: buildJWT(t, map[string]any{
				"scope": []string{"claude_max", "claude_max_20x"},
			}),
			wantTier:   "claude_max_20x",
			wantClaims: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tier, rawClaims := ParseSubscriptionTier(tc.idToken)
			if tier != tc.wantTier {
				t.Errorf("tier: got %q, want %q", tier, tc.wantTier)
			}
			if tc.wantClaims && rawClaims == nil {
				t.Error("rawClaims: expected non-nil, got nil")
			}
			if !tc.wantClaims && rawClaims != nil {
				t.Errorf("rawClaims: expected nil, got %v", rawClaims)
			}
		})
	}
}

// TestParseSubscriptionTier_NoPanic verifies that no input causes a panic.
func TestParseSubscriptionTier_NoPanic(t *testing.T) {
	t.Parallel()
	inputs := []string{
		"",
		".",
		"..",
		"...",
		"a.b.c",
		strings.Repeat("x", 10000),
		"header." + base64.RawURLEncoding.EncodeToString([]byte("{bad json")) + ".sig",
	}
	for _, input := range inputs {
		input := input
		t.Run("no_panic_"+input[:min(len(input), 20)], func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("ParseSubscriptionTier panicked: %v", r)
				}
			}()
			ParseSubscriptionTier(input)
		})
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
