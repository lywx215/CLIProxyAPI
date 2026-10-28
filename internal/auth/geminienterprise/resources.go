package geminienterprise

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

var resourcePart = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Bound metadata traffic across simultaneous discovery operations as well.
var resourceRequests = make(chan struct{}, 3)

// Probe known locations with Engine.Get; end users need no list permission.
var discoveryLocations = append(SupportedLocations(), "asia-northeast1", "sg", "europe-west2")

type resourceProject struct {
	Name      string `json:"name"`
	ProjectID string `json:"projectId"`
	State     string `json:"state"`
}

// ResourceBinding is the public, editable portion of a saved credential.
type ResourceBinding struct {
	ProjectID    string `json:"project_id"`
	Location     string `json:"location"`
	CollectionID string `json:"collection_id"`
	EngineID     string `json:"engine_id"`
	AssistantID  string `json:"assistant_id"`
}

func Resources(c config.GeminiEnterpriseOAuth) ResourceBinding {
	c = c.WithDefaults()
	return ResourceBinding{c.ProjectID, c.Location, c.CollectionID, c.EngineID, c.AssistantID}
}

// Apply retains the credential's OAuth application; callers cannot change it
// through the regional resource editor.
func (r ResourceBinding) Apply(c config.GeminiEnterpriseOAuth) config.GeminiEnterpriseOAuth {
	c.ProjectID, c.Location, c.CollectionID, c.EngineID, c.AssistantID = strings.TrimSpace(r.ProjectID), strings.TrimSpace(r.Location), strings.TrimSpace(r.CollectionID), strings.TrimSpace(r.EngineID), strings.TrimSpace(r.AssistantID)
	return c.WithDefaults()
}

// ValidateResources performs read-only application and catalog checks. A region
// switch never creates or migrates a Google resource, or calls a model.
func ValidateResources(ctx context.Context, client *http.Client, binding config.GeminiEnterpriseOAuth, token string) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	var engine struct {
		Name string `json:"name"`
	}
	status, err := resourceJSON(ctx, client, OriginForLocation(binding.Location)+"/v1/"+Engine(binding), token, binding.ProjectID, &engine)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("gemini-enterprise: application validation returned HTTP %d; check the region, resource IDs and access permissions", status)
	}
	if engine.Name != Engine(binding) {
		return fmt.Errorf("gemini-enterprise: Google returned an inconsistent application resource; use the project's canonical number")
	}
	_, err = Discover(ctx, client, binding, token)
	return err
}

// ResolveResources finds a unique readable engine using the authorized user's
// project visibility. It never reuses another account's saved project binding.
func ResolveResources(ctx context.Context, client *http.Client, binding config.GeminiEnterpriseOAuth, token string) (config.GeminiEnterpriseOAuth, error) {
	if err := binding.ValidateDiscovery(); err != nil {
		return binding, err
	}
	binding = binding.WithDefaults()
	if !resourcePart.MatchString(binding.EngineID) {
		return binding, fmt.Errorf("gemini-enterprise: a valid engine ID is required for legacy resource resolution")
	}
	projects, err := visibleProjects(ctx, client, token)
	if err != nil {
		return binding, err
	}
	var matches []config.GeminiEnterpriseOAuth
	for _, project := range projects {
		for _, location := range discoveryLocations {
			if err := ctx.Err(); err != nil {
				return binding, err
			}
			candidate := binding
			candidate.ProjectID, candidate.Location, candidate.CollectionID = project, location, "default_collection"
			var engine struct {
				Name string `json:"name"`
			}
			status, errGet := resourceJSON(ctx, client, OriginForLocation(location)+"/v1/"+Engine(candidate), token, project, &engine)
			if errGet != nil {
				return binding, errGet
			}
			if status == http.StatusForbidden || status == http.StatusNotFound || status == http.StatusBadRequest {
				continue
			}
			if status != http.StatusOK {
				return binding, fmt.Errorf("gemini-enterprise: application lookup returned HTTP %d; retry or enter resources manually", status)
			}
			parts := strings.Split(engine.Name, "/")
			if len(parts) != 8 || parts[0] != "projects" || parts[1] != project || parts[2] != "locations" || parts[3] != location || parts[4] != "collections" || parts[5] != "default_collection" || parts[6] != "engines" || parts[7] != binding.EngineID {
				return binding, fmt.Errorf("gemini-enterprise: Google returned an inconsistent application resource")
			}
			matches = append(matches, candidate)
			if len(matches) > 1 {
				return binding, fmt.Errorf("gemini-enterprise: this engine ID matches multiple resources; specify its project and location manually")
			}
		}
	}
	if len(matches) != 1 {
		return binding, fmt.Errorf("gemini-enterprise: no readable application matched this engine ID; check the ID or enter its project, location and collection manually")
	}
	return matches[0], nil
}

func visibleProjects(ctx context.Context, client *http.Client, token string) ([]string, error) {
	var projects []string
	seenProjects, seenPages := map[string]bool{}, map[string]bool{}
	pageToken := ""
	for page := 0; page < 100; page++ {
		query := url.Values{"pageSize": {"100"}}
		if pageToken != "" {
			query.Set("pageToken", pageToken)
		}
		var result struct {
			Projects []resourceProject `json:"projects"`
			Next     string            `json:"nextPageToken"`
		}
		status, err := resourceJSON(ctx, client, "https://cloudresourcemanager.googleapis.com/v3/projects:search?"+query.Encode(), token, "", &result)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("gemini-enterprise: project discovery returned HTTP %d; allow project read access or enter the project manually", status)
		}
		for _, project := range result.Projects {
			id := strings.TrimPrefix(project.Name, "projects/")
			if project.State != "ACTIVE" || !strings.HasPrefix(project.Name, "projects/") || !resourcePart.MatchString(id) {
				continue
			}
			if !seenProjects[id] {
				projects = append(projects, id)
				seenProjects[id] = true
			}
		}
		if result.Next == "" {
			return projects, nil
		}
		if seenPages[result.Next] {
			return nil, fmt.Errorf("gemini-enterprise: project discovery pagination did not complete")
		}
		seenPages[result.Next] = true
		pageToken = result.Next
	}
	return nil, fmt.Errorf("gemini-enterprise: project discovery exceeded its page limit; enter the project manually")
}

func resourceJSON(ctx context.Context, client *http.Client, endpoint, token, quotaProject string, value any) (int, error) {
	return resourceJSONWithDiagnostic(ctx, client, endpoint, token, quotaProject, value, false)
}

func resourceJSONDiagnostic(ctx context.Context, client *http.Client, endpoint, token, quotaProject string, value any) (int, error) {
	return resourceJSONWithDiagnostic(ctx, client, endpoint, token, quotaProject, value, true)
}

func resourceJSONWithDiagnostic(ctx context.Context, client *http.Client, endpoint, token, quotaProject string, value any, diagnostic bool) (int, error) {
	if client == nil {
		return 0, fmt.Errorf("gemini-enterprise: resource HTTP client is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, fmt.Errorf("gemini-enterprise: invalid resource request")
	}
	if req.URL.Scheme != "https" || req.URL.User != nil || req.URL.Opaque != "" || req.URL.Fragment != "" || (!OfficialResource(req) && req.URL.Host != "cloudresourcemanager.googleapis.com") {
		return 0, fmt.Errorf("gemini-enterprise: untrusted resource discovery origin")
	}
	if budget, ok := ctx.Value(discoveryBudgetKey{}).(*discoveryBudget); ok && budget.remaining.Add(-1) < 0 {
		return 0, &discoveryListError{"request_limit", "Resource discovery exceeded its request limit."}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if quotaProject != "" {
		req.Header.Set("X-Goog-User-Project", quotaProject)
	}
	copyClient := *client
	copyClient.Timeout = 0
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	select {
	case resourceRequests <- struct{}{}:
		defer func() { <-resourceRequests }()
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	response, err := copyClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, &MetadataError{Code: "connection_failed", Message: "The Google metadata connection failed."}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		if diagnostic {
			data, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
			return response.StatusCode, ParseMetadataError(response.StatusCode, data)
		}
		return response.StatusCode, nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4*1024*1024+1))
	if err != nil || len(data) > 4*1024*1024 || json.Unmarshal(data, value) != nil {
		return 0, fmt.Errorf("gemini-enterprise: invalid resource discovery response")
	}
	return response.StatusCode, nil
}
