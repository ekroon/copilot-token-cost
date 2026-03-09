package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func parseTS(s string) (time.Time, bool) {
	// Try common ISO formats
	for _, layout := range []string{
		"2006-01-02T15:04:05",
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05-07:00",
		"2006-01-02T15:04:05.000",
	} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, true
		}
	}
	// Try prefix match
	if len(s) >= 19 {
		if t, err := time.ParseInLocation("2006-01-02T15:04:05", s[:19], time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func parseExplicitPeriodValue(raw string) (dateWindowSpec, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	switch value {
	case "all":
		return dateWindowSpec{AllTime: true}, nil
	case "today":
		return dateWindowSpec{Mode: "today"}, nil
	case "yesterday":
		return dateWindowSpec{Mode: "yesterday"}, nil
	}

	days, err := strconv.Atoi(value)
	if err != nil || days <= 0 {
		return dateWindowSpec{}, fmt.Errorf("--period must be one of: today, yesterday, all, or a positive day count")
	}

	return dateWindowSpec{Mode: "days", Days: days}, nil
}

func runLegacyCLI() {
	loadPricing()

	allFlag := flag.Bool("all", false, "Process all available logs")
	todayFlag := flag.Bool("today", false, "Today only")
	yesterdayFlag := flag.Bool("yesterday", false, "Yesterday only")
	periodFlag := flag.String("period", "", "Date window: today|yesterday|all|N (days)")
	fromDays := flag.Int("from", -1, "Start from N days ago (0=today, 1=yesterday)")
	toDays := flag.Int("to", -1, "End at N days ago (0=today, 1=yesterday)")
	logsDirFlag := flag.String("logs-dir", "", "Override logs directory")
	projectFilter := flag.String("project", "", "Filter by project/workspace path (case-insensitive substring)")
	jsonFlag := flag.Bool("json", false, "Output as JSON")
	syncFlag := flag.Bool("sync", false, "Force full re-sync of all log files")
	importFile := flag.String("import-file", "", "Import data from JSONL or SQLite file")
	exportFile := flag.String("export-file", "", "Export data as JSONL")
	codespacesSync := flag.Bool("codespaces-sync", false, "Sync Copilot data from running Codespaces via gh cs cp")
	codespacesIncludeStopped := flag.Bool("codespaces-include-stopped", false, "Include stopped Codespaces (will wake and sync them)")
	webFlag := flag.Bool("web", false, "Run in web mode (respects date-window flags)")
	webListen := flag.String("web-listen", "127.0.0.1:7331", "Web mode listen address")
	webRefreshInterval := flag.Duration("web-refresh-interval", 30*time.Second, "Web mode refresh interval")
	webLocalStreaming := flag.Bool("web-local-streaming", false, "Enable experimental realtime local log streaming in web mode")
	webCodespacesMode := flag.String("web-codespaces-mode", "auto", "Web mode Codespaces sync mode: manual|auto (default auto: background startup sync + periodic sync)")
	webCodespacesStreaming := flag.Bool("web-codespaces-streaming", false, "Enable experimental codespaces streaming status from tail checkpoints")
	webCodespacesInterval := flag.Duration("web-codespaces-interval", 5*time.Minute, "Web mode Codespaces periodic sync interval when mode=auto")
	webLogMode := flag.String("web-log-mode", "compact", "Web mode stderr logging: compact|verbose|errors")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: copilot-token-cost [days] [--period VALUE] [--all] [--today] [--yesterday]\n")
		fmt.Fprintf(os.Stderr, "                         [--from N] [--to N] [--logs-dir PATH] [--project TEXT] [--json]\n")
		fmt.Fprintf(os.Stderr, "                         [--sync] [--import-file FILE] [--export-file FILE]\n\n")
		fmt.Fprintf(os.Stderr, "                         [--codespaces-sync] [--codespaces-include-stopped]\n\n")
		fmt.Fprintf(os.Stderr, "                         [--web] [--web-listen ADDR] [--web-refresh-interval DURATION]\n")
		fmt.Fprintf(os.Stderr, "                         [--web-local-streaming] [--web-codespaces-mode manual|auto]\n")
		fmt.Fprintf(os.Stderr, "                         [--web-codespaces-streaming] [--web-codespaces-interval DURATION]\n")
		fmt.Fprintf(os.Stderr, "                         [--web-log-mode compact|verbose|errors]\n\n")
		fmt.Fprintf(os.Stderr, "       copilot-token-cost sql [--json] \"SQL QUERY\"\n\n")
		fmt.Fprintf(os.Stderr, "Copilot CLI Token Cost Calculator\n\n")
		fmt.Fprintf(os.Stderr, "Prompt text storage is always-on when prompt text is available; unavailable prompt text is stored as NULL.\n\n")
		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost              # last 7 days\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost 30           # last 30 days\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --period 30  # last 30 days (explicit flag)\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost 1            # today\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --today      # today\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --yesterday  # yesterday only\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --period all # all logs\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --from 3     # 3 days ago until now\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --all        # all logs\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --project graph-hopper  # filter to matching projects\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --sync       # force full re-sync\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --export-file data.jsonl  # export\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --import-file data.jsonl  # import\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --codespaces-sync  # sync running codespaces\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --web --today  # web mode with date window\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --web --web-codespaces-mode manual  # disable auto codespaces sync\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --web --web-local-streaming  # enable experimental realtime local streaming\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --web --web-codespaces-interval 15s  # near-continuous codespaces sync\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --web --web-codespaces-streaming  # show experimental live streaming status\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost --web --web-log-mode verbose  # keep line-by-line sync logs\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost sql \"SELECT COUNT(*) FROM api_calls\"  # direct SQL query\n")
		fmt.Fprintf(os.Stderr, "  copilot-token-cost sql --json \"SELECT DISTINCT cwd FROM session_workspaces\"  # SQL with JSON output\n")
	}
	flag.Parse()

	webCodespacesModeValue := strings.ToLower(strings.TrimSpace(*webCodespacesMode))
	if webCodespacesModeValue != "manual" && webCodespacesModeValue != "auto" {
		fmt.Fprintln(os.Stderr, "--web-codespaces-mode must be one of: manual, auto")
		os.Exit(1)
	}
	webLogModeValue := normalizeWebLogMode(*webLogMode)
	if webLogModeValue == "" {
		fmt.Fprintln(os.Stderr, "--web-log-mode must be one of: compact, verbose, errors")
		os.Exit(1)
	}

	if *webFlag && *jsonFlag {
		fmt.Fprintln(os.Stderr, "--web cannot be used with --json")
		os.Exit(1)
	}
	if *webFlag && *exportFile != "" {
		fmt.Fprintln(os.Stderr, "--web cannot be used with --export-file")
		os.Exit(1)
	}

	if *codespacesIncludeStopped && !*codespacesSync && !*webFlag {
		fmt.Fprintln(os.Stderr, "--codespaces-include-stopped requires either --codespaces-sync or --web")
		os.Exit(1)
	}

	home, _ := os.UserHomeDir()
	logsDir := filepath.Join(home, ".copilot", "logs")
	if *logsDirFlag != "" {
		logsDir = *logsDirFlag
	}
	sessionDir := filepath.Join(home, ".copilot", "session-state")

	logsExist := true
	if _, err := os.Stat(logsDir); os.IsNotExist(err) {
		logsExist = false
	}

	now := time.Now()
	todayMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)

	var cutoff time.Time
	var cutoffEnd *time.Time
	var periodLabel string
	useCutoffMin := false
	explicitPeriodValue := strings.TrimSpace(*periodFlag)
	var dateFromDisplay, dateToDisplay, dateRange string
	var dateFromQuery, dateToQuery string // ISO timestamps for DB queries
	var syncFrom, syncTo *time.Time

	// Parse positional days argument
	var days int
	daysSet := false
	if flag.NArg() > 0 {
		if d, err := strconv.Atoi(flag.Arg(0)); err == nil {
			days = d
			daysSet = true
		}
	}

	var dateWindow dateWindowSpec
	if *allFlag {
		useCutoffMin = true
		periodLabel = "all time"
		dateWindow = dateWindowSpec{AllTime: true}
	} else if *todayFlag {
		cutoff = todayMidnight
		periodLabel = "today"
		dateWindow = dateWindowSpec{Mode: "today"}
	} else if *yesterdayFlag {
		cutoff = todayMidnight.AddDate(0, 0, -1)
		end := todayMidnight
		cutoffEnd = &end
		periodLabel = "yesterday"
		dateWindow = dateWindowSpec{Mode: "yesterday"}
	} else if *fromDays >= 0 {
		fd := *fromDays
		td := *toDays
		if td < 0 {
			td = 0
		}
		if fd < td {
			fd, td = td, fd
		}
		cutoff = todayMidnight.AddDate(0, 0, -fd)
		if td > 0 {
			end := todayMidnight.AddDate(0, 0, -td+1)
			cutoffEnd = &end
		}
		if fd == td {
			periodLabel = fmt.Sprintf("%s (1 day)", cutoff.Format("2006-01-02"))
		} else {
			toStr := "today"
			if td > 0 {
				toStr = fmt.Sprintf("%dd ago", td)
			}
			periodLabel = fmt.Sprintf("%dd ago → %s", fd, toStr)
		}
		dateWindow = dateWindowSpec{Mode: "from-to", FromDays: fd, ToDays: td}
	} else if explicitPeriodValue != "" {
		spec, err := parseExplicitPeriodValue(explicitPeriodValue)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		dateWindow = spec
		bounds := dateWindow.computeBounds(now)
		periodLabel = bounds.PeriodLabel
		dateRange = bounds.DateRange
		dateFromQuery = bounds.DateFromQuery
		dateToQuery = bounds.DateToQuery
		syncFrom = bounds.SyncFrom
		syncTo = bounds.SyncTo
	} else {
		if !daysSet {
			days = 7
		}
		cutoff = todayMidnight.AddDate(0, 0, -(days - 1))
		if days == 1 {
			periodLabel = "last 1 day"
		} else {
			periodLabel = fmt.Sprintf("last %d days", days)
		}
		dateWindow = dateWindowSpec{Mode: "days", Days: days}
	}

	// Date range label and DB query params
	if explicitPeriodValue == "" && !useCutoffMin {
		dateFromDisplay = cutoff.Format("2006-01-02")
		dateFromQuery = cutoff.Format("2006-01-02T15:04:05")
	}
	if explicitPeriodValue == "" && cutoffEnd != nil {
		dateToDisplay = cutoffEnd.AddDate(0, 0, -1).Format("2006-01-02")
		dateToQuery = cutoffEnd.Format("2006-01-02T15:04:05")
	} else if explicitPeriodValue == "" {
		dateToDisplay = now.Format("2006-01-02")
	}
	if explicitPeriodValue == "" && dateFromDisplay != "" {
		dateRange = dateFromDisplay + " → " + dateToDisplay
	}

	if explicitPeriodValue == "" && !useCutoffMin {
		c := cutoff
		syncFrom = &c
	}
	if explicitPeriodValue == "" && cutoffEnd != nil {
		c := *cutoffEnd
		syncTo = &c
	}

	if *webFlag {
		cfg := webModeConfig{
			ListenAddress:            *webListen,
			RefreshInterval:          *webRefreshInterval,
			LocalStreaming:           *webLocalStreaming,
			CodespacesMode:           webCodespacesModeValue,
			CodespacesStreaming:      *webCodespacesStreaming,
			CodespacesInterval:       *webCodespacesInterval,
			WebLogMode:               webLogModeValue,
			CodespacesIncludeStopped: *codespacesIncludeStopped,
			LogsDir:                  logsDir,
			SessionDir:               sessionDir,
			DateWindow:               dateWindow,
		}
		if err := runWebMode(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// ─── DB setup and sync ─────────────────────────────────────────────
	dbPath := getDBPath()
	database := initDB(dbPath)
	defer database.Close()

	if logsExist {
		syncLogsToDB(database, logsDir, sessionDir, *syncFlag, "local", syncFrom, syncTo)
	}

	if *codespacesSync {
		syncCodespacesToDB(database, *codespacesIncludeStopped, *syncFlag)
	}

	if *importFile != "" {
		if strings.HasSuffix(*importFile, ".db") || strings.HasSuffix(*importFile, ".sqlite") {
			importSQLiteDB(database, *importFile, "")
		} else {
			importJSONL(database, *importFile, "")
		}
	}

	if *exportFile != "" {
		exportJSONL(database, *exportFile)
		return
	}

	projectFilterValue := strings.TrimSpace(*projectFilter)

	// ─── Query aggregated stats from DB ────────────────────────────────
	aggregatedStats := loadAggregatedStats(database, dateFromQuery, dateToQuery, projectFilterValue)
	dailyStats := aggregatedStats.DailyStats
	modelStats := aggregatedStats.ModelStats
	projectStats := aggregatedStats.ProjectStats
	filtered := aggregatedStats.Records
	sessionWorkspaces := aggregatedStats.SessionWorkspaces
	totalRecords := aggregatedStats.TotalRecords
	logFileCount := aggregatedStats.LogFileCount

	if totalRecords == 0 {
		fmt.Printf("No API calls found in %s.\n", periodLabel)
		return
	}

	// ─── JSON output ────────────────────────────────────────────────────
	if *jsonFlag {
		type jsonStats struct {
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
		type dailyModel struct {
			jsonStats
		}
		type dailyDay struct {
			Models           map[string]jsonStats `json:"-"`
			TotalCost        float64              `json:"_total_cost"`
			TotalCostNoCache float64              `json:"_total_cost_without_cache"`
		}

		type output struct {
			Period                  string                            `json:"period"`
			DateRange               *string                           `json:"date_range"`
			LogFiles                int                               `json:"log_files"`
			APICalls                int                               `json:"api_calls"`
			Models                  map[string]jsonStats              `json:"models"`
			Daily                   map[string]map[string]interface{} `json:"daily"`
			Projects                map[string]jsonStats              `json:"projects"`
			TotalCost               float64                           `json:"total_cost"`
			TotalCostNoCache        float64                           `json:"total_cost_without_cache"`
			TotalPremiumRequestCost float64                           `json:"total_premium_request_cost"`
		}

		out := output{
			Period:   periodLabel,
			LogFiles: logFileCount,
			APICalls: totalRecords,
			Models:   make(map[string]jsonStats),
			Daily:    make(map[string]map[string]interface{}),
			Projects: make(map[string]jsonStats),
		}
		if dateRange != "" {
			out.DateRange = &dateRange
		}

		// Models
		models := sortedKeys(modelStats)
		for _, model := range models {
			s := modelStats[model]
			cost := sumDailyCost(model, dailyStats, calcCost)
			costNC := sumDailyCost(model, dailyStats, calcCostNocache)
			premCost := sumDailyPremCost(model, dailyStats)
			out.TotalCost += cost
			out.TotalCostNoCache += costNC
			out.Models[model] = jsonStats{
				APICalls: s.APICalls, PromptTokens: s.PromptTokens,
				CompletionTokens: s.CompletionTokens, CacheCreationTokens: s.CacheCreationTokens,
				CacheReadTokens: s.CacheReadTokens, PremiumRequests: s.PremiumRequests,
				PremiumRequestCost: roundN(premCost, 4),
				InputUncached:      uncachedInput(s),
				Cost:               roundN(cost, 4), CostWithoutCache: roundN(costNC, 4),
			}
			out.TotalPremiumRequestCost += premCost
		}

		// Daily
		daysKeys := sortedKeysStr(dailyStats)
		for _, day := range daysKeys {
			dayMap := make(map[string]interface{})
			var dayTotal, dayTotalNC float64
			for model, s := range dailyStats[day] {
				cost := calcCost(model, s, day)
				costNC := calcCostNocache(model, s, day)
				dayTotal += cost
				dayTotalNC += costNC
				dayMap[model] = jsonStats{
					APICalls: s.APICalls, PromptTokens: s.PromptTokens,
					CompletionTokens: s.CompletionTokens, CacheCreationTokens: s.CacheCreationTokens,
					CacheReadTokens: s.CacheReadTokens, PremiumRequests: s.PremiumRequests,
					PremiumRequestCost: roundN(s.PremiumRequests*getPremiumRequestCost(day), 4),
					InputUncached:      uncachedInput(s),
					Cost:               roundN(cost, 4), CostWithoutCache: roundN(costNC, 4),
				}
			}
			dayMap["_total_cost"] = roundN(dayTotal, 4)
			dayMap["_total_cost_without_cache"] = roundN(dayTotalNC, 4)
			out.Daily[day] = dayMap
		}

		// Projects - recalculate costs per record
		projCosts := make(map[string][2]float64) // [cost, costNC]
		for _, r := range filtered {
			cwd := ""
			if r.SessionID != "" {
				if meta, ok := sessionWorkspaces[r.Source+"\x1f"+r.SessionID]; ok {
					cwd = meta.CWD
				}
			}
			proj := "(unknown)"
			if cwd != "" {
				proj = projectName(cwd)
			}
			model := normalizeModel(r.Model)
			rs := &Stats{
				PromptTokens: r.PromptTokens, CompletionTokens: r.CompletionTokens,
				CacheCreationTokens: r.CacheCreationTokens, CacheReadTokens: r.CacheReadTokens,
			}
			c := projCosts[proj]
			c[0] += calcCost(model, rs, r.Timestamp)
			c[1] += calcCostNocache(model, rs, r.Timestamp)
			projCosts[proj] = c
		}
		for proj, s := range projectStats {
			pc := projCosts[proj]
			out.Projects[proj] = jsonStats{
				APICalls: s.APICalls, PromptTokens: s.PromptTokens,
				CompletionTokens: s.CompletionTokens, CacheCreationTokens: s.CacheCreationTokens,
				CacheReadTokens: s.CacheReadTokens, PremiumRequests: s.PremiumRequests,
				PremiumRequestCost: roundN(s.PremiumRequests*getPremiumRequestCost(""), 4),
				InputUncached:      uncachedInput(s),
				Cost:               roundN(pc[0], 4), CostWithoutCache: roundN(pc[1], 4),
			}
		}

		out.TotalCost = roundN(out.TotalCost, 4)
		out.TotalCostNoCache = roundN(out.TotalCostNoCache, 4)
		out.TotalPremiumRequestCost = roundN(out.TotalPremiumRequestCost, 4)

		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(out)
		return
	}

	// ─── Pretty output ──────────────────────────────────────────────────
	fmt.Println()
	title := "COPILOT CLI - TOKEN USAGE & COST REPORT"
	titleWidth := len(title) + 10
	titlePadL := (titleWidth - len(title)) / 2
	titlePadR := titleWidth - len(title) - titlePadL
	fmt.Printf("╔%s╗\n", strings.Repeat("═", titleWidth))
	fmt.Printf("║%s%s%s║\n", strings.Repeat(" ", titlePadL), title, strings.Repeat(" ", titlePadR))
	fmt.Printf("╚%s╝\n", strings.Repeat("═", titleWidth))

	totalPremium := 0.0
	for _, s := range modelStats {
		totalPremium += s.PremiumRequests
	}
	dateSuffix := ""
	if dateRange != "" {
		dateSuffix = " (" + dateRange + ")"
	}
	projectSuffix := ""
	if projectFilterValue != "" {
		projectSuffix = "  │  Project filter: " + projectFilterValue
	}
	fmt.Printf("  Period: %s%s  │  Log files: %d  │  API calls: %s  │  Premium requests: %s\n",
		periodLabel, dateSuffix, logFileCount, commaInt(totalRecords), commaFloat(totalPremium, 0))
	if projectSuffix != "" {
		fmt.Println(projectSuffix)
	}
	fmt.Println()

	// ── Per-model table ─────────────────────────────────────────────────
	modelHeaders := []string{"Model", "Calls", "Premium", "Prem Cost", "Input", "Cached", "Cache Write", "Output", "Hit%", "Cost", "No-Cache"}
	var modelRows [][]string
	var tCost, tNC, tPremCost float64
	var tUnc, tCached, tCW, tOut, tCalls, tPrompt int
	var tPremium float64

	sortedModels := sortedKeysByFunc(modelStats, func(m string) float64 {
		return -sumDailyCost(m, dailyStats, calcCostNocache)
	})

	for _, model := range sortedModels {
		s := modelStats[model]
		cost := sumDailyCost(model, dailyStats, calcCost)
		costNC := sumDailyCost(model, dailyStats, calcCostNocache)
		unc := uncachedInput(s)
		tCost += cost
		tNC += costNC
		tUnc += unc
		tCached += s.CacheReadTokens
		tCW += s.CacheCreationTokens
		tOut += s.CompletionTokens
		tCalls += s.APICalls
		tPrompt += s.PromptTokens
		tPremium += s.PremiumRequests

		p := getPricing(model, "")
		mult := getPremiumMultiplier(model, "")
		premiumStr := commaFloat(s.PremiumRequests, 0)
		premCost := sumDailyPremCost(model, dailyStats)
		tPremCost += premCost
		premCostStr := fmtCost(premCost)
		if mult == 0 {
			premiumStr = "-"
			premCostStr = "-"
		}
		costStr, costNCStr := fmtCost(cost), fmtCost(costNC)
		if p == nil {
			costStr, costNCStr = "N/A", "N/A"
		}
		modelRows = append(modelRows, []string{
			model, commaInt(s.APICalls), premiumStr, premCostStr,
			fmtTokens(unc), fmtTokens(s.CacheReadTokens),
			fmtTokens(s.CacheCreationTokens), fmtTokens(s.CompletionTokens),
			cacheHitPct(s.PromptTokens, s.CacheReadTokens),
			costStr, costNCStr,
		})
	}
	modelFooter := []string{
		"TOTAL", commaInt(tCalls), commaFloat(tPremium, 0), fmtCost(tPremCost),
		fmtTokens(tUnc), fmtTokens(tCached),
		fmtTokens(tCW), fmtTokens(tOut),
		cacheHitPct(tPrompt, tCached),
		fmtCost(tCost), fmtCost(tNC),
	}
	var modelNotes []string
	if tNC > 0 {
		savingsPct := (1 - tCost/tNC) * 100
		modelNotes = append(modelNotes, fmt.Sprintf("💰 Cache savings: %s (%.0f%% reduction)", fmtCost(tNC-tCost), savingsPct))
	}
	printTable("PER-MODEL SUMMARY", modelHeaders, modelRows, modelFooter, modelNotes)
	fmt.Println()

	// ── Cost per premium request ────────────────────────────────────────
	premHeaders := []string{"Model", "Multiplier", "Premiums", "API Cost", "$/Premium", "Prem Cost", "Discount"}
	var premRows [][]string
	var premTotalCost float64
	var premTotalReqs float64

	sortedPremModels := sortedKeysByFunc(modelStats, func(m string) float64 {
		return -modelStats[m].PremiumRequests
	})

	for _, model := range sortedPremModels {
		s := modelStats[model]
		mult := getPremiumMultiplier(model, "")
		if mult == 0 {
			continue
		}
		cost := sumDailyCost(model, dailyStats, calcCost)
		if s.PremiumRequests > 0 {
			premTotalCost += cost
			premTotalReqs += s.PremiumRequests
			costPer := cost / s.PremiumRequests
			premCost := sumDailyPremCost(model, dailyStats)
			discount := "-"
			if cost > 0 {
				discount = fmt.Sprintf("%.0f%%", (1-premCost/cost)*100)
			}
			multStr := fmt.Sprintf("%.2g×", mult)
			premRows = append(premRows, []string{
				model, multStr, commaFloat(s.PremiumRequests, 0),
				fmtCost(cost), fmtCost(costPer), fmtCost(premCost), discount,
			})
		} else {
			multStr := fmt.Sprintf("%.2g×", mult)
			premRows = append(premRows, []string{
				model, multStr, "-",
				fmtCost(cost), "N/A", "-", "-",
			})
		}
	}

	if len(premRows) > 0 {
		avgCost := 0.0
		if premTotalReqs > 0 {
			avgCost = premTotalCost / premTotalReqs
		}
		totalPremCost := sumDailyPremCostAll(dailyStats)
		totalDiscount := "-"
		if premTotalCost > 0 {
			totalDiscount = fmt.Sprintf("%.0f%%", (1-totalPremCost/premTotalCost)*100)
		}
		premFooter := []string{"TOTAL", "", commaFloat(premTotalReqs, 0), fmtCost(premTotalCost), fmtCost(avgCost), fmtCost(totalPremCost), totalDiscount}
		premNotes := []string{"ℹ️  Models with 0× multiplier (free tier) are excluded"}
		missingCost := tCost - premTotalCost
		if missingCost > 0.001 {
			premNotes = append(premNotes, fmt.Sprintf("⚠  %s from models without premium data excluded from $/premium avg", fmtCost(missingCost)))
		}
		printTable("COST PER PREMIUM REQUEST", premHeaders, premRows, premFooter, premNotes)
		fmt.Println()
	}

	// ── Daily table ─────────────────────────────────────────────────────
	dailyHeaders := []string{"Date", "Calls", "Premium", "Input", "Cached", "Output", "Hit%", "Cost", "No-Cache", "Prem Cost", "Discount"}
	var dailyRows [][]string
	dailyDays := sortedKeysStr(dailyStats)
	for _, day := range dailyDays {
		var dCalls, dUnc, dCached, dOut int
		var dPremium, dCost, dNC float64
		for model, s := range dailyStats[day] {
			dCalls += s.APICalls
			dPremium += s.PremiumRequests
			dUnc += uncachedInput(s)
			dCached += s.CacheReadTokens
			dOut += s.CompletionTokens
			dCost += calcCost(model, s, day)
			dNC += calcCostNocache(model, s, day)
		}
		dTotal := dUnc + dCached
		dPremCost := dPremium * getPremiumRequestCost(day)
		dDiscount := "-"
		if dCost > 0 {
			dDiscount = fmt.Sprintf("%.0f%%", (1-dPremCost/dCost)*100)
		}
		dailyRows = append(dailyRows, []string{
			day, commaInt(dCalls), commaFloat(dPremium, 0),
			fmtTokens(dUnc), fmtTokens(dCached),
			fmtTokens(dOut), cacheHitPct(dTotal, dCached),
			fmtCost(dCost), fmtCost(dNC), fmtCost(dPremCost), dDiscount,
		})
	}
	printTable("DAILY BREAKDOWN", dailyHeaders, dailyRows, nil, nil)
	fmt.Println()

	// ── Per-project table ───────────────────────────────────────────────
	projCosts := make(map[string][3]float64)
	for _, r := range filtered {
		cwd := ""
		if r.SessionID != "" {
			if meta, ok := sessionWorkspaces[r.Source+"\x1f"+r.SessionID]; ok {
				cwd = meta.CWD
			}
		}
		proj := "(unknown)"
		if cwd != "" {
			proj = projectName(cwd)
		}
		model := normalizeModel(r.Model)
		rs := &Stats{
			PromptTokens: r.PromptTokens, CompletionTokens: r.CompletionTokens,
			CacheCreationTokens: r.CacheCreationTokens, CacheReadTokens: r.CacheReadTokens,
		}
		c := projCosts[proj]
		c[0] += calcCost(model, rs, r.Timestamp)
		c[1] += calcCostNocache(model, rs, r.Timestamp)
		if r.IsUserTurn {
			c[2] += getPremiumMultiplier(model, r.Timestamp) * getPremiumRequestCost(r.Timestamp)
		}
		projCosts[proj] = c
	}

	projHeaders := []string{"Project", "Calls", "Premium", "Input", "Cached", "Output", "Cost", "Prem Cost"}
	// Sort projects by no-cache cost descending
	type projEntry struct {
		name string
		s    *Stats
	}
	var projList []projEntry
	for name, s := range projectStats {
		projList = append(projList, projEntry{name, s})
	}
	sort.Slice(projList, func(i, j int) bool {
		return projCosts[projList[i].name][1] > projCosts[projList[j].name][1]
	})

	var projRows [][]string
	for _, pe := range projList {
		s := pe.s
		pc := projCosts[pe.name]
		projRows = append(projRows, []string{
			pe.name, commaInt(s.APICalls), commaFloat(s.PremiumRequests, 0),
			fmtTokens(uncachedInput(s)),
			fmtTokens(s.CacheReadTokens), fmtTokens(s.CompletionTokens),
			fmtCost(pc[0]), fmtCost(pc[2]),
		})
	}
	printTable("PER-PROJECT BREAKDOWN", projHeaders, projRows, nil, nil)
	fmt.Println()

	// ── Pricing reference ───────────────────────────────────────────────
	priceHeaders := []string{"Model", "Input/1M", "Output/1M", "Cache Read/1M", "Cache Write/1M"}
	var usedList []string
	for m := range modelStats {
		usedList = append(usedList, m)
	}
	sort.Strings(usedList)

	var priceRows [][]string
	for _, model := range usedList {
		p := getPricing(model, "")
		if p != nil {
			priceRows = append(priceRows, []string{
				model,
				fmt.Sprintf("$%.2f", p.Input),
				fmt.Sprintf("$%.2f", p.Output),
				fmt.Sprintf("$%.3f", p.CacheRead),
				fmt.Sprintf("$%.2f", p.CacheWrite),
			})
		} else {
			priceRows = append(priceRows, []string{model, "N/A", "N/A", "N/A", "N/A"})
		}
	}
	printTable("PRICING REFERENCE", priceHeaders, priceRows, nil, nil)
	fmt.Println()
	fmt.Println("  ⚠  Estimated API-equivalent costs. Copilot subscriptions include token usage.")
	fmt.Println()
}
