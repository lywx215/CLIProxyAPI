package cliproxy

import (
	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func (s *Service) restoreEnterpriseCatalog(auth *coreauth.Auth) {
	if auth == nil || !s.enterpriseActive(auth, s.enterpriseKey(auth)) {
		return
	}
	d := s.enterpriseState()
	key := s.enterpriseKey(auth)
	d.mu.Lock()
	_, exists := d.catalogs[key]
	d.mu.Unlock()
	if exists {
		return
	}
	snapshot, ok := s.coreManager.StoredEnterpriseCatalog(auth)
	if !ok {
		return
	}
	d.mu.Lock()
	if _, exists = d.catalogs[key]; exists {
		d.mu.Unlock()
		return
	}
	d.sequence++
	successAt := snapshot.CheckedAt
	state := ge.CatalogState{Source: "cached", Status: "stale", LastSuccessAt: &successAt}
	catalog := enterpriseCatalog{models: snapshot.Models, known: true, revision: d.sequence, state: state}
	d.catalogs[key] = catalog
	s.coreManager.PublishEnterprisePolicyCatalog(auth, snapshot.Models)
	s.coreManager.PublishEnterpriseCatalogState(auth, state)
	d.mu.Unlock()
}
