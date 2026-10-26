package geminienterprise

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

const discoveryPageLimit = 100
const discoveryItemLimit = 10000
const discoveryRequestLimit = 10000

type discoveryBudgetKey struct{}
type discoveryBudget struct{ remaining atomic.Int64 }

var accountDiscoveryLocations = []string{"global", "us", "eu"}

type DiscoveryProject struct {
	ProjectID    string `json:"project_id"`
	ProjectName  string `json:"project_name,omitempty"`
	ProjectAlias string `json:"project_alias,omitempty"`
}

type DiscoveryCoverage struct {
	ProjectID string `json:"project_id,omitempty"`
	Location  string `json:"location,omitempty"`
	Status    string `json:"status"`
	Code      string `json:"code,omitempty"`
	Message   string `json:"message,omitempty"`
}

type DiscoveryWarning struct {
	ProjectID string `json:"project_id,omitempty"`
	Location  string `json:"location,omitempty"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

type DiscoveryResult struct {
	Collections []Collection        `json:"collections"`
	Projects    []DiscoveryProject  `json:"projects"`
	Coverage    []DiscoveryCoverage `json:"coverage"`
	Warnings    []DiscoveryWarning  `json:"warnings"`
	Status      string              `json:"status"`
	CheckedAt   time.Time           `json:"checked_at"`
	previous    map[string]ResourceBinding
}

type discoveryResource struct {
	Name string `json:"name"`
}

type discoveryListError struct{ code, message string }

func (e *discoveryListError) Error() string { return "gemini-enterprise: " + e.message }

// DiscoverResources performs metadata GETs only. A bounded worker pool covers
// every active visible project in all three supported locations independently.
func DiscoverResources(ctx context.Context, client *http.Client, application config.GeminiEnterpriseOAuth, token string, existing []Collection) (DiscoveryResult, error) {
	result := DiscoveryResult{Collections: []Collection{}, Projects: []DiscoveryProject{}, Coverage: []DiscoveryCoverage{}, Warnings: []DiscoveryWarning{}, Status: "failed", CheckedAt: time.Now().UTC(), previous: make(map[string]ResourceBinding)}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := application.ValidateApplication(); err != nil {
		return result, err
	}
	if strings.TrimSpace(token) == "" {
		return result, fmt.Errorf("gemini-enterprise: an OAuth access token is required for discovery")
	}
	if client == nil {
		return result, fmt.Errorf("gemini-enterprise: a metadata HTTP client is required")
	}
	budget := &discoveryBudget{}
	budget.remaining.Store(discoveryRequestLimit)
	ctx = context.WithValue(ctx, discoveryBudgetKey{}, budget)
	projects, projectErr := discoveryProjects(ctx, client, token)
	sort.Slice(projects, func(i, j int) bool { return projects[i].ProjectID < projects[j].ProjectID })
	result.Projects = projects
	canonicalProjects := make(map[string]string, len(projects)*2)
	for _, project := range projects {
		canonicalProjects[project.ProjectID] = project.ProjectID
		if project.ProjectAlias != "" {
			canonicalProjects[project.ProjectAlias] = project.ProjectID
		}
	}
	oldByScope := make(map[string][]Collection)
	for _, collection := range CloneAccount(Account{Collections: existing}).Collections {
		if collection.ID == "" || collection.ID != CollectionID(collection.ResourceBinding) {
			continue
		}
		previous := collection.ResourceBinding
		result.previous[collection.ID] = previous
		if canonical, ok := canonicalProjects[collection.ProjectID]; ok && canonical != collection.ProjectID {
			collection.LegacyAliases = appendUniqueAlias(collection.LegacyAliases, collection.ID)
			collection.ProjectID = canonical
			collection.ID = CollectionID(collection.ResourceBinding)
			result.previous[collection.ID] = previous
		}
		key := collection.ProjectID + "/" + collection.Location
		oldByScope[key] = append(oldByScope[key], collection)
	}
	if projectErr != nil {
		warning := discoveryWarning("", "", projectErr)
		result.Warnings = append(result.Warnings, warning)
		result.Coverage = append(result.Coverage, DiscoveryCoverage{Status: "failed", Code: warning.Code, Message: warning.Message})
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	type scope struct {
		project  DiscoveryProject
		location string
	}
	jobs := make(chan scope)
	var mu sync.Mutex
	var workers sync.WaitGroup
	for i := 0; i < 3; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range jobs {
				collections, warnings := discoverScope(ctx, client, token, job.project, job.location, oldByScope[job.project.ProjectID+"/"+job.location])
				coverage := DiscoveryCoverage{ProjectID: job.project.ProjectID, Location: job.location, Status: "ok"}
				if len(warnings) > 0 {
					coverage.Status, coverage.Code, coverage.Message = "partial", warnings[0].Code, warnings[0].Message
				}
				mu.Lock()
				result.Collections = append(result.Collections, collections...)
				result.Warnings = append(result.Warnings, warnings...)
				result.Coverage = append(result.Coverage, coverage)
				mu.Unlock()
			}
		}()
	}
dispatch:
	for _, project := range projects {
		for _, location := range accountDiscoveryLocations {
			select {
			case jobs <- scope{project, location}:
			case <-ctx.Done():
				break dispatch
			}
		}
	}
	close(jobs)
	workers.Wait()
	sort.Slice(result.Collections, func(i, j int) bool {
		return collectionResourceName(result.Collections[i].ResourceBinding) < collectionResourceName(result.Collections[j].ResourceBinding)
	})
	sort.Slice(result.Coverage, func(i, j int) bool {
		a, b := result.Coverage[i], result.Coverage[j]
		return a.ProjectID+"/"+a.Location < b.ProjectID+"/"+b.Location
	})
	sort.Slice(result.Warnings, func(i, j int) bool {
		a, b := result.Warnings[i], result.Warnings[j]
		return a.ProjectID+"/"+a.Location+"/"+a.Code+"/"+a.Message < b.ProjectID+"/"+b.Location+"/"+b.Code+"/"+b.Message
	})
	if len(result.Warnings) > 1 {
		warnings := result.Warnings[:1]
		for _, warning := range result.Warnings[1:] {
			if warning != warnings[len(warnings)-1] {
				warnings = append(warnings, warning)
			}
		}
		result.Warnings = warnings
	}
	result.Status = "ok"
	if len(result.Warnings) > 0 {
		result.Status = "partial"
	}
	if projectErr != nil && len(projects) == 0 {
		result.Status = "failed"
	}
	if ctx.Err() != nil {
		result.Status = "partial"
		return result, ctx.Err()
	}
	if result.Status == "failed" {
		return result, projectErr
	}
	return result, nil
}

func discoveryProjects(ctx context.Context, client *http.Client, token string) ([]DiscoveryProject, error) {
	projects := []DiscoveryProject{}
	seen, pages := map[string]bool{}, map[string]bool{}
	pageToken := ""
	for page := 0; page < discoveryPageLimit; page++ {
		var body struct {
			Projects []struct {
				Name        string `json:"name"`
				DisplayName string `json:"displayName"`
				ProjectID   string `json:"projectId"`
				State       string `json:"state"`
			} `json:"projects"`
			Next string `json:"nextPageToken"`
		}
		endpoint := "https://cloudresourcemanager.googleapis.com/v3/projects:search?" + discoveryQuery(pageToken)
		status, err := resourceJSON(ctx, client, endpoint, token, "", &body)
		if err != nil {
			return projects, discoveryRequestError(ctx, err, "Project discovery could not be completed.")
		}
		if status != http.StatusOK {
			return projects, discoveryHTTPError(status, "Project discovery could not be completed.")
		}
		for _, project := range body.Projects {
			if project.State != "ACTIVE" {
				continue
			}
			id := strings.TrimPrefix(project.Name, "projects/")
			if !strings.HasPrefix(project.Name, "projects/") || !numericProject(id) {
				return projects, &discoveryListError{"invalid_response", "Project discovery returned an invalid project number."}
			}
			if seen[id] {
				continue
			}
			if len(projects) >= discoveryItemLimit {
				return projects, &discoveryListError{"item_limit", "Project discovery exceeded its item limit."}
			}
			seen[id] = true
			alias := project.ProjectID
			if !resourcePart.MatchString(alias) {
				alias = ""
			}
			projects = append(projects, DiscoveryProject{ProjectID: id, ProjectName: safeProjectName(project.DisplayName), ProjectAlias: alias})
		}
		if body.Next == "" {
			sort.Slice(projects, func(i, j int) bool { return projects[i].ProjectID < projects[j].ProjectID })
			return projects, nil
		}
		if len(body.Next) > 4096 || pages[body.Next] {
			return projects, &discoveryListError{"pagination_cycle", "Project discovery pagination did not complete."}
		}
		pages[body.Next], pageToken = true, body.Next
	}
	return projects, &discoveryListError{"page_limit", "Project discovery exceeded its page limit."}
}

func discoverScope(ctx context.Context, client *http.Client, token string, project DiscoveryProject, location string, existing []Collection) ([]Collection, []DiscoveryWarning) {
	collections := []Collection{}
	warnings := []DiscoveryWarning{}
	warn := func(err error) { warnings = append(warnings, discoveryWarning(project.ProjectID, location, err)) }
	parent := "projects/" + project.ProjectID + "/locations/" + location
	names, listErr := discoveryNames(ctx, client, token, project.ProjectID, location, "v1alpha", parent, "collections")
	if listErr != nil {
		warn(listErr)
	}
	oldByName := map[string]Collection{}
	for _, old := range existing {
		name := collectionResourceName(old.ResourceBinding)
		if selected, exists := oldByName[name]; exists {
			for _, alias := range old.LegacyAliases {
				selected.LegacyAliases = appendUniqueAlias(selected.LegacyAliases, alias)
			}
			oldByName[name] = selected
		} else {
			oldByName[name] = old
		}
	}
	seen := map[string]bool{}
	for _, name := range names {
		seen[name] = true
		parts := strings.Split(name, "/")
		resources := ResourceBinding{ProjectID: project.ProjectID, Location: location, CollectionID: parts[5]}
		old, hasOld := oldByName[name]
		collection, errs, usable := discoverCollection(ctx, client, token, project, resources, old, hasOld)
		for _, err := range errs {
			warn(err)
		}
		if usable {
			collections = append(collections, collection)
		}
	}
	// A list failure or missing collection never removes a saved binding. Exact
	// Engine.Get is the only fallback that can confirm an old engine is missing.
	for name, old := range oldByName {
		if seen[name] {
			continue
		}
		if listErr != nil {
			continue
		}
		collection, err := probeExistingCollection(ctx, client, token, project, old)
		if err != nil {
			warn(err)
			continue
		}
		collections = append(collections, collection)
	}
	return collections, warnings
}

func discoverCollection(ctx context.Context, client *http.Client, token string, project DiscoveryProject, resources ResourceBinding, old Collection, hasOld bool) (Collection, []error, bool) {
	parent := collectionResourceName(resources)
	engines, err := discoveryNames(ctx, client, token, resources.ProjectID, resources.Location, "v1", parent, "engines")
	sort.Strings(engines)
	errs := []error{}
	if err != nil {
		errs = append(errs, err)
	}
	assistants := make(map[string][]string)
	for _, engine := range engines {
		names, errAssistants := discoveryNames(ctx, client, token, resources.ProjectID, resources.Location, "v1", engine, "assistants")
		if errAssistants != nil {
			errs = append(errs, errAssistants)
			continue
		}
		assistants[engine] = names
	}
	if hasOld {
		old.ProjectName = project.ProjectName
		engine := parent + "/engines/" + old.EngineID
		if names, readable := assistants[engine]; readable && containsResource(names, engine+"/assistants/"+old.AssistantID) {
			old.Missing = false
			return old, errs, true
		}
		if err != nil {
			return old, errs, true
		}
		if containsResource(engines, engine) {
			if _, readable := assistants[engine]; !readable {
				return old, errs, true
			}
		}
		checked, errProbe := probeExistingCollection(ctx, client, token, project, old)
		if errProbe != nil {
			errs = append(errs, errProbe)
			return old, errs, true
		}
		return checked, errs, true
	}
	// New bindings are selected by full engine resource name, then assistant name.
	// The well-known default assistant wins over lexical order when it exists.
	for _, engine := range engines {
		names, readable := assistants[engine]
		if !readable || len(names) == 0 {
			continue
		}
		selected := names[0]
		if containsResource(names, engine+"/assistants/default_assistant") {
			selected = engine + "/assistants/default_assistant"
		}
		resources.EngineID = strings.TrimPrefix(engine, parent+"/engines/")
		resources.AssistantID = strings.TrimPrefix(selected, engine+"/assistants/")
		return Collection{ID: CollectionID(resources), Revision: 1, ResourceBinding: resources, ProjectName: project.ProjectName}, errs, true
	}
	return Collection{}, errs, false
}

func probeExistingCollection(ctx context.Context, client *http.Client, token string, project DiscoveryProject, old Collection) (Collection, error) {
	old.ProjectName = project.ProjectName
	engine := collectionResourceName(old.ResourceBinding) + "/engines/" + old.EngineID
	var body discoveryResource
	status, err := resourceJSON(ctx, client, OriginForLocation(old.Location)+"/v1/"+engine, token, project.ProjectID, &body)
	if err != nil {
		return old, discoveryRequestError(ctx, err, "Saved application could not be verified.")
	}
	if status == http.StatusNotFound {
		old.Missing = true
		return old, nil
	}
	if status != http.StatusOK {
		return old, discoveryHTTPError(status, "Saved application could not be verified.")
	}
	if body.Name != engine {
		return old, &discoveryListError{"invalid_response", "Saved application verification returned an inconsistent resource."}
	}
	assistant := engine + "/assistants/" + old.AssistantID
	status, err = resourceJSON(ctx, client, OriginForLocation(old.Location)+"/v1/"+assistant, token, project.ProjectID, &body)
	if err != nil {
		return old, discoveryRequestError(ctx, err, "Saved assistant could not be verified.")
	}
	if status == http.StatusNotFound {
		old.Missing = true
		return old, nil
	}
	if status != http.StatusOK {
		return old, discoveryHTTPError(status, "Saved assistant could not be verified.")
	}
	if body.Name != assistant {
		return old, &discoveryListError{"invalid_response", "Saved assistant verification returned an inconsistent resource."}
	}
	old.Missing = false
	return old, nil
}

func discoveryNames(ctx context.Context, client *http.Client, token, project, location, version, parent, kind string) ([]string, error) {
	names := []string{}
	seen, pages := map[string]bool{}, map[string]bool{}
	pageToken := ""
	for page := 0; page < discoveryPageLimit; page++ {
		var result struct {
			Collections []discoveryResource `json:"collections"`
			Engines     []discoveryResource `json:"engines"`
			Assistants  []discoveryResource `json:"assistants"`
			Next        string              `json:"nextPageToken"`
		}
		status, err := resourceJSON(ctx, client, OriginForLocation(location)+"/"+version+"/"+parent+"/"+kind+"?"+discoveryQuery(pageToken), token, project, &result)
		if err != nil {
			return names, discoveryRequestError(ctx, err, "Resource enumeration could not be completed.")
		}
		if status != http.StatusOK {
			return names, discoveryHTTPError(status, "Resource enumeration could not be completed.")
		}
		items := result.Collections
		if kind == "engines" {
			items = result.Engines
		} else if kind == "assistants" {
			items = result.Assistants
		}
		for _, item := range items {
			id := strings.TrimPrefix(item.Name, parent+"/"+kind+"/")
			if !strings.HasPrefix(item.Name, parent+"/"+kind+"/") || !resourcePart.MatchString(id) {
				return names, &discoveryListError{"invalid_response", "Resource enumeration returned an inconsistent resource."}
			}
			if seen[item.Name] {
				continue
			}
			if len(names) >= discoveryItemLimit {
				return names, &discoveryListError{"item_limit", "Resource enumeration exceeded its item limit."}
			}
			seen[item.Name] = true
			names = append(names, item.Name)
		}
		if result.Next == "" {
			sort.Strings(names)
			return names, nil
		}
		if len(result.Next) > 4096 || pages[result.Next] {
			return names, &discoveryListError{"pagination_cycle", "Resource enumeration pagination did not complete."}
		}
		pages[result.Next], pageToken = true, result.Next
	}
	return names, &discoveryListError{"page_limit", "Resource enumeration exceeded its page limit."}
}

func discoveryQuery(pageToken string) string {
	query := url.Values{"pageSize": {"100"}}
	if pageToken != "" {
		query.Set("pageToken", pageToken)
	}
	return query.Encode()
}

func numericProject(id string) bool {
	return id != "" && id[0] != '0' && strings.Trim(id, "0123456789") == ""
}

func safeProjectName(name string) string {
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, name))
	if len(name) > 512 {
		return ""
	}
	return name
}

func containsResource(names []string, name string) bool {
	for _, item := range names {
		if item == name {
			return true
		}
	}
	return false
}

func discoveryHTTPError(status int, message string) error {
	code := fmt.Sprintf("http_%d", status)
	switch status {
	case http.StatusUnauthorized:
		code = "unauthorized"
	case http.StatusForbidden:
		code = "permission_denied"
	case http.StatusNotFound:
		code = "not_found"
	case http.StatusTooManyRequests:
		code = "rate_limited"
	}
	return &discoveryListError{code, message}
}

func discoveryRequestError(ctx context.Context, err error, message string) error {
	if ctx.Err() != nil {
		return &discoveryListError{"cancelled", "Resource discovery was cancelled."}
	}
	if failure, ok := err.(*discoveryListError); ok {
		return failure
	}
	return &discoveryListError{"invalid_response", message}
}

func discoveryWarning(project, location string, err error) DiscoveryWarning {
	warning := DiscoveryWarning{ProjectID: project, Location: location, Code: "discovery_failed", Message: "Resource discovery could not be completed."}
	if failure, ok := err.(*discoveryListError); ok {
		warning.Code, warning.Message = failure.code, failure.message
	}
	return warning
}
