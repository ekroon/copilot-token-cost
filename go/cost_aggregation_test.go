package main

import (
	"math"
	"testing"
)

func setTestPricing(t *testing.T) {
	t.Helper()
	original := pricingPeriods
	pricingPeriods = []PricingPeriod{
		{
			EffectiveFrom:      "2025-01-01",
			PremiumRequestCost: 0.05,
			ModelPricing: map[string]Pricing{
				"model-a": {Input: 10, Output: 20, CacheRead: 1, CacheWrite: 2},
			},
			PremiumMultiplier: map[string]float64{
				"model-a": 2.5,
			},
		},
		{
			EffectiveFrom:      "2024-01-01",
			PremiumRequestCost: 0.02,
			ModelPricing: map[string]Pricing{
				"model-a": {Input: 10, Output: 20, CacheRead: 1, CacheWrite: 2},
			},
			PremiumMultiplier: map[string]float64{
				"model-a": 1.0,
			},
		},
	}
	t.Cleanup(func() { pricingPeriods = original })
}

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-12
}

func TestStatsAddAggregatesAndPremiumRequests(t *testing.T) {
	setTestPricing(t)
	s := newStats()

	s.add(Record{
		PromptTokens:        100,
		CompletionTokens:    50,
		CacheCreationTokens: 10,
		CacheReadTokens:     20,
		IsUserTurn:          true,
		Timestamp:           "2025-01-02T10:00:00",
	}, "model-a")

	s.add(Record{
		PromptTokens:        5,
		CompletionTokens:    7,
		CacheCreationTokens: 2,
		CacheReadTokens:     3,
		IsUserTurn:          false,
		Timestamp:           "2025-01-02T10:01:00",
	}, "model-a")

	if s.APICalls != 2 {
		t.Fatalf("APICalls = %d, want 2", s.APICalls)
	}
	if s.PromptTokens != 105 || s.CompletionTokens != 57 || s.CacheCreationTokens != 12 || s.CacheReadTokens != 23 {
		t.Fatalf("unexpected token totals: %+v", s)
	}
	if !almostEqual(s.PremiumRequests, 2.5) {
		t.Fatalf("PremiumRequests = %.12f, want 2.5", s.PremiumRequests)
	}
}

func TestCalcCostAndCalcCostNocache(t *testing.T) {
	setTestPricing(t)
	s := &Stats{
		PromptTokens:        100,
		CompletionTokens:    50,
		CacheReadTokens:     30,
		CacheCreationTokens: 80,
	}

	gotCost := calcCost("model-a", s, "2025-01-02")
	wantCost := float64(50)/1e6*20 + float64(30)/1e6*1 + float64(80)/1e6*2
	if !almostEqual(gotCost, wantCost) {
		t.Fatalf("calcCost = %.12f, want %.12f", gotCost, wantCost)
	}

	gotNoCache := calcCostNocache("model-a", s, "2025-01-02")
	wantNoCache := float64(100)/1e6*10 + float64(50)/1e6*20
	if !almostEqual(gotNoCache, wantNoCache) {
		t.Fatalf("calcCostNocache = %.12f, want %.12f", gotNoCache, wantNoCache)
	}

	if calcCost("unknown-model", s, "2025-01-02") != 0 {
		t.Fatalf("calcCost with unknown model should be 0")
	}
	if calcCostNocache("unknown-model", s, "2025-01-02") != 0 {
		t.Fatalf("calcCostNocache with unknown model should be 0")
	}
}

func TestSumDailyCost(t *testing.T) {
	dailyStats := map[string]map[string]*Stats{
		"2025-01-01": {"model-a": {PromptTokens: 10}, "model-b": {PromptTokens: 999}},
		"2025-01-02": {"model-a": {PromptTokens: 5}},
	}

	got := sumDailyCost("model-a", dailyStats, func(_ string, s *Stats, _ string) float64 {
		return float64(s.PromptTokens)
	})
	if got != 15 {
		t.Fatalf("sumDailyCost = %.0f, want 15", got)
	}
}

func TestSumDailyPremCosts(t *testing.T) {
	setTestPricing(t)
	dailyStats := map[string]map[string]*Stats{
		"2025-01-02": {"model-a": {PremiumRequests: 2}, "model-b": {PremiumRequests: 3}},
		"2024-06-01": {"model-a": {PremiumRequests: 3}, "model-b": {PremiumRequests: 4}},
	}

	gotModel := sumDailyPremCost("model-a", dailyStats)
	wantModel := 2*0.05 + 3*0.02
	if !almostEqual(gotModel, wantModel) {
		t.Fatalf("sumDailyPremCost = %.12f, want %.12f", gotModel, wantModel)
	}

	gotAll := sumDailyPremCostAll(dailyStats)
	wantAll := (2+3)*0.05 + (3+4)*0.02
	if !almostEqual(gotAll, wantAll) {
		t.Fatalf("sumDailyPremCostAll = %.12f, want %.12f", gotAll, wantAll)
	}
}

func TestUncachedInput(t *testing.T) {
	if got := uncachedInput(&Stats{PromptTokens: 100, CacheReadTokens: 20, CacheCreationTokens: 10}); got != 70 {
		t.Fatalf("uncachedInput = %d, want 70", got)
	}
	if got := uncachedInput(&Stats{PromptTokens: 10, CacheReadTokens: 9, CacheCreationTokens: 5}); got != 0 {
		t.Fatalf("uncachedInput = %d, want 0", got)
	}
}
