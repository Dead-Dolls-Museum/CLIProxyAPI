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

// AnthropicUsageURL is the OAuth-authenticated usage endpoint exposed by
// Anthropic. It returns per-window utilisation percentages and reset times.
const AnthropicUsageURL = "https://api.anthropic.com/api/oauth/usage"

// UsageWindow holds the utilisation data for a single rate-limit window.
// ResetsAt is a pointer so that a JSON null maps to nil (some windows have no
// reset time — e.g. promotional windows at 0% usage).
type UsageWindow struct {
	// Utilization is the percentage of the window's quota that has been
	// consumed (0–100).
	Utilization float64
	// ResetsAt is the UTC timestamp when the window resets. Nil when not
	// applicable or when upstream returned null.
	ResetsAt *time.Time
}

// ExtraUsage holds the pay-as-you-go / extra-credit usage data.
type ExtraUsage struct {
	IsEnabled      bool
	MonthlyLimit   float64
	UsedCredits    float64
	UtilizationPct float64
	Currency       string
}

// Usage is the parsed representation of Anthropic's /api/oauth/usage response.
type Usage struct {
	FiveHour       *UsageWindow
	SevenDay       *UsageWindow
	SevenDaySonnet *UsageWindow
	SevenDayOpus   *UsageWindow
	// OtherWindows collects any non-null windows that are not in the four
	// canonical fields above (e.g. seven_day_oauth_apps, tangelo, etc.).
	// Nil when all such windows were null.
	OtherWindows map[string]*UsageWindow
	ExtraUsage   *ExtraUsage
}

// FetchUsage fetches Anthropic's OAuth usage data for the supplied access
// token. A nil client falls back to a fresh http.Client with
// DefaultProfileTimeout.
func FetchUsage(ctx context.Context, accessToken string, client *http.Client) (*Usage, error) {
	return fetchUsageFromURL(ctx, accessToken, client, AnthropicUsageURL)
}

// fetchUsageFromURL is the underlying implementation that allows tests to
// substitute the upstream URL.
func fetchUsageFromURL(ctx context.Context, accessToken string, client *http.Client, url string) (*Usage, error) {
	if strings.TrimSpace(accessToken) == "" {
		return nil, fmt.Errorf("claude usage: access token is empty")
	}
	if client == nil {
		client = &http.Client{Timeout: DefaultProfileTimeout}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("claude usage: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-beta", AnthropicOAuthBetaHeader)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claude usage: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
		return nil, fmt.Errorf("claude usage: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("claude usage: read body: %w", err)
	}

	usage, err := parseUsageBody(raw)
	if err != nil {
		return nil, fmt.Errorf("claude usage: decode: %w", err)
	}
	return usage, nil
}

// rawUsageWindow is the on-wire shape of a single window entry. Utilization
// and ResetsAt may be null in the Anthropic response.
type rawUsageWindow struct {
	Utilization *json.Number `json:"utilization"`
	ResetsAt    *string      `json:"resets_at"`
}

type rawExtraUsage struct {
	IsEnabled    bool         `json:"is_enabled"`
	MonthlyLimit *json.Number `json:"monthly_limit"`
	UsedCredits  *json.Number `json:"used_credits"`
	Utilization  *json.Number `json:"utilization"`
	Currency     string       `json:"currency"`
}

// rawUsageBody captures the complete on-wire JSON shape. Every named window is
// decoded individually, and everything else is captured by the generic map so
// we never silently lose data.
type rawUsageBody struct {
	FiveHour       *rawUsageWindow `json:"five_hour"`
	SevenDay       *rawUsageWindow `json:"seven_day"`
	SevenDaySonnet *rawUsageWindow `json:"seven_day_sonnet"`
	SevenDayOpus   *rawUsageWindow `json:"seven_day_opus"`

	// Other windows — decoded explicitly so we can collect non-null ones.
	SevenDayOAuthApps   *rawUsageWindow `json:"seven_day_oauth_apps"`
	SevenDayCowork      *rawUsageWindow `json:"seven_day_cowork"`
	SevenDayOmelette    *rawUsageWindow `json:"seven_day_omelette"`
	Tangelo             *rawUsageWindow `json:"tangelo"`
	IguanaNecktie       *rawUsageWindow `json:"iguana_necktie"`
	OmelettePromotional *rawUsageWindow `json:"omelette_promotional"`

	ExtraUsage *rawExtraUsage `json:"extra_usage"`
}

func parseUsageBody(data []byte) (*Usage, error) {
	var raw rawUsageBody
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}

	out := &Usage{
		FiveHour:       convertWindow(raw.FiveHour),
		SevenDay:       convertWindow(raw.SevenDay),
		SevenDaySonnet: convertWindow(raw.SevenDaySonnet),
		SevenDayOpus:   convertWindow(raw.SevenDayOpus),
	}

	// Collect non-null "other" windows.
	others := map[string]*rawUsageWindow{
		"seven_day_oauth_apps": raw.SevenDayOAuthApps,
		"seven_day_cowork":     raw.SevenDayCowork,
		"seven_day_omelette":   raw.SevenDayOmelette,
		"tangelo":              raw.Tangelo,
		"iguana_necktie":       raw.IguanaNecktie,
		"omelette_promotional": raw.OmelettePromotional,
	}
	for k, v := range others {
		if v == nil {
			continue
		}
		w := convertWindow(v)
		if w == nil {
			continue
		}
		if out.OtherWindows == nil {
			out.OtherWindows = make(map[string]*UsageWindow)
		}
		out.OtherWindows[k] = w
	}

	if raw.ExtraUsage != nil {
		eu := &ExtraUsage{
			IsEnabled: raw.ExtraUsage.IsEnabled,
			Currency:  raw.ExtraUsage.Currency,
		}
		if raw.ExtraUsage.MonthlyLimit != nil {
			if v, err := raw.ExtraUsage.MonthlyLimit.Float64(); err == nil {
				eu.MonthlyLimit = v
			}
		}
		if raw.ExtraUsage.UsedCredits != nil {
			if v, err := raw.ExtraUsage.UsedCredits.Float64(); err == nil {
				eu.UsedCredits = v
			}
		}
		if raw.ExtraUsage.Utilization != nil {
			if v, err := raw.ExtraUsage.Utilization.Float64(); err == nil {
				eu.UtilizationPct = v
			}
		}
		out.ExtraUsage = eu
	}

	return out, nil
}

// convertWindow converts a rawUsageWindow to a UsageWindow. Returns nil when
// the input is nil (upstream JSON null).
func convertWindow(r *rawUsageWindow) *UsageWindow {
	if r == nil {
		return nil
	}
	w := &UsageWindow{}
	if r.Utilization != nil {
		if v, err := r.Utilization.Float64(); err == nil {
			w.Utilization = v
		}
	}
	if r.ResetsAt != nil {
		ts := strings.TrimSpace(*r.ResetsAt)
		if ts != "" {
			if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				utc := t.UTC()
				w.ResetsAt = &utc
			}
		}
	}
	return w
}
