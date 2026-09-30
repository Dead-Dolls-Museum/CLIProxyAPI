package auth

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// ClaudeRateWindow captures a single Anthropic rate-limit window's observed state.
// Remaining is a pointer so that a value of 0 (quota exhausted) is preserved rather
// than omitted; nil means the field was absent from the response headers.
type ClaudeRateWindow struct {
	// Status is the string value of the window status header (e.g. "allowed", "rejected").
	Status string `json:"status,omitempty"`
	// Remaining is the remaining quota units for the window.
	Remaining *int64 `json:"remaining,omitempty"`
	// ResetAt is the Unix epoch second when the window resets.
	ResetAt *int64 `json:"reset_at,omitempty"`
}

// ClaudeLimitsSnapshot holds a point-in-time view of all observed rate-limit windows
// for a single Claude credential. It is stored under Auth.Metadata["claude_limits"].
type ClaudeLimitsSnapshot struct {
	// FiveHour contains the 5-hour rolling window state.
	FiveHour *ClaudeRateWindow `json:"five_hour,omitempty"`
	// Weekly contains the weekly window state.
	Weekly *ClaudeRateWindow `json:"weekly,omitempty"`
	// WeeklyOpus contains the opus-specific weekly window state.
	WeeklyOpus *ClaudeRateWindow `json:"weekly_opus,omitempty"`
	// LastObservedAt is the time this snapshot was captured.
	LastObservedAt time.Time `json:"last_observed_at"`
}

// anthropicWindowDef maps a window name to the canonical header base name used to
// derive the three sub-headers: -status, -remaining, -reset.
var anthropicWindowDefs = []struct {
	field    string
	baseName string
}{
	{"five_hour", "anthropic-ratelimit-unified-5h"},
	{"weekly", "anthropic-ratelimit-unified-weekly"},
	{"weekly_opus", "anthropic-ratelimit-unified-opus-weekly"},
}

// ParseClaudeRateLimits inspects h for Anthropic rate-limit headers and returns a
// populated ClaudeLimitsSnapshot. The bool return value is true when at least one
// field was successfully parsed; false means h contained no recognisable data.
//
// The function is tolerant: missing headers produce nil pointer fields; headers with
// non-numeric values for -remaining / -reset are logged at warn level and skipped.
// Unknown header variants are silently ignored.
func ParseClaudeRateLimits(h http.Header) (ClaudeLimitsSnapshot, bool) {
	snap := ClaudeLimitsSnapshot{LastObservedAt: time.Now().UTC()}
	anyPopulated := false

	for _, def := range anthropicWindowDefs {
		status := strings.TrimSpace(h.Get(def.baseName + "-status"))
		remainingStr := strings.TrimSpace(h.Get(def.baseName + "-remaining"))
		resetStr := strings.TrimSpace(h.Get(def.baseName + "-reset"))

		if status == "" && remainingStr == "" && resetStr == "" {
			continue
		}

		win := &ClaudeRateWindow{}
		populated := false

		if status != "" {
			win.Status = status
			populated = true
		}

		if remainingStr != "" {
			v, err := strconv.ParseInt(remainingStr, 10, 64)
			if err != nil {
				log.Warnf("claude_limits: could not parse %s-remaining %q: %v", def.baseName, remainingStr, err)
			} else {
				win.Remaining = &v
				populated = true
			}
		}

		if resetStr != "" {
			v, err := strconv.ParseInt(resetStr, 10, 64)
			if err != nil {
				log.Warnf("claude_limits: could not parse %s-reset %q: %v", def.baseName, resetStr, err)
			} else {
				win.ResetAt = &v
				populated = true
			}
		}

		if !populated {
			continue
		}

		anyPopulated = true
		switch def.field {
		case "five_hour":
			snap.FiveHour = win
		case "weekly":
			snap.Weekly = win
		case "weekly_opus":
			snap.WeeklyOpus = win
		}
	}

	return snap, anyPopulated
}

// StoreClaudeLimits persists snap into a.Metadata["claude_limits"] using the same
// locking pattern as the rest of the Auth mutation code: the caller must hold the
// manager mutex before calling this, or must pass a live *Auth already retrieved
// under that mutex.
//
// This function follows the direct-map-write pattern used by all other Metadata
// mutations in this package (e.g. the codex executor writing id_token at line 716
// of codex_executor.go) — the manager always holds m.mu.Lock() when mutating
// m.auths[id].Metadata entries.
func StoreClaudeLimits(a *Auth, snap ClaudeLimitsSnapshot) {
	if a == nil {
		return
	}
	if a.Metadata == nil {
		a.Metadata = make(map[string]any)
	}
	a.Metadata["claude_limits"] = snap
}

// observeClaudeHeaders parses Anthropic rate-limit headers from h and, when any are
// present, stores the resulting ClaudeLimitsSnapshot into the auth entry identified
// by authID. It is a no-op for non-Claude providers (provider != "claude") and when
// h contains no recognisable Anthropic rate-limit headers.
func (m *Manager) observeClaudeHeaders(authID string, h http.Header) {
	if m == nil || authID == "" || len(h) == 0 {
		return
	}
	snap, ok := ParseClaudeRateLimits(h)
	if !ok {
		return
	}
	m.mu.Lock()
	if auth, exists := m.auths[authID]; exists && auth != nil &&
		strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") {
		StoreClaudeLimits(auth, snap)
	}
	m.mu.Unlock()
}

// LoadClaudeLimits retrieves the last-observed ClaudeLimitsSnapshot from Auth metadata.
// It returns (snapshot, true) when a valid snapshot is present, and (zero, false) when
// no observation has been stored yet.
func LoadClaudeLimits(a *Auth) (ClaudeLimitsSnapshot, bool) {
	if a == nil || a.Metadata == nil {
		return ClaudeLimitsSnapshot{}, false
	}
	raw, ok := a.Metadata["claude_limits"]
	if !ok {
		return ClaudeLimitsSnapshot{}, false
	}
	switch v := raw.(type) {
	case ClaudeLimitsSnapshot:
		return v, true
	default:
		return ClaudeLimitsSnapshot{}, false
	}
}
