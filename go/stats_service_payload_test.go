package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBuildStatsPayloadMatchesCurrentJSONMath(t *testing.T) {
	setTestPricing(t)

	aggregated := aggregatedStats{
		DailyStats: map[string]map[string]*Stats{
			"2025-01-02": {
				"model-a": {
					APICalls:            1,
					UserTurns:           1,
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
					UserTurns:           0,
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
				UserTurns:           1,
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
				UserTurns:           1,
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
		SessionWorkspaces: map[string]workspaceMeta{},
		TotalRecords:      2,
		LogFileCount:      7,
	}

	payload := buildStatsPayload(aggregated, "last 2 days", "2024-06-01 → 2025-01-02", 0)

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
	if model.UserTurns != 1 {
		t.Fatalf("model user_turns mismatch: %+v", model)
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
	if day1Model.UserTurns != 1 {
		t.Fatalf("day1 user_turns mismatch: %+v", day1Model)
	}
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
	if day2Model.UserTurns != 0 {
		t.Fatalf("day2 user_turns mismatch: %+v", day2Model)
	}
	assertFloatEqual(t, day2["_total_cost"].(float64), 0.0005)
	assertFloatEqual(t, day2["_total_cost_without_cache"].(float64), 0.0006)

	dayProject := payload.DailyProjects["2025-01-02"]["(unknown)"]
	if dayProject.PromptTokens != 100 || dayProject.CompletionTokens != 50 || dayProject.UserTurns != 0 {
		t.Fatalf("daily project day1 mismatch: %+v", dayProject)
	}
	assertFloatEqual(t, dayProject.Cost, 0.0017)
	assertFloatEqual(t, dayProject.CostWithoutCache, 0.0020)
	assertFloatEqual(t, dayProject.PremiumRequestCost, 0.00)

	project := payload.Projects["(unknown)"]
	if project.UserTurns != 1 {
		t.Fatalf("project user_turns mismatch: %+v", project)
	}
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
		SessionWorkspaces: map[string]workspaceMeta{},
	}, "all time", "", 0)
	if payload.DateRange != nil {
		t.Fatalf("expected nil date_range, got %v", *payload.DateRange)
	}
}

func TestBuildStatsPayloadHourlyBucketsRespectTimezoneOffset(t *testing.T) {
	setTestPricing(t)

	record := Record{
		Model:               "model-a",
		PromptTokens:        120,
		CompletionTokens:    30,
		CacheCreationTokens: 10,
		CacheReadTokens:     20,
		IsUserTurn:          true,
		Timestamp:           "2025-01-02T00:30:00",
	}
	payload := buildStatsPayload(aggregatedStats{
		DailyStats:        map[string]map[string]*Stats{},
		ModelStats:        map[string]*Stats{},
		ProjectStats:      map[string]*Stats{},
		Records:           []Record{record},
		SessionWorkspaces: map[string]workspaceMeta{},
	}, "all time", "", 60)

	if payload.ViewerTimezoneOffsetMin != 60 {
		t.Fatalf("viewer timezone offset=%d, want=60", payload.ViewerTimezoneOffsetMin)
	}
	if len(payload.Hourly) != 24 {
		t.Fatalf("hourly bucket count=%d, want=24", len(payload.Hourly))
	}
	if payload.Hourly["23"].APICalls != 1 {
		t.Fatalf("hourly[23] calls=%d, want=1", payload.Hourly["23"].APICalls)
	}
	if payload.Hourly["23"].UserTurns != 1 {
		t.Fatalf("hourly[23] user_turns=%d, want=1", payload.Hourly["23"].UserTurns)
	}
	if payload.Hourly["00"].APICalls != 0 {
		t.Fatalf("hourly[00] calls=%d, want=0", payload.Hourly["00"].APICalls)
	}

	expectedPremiumRequests := getPremiumMultiplier(record.Model, record.Timestamp)
	expectedPremiumCost := expectedPremiumRequests * getPremiumRequestCost(record.Timestamp)
	expectedStats := &Stats{
		PromptTokens:        record.PromptTokens,
		CompletionTokens:    record.CompletionTokens,
		CacheCreationTokens: record.CacheCreationTokens,
		CacheReadTokens:     record.CacheReadTokens,
	}
	expectedCost := calcCost(record.Model, expectedStats, record.Timestamp)
	expectedNoCache := calcCostNocache(record.Model, expectedStats, record.Timestamp)

	assertFloatEqual(t, payload.Hourly["23"].PremiumRequests, expectedPremiumRequests)
	assertFloatEqual(t, payload.Hourly["23"].PremiumRequestCost, roundN(expectedPremiumCost, 4))
	assertFloatEqual(t, payload.Hourly["23"].Cost, roundN(expectedCost, 4))
	assertFloatEqual(t, payload.Hourly["23"].CostWithoutCache, roundN(expectedNoCache, 4))
}

func TestBuildProjectStatsMapMergesCodespacesAndLocalWorkspacePaths(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home dir: %v", err)
	}
	localPath := filepath.Join(home, "develop", "graph-hopper")
	projectStats := buildProjectStatsMap(map[string]*dbModelStats{
		localPath: {
			APICalls:         2,
			UserTurns:        1,
			PromptTokens:     20,
			CompletionTokens: 4,
		},
		"/workspaces/graph-hopper": {
			APICalls:         3,
			UserTurns:        2,
			PromptTokens:     30,
			CompletionTokens: 6,
		},
	})
	wantProject := projectName(localPath)
	if len(projectStats) != 1 {
		t.Fatalf("project count=%d, want=1 (%q)", len(projectStats), wantProject)
	}
	got, ok := projectStats[wantProject]
	if !ok {
		t.Fatalf("missing merged project key %q in %#v", wantProject, projectStats)
	}
	if got.APICalls != 5 || got.UserTurns != 3 || got.PromptTokens != 50 || got.CompletionTokens != 10 {
		t.Fatalf("merged project stats=%+v, want calls=5 user_turns=3 prompt=50 completion=10", *got)
	}
}

func TestBuildStatsPayloadMergesCodespacesProjectCostsIntoLocalProject(t *testing.T) {
	setTestPricing(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home dir: %v", err)
	}
	localPath := filepath.Join(home, "develop", "graph-hopper")
	project := projectName(localPath)
	aggregated := aggregatedStats{
		DailyStats: map[string]map[string]*Stats{},
		ModelStats: map[string]*Stats{},
		ProjectStats: map[string]*Stats{
			project: {
				APICalls:         2,
				UserTurns:        2,
				PromptTokens:     20,
				CompletionTokens: 4,
				PremiumRequests:  2,
			},
		},
		ProjectModelStats: map[string]map[string]*Stats{
			project: {
				"model-a": {
					APICalls:         2,
					UserTurns:        2,
					PromptTokens:     20,
					CompletionTokens: 4,
					PremiumRequests:  2,
				},
			},
		},
		Records: []Record{
			{
				Model:            "model-a",
				PromptTokens:     10,
				CompletionTokens: 2,
				Timestamp:        "2025-01-02T10:00:00",
				SessionID:        "sid-local",
				Source:           "local",
			},
			{
				Model:            "model-a",
				PromptTokens:     10,
				CompletionTokens: 2,
				Timestamp:        "2025-01-02T10:01:00",
				SessionID:        "sid-codespace",
				Source:           "codespace:test",
			},
		},
		SessionWorkspaces: map[string]workspaceMeta{
			"local\x1fsid-local":              {CWD: localPath},
			"codespace:test\x1fsid-codespace": {CWD: "/workspaces/graph-hopper"},
		},
	}

	payload := buildStatsPayload(aggregated, "all time", "", 0)
	if _, ok := payload.Projects["/workspaces/graph-hopper"]; ok {
		t.Fatalf("unexpected separate codespaces project key in payload: %#v", payload.Projects)
	}
	projectStats, ok := payload.Projects[project]
	if !ok {
		t.Fatalf("missing merged project key %q in %#v", project, payload.Projects)
	}
	if projectStats.Cost <= 0 {
		t.Fatalf("merged project cost=%v, want >0", projectStats.Cost)
	}
	modelStats, ok := payload.ProjectModels[project]["model-a"]
	if !ok {
		t.Fatalf("missing merged project model stats for %q model-a: %#v", project, payload.ProjectModels)
	}
	if modelStats.Cost <= 0 {
		t.Fatalf("merged project model cost=%v, want >0", modelStats.Cost)
	}
}

func TestParseRecordTimestampEdgeCases(t *testing.T) {
	tcs := []struct {
		name      string
		timestamp string
		want      time.Time
		ok        bool
	}{
		{name: "empty", timestamp: "   ", ok: false},
		{name: "rfc3339 trimmed", timestamp: " 2025-01-02T03:04:05Z ", want: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC), ok: true},
		{name: "milliseconds no zone", timestamp: "2025-01-02T03:04:05.000", want: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC), ok: true},
		{name: "fallback with extra suffix", timestamp: "2025-01-02T03:04:05+02:00 extra", want: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC), ok: true},
		{name: "invalid short timestamp", timestamp: "2025-01-02T03:04", ok: false},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseRecordTimestamp(tc.timestamp)
			if ok != tc.ok {
				t.Fatalf("ok=%v, want=%v (time=%q)", ok, tc.ok, tc.timestamp)
			}
			if tc.ok && !got.Equal(tc.want) {
				t.Fatalf("parsed=%s, want=%s", got.Format(time.RFC3339Nano), tc.want.Format(time.RFC3339Nano))
			}
		})
	}
}

func TestWorkspacePathHelpersNormalizationAndCanonicalization(t *testing.T) {
	tcs := []struct {
		path           string
		wantBase       string
		wantCodespaces bool
	}{
		{path: "  /workspaces/my-repo/  ", wantBase: "my-repo", wantCodespaces: true},
		{path: "  \\workspaces\\my-repo\\  ", wantBase: "my-repo", wantCodespaces: true},
		{path: "  /Users/dev/my-repo/  ", wantBase: "my-repo", wantCodespaces: false},
		{path: "my-repo", wantBase: "", wantCodespaces: false},
		{path: "/workspaces/", wantBase: "workspaces", wantCodespaces: false},
	}

	for _, tc := range tcs {
		if got := workspacePathBaseName(tc.path); got != tc.wantBase {
			t.Fatalf("workspacePathBaseName(%q)=%q, want=%q", tc.path, got, tc.wantBase)
		}
		if got := isCodespacesWorkspacePath(tc.path); got != tc.wantCodespaces {
			t.Fatalf("isCodespacesWorkspacePath(%q)=%v, want=%v", tc.path, got, tc.wantCodespaces)
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home dir: %v", err)
	}
	localPath := filepath.Join(home, "develop", "graph-hopper")
	localByBase := buildLocalProjectByBase([]string{
		"  \\workspaces\\graph-hopper\\  ",
		localPath,
	})
	if got := canonicalProjectLabel("  \\workspaces\\graph-hopper\\  ", localByBase); got != projectName(localPath) {
		t.Fatalf("canonical project=%q, want=%q", got, projectName(localPath))
	}
}

func TestBuildStatsPayloadProjectCostsReconcileWithModelCosts(t *testing.T) {
	setTestPricing(t)

	day := "2025-01-02"
	project := "/workspaces/github"
	aggregated := aggregatedStats{
		DailyStats: map[string]map[string]*Stats{
			day: {
				"model-a": {
					APICalls:            2,
					PromptTokens:        1_100_000,
					CompletionTokens:    0,
					CacheCreationTokens: 0,
					CacheReadTokens:     500_000,
				},
			},
		},
		ModelStats: map[string]*Stats{
			"model-a": {
				APICalls:            2,
				PromptTokens:        1_100_000,
				CompletionTokens:    0,
				CacheCreationTokens: 0,
				CacheReadTokens:     500_000,
			},
		},
		ProjectStats: map[string]*Stats{
			project: {
				APICalls:            2,
				PromptTokens:        1_100_000,
				CompletionTokens:    0,
				CacheCreationTokens: 0,
				CacheReadTokens:     500_000,
			},
		},
		Records: []Record{
			{
				Model:               "model-a",
				PromptTokens:        100_000,
				CompletionTokens:    0,
				CacheCreationTokens: 0,
				CacheReadTokens:     500_000,
				Timestamp:           day + "T10:00:00",
				SessionID:           "sid-1",
				Source:              "codespace:test",
			},
			{
				Model:               "model-a",
				PromptTokens:        1_000_000,
				CompletionTokens:    0,
				CacheCreationTokens: 0,
				CacheReadTokens:     0,
				Timestamp:           day + "T10:01:00",
				SessionID:           "sid-1",
				Source:              "codespace:test",
			},
		},
		SessionWorkspaces: map[string]workspaceMeta{
			"codespace:test\x1fsid-1": {CWD: project},
		},
		TotalRecords: 2,
	}

	payload := buildStatsPayload(aggregated, "today", day+" → "+day, 0)

	dayModel, ok := payload.Daily[day]["model-a"].(statsPayloadStats)
	if !ok {
		t.Fatalf("daily model payload type mismatch: %T", payload.Daily[day]["model-a"])
	}
	dayProject := payload.DailyProjects[day][project]
	assertFloatEqual(t, dayProject.Cost, dayModel.Cost)

	projectTotals := payload.Projects[project]
	modelTotals := payload.Models["model-a"]
	assertFloatEqual(t, projectTotals.Cost, modelTotals.Cost)
	if projectTotals.PromptTokens != modelTotals.PromptTokens || projectTotals.CacheReadTokens != modelTotals.CacheReadTokens {
		t.Fatalf("project/model token totals mismatch: project=%+v model=%+v", projectTotals, modelTotals)
	}
}
