package main

import "testing"

func TestBuildStatsPayloadMatchesCurrentJSONMath(t *testing.T) {
	setTestPricing(t)

	aggregated := aggregatedStats{
		DailyStats: map[string]map[string]*Stats{
			"2025-01-02": {
				"model-a": {
					APICalls:            1,
					PromptTokens:        100,
					CompletionTokens:    50,
					CacheCreationTokens: 10,
					CacheReadTokens:     20,
					PremiumRequests:     2,
				},
			},
			"2024-06-01": {
				"model-a": {
					APICalls:            1,
					PromptTokens:        40,
					CompletionTokens:    10,
					CacheCreationTokens: 5,
					CacheReadTokens:     5,
					PremiumRequests:     1,
				},
			},
		},
		ModelStats: map[string]*Stats{
			"model-a": {
				APICalls:            2,
				PromptTokens:        140,
				CompletionTokens:    60,
				CacheCreationTokens: 15,
				CacheReadTokens:     25,
				PremiumRequests:     3,
			},
		},
		ProjectStats: map[string]*Stats{
			"(unknown)": {
				APICalls:            2,
				PromptTokens:        140,
				CompletionTokens:    60,
				CacheCreationTokens: 15,
				CacheReadTokens:     25,
				PremiumRequests:     3,
			},
		},
		Records: []Record{
			{
				Model:               "model-a",
				PromptTokens:        100,
				CompletionTokens:    50,
				CacheCreationTokens: 10,
				CacheReadTokens:     20,
				Timestamp:           "2025-01-02T10:00:00",
			},
			{
				Model:               "model-a",
				PromptTokens:        40,
				CompletionTokens:    10,
				CacheCreationTokens: 5,
				CacheReadTokens:     5,
				Timestamp:           "2024-06-01T12:00:00",
			},
		},
		SessionWorkspaces: map[string]string{},
		TotalRecords:      2,
		LogFileCount:      7,
	}

	payload := buildStatsPayload(aggregated, "last 2 days", "2024-06-01 → 2025-01-02")

	if payload.Period != "last 2 days" {
		t.Fatalf("period=%q", payload.Period)
	}
	if payload.DateRange == nil || *payload.DateRange != "2024-06-01 → 2025-01-02" {
		t.Fatalf("date_range=%v", payload.DateRange)
	}
	if payload.LogFiles != 7 || payload.APICalls != 2 {
		t.Fatalf("totals mismatch: log_files=%d api_calls=%d", payload.LogFiles, payload.APICalls)
	}

	model := payload.Models["model-a"]
	if model.APICalls != 2 || model.PromptTokens != 140 || model.CompletionTokens != 60 {
		t.Fatalf("model totals mismatch: %+v", model)
	}
	if model.CacheCreationTokens != 15 || model.CacheReadTokens != 25 || model.InputUncached != 100 {
		t.Fatalf("model cache totals mismatch: %+v", model)
	}
	assertFloatEqual(t, model.PremiumRequests, 3)
	assertFloatEqual(t, model.PremiumRequestCost, 0.12)
	assertFloatEqual(t, model.Cost, 0.0023)
	assertFloatEqual(t, model.CostWithoutCache, 0.0026)

	day1 := payload.Daily["2025-01-02"]
	day1Model, ok := day1["model-a"].(statsPayloadStats)
	if !ok {
		t.Fatalf("daily model payload type mismatch: %T", day1["model-a"])
	}
	assertFloatEqual(t, day1Model.Cost, 0.0017)
	assertFloatEqual(t, day1Model.CostWithoutCache, 0.0020)
	assertFloatEqual(t, day1Model.PremiumRequestCost, 0.10)
	assertFloatEqual(t, day1["_total_cost"].(float64), 0.0017)
	assertFloatEqual(t, day1["_total_cost_without_cache"].(float64), 0.0020)

	day2 := payload.Daily["2024-06-01"]
	day2Model, ok := day2["model-a"].(statsPayloadStats)
	if !ok {
		t.Fatalf("daily model payload type mismatch: %T", day2["model-a"])
	}
	assertFloatEqual(t, day2Model.Cost, 0.0005)
	assertFloatEqual(t, day2Model.CostWithoutCache, 0.0006)
	assertFloatEqual(t, day2Model.PremiumRequestCost, 0.02)
	assertFloatEqual(t, day2["_total_cost"].(float64), 0.0005)
	assertFloatEqual(t, day2["_total_cost_without_cache"].(float64), 0.0006)

	project := payload.Projects["(unknown)"]
	assertFloatEqual(t, project.Cost, 0.0023)
	assertFloatEqual(t, project.CostWithoutCache, 0.0026)
	assertFloatEqual(t, project.PremiumRequestCost, 0.15)

	assertFloatEqual(t, payload.TotalCost, 0.0023)
	assertFloatEqual(t, payload.TotalCostNoCache, 0.0026)
	assertFloatEqual(t, payload.TotalPremiumRequestCost, 0.12)
}

func TestBuildStatsPayloadOmitsDateRangeWhenEmpty(t *testing.T) {
	payload := buildStatsPayload(aggregatedStats{
		DailyStats:        map[string]map[string]*Stats{},
		ModelStats:        map[string]*Stats{},
		ProjectStats:      map[string]*Stats{},
		Records:           []Record{},
		SessionWorkspaces: map[string]string{},
	}, "all time", "")
	if payload.DateRange != nil {
		t.Fatalf("expected nil date_range, got %v", *payload.DateRange)
	}
}
