package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"golang.org/x/oauth2"
)

func (m *Manager) enterpriseRuntime(ctx context.Context) (*coreauth.Manager, error) {
	if m.enterpriseManager != nil {
		return m.enterpriseManager, nil
	}
	runtime := coreauth.NewManager(m.store, nil, nil)
	if err := runtime.Load(ctx); err != nil {
		return nil, err
	}
	return runtime, nil
}

func (m *Manager) saveEnterpriseAuthorization(ctx context.Context, record *coreauth.Auth) (*coreauth.Auth, string, error) {
	runtime, err := m.enterpriseRuntime(ctx)
	if err != nil {
		return record, "", err
	}
	saved, err := runtime.ReauthorizeEnterpriseAccount(coreauth.WithAuthCreationIntent(ctx), record)
	if err != nil {
		return record, "", err
	}
	path := saved.Attributes[coreauth.AttributePath]
	if path == "" {
		path = saved.FileName
	}
	if path == "" {
		path = saved.ID
	}
	return saved, path, nil
}

func (m *Manager) saveEnterpriseDiscovery(ctx context.Context, base, proposed *coreauth.Auth) (*coreauth.Auth, error) {
	if !coreauth.IsEnterpriseAccountAuth(proposed) {
		_, err := m.store.Save(coreauth.WithAuthCreationIntent(ctx), proposed)
		return proposed, err
	}
	baseline, errBase := ge.ReadAccount(base.Metadata)
	discovered, errDiscovery := ge.ReadAccount(proposed.Metadata)
	if errBase != nil || errDiscovery != nil || baseline == nil || discovered == nil {
		return nil, coreauth.ErrEnterpriseAccountChanged
	}
	for attempt := 0; attempt < 3; attempt++ {
		runtime, err := m.enterpriseRuntime(ctx)
		if err != nil {
			return nil, err
		}
		current, _, ok := runtime.EnterpriseAccountSnapshot(base.ID)
		if !ok {
			// Some embedding stores deliberately do not list their writes.
			_, err = m.store.Save(coreauth.WithAuthCreationIntent(ctx), proposed)
			return proposed, err
		}
		updated, err := runtime.UpdateEnterpriseAccount(ctx, current, 0, func(account *ge.Account) error {
			mergeEnterprisePostLoginGraph(account, *baseline, *discovered)
			return nil
		})
		if !errors.Is(err, coreauth.ErrEnterpriseAccountChanged) || m.enterpriseManager != nil {
			return updated, err
		}
	}
	return nil, coreauth.ErrEnterpriseAccountChanged
}

func mergeEnterprisePostLoginGraph(current *ge.Account, baseline, discovered ge.Account) {
	previous := make(map[string]ge.Collection)
	for _, collection := range baseline.Collections {
		previous[collection.ID] = collection
	}
	for _, proposed := range discovered.Collections {
		index := -1
		for i, collection := range current.Collections {
			if collection.ID == proposed.ID || containsEnterpriseLoginAlias(proposed.LegacyAliases, collection.ID) {
				index = i
				break
			}
		}
		if index < 0 {
			current.Collections = append(current.Collections, proposed)
			continue
		}
		collection := &current.Collections[index]
		before, known := previous[collection.ID]
		if known && collection.ResourceBinding == before.ResourceBinding {
			collection.ResourceBinding, collection.Missing = proposed.ResourceBinding, proposed.Missing
		}
		if collection.ID != proposed.ID {
			collection.LegacyAliases = append(collection.LegacyAliases, collection.ID)
			collection.ProjectID, collection.ID = proposed.ProjectID, proposed.ID
			collection.Revision++
		}
		collection.ProjectName = proposed.ProjectName
		for _, alias := range proposed.LegacyAliases {
			if !containsEnterpriseLoginAlias(collection.LegacyAliases, alias) {
				collection.LegacyAliases = append(collection.LegacyAliases, alias)
			}
		}
	}
	current.Discovery = ge.CloneAccount(discovered).Discovery
}

func containsEnterpriseLoginAlias(aliases []string, alias string) bool {
	for _, item := range aliases {
		if item == alias {
			return true
		}
	}
	return false
}

// consolidateEnterpriseLogin runs only after an explicit OAuth authorization has
// been saved. Each source's principal is independently verified before merging.
func (m *Manager) consolidateEnterpriseLogin(ctx context.Context, cfg *config.Config, record *coreauth.Auth) (*coreauth.Auth, error) {
	if m.store == nil || !coreauth.IsEnterpriseAccountAuth(record) {
		return record, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runtime, err := m.enterpriseRuntime(ctx)
	if err != nil {
		return record, err
	}
	if _, ok := runtime.GetByID(record.ID); !ok {
		return record, nil
	}
	proxy := ""
	if cfg != nil {
		proxy = cfg.ProxyURL
	}
	if m.enterpriseManager == nil {
		runtime.RegisterExecutor(&enterpriseLoginTokenExecutor{proxy: proxy})
	}
	email, clientID := ge.String(record.Metadata, "email"), ge.String(record.Metadata, "client_id")
	var verified []string
	for _, source := range runtime.List() {
		if source.ID == record.ID || source.Provider != ge.Provider || coreauth.IsEnterpriseCollectionAuth(source) || !strings.EqualFold(ge.String(source.Metadata, "email"), email) || ge.String(source.Metadata, "client_id") != clientID {
			continue
		}
		fresh, err := runtime.EnsureEnterpriseAccountToken(ctx, source.ID)
		if err != nil {
			if ctx.Err() != nil {
				return record, ctx.Err()
			}
			continue
		}
		sourceProxy := fresh.ProxyURL
		if sourceProxy == "" {
			sourceProxy = proxy
		}
		client, errClient := ge.Client(sourceProxy)
		if errClient != nil {
			continue
		}
		principal, errPrincipal := ge.Email(ctx, client, ge.String(fresh.Metadata, "access_token"))
		client.CloseIdleConnections()
		if errPrincipal == nil && strings.EqualFold(principal, email) {
			verified = append(verified, source.ID)
		}
	}
	if len(verified) == 0 {
		return record, nil
	}
	merged, _, err := runtime.MergeEnterpriseAccounts(ctx, record.ID, verified)
	if merged != nil {
		return merged, err
	}
	return record, err
}

// This temporary manager only acquires credentials; it never executes requests.
type enterpriseLoginTokenExecutor struct{ proxy string }

func (*enterpriseLoginTokenExecutor) Identifier() string { return ge.Provider }
func (*enterpriseLoginTokenExecutor) Execute(context.Context, *coreauth.Auth, coreexec.Request, coreexec.Options) (coreexec.Response, error) {
	return coreexec.Response{}, fmt.Errorf("login credential service does not execute generation")
}
func (*enterpriseLoginTokenExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexec.Request, coreexec.Options) (*coreexec.StreamResult, error) {
	return nil, fmt.Errorf("login credential service does not execute generation")
}
func (*enterpriseLoginTokenExecutor) CountTokens(context.Context, *coreauth.Auth, coreexec.Request, coreexec.Options) (coreexec.Response, error) {
	return coreexec.Response{}, fmt.Errorf("login credential service does not count tokens")
}
func (*enterpriseLoginTokenExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("login credential service does not proxy requests")
}
func (e *enterpriseLoginTokenExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	application := ge.FromMetadata(auth.Metadata)
	oauth, err := ge.ApplicationOAuth(application, "")
	if err != nil {
		return nil, err
	}
	proxy := auth.ProxyURL
	if proxy == "" {
		proxy = e.proxy
	}
	client, err := ge.Client(proxy)
	if err != nil {
		return nil, err
	}
	defer client.CloseIdleConnections()
	credentialCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	credentialCtx = context.WithValue(credentialCtx, oauth2.HTTPClient, client)
	token, err := oauth.TokenSource(credentialCtx, &oauth2.Token{RefreshToken: ge.String(auth.Metadata, "refresh_token")}).Token()
	if err != nil {
		return nil, ge.TokenError(err)
	}
	if token.AccessToken == "" {
		return nil, fmt.Errorf("gemini-enterprise: empty refreshed access token")
	}
	updated := auth.Clone()
	ge.PutToken(updated.Metadata, token)
	return updated, nil
}
