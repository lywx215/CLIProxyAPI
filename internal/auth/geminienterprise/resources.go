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

// Probe known locations with Engine.Get; end users need no list permission.
var discoveryLocations = []string{"global", "us", "eu", "ca", "in", "asia-northeast1", "sg", "europe-west2"}

type resourceProject struct {
	Name      string `json:"name"`
	ProjectID string `json:"projectId"`
	State     string `json:"state"`
}

// ResolveResources finds a unique readable engine using the authorized user's
// project visibility. It never reuses another account's saved project binding.
func ResolveResources(ctx context.Context, client *http.Client, binding config.GeminiEnterpriseOAuth, token string) (config.GeminiEnterpriseOAuth, error) {
	if err := binding.ValidateDiscovery(); err != nil {
		return binding, err
	}
	binding = binding.WithDefaults()
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
			status, errGet := resourceJSON(ctx, client, Origin+"/v1/"+Engine(candidate), token, project, &engine)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, fmt.Errorf("gemini-enterprise: invalid resource request")
	}
	if req.URL.Scheme != "https" || req.URL.User != nil || (req.URL.Host != "discoveryengine.googleapis.com" && req.URL.Host != "cloudresourcemanager.googleapis.com") {
		return 0, fmt.Errorf("gemini-enterprise: untrusted resource discovery origin")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if quotaProject != "" {
		req.Header.Set("X-Goog-User-Project", quotaProject)
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := copyClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, fmt.Errorf("gemini-enterprise: resource discovery connection failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return response.StatusCode, nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4*1024*1024+1))
	if err != nil || len(data) > 4*1024*1024 || json.Unmarshal(data, value) != nil {
		return 0, fmt.Errorf("gemini-enterprise: invalid resource discovery response")
	}
	return response.StatusCode, nil
}
