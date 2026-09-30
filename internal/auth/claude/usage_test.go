package claude

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// realUsagePayload mirrors the confirmed production response shape.
const realUsagePayload = `{
  "five_hour":          {"utilization": 3.0, "resets_at": "2026-05-07T21:00:01.396402+00:00"},
  "seven_day":          {"utilization": 6.0, "resets_at": "2026-05-14T01:00:00.396427+00:00"},
  "seven_day_sonnet":   {"utilization": 2.0, "resets_at": "2026-05-14T01:00:00.396437+00:00"},
  "seven_day_opus":     null,
  "seven_day_oauth_apps": null,
  "seven_day_cowork":   null,
  "seven_day_omelette": {"utilization": 0.0, "resets_at": null},
  "tangelo":            null,
  "iguana_necktie":     null,
  "omelette_promotional": null,
  "extra_usage": {
    "is_enabled": true,
    "monthly_limit": 5000,
    "used_credits": 566.0,
    "utilization": 11.32,
    "currency": "USD"
  }
}`

func TestParseUsageBody(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name             string
		body             string
		wantFiveHourU    float64
		wantSevenDayU    float64
		wantSonnetU      float64
		wantOpusNil      bool
		wantOtherKeys    []string
		wantExtraEnabled bool
		wantMonthly      float64
		wantUsed         float64
		wantUtilPct      float64
		wantCurrency     string
	}{
		{
			name:             "real production payload",
			body:             realUsagePayload,
			wantFiveHourU:    3.0,
			wantSevenDayU:    6.0,
			wantSonnetU:      2.0,
			wantOpusNil:      true,
			wantOtherKeys:    nil, // seven_day_omelette has utilization=0 but is non-null → collected
			wantExtraEnabled: true,
			wantMonthly:      5000,
			wantUsed:         566.0,
			wantUtilPct:      11.32,
			wantCurrency:     "USD",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u, err := parseUsageBody([]byte(tc.body))
			if err != nil {
				t.Fatalf("parseUsageBody error: %v", err)
			}

			if u.FiveHour == nil {
				t.Fatal("five_hour: expected non-nil")
			}
			if u.FiveHour.Utilization != tc.wantFiveHourU {
				t.Errorf("five_hour.utilization = %v, want %v", u.FiveHour.Utilization, tc.wantFiveHourU)
			}
			if u.FiveHour.ResetsAt == nil {
				t.Error("five_hour.resets_at: expected non-nil")
			}

			if u.SevenDay == nil {
				t.Fatal("seven_day: expected non-nil")
			}
			if u.SevenDay.Utilization != tc.wantSevenDayU {
				t.Errorf("seven_day.utilization = %v, want %v", u.SevenDay.Utilization, tc.wantSevenDayU)
			}

			if u.SevenDaySonnet == nil {
				t.Fatal("seven_day_sonnet: expected non-nil")
			}
			if u.SevenDaySonnet.Utilization != tc.wantSonnetU {
				t.Errorf("seven_day_sonnet.utilization = %v, want %v", u.SevenDaySonnet.Utilization, tc.wantSonnetU)
			}

			if tc.wantOpusNil && u.SevenDayOpus != nil {
				t.Errorf("seven_day_opus: expected nil, got %+v", u.SevenDayOpus)
			}

			// seven_day_omelette: {"utilization": 0.0, "resets_at": null} — non-null object → in OtherWindows
			if u.OtherWindows == nil {
				t.Error("other_windows: expected non-nil (seven_day_omelette is a non-null object)")
			} else {
				if _, ok := u.OtherWindows["seven_day_omelette"]; !ok {
					t.Error("other_windows: missing seven_day_omelette")
				}
				// null windows should NOT appear in OtherWindows
				for _, nullKey := range []string{"tangelo", "iguana_necktie", "omelette_promotional", "seven_day_oauth_apps", "seven_day_cowork"} {
					if _, ok := u.OtherWindows[nullKey]; ok {
						t.Errorf("other_windows: %s should not be present (was null)", nullKey)
					}
				}
			}

			if u.ExtraUsage == nil {
				t.Fatal("extra_usage: expected non-nil")
			}
			if u.ExtraUsage.IsEnabled != tc.wantExtraEnabled {
				t.Errorf("extra_usage.is_enabled = %v, want %v", u.ExtraUsage.IsEnabled, tc.wantExtraEnabled)
			}
			if u.ExtraUsage.MonthlyLimit != tc.wantMonthly {
				t.Errorf("extra_usage.monthly_limit = %v, want %v", u.ExtraUsage.MonthlyLimit, tc.wantMonthly)
			}
			if u.ExtraUsage.UsedCredits != tc.wantUsed {
				t.Errorf("extra_usage.used_credits = %v, want %v", u.ExtraUsage.UsedCredits, tc.wantUsed)
			}
			if u.ExtraUsage.UtilizationPct != tc.wantUtilPct {
				t.Errorf("extra_usage.utilization_pct = %v, want %v", u.ExtraUsage.UtilizationPct, tc.wantUtilPct)
			}
			if u.ExtraUsage.Currency != tc.wantCurrency {
				t.Errorf("extra_usage.currency = %q, want %q", u.ExtraUsage.Currency, tc.wantCurrency)
			}
		})
	}
}

func TestParseUsageBody_NullWindows(t *testing.T) {
	t.Parallel()
	body := `{
		"five_hour": null,
		"seven_day": null,
		"seven_day_sonnet": null,
		"seven_day_opus": null
	}`
	u, err := parseUsageBody([]byte(body))
	if err != nil {
		t.Fatalf("parseUsageBody error: %v", err)
	}
	if u.FiveHour != nil {
		t.Errorf("five_hour: expected nil, got %+v", u.FiveHour)
	}
	if u.SevenDay != nil {
		t.Errorf("seven_day: expected nil, got %+v", u.SevenDay)
	}
	if u.OtherWindows != nil {
		t.Errorf("other_windows: expected nil, got %v", u.OtherWindows)
	}
	if u.ExtraUsage != nil {
		t.Errorf("extra_usage: expected nil, got %+v", u.ExtraUsage)
	}
}

func TestParseUsageBody_MissingExtraUsage(t *testing.T) {
	t.Parallel()
	body := `{
		"five_hour": {"utilization": 10.0, "resets_at": "2026-05-07T21:00:00Z"},
		"seven_day": null,
		"seven_day_sonnet": null,
		"seven_day_opus": null
	}`
	u, err := parseUsageBody([]byte(body))
	if err != nil {
		t.Fatalf("parseUsageBody error: %v", err)
	}
	if u.FiveHour == nil {
		t.Fatal("five_hour: expected non-nil")
	}
	if u.FiveHour.Utilization != 10.0 {
		t.Errorf("five_hour.utilization = %v, want 10.0", u.FiveHour.Utilization)
	}
	if u.ExtraUsage != nil {
		t.Errorf("extra_usage: expected nil when absent from JSON")
	}
}

func TestParseUsageBody_IntegerUtilization(t *testing.T) {
	t.Parallel()
	// Verify integer JSON numbers are handled (not just float literals).
	body := `{"five_hour": {"utilization": 5, "resets_at": null}, "seven_day": null, "seven_day_sonnet": null, "seven_day_opus": null}`
	u, err := parseUsageBody([]byte(body))
	if err != nil {
		t.Fatalf("parseUsageBody error: %v", err)
	}
	if u.FiveHour == nil {
		t.Fatal("five_hour: expected non-nil")
	}
	if u.FiveHour.Utilization != 5.0 {
		t.Errorf("five_hour.utilization = %v, want 5.0", u.FiveHour.Utilization)
	}
	if u.FiveHour.ResetsAt != nil {
		t.Errorf("five_hour.resets_at: expected nil for JSON null, got %v", u.FiveHour.ResetsAt)
	}
}

func TestFetchUsage_HappyPath(t *testing.T) {
	t.Parallel()
	var gotAuth, gotBeta, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotBeta = r.Header.Get("anthropic-beta")
		gotCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(realUsagePayload))
	}))
	defer srv.Close()

	u, err := fetchUsageFromURL(context.Background(), "tok-abc", srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("FetchUsage error: %v", err)
	}

	if gotAuth != "Bearer tok-abc" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotBeta != AnthropicOAuthBetaHeader {
		t.Errorf("anthropic-beta = %q", gotBeta)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}

	if u.FiveHour == nil || u.FiveHour.Utilization != 3.0 {
		t.Errorf("five_hour.utilization = %v, want 3.0", u.FiveHour)
	}
	if u.ExtraUsage == nil || !u.ExtraUsage.IsEnabled {
		t.Error("extra_usage.is_enabled: expected true")
	}
}

func TestFetchUsage_401(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	_, err := fetchUsageFromURL(context.Background(), "bad-tok", srv.Client(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 error, got %v", err)
	}
}

func TestFetchUsage_MalformedJSON(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer srv.Close()

	_, err := fetchUsageFromURL(context.Background(), "tok", srv.Client(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("expected decode error, got %v", err)
	}
}

func TestFetchUsage_EmptyToken(t *testing.T) {
	t.Parallel()
	if _, err := FetchUsage(context.Background(), "  ", nil); err == nil {
		t.Fatal("expected error for empty access token")
	}
}

func TestConvertWindow_NullResetsAt(t *testing.T) {
	t.Parallel()
	body := `{"five_hour": {"utilization": 0.0, "resets_at": null}, "seven_day": null, "seven_day_sonnet": null, "seven_day_opus": null}`
	u, err := parseUsageBody([]byte(body))
	if err != nil {
		t.Fatalf("parseUsageBody error: %v", err)
	}
	if u.FiveHour == nil {
		t.Fatal("five_hour: expected non-nil")
	}
	if u.FiveHour.ResetsAt != nil {
		t.Errorf("resets_at: expected nil for JSON null, got %v", u.FiveHour.ResetsAt)
	}
}

func TestConvertWindow_ResetsAtRFC3339(t *testing.T) {
	t.Parallel()
	body := `{"five_hour": {"utilization": 1.0, "resets_at": "2026-05-07T21:00:01.396402+00:00"}, "seven_day": null, "seven_day_sonnet": null, "seven_day_opus": null}`
	u, err := parseUsageBody([]byte(body))
	if err != nil {
		t.Fatalf("parseUsageBody error: %v", err)
	}
	if u.FiveHour == nil || u.FiveHour.ResetsAt == nil {
		t.Fatal("five_hour.resets_at: expected non-nil")
	}
	want := time.Date(2026, 5, 7, 21, 0, 1, 0, time.UTC)
	// Nano-seconds differ; just check the truncated second.
	if u.FiveHour.ResetsAt.Truncate(time.Second) != want {
		t.Errorf("resets_at = %v, want ~%v", u.FiveHour.ResetsAt, want)
	}
	if u.FiveHour.ResetsAt.Location() != time.UTC {
		t.Errorf("resets_at location = %v, want UTC", u.FiveHour.ResetsAt.Location())
	}
}
