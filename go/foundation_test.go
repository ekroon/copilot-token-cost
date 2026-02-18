package main

import (
	"math"
	"testing"
)

func ensurePricingLoaded(t *testing.T) {
	t.Helper()
	if len(pricingPeriods) == 0 {
		loadPricing()
	}
	if len(pricingPeriods) == 0 {
		t.Fatal("pricingPeriods should not be empty after loadPricing")
	}
}

func assertFloatEqual(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestLoadPricing(t *testing.T) {
	pricingPeriods = nil
	loadPricing()
	if len(pricingPeriods) == 0 {
		t.Fatal("loadPricing did not load any pricing periods")
	}
	if pricingPeriods[0].EffectiveFrom == "" {
		t.Fatal("first pricing period effective_from should not be empty")
	}
}

func TestGetPeriod(t *testing.T) {
	ensurePricingLoaded(t)

	tests := []struct {
		name      string
		timestamp string
		wantDate  string
	}{
		{"empty timestamp uses newest", "", pricingPeriods[0].EffectiveFrom},
		{"date in top period", "2026-02-17T10:00:00", "2026-02-17"},
		{"date in middle period", "2026-02-06T01:02:03", "2026-02-05"},
		{"date before oldest uses oldest", "2020-01-01T00:00:00", pricingPeriods[len(pricingPeriods)-1].EffectiveFrom},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getPeriod(tt.timestamp)
			if got == nil {
				t.Fatal("getPeriod returned nil")
			}
			if got.EffectiveFrom != tt.wantDate {
				t.Fatalf("got %q, want %q", got.EffectiveFrom, tt.wantDate)
			}
		})
	}
}

func TestNormalizeModel(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain model unchanged", "gpt-5-mini", "gpt-5-mini"},
		{"strip capi prefix", "capi:claude-sonnet-4.6", "claude-sonnet-4.6"},
		{"strip sweagent capi prefix", "sweagent-capi:gpt-5.2", "gpt-5.2"},
		{"strip capi routing prefix", "capi-us-ptuc-ab12-ib-claude-sonnet-4.6", "claude-sonnet-4.6"},
		{"strip reasoning effort suffix", "claude-sonnet-4.6:defaultReasoningEffort=high", "claude-sonnet-4.6"},
		{"strip date stamp suffix", "gpt-5.1-2026-02-17", "gpt-5.1"},
		{"strip all patterns", "sweagent-capi:capi-us-ptuc-ab12-ib-gpt-5.1:defaultReasoningEffort=medium-2026-02-17", "gpt-5.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeModel(tt.input); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetPricing(t *testing.T) {
	ensurePricingLoaded(t)

	tests := []struct {
		name      string
		model     string
		timestamp string
		wantNil   bool
		wantInput float64
	}{
		{"exact model match", "claude-sonnet-4.6", "2026-02-17T00:00:00", false, 3.00},
		{"normalized model match", "capi:claude-sonnet-4.6:defaultReasoningEffort=high", "2026-02-17T00:00:00", false, 3.00},
		{"prefix model match", "claude-sonnet-4.6-extended", "2026-02-17T00:00:00", false, 3.00},
		{"unknown model", "not-a-real-model", "2026-02-17T00:00:00", true, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getPricing(tt.model, tt.timestamp)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("expected nil pricing, got %+v", *got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected pricing, got nil")
			}
			assertFloatEqual(t, got.Input, tt.wantInput)
		})
	}
}

func TestGetPremiumMultiplier(t *testing.T) {
	ensurePricingLoaded(t)

	tests := []struct {
		name      string
		model     string
		timestamp string
		want      float64
	}{
		{"exact model match", "gpt-5-mini", "2026-02-17T00:00:00", 0},
		{"normalized model match", "capi:gpt-5.1-codex-mini:defaultReasoningEffort=high", "2026-02-17T00:00:00", 0.33},
		{"prefix model match", "gemini-3-pro-preview-experimental", "2026-02-17T00:00:00", 1},
		{"unknown model defaults to one", "not-a-real-model", "2026-02-17T00:00:00", 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getPremiumMultiplier(tt.model, tt.timestamp)
			assertFloatEqual(t, got, tt.want)
		})
	}
}
