package httplayer

import (
	"encoding/json"
	"net/http"

	"copilot-token-cost/internal/domain"
	"copilot-token-cost/internal/web"
)

type Service struct {
	web *web.Service
}

func NewService(webService *web.Service) *Service {
	return &Service{web: webService}
}

func (s *Service) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", s.HandleHealthz)
	mux.HandleFunc("/api/snapshot", s.HandleSnapshot)
}

func (s *Service) HandleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Service) HandleSnapshot(w http.ResponseWriter, r *http.Request) {
	models := map[string]*domain.Stats{}
	if s.web != nil {
		snapshot, err := s.web.Snapshot(r.Context(), "", "http")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		models = snapshot
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]map[string]*domain.Stats{"models": models})
}
