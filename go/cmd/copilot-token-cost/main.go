package main

import (
	"context"
	"fmt"
	"os"

	"copilot-token-cost/internal/costing"
	httplayer "copilot-token-cost/internal/http"
	"copilot-token-cost/internal/parsing"
	"copilot-token-cost/internal/storage"
	syncservice "copilot-token-cost/internal/sync"
	"copilot-token-cost/internal/web"
)

func main() {
	parser := parsing.NewService()
	storageService := storage.NewService(nil)
	syncService := syncservice.NewService(storageService, parser)
	costingService := costing.NewService()
	webService := web.NewService(syncService, costingService)
	httpService := httplayer.NewService(webService)
	_, _ = webService.Snapshot(context.Background(), "", "cmd")
	_ = httpService
	fmt.Fprintln(os.Stderr, "copilot-token-cost cmd scaffold initialized; legacy CLI remains at go/main.go")
}
