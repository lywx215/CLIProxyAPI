package cliproxy

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/budgetbackup"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

type geminiBudgetWorker struct {
	mu        sync.Mutex
	cancel    context.CancelFunc
	signature string
}

func (s *Service) configureGeminiBudget(ctx context.Context, cfg *config.Config) {
	if s == nil || s.coreManager == nil || cfg == nil {
		return
	}
	engine, err := s.coreManager.EnsureGeminiBudget(ctx, false)
	if err != nil {
		log.Warn("Gemini budget ledger unavailable; managed requests are paused")
	}
	p := cfg.GeminiFreeTierPolicy.WithDefaults()
	s.geminiBudgetWorker.mu.Lock()
	defer s.geminiBudgetWorker.mu.Unlock()
	if engine == nil || !p.COSBackup.Enabled {
		if s.geminiBudgetWorker.cancel != nil {
			s.geminiBudgetWorker.cancel()
			s.geminiBudgetWorker.cancel = nil
		}
		s.geminiBudgetWorker.signature = ""
		s.coreManager.SetGeminiBudgetBackup(nil)
		return
	}
	encoded, _ := json.Marshal(p.COSBackup)
	signature := p.StoragePath + string(encoded)
	if s.geminiBudgetWorker.signature == signature && s.geminiBudgetWorker.cancel != nil {
		return
	}
	if s.geminiBudgetWorker.cancel != nil {
		s.geminiBudgetWorker.cancel()
	}
	service, err := budgetbackup.NewFromEnvironment(ctx, budgetbackup.Options{Schedule: p.COSBackup.Schedule, ObjectKey: p.COSBackup.ObjectKey}, engine)
	if err != nil {
		s.coreManager.SetGeminiBudgetBackup(nil)
		s.geminiBudgetWorker.signature = ""
		s.geminiBudgetWorker.cancel = nil
		log.Warn("Gemini budget backup unavailable; local ledger remains authoritative")
		return
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	s.geminiBudgetWorker.cancel = cancel
	s.geminiBudgetWorker.signature = signature
	s.coreManager.SetGeminiBudgetBackup(service)
	go service.Run(workerCtx)
}

func (s *Service) stopGeminiBudgetWorker() {
	s.geminiBudgetWorker.mu.Lock()
	defer s.geminiBudgetWorker.mu.Unlock()
	if s.geminiBudgetWorker.cancel != nil {
		s.geminiBudgetWorker.cancel()
		s.geminiBudgetWorker.cancel = nil
	}
	if s.coreManager != nil {
		s.coreManager.SetGeminiBudgetBackup(nil)
	}
}
