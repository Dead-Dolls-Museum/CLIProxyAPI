package auth

import (
	"net/http"
	"testing"
)

func TestParseClaudeRateLimits(t *testing.T) {
	t.Parallel()

	makeHeader := func(pairs ...string) http.Header {
		h := make(http.Header)
		for i := 0; i+1 < len(pairs); i += 2 {
			h.Set(pairs[i], pairs[i+1])
		}
		return h
	}

	int64ptr := func(v int64) *int64 { return &v }

	tests := []struct {
		name       string
		header     http.Header
		wantAny    bool
		wantFiveH  *ClaudeRateWindow
		wantWeekly *ClaudeRateWindow
		wantOpus   *ClaudeRateWindow
	}{
		{
			name: "all three windows present",
			header: makeHeader(
				"anthropic-ratelimit-unified-5h-status", "allowed",
				"anthropic-ratelimit-unified-5h-remaining", "32145",
				"anthropic-ratelimit-unified-5h-reset", "1746630000",
				"anthropic-ratelimit-unified-weekly-status", "allowed",
				"anthropic-ratelimit-unified-weekly-remaining", "412300",
				"anthropic-ratelimit-unified-weekly-reset", "1747120000",
				"anthropic-ratelimit-unified-opus-weekly-status", "allowed",
				"anthropic-ratelimit-unified-opus-weekly-remaining", "12000",
				"anthropic-ratelimit-unified-opus-weekly-reset", "1747120000",
			),
			wantAny: true,
			wantFiveH: &ClaudeRateWindow{
				Status:    "allowed",
				Remaining: int64ptr(32145),
				ResetAt:   int64ptr(1746630000),
			},
			wantWeekly: &ClaudeRateWindow{
				Status:    "allowed",
				Remaining: int64ptr(412300),
				ResetAt:   int64ptr(1747120000),
			},
			wantOpus: &ClaudeRateWindow{
				Status:    "allowed",
				Remaining: int64ptr(12000),
				ResetAt:   int64ptr(1747120000),
			},
		},
		{
			name: "only 5h window present",
			header: makeHeader(
				"anthropic-ratelimit-unified-5h-status", "allowed_warning",
				"anthropic-ratelimit-unified-5h-remaining", "100",
				"anthropic-ratelimit-unified-5h-reset", "1746630000",
			),
			wantAny: true,
			wantFiveH: &ClaudeRateWindow{
				Status:    "allowed_warning",
				Remaining: int64ptr(100),
				ResetAt:   int64ptr(1746630000),
			},
			wantWeekly: nil,
			wantOpus:   nil,
		},
		{
			name: "missing remaining within a window",
			header: makeHeader(
				"anthropic-ratelimit-unified-5h-status", "rejected",
				"anthropic-ratelimit-unified-5h-reset", "1746630000",
			),
			wantAny: true,
			wantFiveH: &ClaudeRateWindow{
				Status:    "rejected",
				Remaining: nil, // absent
				ResetAt:   int64ptr(1746630000),
			},
		},
		{
			name: "malformed integer for remaining — that field skipped, others retained",
			header: makeHeader(
				"anthropic-ratelimit-unified-5h-status", "allowed",
				"anthropic-ratelimit-unified-5h-remaining", "not-a-number",
				"anthropic-ratelimit-unified-5h-reset", "1746630000",
			),
			wantAny: true,
			wantFiveH: &ClaudeRateWindow{
				Status:    "allowed",
				Remaining: nil, // skipped due to parse error
				ResetAt:   int64ptr(1746630000),
			},
		},
		{
			name:    "empty headers — bool false",
			header:  make(http.Header),
			wantAny: false,
		},
		{
			name: "zero remaining is preserved (not elided)",
			header: makeHeader(
				"anthropic-ratelimit-unified-5h-status", "rejected",
				"anthropic-ratelimit-unified-5h-remaining", "0",
				"anthropic-ratelimit-unified-5h-reset", "1746630000",
			),
			wantAny: true,
			wantFiveH: &ClaudeRateWindow{
				Status:    "rejected",
				Remaining: int64ptr(0),
				ResetAt:   int64ptr(1746630000),
			},
		},
		{
			name: "http.Header canonicalises: mixed-case header names work",
			// http.Header.Set canonicalises names via textproto.CanonicalMIMEHeaderKey.
			// The canonical form of "anthropic-ratelimit-unified-5h-status" is
			// "Anthropic-Ratelimit-Unified-5h-Status" — note lowercase 'h' in '5h'.
			// ParseClaudeRateLimits uses h.Get() which relies on canonicalisation,
			// so direct map insertion must use the exact canonical key to be found.
			header: func() http.Header {
				h := make(http.Header)
				h["Anthropic-Ratelimit-Unified-5h-Status"] = []string{"allowed"}
				h["Anthropic-Ratelimit-Unified-5h-Remaining"] = []string{"500"}
				return h
			}(),
			wantAny: true,
			wantFiveH: &ClaudeRateWindow{
				Status:    "allowed",
				Remaining: int64ptr(500),
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, gotAny := ParseClaudeRateLimits(tc.header)
			if gotAny != tc.wantAny {
				t.Errorf("bool: got %v, want %v", gotAny, tc.wantAny)
			}
			assertWindow(t, "FiveHour", got.FiveHour, tc.wantFiveH)
			assertWindow(t, "Weekly", got.Weekly, tc.wantWeekly)
			assertWindow(t, "WeeklyOpus", got.WeeklyOpus, tc.wantOpus)
			if gotAny && got.LastObservedAt.IsZero() {
				t.Error("LastObservedAt should not be zero when any field was populated")
			}
		})
	}
}

func assertWindow(t *testing.T, name string, got, want *ClaudeRateWindow) {
	t.Helper()
	if want == nil && got == nil {
		return
	}
	if want == nil && got != nil {
		t.Errorf("%s: expected nil, got %+v", name, got)
		return
	}
	if want != nil && got == nil {
		t.Errorf("%s: expected %+v, got nil", name, want)
		return
	}
	if got.Status != want.Status {
		t.Errorf("%s.Status: got %q, want %q", name, got.Status, want.Status)
	}
	assertInt64Ptr(t, name+".Remaining", got.Remaining, want.Remaining)
	assertInt64Ptr(t, name+".ResetAt", got.ResetAt, want.ResetAt)
}

func assertInt64Ptr(t *testing.T, label string, got, want *int64) {
	t.Helper()
	if want == nil && got == nil {
		return
	}
	if want == nil && got != nil {
		t.Errorf("%s: expected nil, got %d", label, *got)
		return
	}
	if want != nil && got == nil {
		t.Errorf("%s: expected %d, got nil", label, *want)
		return
	}
	if *got != *want {
		t.Errorf("%s: got %d, want %d", label, *got, *want)
	}
}

func TestStoreAndLoadClaudeLimits(t *testing.T) {
	t.Parallel()

	a := &Auth{
		ID:       "test-auth",
		Provider: "claude",
		Metadata: make(map[string]any),
	}

	h := make(http.Header)
	h.Set("anthropic-ratelimit-unified-5h-status", "allowed")
	h.Set("anthropic-ratelimit-unified-5h-remaining", "42")
	h.Set("anthropic-ratelimit-unified-5h-reset", "1746630000")

	snap, ok := ParseClaudeRateLimits(h)
	if !ok {
		t.Fatal("expected ParseClaudeRateLimits to return true")
	}

	StoreClaudeLimits(a, snap)

	got, loaded := LoadClaudeLimits(a)
	if !loaded {
		t.Fatal("expected LoadClaudeLimits to return true")
	}
	if got.FiveHour == nil {
		t.Fatal("expected FiveHour to be non-nil after store/load")
	}
	if got.FiveHour.Status != "allowed" {
		t.Errorf("FiveHour.Status: got %q, want %q", got.FiveHour.Status, "allowed")
	}
	if got.FiveHour.Remaining == nil || *got.FiveHour.Remaining != 42 {
		t.Errorf("FiveHour.Remaining: got %v, want 42", got.FiveHour.Remaining)
	}
}

func TestLoadClaudeLimits_NoObservation(t *testing.T) {
	t.Parallel()

	a := &Auth{Provider: "claude", Metadata: nil}
	_, loaded := LoadClaudeLimits(a)
	if loaded {
		t.Error("expected loaded=false for auth with no metadata")
	}

	a2 := &Auth{Provider: "claude", Metadata: make(map[string]any)}
	_, loaded2 := LoadClaudeLimits(a2)
	if loaded2 {
		t.Error("expected loaded=false for auth with no claude_limits key")
	}
}
