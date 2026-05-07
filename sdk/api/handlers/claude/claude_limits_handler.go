package claude

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// claudeLimitsRateWindow is the JSON-serialisable form of a single rate-limit window.
// All numeric fields use pointers so that a value of 0 (quota exhausted) serialises
// as 0 rather than being omitted, while a truly absent field serialises as null.
type claudeLimitsRateWindow struct {
	Status    string `json:"status,omitempty"`
	Remaining *int64 `json:"remaining,omitempty"`
	ResetAt   *int64 `json:"reset_at,omitempty"`
}

// claudeLimitsRateLimits groups all three observed Anthropic rate-limit windows.
type claudeLimitsRateLimits struct {
	FiveHour   *claudeLimitsRateWindow `json:"five_hour,omitempty"`
	Weekly     *claudeLimitsRateWindow `json:"weekly,omitempty"`
	WeeklyOpus *claudeLimitsRateWindow `json:"weekly_opus,omitempty"`
}

// claudeLimitsQuota exposes credential quota state to the caller.
type claudeLimitsQuota struct {
	Exceeded      bool       `json:"exceeded"`
	NextRecoverAt *time.Time `json:"next_recover_at"`
}

// claudeLimitsCredential is one element in the /v1/claude/limits response array.
type claudeLimitsCredential struct {
	AuthID           string                  `json:"auth_id"`
	Email            string                  `json:"email,omitempty"`
	SubscriptionTier string                  `json:"subscription_tier"`
	RateLimits       *claudeLimitsRateLimits `json:"rate_limits"`
	Quota            claudeLimitsQuota       `json:"quota"`
	LastObservedAt   *time.Time              `json:"last_observed_at"`
}

// claudeLimitsResponse is the top-level JSON envelope for GET /v1/claude/limits.
type claudeLimitsResponse struct {
	Credentials []claudeLimitsCredential `json:"credentials"`
}

// ClaudeLimits handles GET /v1/claude/limits.
// It returns the last-observed Anthropic rate-limit window state and subscription
// tier for every Claude credential currently managed by the AuthManager.
//
// When no requests have flowed through the proxy for a credential yet,
// rate_limits and last_observed_at are both null — the endpoint never
// synthesises probe calls.
func (h *ClaudeCodeAPIHandler) ClaudeLimits(c *gin.Context) {
	auths := h.AuthManager.List()

	creds := make([]claudeLimitsCredential, 0, len(auths))
	for _, a := range auths {
		if a == nil {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(a.Provider), "claude") {
			continue
		}

		cred := buildClaudeLimitsCredential(a)
		creds = append(creds, cred)
	}

	c.JSON(http.StatusOK, claudeLimitsResponse{Credentials: creds})
}

// buildClaudeLimitsCredential converts a live Auth snapshot into the response DTO.
func buildClaudeLimitsCredential(a *coreauth.Auth) claudeLimitsCredential {
	cred := claudeLimitsCredential{
		AuthID:           a.ID,
		SubscriptionTier: "unknown",
	}

	// Extract email from metadata (standard OAuth pattern).
	if a.Metadata != nil {
		if v, ok := a.Metadata["email"].(string); ok {
			cred.Email = strings.TrimSpace(v)
		}
		if tier, ok := a.Metadata["subscription_tier"].(string); ok && tier != "" {
			cred.SubscriptionTier = tier
		}
	}

	// Quota state.
	cred.Quota = claudeLimitsQuota{
		Exceeded: a.Quota.Exceeded,
	}
	if !a.Quota.NextRecoverAt.IsZero() {
		t := a.Quota.NextRecoverAt.UTC()
		cred.Quota.NextRecoverAt = &t
	}

	// Rate-limit snapshot — nil when no observation has been stored yet.
	snap, hasSnap := coreauth.LoadClaudeLimits(a)
	if hasSnap {
		cred.RateLimits = snapshotToRateLimits(snap)
		t := snap.LastObservedAt.UTC()
		cred.LastObservedAt = &t
	}

	return cred
}

// snapshotToRateLimits converts the internal ClaudeLimitsSnapshot to the DTO shape.
func snapshotToRateLimits(snap coreauth.ClaudeLimitsSnapshot) *claudeLimitsRateLimits {
	rl := &claudeLimitsRateLimits{}
	if snap.FiveHour != nil {
		rl.FiveHour = windowToDTO(snap.FiveHour)
	}
	if snap.Weekly != nil {
		rl.Weekly = windowToDTO(snap.Weekly)
	}
	if snap.WeeklyOpus != nil {
		rl.WeeklyOpus = windowToDTO(snap.WeeklyOpus)
	}
	// Return nil when all windows are absent (empty snapshot).
	if rl.FiveHour == nil && rl.Weekly == nil && rl.WeeklyOpus == nil {
		return nil
	}
	return rl
}

// windowToDTO converts a ClaudeRateWindow to its handler DTO equivalent.
func windowToDTO(w *coreauth.ClaudeRateWindow) *claudeLimitsRateWindow {
	if w == nil {
		return nil
	}
	return &claudeLimitsRateWindow{
		Status:    w.Status,
		Remaining: w.Remaining,
		ResetAt:   w.ResetAt,
	}
}
