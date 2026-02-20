package main

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

type aggregatedStats struct {
	DailyStats        map[string]map[string]*Stats
	ModelStats        map[string]*Stats
	ProjectStats      map[string]*Stats
	ProjectModelStats map[string]map[string]*Stats
	Records           []Record
	SessionWorkspaces map[string]workspaceMeta
	TotalRecords      int
	LogFileCount      int
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
	filtered := aggregated.Records
	sessionWorkspaces := aggregated.SessionWorkspaces
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

	projCosts := make(map[string][2]float64)
	projModelCosts := make(map[string]map[string][2]float64)
	dailyProjectStats := make(map[string]map[string]*Stats)
	dailyProjectCosts := make(map[string]map[string][2]float64)
	workspacePaths := make([]string, 0, len(filtered))
	for _, r := range filtered {
		if r.SessionID == "" {
			continue
		}
		if meta, ok := sessionWorkspaces[r.Source+"\x1f"+r.SessionID]; ok && meta.CWD != "" {
			workspacePaths = append(workspacePaths, meta.CWD)
		}
	}
	localProjectsByBase := buildLocalProjectByBase(workspacePaths)
	for _, r := range filtered {
		cwd := ""
		if r.SessionID != "" {
			if meta, ok := sessionWorkspaces[r.Source+"\x1f"+r.SessionID]; ok {
				cwd = meta.CWD
			}
		}
		proj := canonicalProjectLabel(cwd, localProjectsByBase)
		model := normalizeModel(r.Model)
		rs := &Stats{
			PromptTokens:        r.PromptTokens,
			CompletionTokens:    r.CompletionTokens,
			CacheCreationTokens: r.CacheCreationTokens,
			CacheReadTokens:     r.CacheReadTokens,
		}
		cost := calcCost(model, rs, r.Timestamp)
		costNC := calcCostNocache(model, rs, r.Timestamp)
		day := "unknown"
		if len(r.Timestamp) >= 10 {
			day = r.Timestamp[:10]
		}
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
		if dailyProjectStats[day] == nil {
			dailyProjectStats[day] = make(map[string]*Stats)
		}
		if dailyProjectCosts[day] == nil {
			dailyProjectCosts[day] = make(map[string][2]float64)
		}
		if _, ok := dailyProjectStats[day][proj]; !ok {
			dailyProjectStats[day][proj] = newStats()
		}
		dailyProjectStats[day][proj].add(r, model)
		dayProjectCost := dailyProjectCosts[day][proj]
		dayProjectCost[0] += cost
		dayProjectCost[1] += costNC
		dailyProjectCosts[day][proj] = dayProjectCost
	}

	for proj, s := range projectStats {
		pc := projCosts[proj]
		out.Projects[proj] = statsPayloadStats{
			APICalls:            s.APICalls,
			UserTurns:           s.UserTurns,
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
					UserTurns:           s.UserTurns,
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
	if len(dailyProjectStats) > 0 {
		out.DailyProjects = make(map[string]map[string]statsPayloadStats)
		for day, projects := range dailyProjectStats {
			out.DailyProjects[day] = make(map[string]statsPayloadStats)
			dayCosts := dailyProjectCosts[day]
			for project, stats := range projects {
				projectCosts := dayCosts[project]
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
					Cost:                roundN(projectCosts[0], 4),
					CostWithoutCache:    roundN(projectCosts[1], 4),
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

func buildProjectStatsMap(dbProjectStats map[string]*dbModelStats) map[string]*Stats {
	projectStats := make(map[string]*Stats)
	cwds := make([]string, 0, len(dbProjectStats))
	for cwd := range dbProjectStats {
		cwds = append(cwds, cwd)
	}
	localByBase := buildLocalProjectByBase(cwds)
	for cwd, dbs := range dbProjectStats {
		proj := canonicalProjectLabel(cwd, localByBase)
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
	cwds := make([]string, 0, len(dbProjectModelStats))
	for cwd := range dbProjectModelStats {
		cwds = append(cwds, cwd)
	}
	localByBase := buildLocalProjectByBase(cwds)
	for cwd, models := range dbProjectModelStats {
		proj := canonicalProjectLabel(cwd, localByBase)
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
