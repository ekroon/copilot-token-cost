package costing

import (
	"regexp"
	"strings"

	"copilot-token-cost/internal/domain"
)

type Pricing struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

type PricingPeriod struct {
	EffectiveFrom      string             `json:"effective_from"`
	PremiumRequestCost float64            `json:"premium_request_cost"`
	ModelPricing       map[string]Pricing `json:"model_pricing"`
	PremiumMultiplier  map[string]float64 `json:"premium_multiplier"`
}

var (
	reCapiRouting  = regexp.MustCompile(`^capi-[a-z]+-ptuc-[a-z0-9]+(?:-ib)?-`)
	reReasonEffort = regexp.MustCompile(`:defaultReasoningEffort=\w+`)
	reDateStamp    = regexp.MustCompile(`-\d{4}-\d{2}-\d{2}$`)
)

type Service struct {
	pricingPeriods []PricingPeriod
}

func NewService() *Service { return &Service{pricingPeriods: nil} }

func (s *Service) SetPricingPeriods(periods []PricingPeriod) { s.pricingPeriods = periods }

func (s *Service) AggregateByModel(records []domain.Record) map[string]*domain.Stats {
	return AggregateByModel(records, func(model, timestamp string) float64 {
		return GetPremiumMultiplier(s.pricingPeriods, model, timestamp)
	})
}

func AggregateByModel(records []domain.Record, premiumMultiplier func(string, string) float64) map[string]*domain.Stats {
	aggregated := make(map[string]*domain.Stats)
	for _, record := range records {
		model := NormalizeModel(record.Model)
		if model == "" {
			model = "unknown"
		}
		stats := aggregated[model]
		if stats == nil {
			stats = domain.NewStats()
			aggregated[model] = stats
		}
		multiplier := 0.0
		if premiumMultiplier != nil && record.IsUserTurn {
			multiplier = premiumMultiplier(model, record.Timestamp)
		}
		stats.AddRecord(record, multiplier)
	}
	return aggregated
}

func NormalizeModel(name string) string {
	for _, prefix := range []string{"sweagent-capi:", "capi:"} {
		if strings.HasPrefix(name, prefix) {
			name = name[len(prefix):]
		}
	}
	name = reCapiRouting.ReplaceAllString(name, "")
	name = reReasonEffort.ReplaceAllString(name, "")
	name = reDateStamp.ReplaceAllString(name, "")
	return name
}

func GetPeriod(periods []PricingPeriod, timestamp string) *PricingPeriod {
	if len(periods) == 0 {
		return nil
	}
	if timestamp == "" {
		return &periods[0]
	}
	dateStr := timestamp
	if len(dateStr) > 10 {
		dateStr = dateStr[:10]
	}
	for i := range periods {
		if dateStr >= periods[i].EffectiveFrom {
			return &periods[i]
		}
	}
	return &periods[len(periods)-1]
}

func GetPremiumRequestCost(periods []PricingPeriod, timestamp string) float64 {
	period := GetPeriod(periods, timestamp)
	if period == nil {
		return 0
	}
	return period.PremiumRequestCost
}

func GetPricing(periods []PricingPeriod, model string, timestamp string) *Pricing {
	period := GetPeriod(periods, timestamp)
	if period == nil {
		return nil
	}
	n := NormalizeModel(model)
	mp := period.ModelPricing
	if p, ok := mp[n]; ok {
		return &p
	}
	for key, p := range mp {
		if strings.HasPrefix(n, key) || strings.HasPrefix(key, n) {
			cp := p
			return &cp
		}
	}
	return nil
}

func GetPremiumMultiplier(periods []PricingPeriod, model string, timestamp string) float64 {
	period := GetPeriod(periods, timestamp)
	if period == nil {
		return 1
	}
	n := NormalizeModel(model)
	mult := period.PremiumMultiplier
	if m, ok := mult[n]; ok {
		return m
	}
	for key, m := range mult {
		if strings.HasPrefix(n, key) || strings.HasPrefix(key, n) {
			return m
		}
	}
	return 1
}

func CalcCost(periods []PricingPeriod, model string, s *domain.Stats, timestamp string) float64 {
	p := GetPricing(periods, model, timestamp)
	if p == nil {
		return 0.0
	}
	netInput := s.PromptTokens - s.CacheReadTokens - s.CacheCreationTokens
	if netInput < 0 {
		netInput = 0
	}
	return float64(netInput)/1e6*p.Input +
		float64(s.CompletionTokens)/1e6*p.Output +
		float64(s.CacheReadTokens)/1e6*p.CacheRead +
		float64(s.CacheCreationTokens)/1e6*p.CacheWrite
}

func CalcCostNoCache(periods []PricingPeriod, model string, s *domain.Stats, timestamp string) float64 {
	p := GetPricing(periods, model, timestamp)
	if p == nil {
		return 0.0
	}
	return float64(s.PromptTokens)/1e6*p.Input +
		float64(s.CompletionTokens)/1e6*p.Output
}

func SumDailyCost(model string, dailyStats map[string]map[string]*domain.Stats, costFn func(string, *domain.Stats, string) float64) float64 {
	var total float64
	for day, models := range dailyStats {
		if s, ok := models[model]; ok {
			total += costFn(model, s, day)
		}
	}
	return total
}

func SumDailyPremiumCost(periods []PricingPeriod, model string, dailyStats map[string]map[string]*domain.Stats) float64 {
	var total float64
	for day, models := range dailyStats {
		if s, ok := models[model]; ok {
			total += s.PremiumRequests * GetPremiumRequestCost(periods, day)
		}
	}
	return total
}

func SumDailyPremiumCostAll(periods []PricingPeriod, dailyStats map[string]map[string]*domain.Stats) float64 {
	var total float64
	for day, models := range dailyStats {
		prc := GetPremiumRequestCost(periods, day)
		for _, s := range models {
			total += s.PremiumRequests * prc
		}
	}
	return total
}

func UncachedInput(s *domain.Stats) int {
	v := s.PromptTokens - s.CacheReadTokens - s.CacheCreationTokens
	if v < 0 {
		return 0
	}
	return v
}
