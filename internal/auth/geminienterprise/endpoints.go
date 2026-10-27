package geminienterprise

import (
	"fmt"
	"net/http"
	"strings"
)

// SupportedLocations returns the regions supported by discovery and resource editing.
func SupportedLocations() []string {
	return []string{"global", "us", "eu", "ca", "au", "in"}
}

// SupportedLocation reports whether a location has an explicitly supported endpoint.
func SupportedLocation(location string) bool {
	switch location {
	case "global", "us", "eu", "ca", "au", "in":
		return true
	default:
		return false
	}
}

// OriginForLocation selects an explicit official endpoint. Other saved locations
// retain their legacy mapping; no hostname is derived from user input.
func OriginForLocation(location string) string {
	switch location {
	case "us":
		return "https://us-discoveryengine.googleapis.com"
	case "eu":
		return "https://eu-discoveryengine.googleapis.com"
	case "ca":
		return "https://ca-discoveryengine.googleapis.com"
	case "au":
		return "https://au-discoveryengine.googleapis.com"
	case "in":
		return "https://in-discoveryengine.googleapis.com"
	default:
		return Origin
	}
}

// OriginForResource accepts only a canonical project/location resource name.
func OriginForResource(resource string) (string, error) {
	parts := strings.Split(resource, "/")
	if len(parts) < 4 || parts[0] != "projects" || parts[2] != "locations" || !resourcePart.MatchString(parts[1]) || !resourcePart.MatchString(parts[3]) {
		return "", fmt.Errorf("gemini-enterprise: invalid regional resource")
	}
	return OriginForLocation(parts[3]), nil
}

// Official recognizes a closed set of HTTPS Google endpoints, including the
// documented global alias. Redirects are still disabled by each HTTP caller.
func Official(req *http.Request) bool {
	if req == nil || req.URL == nil || req.URL.Scheme != "https" || req.URL.User != nil || req.URL.Opaque != "" || req.URL.Fragment != "" {
		return false
	}
	switch req.URL.Host {
	case "discoveryengine.googleapis.com", "global-discoveryengine.googleapis.com", "us-discoveryengine.googleapis.com", "eu-discoveryengine.googleapis.com",
		"ca-discoveryengine.googleapis.com", "au-discoveryengine.googleapis.com", "in-discoveryengine.googleapis.com":
		return true
	default:
		return false
	}
}

func requestResource(req *http.Request) (string, bool) {
	if !Official(req) || req.URL.EscapedPath() != req.URL.Path {
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/"), "/")
	if len(parts) > 0 && parts[0] == "download" {
		parts = parts[1:]
	}
	if len(parts) < 5 || (parts[0] != "v1" && parts[0] != "v1alpha" && parts[0] != "v1beta") {
		return "", false
	}
	resource := strings.Join(parts[1:], "/")
	for _, part := range parts[1:] {
		if part == "" || part == "." || part == ".." {
			return "", false
		}
	}
	if _, err := OriginForResource(resource); err != nil {
		return "", false
	}
	return resource, true
}

// OfficialResource requires the URL host to match the resource's own location.
func OfficialResource(req *http.Request) bool {
	resource, ok := requestResource(req)
	if !ok {
		return false
	}
	origin, _ := OriginForResource(resource)
	return "https://"+req.URL.Host == origin || (origin == Origin && req.URL.Host == "global-discoveryengine.googleapis.com")
}

// OfficialBinding additionally confines execution credentials to their saved
// project and location, preventing accidental cross-region credential routing.
func OfficialBinding(req *http.Request, project, location string) bool {
	if location == "" {
		location = "global"
	}
	resource, ok := requestResource(req)
	if !ok || !OfficialResource(req) {
		return false
	}
	return strings.HasPrefix(resource+"/", "projects/"+project+"/locations/"+location+"/")
}
