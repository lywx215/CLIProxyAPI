package geminienterprise

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

const AccountVersion = 1

// Collection is one independently configurable inference binding in an account.
type Collection struct {
	ID       string `json:"binding_id"`
	Revision uint64 `json:"revision"`
	ResourceBinding
	ProjectName   string         `json:"project_name,omitempty"`
	Disabled      bool           `json:"disabled"`
	Missing       bool           `json:"missing,omitempty"`
	Settings      map[string]any `json:"settings,omitempty"`
	LegacyAliases []string       `json:"legacy_aliases,omitempty"`
}

// Account owns a single OAuth authorization and its collection graph.
type Account struct {
	Version       int            `json:"version"`
	ID            string         `json:"account_id"`
	Revision      uint64         `json:"revision"`
	Collections   []Collection   `json:"collections"`
	Discovery     map[string]any `json:"discovery,omitempty"`
	MergedSources []string       `json:"merged_sources,omitempty"`
}

// AccountID must be called with an email verified by Google's userinfo endpoint.
// Resource choices and the local secret reference do not affect the identity.
func AccountID(email, clientID string) string {
	email, clientID = strings.ToLower(strings.TrimSpace(email)), strings.TrimSpace(clientID)
	if email == "" || clientID == "" {
		return ""
	}
	return accountDigest("account", email, clientID)
}

// CollectionID excludes engine and assistant choices. Discovery supplies numeric
// project numbers; legacy project IDs remain readable until explicitly resolved.
func CollectionID(resources ResourceBinding) string {
	resources = canonicalCollectionResources(resources)
	if !resourcePart.MatchString(resources.ProjectID) || !resourcePart.MatchString(resources.Location) || !resourcePart.MatchString(resources.CollectionID) {
		return ""
	}
	return accountDigest("collection", resources.ProjectID, resources.Location, resources.CollectionID)
}

func accountDigest(kind string, parts ...string) string {
	data, _ := json.Marshal(append([]string{"gemini-enterprise", kind}, parts...))
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func canonicalCollectionResources(resources ResourceBinding) ResourceBinding {
	resources.ProjectID = strings.TrimPrefix(strings.TrimSpace(resources.ProjectID), "projects/")
	if resources.ProjectID != "" && strings.Trim(resources.ProjectID, "0123456789") == "" {
		resources.ProjectID = strings.TrimLeft(resources.ProjectID, "0")
		if resources.ProjectID == "" {
			resources.ProjectID = "0"
		}
	}
	resources.Location = strings.TrimSpace(resources.Location)
	resources.CollectionID = strings.TrimSpace(resources.CollectionID)
	resources.EngineID = strings.TrimSpace(resources.EngineID)
	resources.AssistantID = strings.TrimSpace(resources.AssistantID)
	if resources.Location == "" {
		resources.Location = "global"
	}
	if resources.CollectionID == "" {
		resources.CollectionID = "default_collection"
	}
	if resources.AssistantID == "" {
		resources.AssistantID = "default_assistant"
	}
	return resources
}

// ReadAccount returns nil for legacy metadata and never returns shared maps.
func ReadAccount(metadata map[string]any) (*Account, error) {
	raw, exists := metadata["enterprise_account"]
	if !exists {
		return nil, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("gemini-enterprise: invalid account metadata")
	}
	var account Account
	if string(data) == "null" || json.Unmarshal(data, &account) != nil || account.Version != AccountVersion || account.ID == "" || account.Revision == 0 || len(account.Collections) > 10000 {
		return nil, fmt.Errorf("gemini-enterprise: invalid account metadata")
	}
	if expected := AccountID(String(metadata, "email"), String(metadata, "client_id")); expected != "" && account.ID != expected {
		return nil, fmt.Errorf("gemini-enterprise: account metadata identity mismatch")
	}
	seen := make(map[string]bool, len(account.Collections))
	for i := range account.Collections {
		collection := &account.Collections[i]
		if collection.ID == "" || collection.ID != CollectionID(collection.ResourceBinding) || collection.Revision == 0 || seen[collection.ID] || !resourcePart.MatchString(collection.EngineID) || !resourcePart.MatchString(collection.AssistantID) {
			return nil, fmt.Errorf("gemini-enterprise: invalid collection metadata")
		}
		seen[collection.ID] = true
	}
	return &account, nil
}

// PutAccount preserves unknown top-level metadata and clones nested mutable state.
// OAuth tokens are deliberately never moved into the account graph.
func PutAccount(metadata map[string]any, account Account) map[string]any {
	copyMetadata, _ := cloneAccountValue(metadata).(map[string]any)
	if copyMetadata == nil {
		copyMetadata = make(map[string]any)
	}
	copyMetadata["enterprise_account"] = CloneAccount(account)
	return copyMetadata
}

// AccountFromLegacy adapts a single binding without changing the source metadata.
// The parent's disabled flag is not copied to the child, so parent off/on retains
// the independent collection preference.
func AccountFromLegacy(metadata map[string]any) (Account, error) {
	if account, err := ReadAccount(metadata); err != nil {
		return Account{}, err
	} else if account != nil {
		return *account, nil
	}
	id := AccountID(String(metadata, "email"), String(metadata, "client_id"))
	if id == "" {
		return Account{}, fmt.Errorf("gemini-enterprise: account email and OAuth client ID are required")
	}
	account := Account{Version: AccountVersion, ID: id, Revision: 1, Collections: []Collection{}}
	binding := FromMetadata(metadata)
	if binding.ProjectID == "" && binding.EngineID == "" {
		return account, nil
	}
	if err := binding.Validate(); err != nil {
		return Account{}, err
	}
	resources := canonicalCollectionResources(Resources(binding))
	collection := Collection{ID: CollectionID(resources), Revision: 1, ResourceBinding: resources}
	for _, key := range []string{"prefix", "proxy_url", "note", "weight", "headers", "priority", "excluded_models", "model_aliases", "oauth_model_alias", "disable_cooling", "request_retry", "request_scoped_errors", "credential_weight", "max_concurrent_requests"} {
		if value, ok := metadata[key]; ok {
			if collection.Settings == nil {
				collection.Settings = make(map[string]any)
			}
			collection.Settings[key] = cloneAccountValue(value)
		}
	}
	account.Collections = append(account.Collections, collection)
	return account, nil
}

func CloneAccount(account Account) Account {
	if account.Collections != nil {
		collections := make([]Collection, len(account.Collections))
		for i, collection := range account.Collections {
			collections[i] = collection
			collections[i].Settings, _ = cloneAccountValue(collection.Settings).(map[string]any)
			collections[i].LegacyAliases = append([]string(nil), collection.LegacyAliases...)
		}
		account.Collections = collections
	}
	account.Discovery, _ = cloneAccountValue(account.Discovery).(map[string]any)
	account.MergedSources = append([]string(nil), account.MergedSources...)
	return account
}

func cloneAccountValue(value any) any {
	if value == nil {
		return nil
	}
	return cloneAccountReflect(reflect.ValueOf(value)).Interface()
}

func cloneAccountReflect(value reflect.Value) reflect.Value {
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		out := reflect.New(value.Type()).Elem()
		out.Set(cloneAccountReflect(value.Elem()))
		return out
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		out := reflect.MakeMapWithSize(value.Type(), value.Len())
		iter := value.MapRange()
		for iter.Next() {
			out.SetMapIndex(iter.Key(), cloneAccountReflect(iter.Value()))
		}
		return out
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		out := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := 0; i < value.Len(); i++ {
			out.Index(i).Set(cloneAccountReflect(value.Index(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(value.Type()).Elem()
		for i := 0; i < value.Len(); i++ {
			out.Index(i).Set(cloneAccountReflect(value.Index(i)))
		}
		return out
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		out := reflect.New(value.Type().Elem())
		out.Elem().Set(cloneAccountReflect(value.Elem()))
		return out
	case reflect.Struct:
		out := reflect.New(value.Type()).Elem()
		out.Set(value)
		for i := 0; i < value.NumField(); i++ {
			if out.Field(i).CanSet() && value.Field(i).CanInterface() {
				out.Field(i).Set(cloneAccountReflect(value.Field(i)))
			}
		}
		return out
	default:
		return value
	}
}

// MergeDiscoveredCollections rebases discovery on current settings and never
// removes a collection because a project, region, or list permission was absent.
func MergeDiscoveredCollections(account Account, result DiscoveryResult) (Account, []string) {
	account = CloneAccount(account)
	if account.Version == 0 {
		account.Version = AccountVersion
	}
	if account.Revision == 0 {
		account.Revision = 1
	}
	byID := make(map[string]int, len(account.Collections))
	for i := range account.Collections {
		byID[account.Collections[i].ID] = i
	}
	added := []string{}
	removed := make(map[int]bool)
	for _, discovered := range result.Collections {
		if discovered.ID == "" || discovered.ID != CollectionID(discovered.ResourceBinding) {
			continue
		}
		i, exists := byID[discovered.ID]
		for _, alias := range discovered.LegacyAliases {
			if index, ok := byID[alias]; ok && !removed[index] {
				if !exists || index < i {
					i, exists = index, true
				}
			}
		}
		if exists {
			duplicates := make(map[int]bool)
			for _, alias := range append([]string{discovered.ID}, discovered.LegacyAliases...) {
				if index, ok := byID[alias]; ok && index != i && !removed[index] {
					duplicates[index] = true
				}
			}
			current := &account.Collections[i]
			previous, tracked := result.previous[current.ID]
			if !tracked {
				previous, tracked = result.previous[discovered.ID]
			}
			if !tracked || current.ResourceBinding == previous {
				changed := false
				if current.ID != discovered.ID || current.ResourceBinding != discovered.ResourceBinding || current.Missing != discovered.Missing {
					if current.ID != discovered.ID {
						current.LegacyAliases = appendUniqueAlias(current.LegacyAliases, current.ID)
						delete(byID, current.ID)
						current.ID = discovered.ID
						byID[current.ID] = i
					}
					current.ResourceBinding, current.Missing = discovered.ResourceBinding, discovered.Missing
					changed = true
				}
				duplicateIndices := make([]int, 0, len(duplicates))
				for index := range duplicates {
					duplicateIndices = append(duplicateIndices, index)
				}
				sort.Ints(duplicateIndices)
				for _, index := range duplicateIndices {
					source := account.Collections[index]
					baseline, wasDiscovered := result.previous[source.ID]
					if wasDiscovered && source.ResourceBinding != baseline {
						continue
					}
					for key, value := range source.Settings {
						if current.Settings == nil {
							current.Settings = make(map[string]any)
						}
						if _, alreadySet := current.Settings[key]; !alreadySet {
							current.Settings[key] = cloneAccountValue(value)
						}
					}
					for _, sourceAlias := range append([]string{source.ID}, source.LegacyAliases...) {
						if sourceAlias != current.ID {
							current.LegacyAliases = appendUniqueAlias(current.LegacyAliases, sourceAlias)
						}
					}
					removed[index], changed = true, true
					delete(byID, source.ID)
					byID[current.ID] = i
				}
				for _, alias := range discovered.LegacyAliases {
					if alias != current.ID {
						current.LegacyAliases = appendUniqueAlias(current.LegacyAliases, alias)
					}
				}
				if changed {
					current.Revision++
				}
			}
			current.ProjectName = discovered.ProjectName
			continue
		}
		discovered.Settings = nil
		discovered.LegacyAliases = nil
		discovered.Disabled, discovered.Revision = false, 1
		byID[discovered.ID] = len(account.Collections)
		account.Collections = append(account.Collections, discovered)
		added = append(added, discovered.ID)
	}
	if len(removed) > 0 {
		collections := make([]Collection, 0, len(account.Collections)-len(removed))
		for i, collection := range account.Collections {
			if !removed[i] {
				collections = append(collections, collection)
			}
		}
		account.Collections = collections
	}
	sort.SliceStable(account.Collections, func(i, j int) bool {
		return collectionResourceName(account.Collections[i].ResourceBinding) < collectionResourceName(account.Collections[j].ResourceBinding)
	})
	account.Discovery = map[string]any{"status": result.Status, "checked_at": result.CheckedAt, "projects": append([]DiscoveryProject(nil), result.Projects...), "coverage": append([]DiscoveryCoverage(nil), result.Coverage...), "warnings": append([]DiscoveryWarning(nil), result.Warnings...)}
	account.Revision++
	return account, added
}

func appendUniqueAlias(aliases []string, alias string) []string {
	if alias == "" {
		return aliases
	}
	for _, existing := range aliases {
		if existing == alias {
			return aliases
		}
	}
	return append(aliases, alias)
}

func collectionResourceName(binding ResourceBinding) string {
	return "projects/" + binding.ProjectID + "/locations/" + binding.Location + "/collections/" + binding.CollectionID
}
