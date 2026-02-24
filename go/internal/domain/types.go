package domain

type Record struct {
	Model               string
	PromptTokens        int
	CompletionTokens    int
	PromptText          *string
	CacheCreationTokens int
	CacheReadTokens     int
	IsUserTurn          bool
	Timestamp           string
	SessionID           string
	LogFile             string
	Source              string
}

type Stats struct {
	APICalls            int     `json:"api_calls"`
	UserTurns           int     `json:"user_turns,omitempty"`
	PromptTokens        int     `json:"prompt_tokens"`
	CompletionTokens    int     `json:"completion_tokens"`
	CacheCreationTokens int     `json:"cache_creation_tokens"`
	CacheReadTokens     int     `json:"cache_read_tokens"`
	PremiumRequests     float64 `json:"premium_requests"`
}

func NewStats() *Stats { return &Stats{} }

func (s *Stats) AddRecord(record Record, premiumMultiplier float64) {
	s.APICalls++
	s.PromptTokens += record.PromptTokens
	s.CompletionTokens += record.CompletionTokens
	s.CacheCreationTokens += record.CacheCreationTokens
	s.CacheReadTokens += record.CacheReadTokens
	if record.IsUserTurn {
		s.UserTurns++
		s.PremiumRequests += premiumMultiplier
	}
}

func MergeStats(dst, src *Stats) {
	dst.APICalls += src.APICalls
	dst.UserTurns += src.UserTurns
	dst.PromptTokens += src.PromptTokens
	dst.CompletionTokens += src.CompletionTokens
	dst.CacheCreationTokens += src.CacheCreationTokens
	dst.CacheReadTokens += src.CacheReadTokens
	dst.PremiumRequests += src.PremiumRequests
}
