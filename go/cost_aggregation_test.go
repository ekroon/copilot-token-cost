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

func TestBuildAllDerivedStatsAppliesMultiplier(t *testing.T) {
	setTestPricing(t)

	raw := map[string]map[string]map[string]*dbModelStats{
		"2025-01-02": {
			"/proj/alpha": {
				"model-a": {
					APICalls:         3,
					UserTurns:        2,
					PromptTokens:     100,
					CompletionTokens: 50,
				},
			},
		},
		"2024-06-01": {
			"/proj/alpha": {
				"model-a": {
					APICalls:         1,
					UserTurns:        1,
					PromptTokens:     40,
					CompletionTokens: 10,
				},
			},
		},
	}

	derived := buildAllDerivedStats(raw)

	// model-a has multiplier 2.5 in 2025 and 1.0 in 2024
	// 2025-01-02: 2 user turns × 2.5 = 5.0 premium requests
	// 2024-06-01: 1 user turn  × 1.0 = 1.0 premium requests
	wantPremiumTotal := 6.0

	// DailyStats
	if !almostEqual(derived.DailyStats["2025-01-02"]["model-a"].PremiumRequests, 5.0) {
		t.Fatalf("daily 2025-01-02 premium=%f, want=5.0", derived.DailyStats["2025-01-02"]["model-a"].PremiumRequests)
	}
	if !almostEqual(derived.DailyStats["2024-06-01"]["model-a"].PremiumRequests, 1.0) {
		t.Fatalf("daily 2024-06-01 premium=%f, want=1.0", derived.DailyStats["2024-06-01"]["model-a"].PremiumRequests)
	}

	// ModelStats
	if !almostEqual(derived.ModelStats["model-a"].PremiumRequests, wantPremiumTotal) {
		t.Fatalf("model premium=%f, want=%f", derived.ModelStats["model-a"].PremiumRequests, wantPremiumTotal)
	}

	// ProjectStats
	proj := projectName("/proj/alpha")
	if !almostEqual(derived.ProjectStats[proj].PremiumRequests, wantPremiumTotal) {
		t.Fatalf("project premium=%f, want=%f", derived.ProjectStats[proj].PremiumRequests, wantPremiumTotal)
	}

	// ProjectModelStats
	if !almostEqual(derived.ProjectModelStats[proj]["model-a"].PremiumRequests, wantPremiumTotal) {
		t.Fatalf("project-model premium=%f, want=%f", derived.ProjectModelStats[proj]["model-a"].PremiumRequests, wantPremiumTotal)
	}

	// TotalRecords
	if derived.TotalRecords != 4 {
		t.Fatalf("total records=%d, want=4", derived.TotalRecords)
	}
}

func TestAllViewsPremiumConsistency(t *testing.T) {
	setTestPricing(t)

	raw := map[string]map[string]map[string]*dbModelStats{
		"2025-01-02": {
			"/proj/alpha": {
				"model-a": {APICalls: 5, UserTurns: 3, PromptTokens: 300, CompletionTokens: 100},
			},
			"/proj/beta": {
				"model-a": {APICalls: 2, UserTurns: 1, PromptTokens: 100, CompletionTokens: 30},
			},
		},
		"2024-06-01": {
			"/proj/alpha": {
				"model-a": {APICalls: 1, UserTurns: 1, PromptTokens: 50, CompletionTokens: 10},
			},
		},
	}

	derived := buildAllDerivedStats(raw)
	payload := buildStatsPayload(aggregatedStats{
		DailyStats:             derived.DailyStats,
		ModelStats:             derived.ModelStats,
		ProjectStats:           derived.ProjectStats,
		ProjectModelStats:      derived.ProjectModelStats,
		ProjectDailyModelStats: derived.ProjectDailyModelStats,
		Records:                []Record{},
		SessionWorkspaces:      map[string]workspaceMeta{},
		TotalRecords:           derived.TotalRecords,
	}, "test", "", 0)

	// Sum premium from all views — they must agree
	var modelPrem, dailyPrem, projectPrem, projectModelPrem, dailyProjectPrem float64

	for _, s := range payload.Models {
		modelPrem += s.PremiumRequests
	}
	for _, dayMap := range payload.Daily {
		for k, v := range dayMap {
			if s, ok := v.(statsPayloadStats); ok && k[0] != '_' {
				dailyPrem += s.PremiumRequests
			}
		}
	}
	for _, s := range payload.Projects {
		projectPrem += s.PremiumRequests
	}
	for _, models := range payload.ProjectModels {
		for _, s := range models {
			projectModelPrem += s.PremiumRequests
		}
	}
	for _, projects := range payload.DailyProjects {
		for _, s := range projects {
			dailyProjectPrem += s.PremiumRequests
		}
	}

	// model-a multiplier: 2.5 on 2025-01-02, 1.0 on 2024-06-01
	// 2025-01-02: (3+1) user turns × 2.5 = 10.0
	// 2024-06-01: 1 user turn × 1.0 = 1.0
	wantTotal := 11.0

	if !almostEqual(modelPrem, wantTotal) {
		t.Fatalf("model premium=%f, want=%f", modelPrem, wantTotal)
	}
	if !almostEqual(dailyPrem, wantTotal) {
		t.Fatalf("daily premium=%f, want=%f", dailyPrem, wantTotal)
	}
	if !almostEqual(projectPrem, wantTotal) {
		t.Fatalf("project premium=%f, want=%f", projectPrem, wantTotal)
	}
	if !almostEqual(projectModelPrem, wantTotal) {
		t.Fatalf("project-model premium=%f, want=%f", projectModelPrem, wantTotal)
	}
	if !almostEqual(dailyProjectPrem, wantTotal) {
		t.Fatalf("daily-project premium=%f, want=%f", dailyProjectPrem, wantTotal)
	}

	// Premium costs must also be consistent across views
	var modelPremCost, projectPremCost, projectModelPremCost float64
	for _, s := range payload.Models {
		modelPremCost += s.PremiumRequestCost
	}
	for _, s := range payload.Projects {
		projectPremCost += s.PremiumRequestCost
	}
	for _, models := range payload.ProjectModels {
		for _, s := range models {
			projectModelPremCost += s.PremiumRequestCost
		}
	}
	if !almostEqual(modelPremCost, projectPremCost) {
		t.Fatalf("model premium cost=%f != project premium cost=%f", modelPremCost, projectPremCost)
	}
	if !almostEqual(modelPremCost, projectModelPremCost) {
		t.Fatalf("model premium cost=%f != project-model premium cost=%f", modelPremCost, projectModelPremCost)
	}
}
