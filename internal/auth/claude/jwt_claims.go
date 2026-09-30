package claude

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	log "github.com/sirupsen/logrus"
)

// ParseSubscriptionTier decodes the payload segment of idToken (a JWT) without
// verifying the cryptographic signature — Anthropic does not publish its signing
// keys, and the token has already been validated by the OAuth server during
// issuance. Treat all parsing failures as non-fatal: the function always returns
// a non-empty tier string and never panics.
//
// Tier detection order (first match wins):
//  1. Top-level string claims: "subscription_tier", "tier", "plan", "subscription"
//  2. Nested under "organization" or "account" (same key names)
//  3. Scope-style tokens inside array claims "scope", "scopes", "permissions":
//     looks for "claude_pro", "claude_max_5x", "claude_max_20x", "claude_max" as
//     individual string elements
//
// Returns "unknown" with nil rawClaims when idToken is empty, malformed, or contains
// no recognisable tier information.
func ParseSubscriptionTier(idToken string) (tier string, rawClaims map[string]any) {
	idToken = strings.TrimSpace(idToken)
	if idToken == "" {
		return "unknown", nil
	}

	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		log.Warnf("jwt_claims: malformed id_token: expected 3 segments, got %d", len(parts))
		return "unknown", nil
	}

	payload, err := base64URLDecodeWithPadding(parts[1])
	if err != nil {
		log.Warnf("jwt_claims: failed to base64-decode id_token payload: %v", err)
		return "unknown", nil
	}

	var claims map[string]any
	if err = json.Unmarshal(payload, &claims); err != nil {
		log.Warnf("jwt_claims: failed to unmarshal id_token payload: %v", err)
		return "unknown", nil
	}

	rawClaims = claims

	// 1. Top-level string fields.
	for _, key := range []string{"subscription_tier", "tier", "plan", "subscription"} {
		if t := stringFromMap(claims, key); t != "" {
			return t, rawClaims
		}
	}

	// 2. Nested under "organization" or "account".
	for _, parentKey := range []string{"organization", "account"} {
		if nested, ok := claims[parentKey].(map[string]any); ok {
			for _, key := range []string{"subscription_tier", "tier", "plan", "subscription"} {
				if t := stringFromMap(nested, key); t != "" {
					return t, rawClaims
				}
			}
		}
	}

	// 3. Scope-style arrays.
	knownTiers := []string{"claude_max_20x", "claude_max_5x", "claude_max", "claude_pro"}
	for _, arrayKey := range []string{"scope", "scopes", "permissions"} {
		if t := tierFromScopeField(claims[arrayKey], knownTiers); t != "" {
			return t, rawClaims
		}
	}

	return "unknown", rawClaims
}

// base64URLDecodeWithPadding decodes a base64url string, adding padding as needed.
func base64URLDecodeWithPadding(s string) ([]byte, error) {
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}
	return base64.URLEncoding.DecodeString(s)
}

// stringFromMap retrieves a string value from m under key, returning "" for
// missing or non-string values.
func stringFromMap(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, isStr := v.(string); isStr {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// tierFromScopeField looks for a known tier token inside a scope field that may be
// either a string (space-separated), []string, or []any.
func tierFromScopeField(raw any, knownTiers []string) string {
	if raw == nil {
		return ""
	}
	var tokens []string
	switch v := raw.(type) {
	case string:
		tokens = strings.Fields(v)
	case []string:
		tokens = v
	case []any:
		for _, elem := range v {
			if s, ok := elem.(string); ok {
				tokens = append(tokens, s)
			}
		}
	default:
		return ""
	}
	// Match longest/most-specific tier first (knownTiers is ordered most→least specific).
	for _, tier := range knownTiers {
		for _, token := range tokens {
			if strings.EqualFold(strings.TrimSpace(token), tier) {
				return tier
			}
		}
	}
	return ""
}
