package main

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

type aggregatedStats struct {
	DailyStats             map[string]map[string]*Stats
	ModelStats             map[string]*Stats
	ProjectStats           map[string]*Stats
	ProjectModelStats      map[string]map[string]*Stats
	ProjectDailyModelStats map[string]map[string]map[string]*Stats
	Records                []Record
	SessionWorkspaces      map[string]workspaceMeta
	TotalRecords           int
	LogFileCount           int
}

type statsPayloadStats struct {
	APICalls            int     `json:"api_calls"`
	UserTurns           int     `json:"user_turns,omitempty"`
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
	Hourly                  map[string]statsPayloadStats            `json:"hourly"`
	ViewerTimezoneOffsetMin int                                     `json:"viewer_timezone_offset_minutes"`
	Projects                map[string]statsPayloadStats            `json:"projects"`
	DailyProjects           map[string]map[string]statsPayloadStats `json:"daily_projects,omitempty"`
	TotalCost               float64                                 `json:"total_cost"`
	TotalCostNoCache        float64                                 `json:"total_cost_without_cache"`
	TotalPremiumRequestCost float64                                 `json:"total_premium_request_cost"`
	ProjectModels           map[string]map[string]statsPayloadStats `json:"project_models,omitempty"`
	SyncStatus              map[string]syncSourceStatus             `json:"sync_status,omitempty"`
}

func normalizeTimezoneOffsetMinutes(offsetMinutes int) int {
	const maxOffsetMinutes = 14 * 60
	if offsetMinutes > maxOffsetMinutes {
		return maxOffsetMinutes
	}
	if offsetMinutes < -maxOffsetMinutes {
		return -maxOffsetMinutes
	}
	return offsetMinutes
}

func parseRecordTimestamp(timestamp string) (time.Time, bool) {
	ts := strings.TrimSpace(timestamp)
	if ts == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02T15:04:05.000",
	} {
		if parsed, err := time.Parse(layout, ts); err == nil {
			return parsed, true
		}
	}
	if len(ts) >= 19 {
		if parsed, err := time.Parse("2006-01-02T15:04:05", ts[:19]); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

type hourlyCostTotals struct {
	cost               float64
	costWithoutCache   float64
	premiumRequestCost float64
}

func buildHourlyStatsMap(records []Record, viewerTimezoneOffsetMin int) map[string]statsPayloadStats {
	hourly := make(map[string]*Stats, 24)
	hourlyCosts := make(map[string]hourlyCostTotals, 24)
	for hour := 0; hour < 24; hour++ {
		hourly[fmt.Sprintf("%02d", hour)] = &Stats{}
	}

	shift := -time.Duration(normalizeTimezoneOffsetMinutes(viewerTimezoneOffsetMin)) * time.Minute
	for _, r := range records {
		parsed, ok := parseRecordTimestamp(r.Timestamp)
		if !ok {
			continue
		}
		hour := parsed.Add(shift).Format("15")
		model := normalizeModel(r.Model)
		s := hourly[hour]
		s.APICalls++
		s.PromptTokens += r.PromptTokens
		s.CompletionTokens += r.CompletionTokens
		s.CacheCreationTokens += r.CacheCreationTokens
		s.CacheReadTokens += r.CacheReadTokens

		premiumRequests := 0.0
		if r.IsUserTurn {
			s.UserTurns++
			premiumRequests = getPremiumMultiplier(model, r.Timestamp)
			s.PremiumRequests += premiumRequests
		}

		rs := &Stats{
			PromptTokens:        r.PromptTokens,
			CompletionTokens:    r.CompletionTokens,
			CacheCreationTokens: r.CacheCreationTokens,
			CacheReadTokens:     r.CacheReadTokens,
		}
		costs := hourlyCosts[hour]
		costs.cost += calcCost(model, rs, r.Timestamp)
		costs.costWithoutCache += calcCostNocache(model, rs, r.Timestamp)
		costs.premiumRequestCost += premiumRequests * getPremiumRequestCost(r.Timestamp)
		hourlyCosts[hour] = costs
	}

	out := make(map[string]statsPayloadStats, 24)
	for hour := 0; hour < 24; hour++ {
		key := fmt.Sprintf("%02d", hour)
		s := hourly[key]
		costs := hourlyCosts[key]
		out[key] = statsPayloadStats{
			APICalls:            s.APICalls,
			UserTurns:           s.UserTurns,
			PromptTokens:        s.PromptTokens,
			CompletionTokens:    s.CompletionTokens,
			CacheCreationTokens: s.CacheCreationTokens,
			CacheReadTokens:     s.CacheReadTokens,
			PremiumRequests:     s.PremiumRequests,
			PremiumRequestCost:  roundN(costs.premiumRequestCost, 4),
			InputUncached:       uncachedInput(s),
			Cost:                roundN(costs.cost, 4),
			CostWithoutCache:    roundN(costs.costWithoutCache, 4),
		}
	}
	return out
}

func buildStatsPayload(aggregated aggregatedStats, periodLabel, dateRange string, viewerTimezoneOffsetMin int) statsPayload {
	dailyStats := aggregated.DailyStats
	modelStats := aggregated.ModelStats
	projectStats := aggregated.ProjectStats
	projectModelStats := aggregated.ProjectModelStats
	projectDailyModelStats := aggregated.ProjectDailyModelStats
	filtered := aggregated.Records
	viewerTimezoneOffsetMin = normalizeTimezoneOffsetMinutes(viewerTimezoneOffsetMin)

	out := statsPayload{
		Period:                  periodLabel,
		LogFiles:                aggregated.LogFileCount,
		APICalls:                aggregated.TotalRecords,
		Models:                  make(map[string]statsPayloadStats),
		Daily:                   make(map[string]map[string]interface{}),
		Hourly:                  buildHourlyStatsMap(filtered, viewerTimezoneOffsetMin),
		ViewerTimezoneOffsetMin: viewerTimezoneOffsetMin,
		Projects:                make(map[string]statsPayloadStats),
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
			UserTurns:           s.UserTurns,
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
				UserTurns:           s.UserTurns,
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

	// Compute project and project-model costs from pre-computed daily-project-model stats
	projectCostTotals := make(map[string][2]float64)
	projectModelCostTotals := make(map[string]map[string][2]float64)
	for day, projects := range projectDailyModelStats {
		for project, models := range projects {
			if _, ok := projectModelCostTotals[project]; !ok {
				projectModelCostTotals[project] = make(map[string][2]float64)
			}
			for model, stats := range models {
				cost := calcCost(model, stats, day)
				costNC := calcCostNocache(model, stats, day)
				projectCost := projectCostTotals[project]
				projectCost[0] += cost
				projectCost[1] += costNC
				projectCostTotals[project] = projectCost
				modelCost := projectModelCostTotals[project][model]
				modelCost[0] += cost
				modelCost[1] += costNC
				projectModelCostTotals[project][model] = modelCost
			}
		}
	}

	for project, stats := range projectStats {
		costs := projectCostTotals[project]
		premCost := sumProjectDailyPremCost(project, projectDailyModelStats)
		out.Projects[project] = statsPayloadStats{
			APICalls:            stats.APICalls,
			UserTurns:           stats.UserTurns,
			PromptTokens:        stats.PromptTokens,
			CompletionTokens:    stats.CompletionTokens,
			CacheCreationTokens: stats.CacheCreationTokens,
			CacheReadTokens:     stats.CacheReadTokens,
			PremiumRequests:     stats.PremiumRequests,
			PremiumRequestCost:  roundN(premCost, 4),
			InputUncached:       uncachedInput(stats),
			Cost:                roundN(costs[0], 4),
			CostWithoutCache:    roundN(costs[1], 4),
		}
	}

	if len(projectModelStats) > 0 {
		out.ProjectModels = make(map[string]map[string]statsPayloadStats)
		for project, models := range projectModelStats {
			out.ProjectModels[project] = make(map[string]statsPayloadStats)
			for model, stats := range models {
				costs := projectModelCostTotals[project][model]
				premCost := sumProjectModelDailyPremCost(project, model, projectDailyModelStats)
				out.ProjectModels[project][model] = statsPayloadStats{
					APICalls:            stats.APICalls,
					UserTurns:           stats.UserTurns,
					PromptTokens:        stats.PromptTokens,
					CompletionTokens:    stats.CompletionTokens,
					CacheCreationTokens: stats.CacheCreationTokens,
					CacheReadTokens:     stats.CacheReadTokens,
					PremiumRequests:     stats.PremiumRequests,
					PremiumRequestCost:  roundN(premCost, 4),
					InputUncached:       uncachedInput(stats),
					Cost:                roundN(costs[0], 4),
					CostWithoutCache:    roundN(costs[1], 4),
				}
			}
		}
	}

	if len(projectDailyModelStats) > 0 {
		out.DailyProjects = make(map[string]map[string]statsPayloadStats)
		dayKeys := make([]string, 0, len(projectDailyModelStats))
		for day := range projectDailyModelStats {
			dayKeys = append(dayKeys, day)
		}
		sort.Strings(dayKeys)
		for _, day := range dayKeys {
			projects := projectDailyModelStats[day]
			out.DailyProjects[day] = make(map[string]statsPayloadStats)
			projectKeys := make([]string, 0, len(projects))
			for project := range projects {
				projectKeys = append(projectKeys, project)
			}
			sort.Strings(projectKeys)
			for _, project := range projectKeys {
				models := projects[project]
				stats := newStats()
				projectCost := 0.0
				projectCostNoCache := 0.0
				for model, modelStats := range models {
					mergeStats(stats, modelStats)
					projectCost += calcCost(model, modelStats, day)
					projectCostNoCache += calcCostNocache(model, modelStats, day)
				}
				out.DailyProjects[day][project] = statsPayloadStats{
					APICalls:            stats.APICalls,
					UserTurns:           stats.UserTurns,
					PromptTokens:        stats.PromptTokens,
					CompletionTokens:    stats.CompletionTokens,
					CacheCreationTokens: stats.CacheCreationTokens,
					CacheReadTokens:     stats.CacheReadTokens,
					PremiumRequests:     stats.PremiumRequests,
					PremiumRequestCost:  roundN(stats.PremiumRequests*getPremiumRequestCost(day), 4),
					InputUncached:       uncachedInput(stats),
					Cost:                roundN(projectCost, 4),
					CostWithoutCache:    roundN(projectCostNoCache, 4),
				}
			}
		}
	}

	out.TotalCost = roundN(out.TotalCost, 4)
	out.TotalCostNoCache = roundN(out.TotalCostNoCache, 4)
	out.TotalPremiumRequestCost = roundN(out.TotalPremiumRequestCost, 4)
	return out
}

func sumProjectDailyPremCost(project string, projectDailyModelStats map[string]map[string]map[string]*Stats) float64 {
	var total float64
	for day, projects := range projectDailyModelStats {
		if models, ok := projects[project]; ok {
			for _, s := range models {
				total += s.PremiumRequests * getPremiumRequestCost(day)
			}
		}
	}
	return total
}

func sumProjectModelDailyPremCost(project, model string, projectDailyModelStats map[string]map[string]map[string]*Stats) float64 {
	var total float64
	for day, projects := range projectDailyModelStats {
		if models, ok := projects[project]; ok {
			if s, ok := models[model]; ok {
				total += s.PremiumRequests * getPremiumRequestCost(day)
			}
		}
	}
	return total
}

func dbStatsToStats(dbs *dbModelStats, premiumRequests float64) *Stats {
	return &Stats{
		APICalls:            dbs.APICalls,
		UserTurns:           dbs.UserTurns,
		PromptTokens:        dbs.PromptTokens,
		CompletionTokens:    dbs.CompletionTokens,
		CacheCreationTokens: dbs.CacheCreationTokens,
		CacheReadTokens:     dbs.CacheReadTokens,
		PremiumRequests:     premiumRequests,
	}
}

func mergeStats(dst, src *Stats) {
	dst.APICalls += src.APICalls
	dst.UserTurns += src.UserTurns
	dst.PromptTokens += src.PromptTokens
	dst.CompletionTokens += src.CompletionTokens
	dst.CacheCreationTokens += src.CacheCreationTokens
	dst.CacheReadTokens += src.CacheReadTokens
	dst.PremiumRequests += src.PremiumRequests
}

func normalizeWorkspacePath(cwd string) string {
	normalized := strings.TrimSpace(strings.ReplaceAll(cwd, "\\", "/"))
	return strings.TrimRight(normalized, "/")
}

func workspacePathBaseName(cwd string) string {
	normalized := normalizeWorkspacePath(cwd)
	if normalized == "" {
		return ""
	}
	lastSlash := strings.LastIndex(normalized, "/")
	if lastSlash < 0 || lastSlash == len(normalized)-1 {
		return ""
	}
	return normalized[lastSlash+1:]
}

func isCodespacesWorkspacePath(cwd string) bool {
	return strings.HasPrefix(normalizeWorkspacePath(cwd), "/workspaces/")
}

func buildLocalProjectByBase(cwds []string) map[string]string {
	localByBase := make(map[string]string)
	sorted := append([]string(nil), cwds...)
	sort.Strings(sorted)
	for _, cwd := range sorted {
		if isCodespacesWorkspacePath(cwd) {
			continue
		}
		base := strings.ToLower(workspacePathBaseName(cwd))
		if base == "" {
			continue
		}
		if _, exists := localByBase[base]; !exists {
			localByBase[base] = projectName(cwd)
		}
	}
	return localByBase
}

func canonicalProjectLabel(cwd string, localByBase map[string]string) string {
	if strings.TrimSpace(cwd) == "" {
		return "(unknown)"
	}
	if !isCodespacesWorkspacePath(cwd) {
		return projectName(cwd)
	}
	base := strings.ToLower(workspacePathBaseName(cwd))
	if base == "" {
		return projectName(cwd)
	}
	if local, exists := localByBase[base]; exists {
		return local
	}
	return projectName(cwd)
}

type derivedStats struct {
	DailyStats             map[string]map[string]*Stats
	ModelStats             map[string]*Stats
	ProjectStats           map[string]*Stats
	ProjectModelStats      map[string]map[string]*Stats
	ProjectDailyModelStats map[string]map[string]map[string]*Stats
	TotalRecords           int
}

func buildAllDerivedStats(raw map[string]map[string]map[string]*dbModelStats) derivedStats {
	dailyStats := make(map[string]map[string]*Stats)
	projectModelDailyStats := make(map[string]map[string]map[string]*Stats)
	totalRecords := 0

	// Collect all CWDs for codespaces→local mapping
	cwdSet := make(map[string]struct{})
	for _, cwds := range raw {
		for cwd := range cwds {
			cwdSet[cwd] = struct{}{}
		}
	}
	cwdList := make([]string, 0, len(cwdSet))
	for cwd := range cwdSet {
		cwdList = append(cwdList, cwd)
	}
	localByBase := buildLocalProjectByBase(cwdList)

	for day, cwds := range raw {
		if dailyStats[day] == nil {
			dailyStats[day] = make(map[string]*Stats)
		}
		for cwd, models := range cwds {
			proj := canonicalProjectLabel(cwd, localByBase)
			for model, dbs := range models {
				premReqs := float64(dbs.UserTurns) * getPremiumMultiplier(model, day)
				s := dbStatsToStats(dbs, premReqs)
				totalRecords += dbs.APICalls

				// DailyStats [day][model]
				if existing, ok := dailyStats[day][model]; ok {
					mergeStats(existing, s)
				} else {
					cp := *s
					dailyStats[day][model] = &cp
				}

				// ProjectDailyModelStats [day][project][model]
				if projectModelDailyStats[day] == nil {
					projectModelDailyStats[day] = make(map[string]map[string]*Stats)
				}
				if projectModelDailyStats[day][proj] == nil {
					projectModelDailyStats[day][proj] = make(map[string]*Stats)
				}
				if existing, ok := projectModelDailyStats[day][proj][model]; ok {
					mergeStats(existing, s)
				} else {
					cp := *s
					projectModelDailyStats[day][proj][model] = &cp
				}
			}
		}
	}

	// Derive ModelStats from DailyStats (sum across days)
	modelStats := make(map[string]*Stats)
	for _, models := range dailyStats {
		for model, s := range models {
			if existing, ok := modelStats[model]; ok {
				mergeStats(existing, s)
			} else {
				cp := *s
				modelStats[model] = &cp
			}
		}
	}

	// Derive ProjectModelStats and ProjectStats from ProjectDailyModelStats
	projectModelStats := make(map[string]map[string]*Stats)
	projectStats := make(map[string]*Stats)
	for _, projects := range projectModelDailyStats {
		for proj, models := range projects {
			if projectModelStats[proj] == nil {
				projectModelStats[proj] = make(map[string]*Stats)
			}
			for model, s := range models {
				if existing, ok := projectModelStats[proj][model]; ok {
					mergeStats(existing, s)
				} else {
					cp := *s
					projectModelStats[proj][model] = &cp
				}
				if existing, ok := projectStats[proj]; ok {
					mergeStats(existing, s)
				} else {
					cp := *s
					projectStats[proj] = &cp
				}
			}
		}
	}

	return derivedStats{
		DailyStats:             dailyStats,
		ModelStats:             modelStats,
		ProjectStats:           projectStats,
		ProjectModelStats:      projectModelStats,
		ProjectDailyModelStats: projectModelDailyStats,
		TotalRecords:           totalRecords,
	}
}

func loadAggregatedStats(db *sql.DB, dateFrom, dateTo, projectFilter string) aggregatedStats {
	hasBranch := sessionWorkspaceColumns(db, "")["branch"]

	var q dbQuerier = db
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err == nil {
		q = tx
		defer tx.Rollback()
	}

	derived := buildAllDerivedStats(queryDailyProjectModelStats(q, dateFrom, dateTo, projectFilter))
	return aggregatedStats{
		DailyStats:             derived.DailyStats,
		ModelStats:             derived.ModelStats,
		ProjectStats:           derived.ProjectStats,
		ProjectModelStats:      derived.ProjectModelStats,
		ProjectDailyModelStats: derived.ProjectDailyModelStats,
		Records:                queryRecords(q, dateFrom, dateTo, projectFilter),
		SessionWorkspaces:      querySessionWorkspaces(q, hasBranch),
		TotalRecords:           derived.TotalRecords,
		LogFileCount:           queryLogFileCount(q, dateFrom, dateTo, projectFilter),
	}
}
