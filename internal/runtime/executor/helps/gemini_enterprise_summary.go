package helps

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// enterpriseRequestSummary contains only counts and fixed schema names, never values.
type enterpriseRequestSummary struct {
	BodyBytes     int
	Parsed        bool
	QueryChars    *int
	QueryBytes    *int
	Fields        []string
	UnknownFields int
	Search        *bool
	Location      string
	Binding       string
	Revision      *uint64
}

// ObserveEnterpriseStage updates the diagnostic phase without changing execution.
func ObserveEnterpriseStage(ctx context.Context, stage string) {
	if !slices.Contains([]string{"input", "final_payload", "binding"}, stage) {
		return
	}
	if a := enterpriseAttemptFrom(ctx); a != nil {
		a.mu.Lock()
		a.phase = stage
		a.mu.Unlock()
	}
}

// ObserveEnterprisePayload must run after payload configuration. It reads only the
// finalized business payload and retains no slice or string from that payload.
func ObserveEnterprisePayload(ctx context.Context, body []byte, auth *coreauth.Auth) {
	a := enterpriseAttemptFrom(ctx)
	if a == nil {
		return
	}
	s := &enterpriseRequestSummary{BodyBytes: len(body)}
	var root gjson.Result
	if gjson.ValidBytes(body) {
		root = gjson.ParseBytes(body)
	}
	model := ""
	if root.IsObject() {
		s.Parsed, s.Fields = true, []string{}
		if finalModel := root.Get("generationSpec.modelId"); finalModel.Type == gjson.String && finalModel.Str != "" {
			model = enterpriseFingerprint(finalModel.Str)
			if enterpriseModelID.MatchString(finalModel.Str) {
				model = strings.Clone(finalModel.Str)
			}
		}
		if query := root.Get("query.text"); query.Type == gjson.String {
			chars, size := utf8.RuneCountInString(query.Str), len(query.Str)
			s.QueryChars, s.QueryBytes = &chars, &size
		}
		collectEnterpriseSchema(root, "", s)
		slices.Sort(s.Fields)
		search := root.Get("toolsSpec.webGroundingSpec").IsObject() || root.Get("webGroundingEnabled").Type == gjson.True || root.Get("googleSearchGroundingEnabled").Type == gjson.True
		s.Search = &search
	}
	if auth != nil {
		binding := ge.FromMetadata(auth.Metadata)
		if slices.Contains([]string{"global", "us", "eu"}, binding.Location) {
			s.Location = binding.Location
		}
		if binding.Validate() == nil {
			s.Binding = enterpriseFingerprint(ge.ApplicationBinding(binding))
		}
		s.Revision = enterpriseBindingRevision(auth.Metadata["enterprise_collection_revision"])
	}
	a.mu.Lock()
	a.summary = s
	if model != "" {
		a.model = model
	}
	a.mu.Unlock()
}

// The closed schema excludes arbitrary keys, even when payload rules add them.
var enterpriseSummarySchema = map[string]bool{
	"query": true, "query.text": true, "generationSpec": true, "generationSpec.modelId": true,
	"generationSpec.thinkingConfig": true, "generationSpec.thinkingConfig.thinkingLevel": true,
	"generationSpec.thinkingConfig.includeThoughts": true, "toolsSpec": true,
	"toolsSpec.webGroundingSpec": true, "toolsSpec.vertexAiSearchSpec": true,
	"toolsSpec.vertexAiSearchSpec.disabled": true, "toolsSpec.vertexAiSearchSpec.filter": true,
	"toolsSpec.imageGenerationSpec": true, "toolsSpec.videoGenerationSpec": true,
	"userMetadata": true, "userMetadata.timeZone": true, "userMetadata.preferredLanguageCode": true,
	"actionSpec": true, "actionSpec.actionDisabled": true, "isSessionLess": true,
	"languageCode": true, "googleSearchGroundingEnabled": true, "webGroundingEnabled": true,
	"answerGenerationMode": true, "assistSkippingMode": true,
}

func collectEnterpriseSchema(node gjson.Result, prefix string, s *enterpriseRequestSummary) {
	if !node.IsObject() {
		return
	}
	node.ForEach(func(key, value gjson.Result) bool {
		path := prefix + key.Str
		if strings.ContainsAny(key.Str, ".[]") || !enterpriseSummarySchema[path] {
			s.UnknownFields++
			return true
		}
		if len(s.Fields) < 64 && !slices.Contains(s.Fields, path) {
			s.Fields = append(s.Fields, strings.Clone(path))
		}
		collectEnterpriseSchema(value, path+".", s)
		return true
	})
}

func enterpriseBindingRevision(value any) *uint64 {
	var revision uint64
	switch v := value.(type) {
	case uint64:
		revision = v
	case int:
		if v > 0 {
			revision = uint64(v)
		}
	case float64:
		if v > 0 && v < math.Exp2(64) && math.Trunc(v) == v {
			revision = uint64(v)
		}
	case json.Number:
		revision, _ = strconv.ParseUint(string(v), 10, 64)
	}
	if revision == 0 {
		return nil
	}
	return &revision
}
