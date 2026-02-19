package main

import "database/sql"

type aggregatedStats struct {
	DailyStats        map[string]map[string]*Stats
	ModelStats        map[string]*Stats
	ProjectStats      map[string]*Stats
	ProjectModelStats map[string]map[string]*Stats
	Records           []Record
	SessionWorkspaces map[string]string
	TotalRecords      int
	LogFileCount      int
}

type statsPayloadStats struct {
	APICalls            int     `json:"api_calls"`
	PromptTokens        int     `json:"prompt_tokens"`
	CompletionTokens    int     `json:"completion_tokens"`
	CacheCreationTokens int     `json:"cache_creation_tokens"`
	CacheReadTokens     int     `json:"cache_read_tokens"`
	PremiumRequests     float64 `json:"premium_requests"`
	PremiumRequestCost  float64 `json:"premium_request_cost"`
	InputUncached       int     `json:"input_uncached_tokens"`
	Cost                float64 `json:"cost"`
	CostWithoutCache    float64 `json:"cost_without_cache"`
}

type syncSourceStatus struct {
	Code      string `json:"code"`
	Reason    string `json:"reason"`
	UpdatedAt string `json:"updated_at"`
}

type statsPayload struct {
	Period                  string                                  `json:"period"`
	DateRange               *string                                 `json:"date_range"`
	LogFiles                int                                     `json:"log_files"`
	APICalls                int                                     `json:"api_calls"`
	Models                  map[string]statsPayloadStats            `json:"models"`
	Daily                   map[string]map[string]interface{}       `json:"daily"`
	Projects                map[string]statsPayloadStats            `json:"projects"`
	TotalCost               float64                                 `json:"total_cost"`
	TotalCostNoCache        float64                                 `json:"total_cost_without_cache"`
	TotalPremiumRequestCost float64                                 `json:"total_premium_request_cost"`
	ProjectModels           map[string]map[string]statsPayloadStats `json:"project_models,omitempty"`
	SyncStatus              map[string]syncSourceStatus             `json:"sync_status,omitempty"`
}

func buildStatsPayload(aggregated aggregatedStats, periodLabel, dateRange string) statsPayload {
	dailyStats := aggregated.DailyStats
	modelStats := aggregated.ModelStats
	projectStats := aggregated.ProjectStats
	filtered := aggregated.Records
	sessionWorkspaces := aggregated.SessionWorkspaces

	out := statsPayload{
		Period:   periodLabel,
		LogFiles: aggregated.LogFileCount,
		APICalls: aggregated.TotalRecords,
		Models:   make(map[string]statsPayloadStats),
		Daily:    make(map[string]map[string]interface{}),
		Projects: make(map[string]statsPayloadStats),
	}
	if dateRange != "" {
		out.DateRange = &dateRange
	}

	models := sortedKeys(modelStats)
	for _, model := range models {
		s := modelStats[model]
		cost := sumDailyCost(model, dailyStats, calcCost)
		costNC := sumDailyCost(model, dailyStats, calcCostNocache)
		premCost := sumDailyPremCost(model, dailyStats)
		out.TotalCost += cost
		out.TotalCostNoCache += costNC
		out.Models[model] = statsPayloadStats{
			APICalls:            s.APICalls,
			PromptTokens:        s.PromptTokens,
			CompletionTokens:    s.CompletionTokens,
			CacheCreationTokens: s.CacheCreationTokens,
			CacheReadTokens:     s.CacheReadTokens,
			PremiumRequests:     s.PremiumRequests,
			PremiumRequestCost:  roundN(premCost, 4),
			InputUncached:       uncachedInput(s),
			Cost:                roundN(cost, 4),
			CostWithoutCache:    roundN(costNC, 4),
		}
		out.TotalPremiumRequestCost += premCost
	}

	daysKeys := sortedKeysStr(dailyStats)
	for _, day := range daysKeys {
		dayMap := make(map[string]interface{})
		var dayTotal, dayTotalNC float64
		for model, s := range dailyStats[day] {
			cost := calcCost(model, s, day)
			costNC := calcCostNocache(model, s, day)
			dayTotal += cost
			dayTotalNC += costNC
			dayMap[model] = statsPayloadStats{
				APICalls:            s.APICalls,
				PromptTokens:        s.PromptTokens,
				CompletionTokens:    s.CompletionTokens,
				CacheCreationTokens: s.CacheCreationTokens,
				CacheReadTokens:     s.CacheReadTokens,
				PremiumRequests:     s.PremiumRequests,
				PremiumRequestCost:  roundN(s.PremiumRequests*getPremiumRequestCost(day), 4),
				InputUncached:       uncachedInput(s),
				Cost:                roundN(cost, 4),
				CostWithoutCache:    roundN(costNC, 4),
			}
		}
		dayMap["_total_cost"] = roundN(dayTotal, 4)
		dayMap["_total_cost_without_cache"] = roundN(dayTotalNC, 4)
		out.Daily[day] = dayMap
	}

	projCosts := make(map[string][2]float64)
	projModelCosts := make(map[string]map[string][2]float64)
	for _, r := range filtered {
		cwd := ""
		if r.SessionID != "" {
			cwd = sessionWorkspaces[r.Source+"\x1f"+r.SessionID]
		}
		proj := "(unknown)"
		if cwd != "" {
			proj = projectName(cwd)
		}
		model := normalizeModel(r.Model)
		rs := &Stats{
			PromptTokens:        r.PromptTokens,
			CompletionTokens:    r.CompletionTokens,
			CacheCreationTokens: r.CacheCreationTokens,
			CacheReadTokens:     r.CacheReadTokens,
		}
		cost := calcCost(model, rs, r.Timestamp)
		costNC := calcCostNocache(model, rs, r.Timestamp)
		c := projCosts[proj]
		c[0] += cost
		c[1] += costNC
		projCosts[proj] = c
		if projModelCosts[proj] == nil {
			projModelCosts[proj] = make(map[string][2]float64)
		}
		mc := projModelCosts[proj][model]
		mc[0] += cost
		mc[1] += costNC
		projModelCosts[proj][model] = mc
	}

	for proj, s := range projectStats {
		pc := projCosts[proj]
		out.Projects[proj] = statsPayloadStats{
			APICalls:            s.APICalls,
			PromptTokens:        s.PromptTokens,
			CompletionTokens:    s.CompletionTokens,
			CacheCreationTokens: s.CacheCreationTokens,
			CacheReadTokens:     s.CacheReadTokens,
			PremiumRequests:     s.PremiumRequests,
			PremiumRequestCost:  roundN(s.PremiumRequests*getPremiumRequestCost(""), 4),
			InputUncached:       uncachedInput(s),
			Cost:                roundN(pc[0], 4),
			CostWithoutCache:    roundN(pc[1], 4),
		}
	}

	projectModelStats := aggregated.ProjectModelStats
	if len(projectModelStats) > 0 {
		out.ProjectModels = make(map[string]map[string]statsPayloadStats)
		for proj, models := range projectModelStats {
			out.ProjectModels[proj] = make(map[string]statsPayloadStats)
			pmc := projModelCosts[proj]
			for model, s := range models {
				mc := pmc[model]
				out.ProjectModels[proj][model] = statsPayloadStats{
					APICalls:            s.APICalls,
					PromptTokens:        s.PromptTokens,
					CompletionTokens:    s.CompletionTokens,
					CacheCreationTokens: s.CacheCreationTokens,
					CacheReadTokens:     s.CacheReadTokens,
					PremiumRequests:     s.PremiumRequests,
					PremiumRequestCost:  roundN(s.PremiumRequests*getPremiumRequestCost(""), 4),
					InputUncached:       uncachedInput(s),
					Cost:                roundN(mc[0], 4),
					CostWithoutCache:    roundN(mc[1], 4),
				}
			}
		}
	}

	out.TotalCost = roundN(out.TotalCost, 4)
	out.TotalCostNoCache = roundN(out.TotalCostNoCache, 4)
	out.TotalPremiumRequestCost = roundN(out.TotalPremiumRequestCost, 4)
	return out
}

func dbStatsToStats(dbs *dbModelStats, premiumRequests float64) *Stats {
	return &Stats{
		APICalls:            dbs.APICalls,
		PromptTokens:        dbs.PromptTokens,
		CompletionTokens:    dbs.CompletionTokens,
		CacheCreationTokens: dbs.CacheCreationTokens,
		CacheReadTokens:     dbs.CacheReadTokens,
		PremiumRequests:     premiumRequests,
	}
}

func mergeStats(dst, src *Stats) {
	dst.APICalls += src.APICalls
	dst.PromptTokens += src.PromptTokens
	dst.CompletionTokens += src.CompletionTokens
	dst.CacheCreationTokens += src.CacheCreationTokens
	dst.CacheReadTokens += src.CacheReadTokens
	dst.PremiumRequests += src.PremiumRequests
}

func buildDailyStatsMap(dbDailyStats map[string]map[string]*dbModelStats) map[string]map[string]*Stats {
	dailyStats := make(map[string]map[string]*Stats)
	for day, models := range dbDailyStats {
		dailyStats[day] = make(map[string]*Stats)
		for model, dbs := range models {
			dailyStats[day][model] = dbStatsToStats(dbs, float64(dbs.UserTurns)*getPremiumMultiplier(model, day))
		}
	}
	return dailyStats
}

func buildModelStatsMap(dbModelStatsMap map[string]*dbModelStats, dailyStats map[string]map[string]*Stats) map[string]*Stats {
	modelStats := make(map[string]*Stats)
	for model, dbs := range dbModelStatsMap {
		var premReqs float64
		for _, models := range dailyStats {
			if s, ok := models[model]; ok {
				premReqs += s.PremiumRequests
			}
		}
		modelStats[model] = dbStatsToStats(dbs, premReqs)
	}
	return modelStats
}

func buildProjectStatsMap(dbProjectStats map[string]*dbModelStats) map[string]*Stats {
	projectStats := make(map[string]*Stats)
	for cwd, dbs := range dbProjectStats {
		proj := "(unknown)"
		if cwd != "" {
			proj = projectName(cwd)
		}
		s := dbStatsToStats(dbs, float64(dbs.UserTurns))
		if existing, ok := projectStats[proj]; ok {
			mergeStats(existing, s)
		} else {
			projectStats[proj] = s
		}
	}
	return projectStats
}

func buildProjectModelStatsMap(dbProjectModelStats map[string]map[string]*dbModelStats) map[string]map[string]*Stats {
	result := make(map[string]map[string]*Stats)
	for cwd, models := range dbProjectModelStats {
		proj := "(unknown)"
		if cwd != "" {
			proj = projectName(cwd)
		}
		if result[proj] == nil {
			result[proj] = make(map[string]*Stats)
		}
		for model, dbs := range models {
			s := dbStatsToStats(dbs, float64(dbs.UserTurns))
			if existing, ok := result[proj][model]; ok {
				mergeStats(existing, s)
			} else {
				result[proj][model] = s
			}
		}
	}
	return result
}

func countTotalRecords(modelStats map[string]*Stats) int {
	totalRecords := 0
	for _, s := range modelStats {
		totalRecords += s.APICalls
	}
	return totalRecords
}

func loadAggregatedStats(db *sql.DB, dateFrom, dateTo, projectFilter string) aggregatedStats {
	dailyStats := buildDailyStatsMap(queryDailyStats(db, dateFrom, dateTo, projectFilter))
	modelStats := buildModelStatsMap(queryModelStats(db, dateFrom, dateTo, projectFilter), dailyStats)
	return aggregatedStats{
		DailyStats:        dailyStats,
		ModelStats:        modelStats,
		ProjectStats:      buildProjectStatsMap(queryProjectStats(db, dateFrom, dateTo, projectFilter)),
		ProjectModelStats: buildProjectModelStatsMap(queryProjectModelStats(db, dateFrom, dateTo, projectFilter)),
		Records:           queryRecords(db, dateFrom, dateTo, projectFilter),
		SessionWorkspaces: querySessionWorkspaces(db),
		TotalRecords:      countTotalRecords(modelStats),
		LogFileCount:      queryLogFileCount(db, dateFrom, dateTo, projectFilter),
	}
}
