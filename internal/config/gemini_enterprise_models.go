package config

import (
	"fmt"
	"regexp"
	"strings"
)

// GeminiEnterpriseModels contains server-local, revisioned application rules.
type GeminiEnterpriseModels struct {
	Revision    uint64                            `yaml:"revision" json:"revision"`
	Definitions []GeminiEnterpriseModelDefinition `yaml:"definitions" json:"definitions"`
}

type GeminiEnterpriseModelDefinition struct {
	ModelID string                        `yaml:"model_id" json:"model_id"`
	Targets []GeminiEnterpriseModelTarget `yaml:"targets" json:"targets"`
}

type GeminiEnterpriseModelTarget struct {
	AccountID    string `yaml:"account_id" json:"account_id"`
	BindingID    string `yaml:"binding_id" json:"binding_id"`
	ProjectID    string `yaml:"project_id" json:"project_id"`
	Location     string `yaml:"location" json:"location"`
	CollectionID string `yaml:"collection_id" json:"collection_id"`
	EngineID     string `yaml:"engine_id" json:"engine_id"`
	AssistantID  string `yaml:"assistant_id" json:"assistant_id"`
	LicenseMode  string `yaml:"license_mode" json:"license_mode"`
	RuleRevision uint64 `yaml:"rule_revision" json:"rule_revision"`
}

var (
	enterpriseNonTextModel  = regexp.MustCompile(`(^|[-_.])(image|video|audio|live|tts|speech|embeddings?)($|[-_.])`)
	enterpriseModelID       = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,127}$`)
	enterpriseIdentity      = regexp.MustCompile(`^[a-f0-9]{64}$`)
	enterpriseProjectNumber = regexp.MustCompile(`^[1-9][0-9]{0,29}$`)
	enterpriseModelResource = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,256}$`)
)

func (c GeminiEnterpriseModels) Validate() error {
	if c.Revision == 0 && len(c.Definitions) != 0 {
		return fmt.Errorf("Gemini Enterprise model definitions require a positive revision")
	}
	if len(c.Definitions) > 256 {
		return fmt.Errorf("too many Gemini Enterprise model definitions")
	}
	models := make(map[string]struct{}, len(c.Definitions))
	for _, definition := range c.Definitions {
		id := definition.ModelID
		if !enterpriseModelID.MatchString(id) || !strings.HasPrefix(id, "gemini-") || id == "gemini-auto" || id == "gemini-enterprise-auto" || enterpriseNonTextModel.MatchString(id) {
			return fmt.Errorf("Gemini Enterprise model definitions require exact text model IDs")
		}
		if _, duplicate := models[id]; duplicate {
			return fmt.Errorf("duplicate Gemini Enterprise model definition")
		}
		models[id] = struct{}{}
		if definition.Targets == nil || len(definition.Targets) > 10000 {
			return fmt.Errorf("Gemini Enterprise model targets require an array of up to 10000 applications")
		}
		targets := make(map[[2]string]struct{}, len(definition.Targets))
		for _, target := range definition.Targets {
			if err := target.validate(); err != nil {
				return err
			}
			key := [2]string{target.AccountID, target.BindingID}
			if _, duplicate := targets[key]; duplicate {
				return fmt.Errorf("duplicate Gemini Enterprise model target")
			}
			targets[key] = struct{}{}
		}
	}
	return nil
}

func (t GeminiEnterpriseModelTarget) validate() error {
	if !enterpriseIdentity.MatchString(t.AccountID) || !enterpriseIdentity.MatchString(t.BindingID) ||
		!enterpriseProjectNumber.MatchString(t.ProjectID) || t.RuleRevision == 0 ||
		(t.LicenseMode != "paid" && t.LicenseMode != "trial") {
		return fmt.Errorf("invalid Gemini Enterprise model target identity, license or revision")
	}
	switch t.Location {
	case "global", "us", "eu", "ca", "au", "in":
	default:
		return fmt.Errorf("invalid Gemini Enterprise model target location")
	}
	for _, part := range []string{t.CollectionID, t.EngineID, t.AssistantID} {
		if !enterpriseModelResource.MatchString(part) || part == "." || part == ".." {
			return fmt.Errorf("invalid Gemini Enterprise model target resource")
		}
	}
	return nil
}

func (c GeminiEnterpriseModels) Definition(model string) (GeminiEnterpriseModelDefinition, bool) {
	for _, definition := range c.Definitions {
		if definition.ModelID == model {
			return definition, true
		}
	}
	return GeminiEnterpriseModelDefinition{}, false
}
