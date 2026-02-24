package web

import (
	"context"

	"copilot-token-cost/internal/costing"
	"copilot-token-cost/internal/domain"
	syncservice "copilot-token-cost/internal/sync"
)

type Service struct {
	sync    *syncservice.Service
	costing *costing.Service
}

func NewService(syncService *syncservice.Service, costingService *costing.Service) *Service {
	if costingService == nil {
		costingService = costing.NewService()
	}
	return &Service{sync: syncService, costing: costingService}
}

func (s *Service) Snapshot(ctx context.Context, content, source string) (map[string]*domain.Stats, error) {
	if s.sync != nil {
		if err := s.sync.Refresh(ctx); err != nil {
			return nil, err
		}
	}
	records := []domain.Record{}
	if s.sync != nil {
		records = s.sync.ParseLogContent(content, source)
	}
	return s.costing.AggregateByModel(records), nil
}
