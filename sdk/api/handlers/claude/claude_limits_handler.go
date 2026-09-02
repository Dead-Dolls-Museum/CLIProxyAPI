package claude

import (
	"context"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claude"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// --------------------------------------------------------------------------
// DTOs — target response shape
// --------------------------------------------------------------------------

// claudeLimitsWindow is the JSON-serialisable form of a single rate-limit
// window returned by Anthropic's /api/oauth/usage endpoint.
// Pointer fields must NOT carry omitempty — nil serialises as explicit JSON
// null, which is the desired behaviour (caller needs to distinguish "absent
// window" from "zero usage").
type claudeLimitsWindow struct {
	UtilizationPct float64    `json:"utilization_pct"`
	RemainingPct   float64    `json:"remaining_pct"`
	ResetsAt       *time.Time `json:"resets_at"`
}

// claudeLimitsExtraUsage is the extra-credit / pay-as-you-go block.
type claudeLimitsExtraUsage struct {
	IsEnabled        bool    `json:"is_enabled"`
	MonthlyLimit     float64 `json:"monthly_limit"`
	UsedCredits      float64 `json:"used_credits"`
	RemainingCredits float64 `json:"remaining_credits"`
	UtilizationPct   float64 `json:"utilization_pct"`
	Currency         string  `json:"currency"`
}

// claudeLimitsLimits groups the four canonical usage windows.
type claudeLimitsLimits struct {
	FiveHour       *claudeLimitsWindow `json:"five_hour"`
	SevenDay       *claudeLimitsWindow `json:"seven_day"`
	SevenDaySonnet *claudeLimitsWindow `json:"seven_day_sonnet"`
	SevenDayOpus   *claudeLimitsWindow `json:"seven_day_opus"`
}

// claudeLimitsQuota exposes credential quota state to the caller.
type claudeLimitsQuota struct {
	Exceeded      bool       `json:"exceeded"`
	NextRecoverAt *time.Time `json:"next_recover_at"`
}

// claudeLimitsCredential is one element in the /v1/claude/limits response.
type claudeLimitsCredential struct {
	AuthID             string                         `json:"auth_id"`
	Email              string                         `json:"email,omitempty"`
	DisplayName        string                         `json:"display_name,omitempty"`
	Plan               string                         `json:"plan"`
	RateLimitTier      string                         `json:"rate_limit_tier,omitempty"`
	SubscriptionStatus string                         `json:"subscription_status,omitempty"`
	Limits             claudeLimitsLimits             `json:"limits"`
	ExtraUsage         *claudeLimitsExtraUsage        `json:"extra_usage,omitempty"`
	OtherWindows       map[string]*claudeLimitsWindow `json:"other_windows,omitempty"`
	Quota              claudeLimitsQuota              `json:"quota"`
	FetchErrors        map[string]string              `json:"fetch_errors,omitempty"`
}

// claudeLimitsResponse is the top-level JSON envelope for GET
// /v1/claude/limits.
type claudeLimitsResponse struct {
	Credentials []claudeLimitsCredential `json:"credentials"`
}

// --------------------------------------------------------------------------
// Cache configuration
// --------------------------------------------------------------------------

// profileCacheTTL is how long a fetched Anthropic /api/oauth/profile payload
// is reused before a refresh is attempted on the next /v1/claude/limits call.
const profileCacheTTL = 60 * time.Second

// usageCacheTTL mirrors profileCacheTTL for the usage endpoint.
const usageCacheTTL = 60 * time.Second

// profileFetchTimeout is the total time budget for the parallel profile+usage
// fetch step.
const profileFetchTimeout = 8 * time.Second

// --------------------------------------------------------------------------
// Fetcher interfaces
// --------------------------------------------------------------------------

// profileFetcher is a function that fetches an Anthropic profile for the given
// access token.
type profileFetcher func(ctx context.Context, accessToken string) (map[string]any, error)

// usageFetcher is a function that fetches Anthropic usage data for the given
// access token.
type usageFetcher func(ctx context.Context, accessToken string) (*claudeauth.Usage, error)

// productionProfileFetcher is the live implementation wired in by
// NewClaudeCodeAPIHandler.
var productionProfileFetcher profileFetcher = func(ctx context.Context, accessToken string) (map[string]any, error) {
	return claudeauth.FetchProfile(ctx, accessToken, nil)
}

// productionUsageFetcher is the live implementation wired in by
// NewClaudeCodeAPIHandler.
var productionUsageFetcher usageFetcher = func(ctx context.Context, accessToken string) (*claudeauth.Usage, error) {
	return claudeauth.FetchUsage(ctx, accessToken, nil)
}

// --------------------------------------------------------------------------
// Per-handler caches
// --------------------------------------------------------------------------

type profileCacheEntry struct {
	fetchedAt time.Time
	tier      string
	raw       map[string]any
	lastErr   string
}

type usageCacheEntry struct {
	fetchedAt time.Time
	usage     *claudeauth.Usage
	lastErr   string
}

// claudeLimitsState holds per-handler cache state. Embedding it in the
// handler struct keeps concurrent tests fully isolated from one another.
type claudeLimitsState struct {
	profileCacheMu sync.Mutex
	profileCache   map[string]profileCacheEntry

	usageCacheMu sync.Mutex
	usageCache   map[string]usageCacheEntry

	fetchProfile profileFetcher
	fetchUsage   usageFetcher
}

func newClaudeLimitsState(pf profileFetcher, uf usageFetcher) *claudeLimitsState {
	return &claudeLimitsState{
		profileCache: map[string]profileCacheEntry{},
		usageCache:   map[string]usageCacheEntry{},
		fetchProfile: pf,
		fetchUsage:   uf,
	}
}

func (s *claudeLimitsState) lookupProfile(authID string) (profileCacheEntry, bool) {
	s.profileCacheMu.Lock()
	defer s.profileCacheMu.Unlock()
	entry, ok := s.profileCache[authID]
	if !ok || time.Since(entry.fetchedAt) >= profileCacheTTL {
		return profileCacheEntry{}, false
	}
	return entry, true
}

func (s *claudeLimitsState) storeProfile(authID string, entry profileCacheEntry) {
	s.profileCacheMu.Lock()
	defer s.profileCacheMu.Unlock()
	s.profileCache[authID] = entry
}

func (s *claudeLimitsState) lookupUsage(authID string) (usageCacheEntry, bool) {
	s.usageCacheMu.Lock()
	defer s.usageCacheMu.Unlock()
	entry, ok := s.usageCache[authID]
	if !ok || time.Since(entry.fetchedAt) >= usageCacheTTL {
		return usageCacheEntry{}, false
	}
	return entry, true
}

func (s *claudeLimitsState) storeUsage(authID string, entry usageCacheEntry) {
	s.usageCacheMu.Lock()
	defer s.usageCacheMu.Unlock()
	s.usageCache[authID] = entry
}

// --------------------------------------------------------------------------
// Handler — ClaudeLimits
// --------------------------------------------------------------------------

// ClaudeLimits handles GET /v1/claude/limits.
//
// For every Claude credential it fetches the Anthropic /api/oauth/profile and
// /api/oauth/usage endpoints in parallel (both with a 60s TTL cache) then
// builds the shaped response. Partial failures are surfaced per-credential in
// the fetch_errors map; the rest of the response is still returned.
func (h *ClaudeCodeAPIHandler) ClaudeLimits(c *gin.Context) {
	auths := h.AuthManager.List()

	claudeAuths := make([]*coreauth.Auth, 0, len(auths))
	for _, a := range auths {
		if a == nil {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(a.Provider), "claude") {
			continue
		}
		claudeAuths = append(claudeAuths, a)
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), profileFetchTimeout)
	defer cancel()

	profiles, usages := h.limitsState.fetchBothParallel(ctx, claudeAuths)

	creds := make([]claudeLimitsCredential, 0, len(claudeAuths))
	for _, a := range claudeAuths {
		creds = append(creds, buildCredential(a, profiles[a.ID], usages[a.ID]))
	}
	c.JSON(http.StatusOK, claudeLimitsResponse{Credentials: creds})
}

// fetchBothParallel fetches profile and usage for each credential in parallel,
// consulting the respective TTL caches first.
func (s *claudeLimitsState) fetchBothParallel(
	ctx context.Context,
	auths []*coreauth.Auth,
) (map[string]profileCacheEntry, map[string]usageCacheEntry) {
	profiles := make(map[string]profileCacheEntry, len(auths))
	usages := make(map[string]usageCacheEntry, len(auths))

	type pair struct {
		id      string
		profile profileCacheEntry
		usage   usageCacheEntry
	}

	results := make(chan pair, len(auths))
	var wg sync.WaitGroup

	for _, a := range auths {
		pCached, pOK := s.lookupProfile(a.ID)
		uCached, uOK := s.lookupUsage(a.ID)

		if pOK && uOK {
			profiles[a.ID] = pCached
			usages[a.ID] = uCached
			continue
		}

		token := claudeAccessToken(a)
		if token == "" {
			noTok := profileCacheEntry{lastErr: "no access token in metadata"}
			noTokU := usageCacheEntry{lastErr: "no access token in metadata"}
			profiles[a.ID] = noTok
			usages[a.ID] = noTokU
			continue
		}

		wg.Add(1)
		go func(authID, accessToken string, pCachedOK bool, pEntry profileCacheEntry, uCachedOK bool, uEntry usageCacheEntry) {
			defer wg.Done()

			var innerWg sync.WaitGroup
			var mu sync.Mutex
			var pResult profileCacheEntry
			var uResult usageCacheEntry

			if pCachedOK {
				pResult = pEntry
			} else {
				innerWg.Add(1)
				go func() {
					defer innerWg.Done()
					prof, err := s.fetchProfile(ctx, accessToken)
					entry := profileCacheEntry{fetchedAt: time.Now()}
					if err != nil {
						log.WithField("auth_id", authID).WithError(err).Debug("claude profile fetch failed")
						entry.lastErr = err.Error()
					} else {
						entry.raw = prof
						entry.tier = claudeauth.ExtractTier(prof)
					}
					s.storeProfile(authID, entry)
					mu.Lock()
					pResult = entry
					mu.Unlock()
				}()
			}

			if uCachedOK {
				uResult = uEntry
			} else {
				innerWg.Add(1)
				go func() {
					defer innerWg.Done()
					usage, err := s.fetchUsage(ctx, accessToken)
					entry := usageCacheEntry{fetchedAt: time.Now()}
					if err != nil {
						log.WithField("auth_id", authID).WithError(err).Debug("claude usage fetch failed")
						entry.lastErr = err.Error()
					} else {
						entry.usage = usage
					}
					s.storeUsage(authID, entry)
					mu.Lock()
					uResult = entry
					mu.Unlock()
				}()
			}

			innerWg.Wait()
			mu.Lock()
			p := pResult
			u := uResult
			mu.Unlock()
			results <- pair{id: authID, profile: p, usage: u}
		}(a.ID, token, pOK, pCached, uOK, uCached)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	for r := range results {
		profiles[r.id] = r.profile
		usages[r.id] = r.usage
	}

	return profiles, usages
}

// claudeAccessToken returns the OAuth access token stored against a Claude
// credential.
func claudeAccessToken(a *coreauth.Auth) string {
	if a == nil || a.Metadata == nil {
		return ""
	}
	if v, ok := a.Metadata["access_token"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// --------------------------------------------------------------------------
// DTO construction
// --------------------------------------------------------------------------

func buildCredential(a *coreauth.Auth, profile profileCacheEntry, usage usageCacheEntry) claudeLimitsCredential {
	cred := claudeLimitsCredential{
		AuthID: a.ID,
		Plan:   "unknown",
	}

	// Extract fields from metadata fallback first.
	if a.Metadata != nil {
		if v, ok := a.Metadata["email"].(string); ok {
			cred.Email = strings.TrimSpace(v)
		}
		if v, ok := a.Metadata["display_name"].(string); ok && v != "" {
			cred.DisplayName = strings.TrimSpace(v)
		}
	}

	// Override with live profile data when available.
	if profile.raw != nil {
		if v := claudeauth.ExtractEmail(profile.raw); v != "" {
			cred.Email = v
		}
		if v := claudeauth.ExtractDisplayName(profile.raw); v != "" {
			cred.DisplayName = v
		}
		if v := claudeauth.ExtractPlan(profile.raw); v != "" {
			cred.Plan = v
		}
		cred.RateLimitTier = claudeauth.ExtractRateLimitTier(profile.raw)
		cred.SubscriptionStatus = claudeauth.ExtractSubscriptionStatus(profile.raw)
	}

	// Build limits from live usage data.
	if usage.usage != nil {
		u := usage.usage
		cred.Limits = claudeLimitsLimits{
			FiveHour:       usageWindowToDTO(u.FiveHour),
			SevenDay:       usageWindowToDTO(u.SevenDay),
			SevenDaySonnet: usageWindowToDTO(u.SevenDaySonnet),
			SevenDayOpus:   usageWindowToDTO(u.SevenDayOpus),
		}
		if u.ExtraUsage != nil {
			remaining := math.Max(0, u.ExtraUsage.MonthlyLimit-u.ExtraUsage.UsedCredits)
			cred.ExtraUsage = &claudeLimitsExtraUsage{
				IsEnabled:        u.ExtraUsage.IsEnabled,
				MonthlyLimit:     u.ExtraUsage.MonthlyLimit,
				UsedCredits:      u.ExtraUsage.UsedCredits,
				RemainingCredits: remaining,
				UtilizationPct:   u.ExtraUsage.UtilizationPct,
				Currency:         u.ExtraUsage.Currency,
			}
		}
		if len(u.OtherWindows) > 0 {
			cred.OtherWindows = make(map[string]*claudeLimitsWindow, len(u.OtherWindows))
			for k, w := range u.OtherWindows {
				cred.OtherWindows[k] = usageWindowToDTO(w)
			}
		}
	}

	// Quota state from the proxy's own cooldown tracking.
	cred.Quota = claudeLimitsQuota{Exceeded: a.Quota.Exceeded}
	if !a.Quota.NextRecoverAt.IsZero() {
		t := a.Quota.NextRecoverAt.UTC()
		cred.Quota.NextRecoverAt = &t
	}

	// Populate fetch_errors only when at least one fetch failed.
	errs := map[string]string{}
	if profile.lastErr != "" {
		errs["profile"] = profile.lastErr
	}
	if usage.lastErr != "" {
		errs["usage"] = usage.lastErr
	}
	if len(errs) > 0 {
		cred.FetchErrors = errs
	}

	return cred
}

// usageWindowToDTO converts a claudeauth.UsageWindow to the response DTO.
// Returns nil when the input is nil (upstream returned JSON null for that
// window).
func usageWindowToDTO(w *claudeauth.UsageWindow) *claudeLimitsWindow {
	if w == nil {
		return nil
	}
	return &claudeLimitsWindow{
		UtilizationPct: w.Utilization,
		RemainingPct:   math.Max(0, 100-w.Utilization),
		ResetsAt:       w.ResetsAt,
	}
}
