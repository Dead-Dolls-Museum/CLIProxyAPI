package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AnthropicProfileURL is the OAuth-authenticated profile endpoint exposed by
// Anthropic. It returns the caller's plan, organisation, and current quota
// usage. The same endpoint is used by the web management UI's "Quota
// Management" panel.
const AnthropicProfileURL = "https://api.anthropic.com/api/oauth/profile"

// AnthropicOAuthBetaHeader is the value Anthropic expects for the
// `anthropic-beta` header when calling OAuth-only endpoints.
const AnthropicOAuthBetaHeader = "oauth-2025-04-20"

// DefaultProfileTimeout is the default per-call timeout used when the caller
// does not supply its own *http.Client.
const DefaultProfileTimeout = 5 * time.Second

// FetchProfile fetches Anthropic's OAuth profile for the supplied access
// token and returns the decoded JSON body as a generic map.
//
// Anthropic does not publicly document the response schema; callers should
// use ExtractTier (and any future helpers) for defensive field access rather
// than relying on a fixed struct.
//
// A nil client falls back to a fresh http.Client with DefaultProfileTimeout.
func FetchProfile(ctx context.Context, accessToken string, client *http.Client) (map[string]any, error) {
	return fetchProfileFromURL(ctx, accessToken, client, AnthropicProfileURL)
}

// fetchProfileFromURL is the underlying implementation that allows tests to
// substitute the upstream URL.
func fetchProfileFromURL(ctx context.Context, accessToken string, client *http.Client, url string) (map[string]any, error) {
	if strings.TrimSpace(accessToken) == "" {
		return nil, fmt.Errorf("claude profile: access token is empty")
	}
	if client == nil {
		client = &http.Client{Timeout: DefaultProfileTimeout}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("claude profile: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-beta", AnthropicOAuthBetaHeader)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claude profile: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
		return nil, fmt.Errorf("claude profile: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("claude profile: decode: %w", err)
	}
	return out, nil
}

// ExtractTier walks a profile payload using common Anthropic claim shapes and
// returns a tier identifier such as "claude_pro", "claude_max_5x", or
// "claude_max_20x". Returns "unknown" when no recognisable tier is found.
//
// Anthropic's response schema is undocumented, so this function tries the
// most plausible field names: top-level scalars, common nested objects
// (organization/account/user), and scope-style arrays.
//
// As of the confirmed production payload shape, callers should prefer
// ExtractRateLimitTier which returns the normalised tier string directly from
// organization.rate_limit_tier.
func ExtractTier(profile map[string]any) string {
	// Prefer the live normalised value when available.
	if tier := ExtractRateLimitTier(profile); tier != "" {
		return tier
	}

	if profile == nil {
		return "unknown"
	}

	scalarKeys := []string{"subscription_tier", "tier", "plan", "subscription"}

	for _, k := range scalarKeys {
		if v := stringFromAny(profile[k]); v != "" {
			return v
		}
	}

	for _, parent := range []string{"organization", "account", "user", "billing"} {
		nested, ok := profile[parent].(map[string]any)
		if !ok {
			continue
		}
		for _, k := range scalarKeys {
			if v := stringFromAny(nested[k]); v != "" {
				return v
			}
		}
	}

	for _, k := range []string{"scope", "scopes", "permissions", "entitlements"} {
		if found := scanScopeArray(profile[k]); found != "" {
			return found
		}
	}

	return "unknown"
}

// ExtractPlan returns the account plan from a profile payload.
// It reads organization.organization_type first; when that is absent it falls
// back to account.has_claude_max / account.has_claude_pro boolean fields.
// Returns "unknown" when no plan can be determined.
func ExtractPlan(profile map[string]any) string {
	if profile == nil {
		return "unknown"
	}
	if org, ok := profile["organization"].(map[string]any); ok {
		if v := stringFromAny(org["organization_type"]); v != "" {
			return v
		}
	}
	// Fallback: boolean flags on account.
	if acct, ok := profile["account"].(map[string]any); ok {
		if boolFromAny(acct["has_claude_max"]) {
			return "claude_max"
		}
		if boolFromAny(acct["has_claude_pro"]) {
			return "claude_pro"
		}
	}
	return "unknown"
}

// ExtractRateLimitTier returns the rate-limit tier from a profile payload,
// stripping the leading "default_" prefix that Anthropic includes in the value
// (e.g. "default_claude_max_20x" → "claude_max_20x").
// Returns "" when the field is absent.
func ExtractRateLimitTier(profile map[string]any) string {
	if profile == nil {
		return ""
	}
	org, ok := profile["organization"].(map[string]any)
	if !ok {
		return ""
	}
	v := stringFromAny(org["rate_limit_tier"])
	if v == "" {
		return ""
	}
	return strings.TrimPrefix(v, "default_")
}

// ExtractSubscriptionStatus returns organization.subscription_status from the
// profile payload. Returns "" when absent.
func ExtractSubscriptionStatus(profile map[string]any) string {
	if profile == nil {
		return ""
	}
	org, ok := profile["organization"].(map[string]any)
	if !ok {
		return ""
	}
	return stringFromAny(org["subscription_status"])
}

// ExtractDisplayName returns the display name for the account. It prefers
// account.display_name and falls back to account.full_name.
func ExtractDisplayName(profile map[string]any) string {
	if profile == nil {
		return ""
	}
	acct, ok := profile["account"].(map[string]any)
	if !ok {
		return ""
	}
	if v := stringFromAny(acct["display_name"]); v != "" {
		return v
	}
	return stringFromAny(acct["full_name"])
}

// ExtractEmail returns account.email from the profile payload.
func ExtractEmail(profile map[string]any) string {
	if profile == nil {
		return ""
	}
	acct, ok := profile["account"].(map[string]any)
	if !ok {
		return ""
	}
	return stringFromAny(acct["email"])
}

// boolFromAny returns true when v is a boolean true value.
func boolFromAny(v any) bool {
	if v == nil {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

func stringFromAny(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// knownTierTokens are recognised tier identifiers, longest first so
// "claude_max_20x" is matched before the less specific "claude_max".
var knownTierTokens = []string{"claude_max_20x", "claude_max_5x", "claude_max", "claude_pro"}

func scanScopeArray(v any) string {
	switch arr := v.(type) {
	case []any:
		for _, item := range arr {
			if s, ok := item.(string); ok {
				if t := matchTierToken(s); t != "" {
					return t
				}
			}
		}
	case []string:
		for _, s := range arr {
			if t := matchTierToken(s); t != "" {
				return t
			}
		}
	case string:
		for _, s := range strings.Fields(arr) {
			if t := matchTierToken(s); t != "" {
				return t
			}
		}
	}
	return ""
}

func matchTierToken(s string) string {
	s = strings.TrimSpace(s)
	for _, t := range knownTierTokens {
		if strings.EqualFold(s, t) {
			return t
		}
	}
	return ""
}
