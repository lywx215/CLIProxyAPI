package geminienterprise

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

const (
	CarbonEnabledKey         = "carbon_enabled"
	CarbonEvidenceKey        = "carbon_test_evidence"
	CarbonEvidenceStoreKey   = "enterprise_carbon_evidence"
	HydrogenEnabledKey       = "hydrogen_enabled"
	HydrogenEvidenceKey      = "hydrogen_test_evidence"
	HydrogenEvidenceStoreKey = "enterprise_hydrogen_evidence"
)

// AdmissionModel identifies independently stored application settings and evidence.
type AdmissionModel struct {
	ID               string
	Label            string
	EnabledKey       string
	EvidenceKey      string
	EvidenceStoreKey string
}

var CarbonAdmissionModel = AdmissionModel{CarbonModel, "Carbon", CarbonEnabledKey, CarbonEvidenceKey, CarbonEvidenceStoreKey}
var HydrogenAdmissionModel = AdmissionModel{HydrogenModel, "Hydrogen", HydrogenEnabledKey, HydrogenEvidenceKey, HydrogenEvidenceStoreKey}

// AdmissionModels returns the supported model identities, including legacy Carbon.
func AdmissionModels() []AdmissionModel {
	return []AdmissionModel{CarbonAdmissionModel, HydrogenAdmissionModel}
}

func AdmissionModelForID(id string) (AdmissionModel, bool) {
	for _, model := range AdmissionModels() {
		if id == model.ID {
			return model, true
		}
	}
	return AdmissionModel{}, false
}

// CarbonTestSummary contains only safe, server-observed test information.
type CarbonTestSummary struct {
	TestID        string    `json:"test_id"`
	Status        string    `json:"status"`
	CheckedAt     time.Time `json:"checked_at"`
	UpstreamModel string    `json:"upstream_model,omitempty"`
	HTTPStatus    int       `json:"http_status"`
	Reason        string    `json:"reason,omitempty"`
}

// CarbonTestEvidence persists independently from collection edit revisions.
type CarbonTestEvidence struct {
	CarbonTestSummary
	Version                  int    `json:"version"`
	AuthorizationFingerprint string `json:"authorization_fingerprint"`
	BindingFingerprint       string `json:"binding_fingerprint"`
	ConfigFingerprint        string `json:"config_fingerprint"`
}

func (e *CarbonTestEvidence) Summary() *CarbonTestSummary {
	if e == nil {
		return nil
	}
	summary := e.CarbonTestSummary
	return &summary
}

type CarbonEvidenceFingerprint struct {
	Authorization string
	Binding       string
	Configuration string
}

type CarbonEvidenceState struct {
	Test        *CarbonTestEvidence
	Valid       bool
	NeedsReview bool
	Reason      string
}

type CarbonSettings struct {
	Enabled  bool
	Evidence *CarbonTestEvidence
}

// CarbonAdmission exposes the effective switch and credential eligibility.
type CarbonAdmission struct {
	Enabled              bool               `json:"enabled"`
	Available            bool               `json:"available"`
	CanTest              bool               `json:"can_test"`
	CanEnable            bool               `json:"can_enable"`
	VerificationRequired bool               `json:"verification_required"`
	Reason               string             `json:"reason,omitempty"`
	Test                 *CarbonTestSummary `json:"test,omitempty"`
}

func (model AdmissionModel) ReadEnabled(metadata map[string]any) (bool, error) {
	if value, exists := metadata[model.EnabledKey]; exists {
		enabled, ok := value.(bool)
		if !ok {
			return false, fmt.Errorf("%s must be a boolean", model.EnabledKey)
		}
		return enabled, nil
	}
	return false, nil
}

func (model AdmissionModel) ReadSettings(metadata map[string]any) (CarbonSettings, error) {
	enabled, err := model.ReadEnabled(metadata)
	if err != nil {
		return CarbonSettings{}, err
	}
	settings := CarbonSettings{Enabled: enabled}
	value, exists := metadata[model.EvidenceKey]
	if !exists && metadata[model.EvidenceStoreKey] != nil {
		value, exists = model.ReadEvidenceStore(metadata)[CarbonBindingFingerprint(FromMetadata(metadata))]
	}
	if exists && value != nil {
		data, err := json.Marshal(value)
		var evidence CarbonTestEvidence
		if err != nil || len(data) > 8192 || json.Unmarshal(data, &evidence) != nil || !model.ValidEvidence(&evidence) {
			return settings, fmt.Errorf("invalid server-owned %s test evidence", model.Label)
		}
		settings.Evidence = &evidence
	}
	return settings, nil
}

func (model AdmissionModel) ReadEvidenceStore(metadata map[string]any) map[string]any {
	result := map[string]any{}
	if raw := metadata[model.EvidenceStoreKey]; raw != nil {
		data, err := json.Marshal(raw)
		if err == nil && len(data) <= 16*1024*1024 {
			_ = json.Unmarshal(data, &result)
		}
	}
	if result == nil {
		return map[string]any{}
	}
	return result
}

func (model AdmissionModel) ValidEvidence(evidence *CarbonTestEvidence) bool {
	if evidence == nil || evidence.Version != 1 || evidence.TestID == "" || len(evidence.TestID) > 128 || evidence.CheckedAt.IsZero() || evidence.HTTPStatus < 0 || evidence.HTTPStatus > 599 {
		return false
	}
	switch evidence.Status {
	case "verified", "failed", "incomplete", "needs_review":
	default:
		return false
	}
	if len(evidence.UpstreamModel) > 128 || len(evidence.Reason) > 96 {
		return false
	}
	for _, fingerprint := range []string{evidence.AuthorizationFingerprint, evidence.BindingFingerprint, evidence.ConfigFingerprint} {
		if len(fingerprint) != 64 {
			return false
		}
		if _, err := hex.DecodeString(fingerprint); err != nil {
			return false
		}
	}
	return evidence.Status != "verified" || evidence.UpstreamModel == model.ID && evidence.HTTPStatus == 200 && evidence.Reason == ""
}

// CarbonAuthorizationFingerprint requires a stable OAuth refresh authorization.
// Short-lived access tokens are never used or returned.
func CarbonAuthorizationFingerprint(metadata map[string]any) string {
	clientID := strings.TrimSpace(String(metadata, "client_id"))
	principal := AccountID(String(metadata, "email"), clientID)
	material := String(metadata, "refresh_token")
	if clientID == "" || principal == "" || material == "" {
		return ""
	}
	return CarbonFingerprintValue([]string{"carbon-authorization-v1", principal, clientID, material})
}

func CarbonBindingFingerprint(binding config.GeminiEnterpriseOAuth) string {
	if binding.Validate() != nil {
		return ""
	}
	return CarbonFingerprintValue(Resources(binding.WithDefaults()))
}

func CarbonFingerprintValue(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (model AdmissionModel) EvidenceMatches(evidence *CarbonTestEvidence, fingerprint CarbonEvidenceFingerprint) bool {
	return model.ValidEvidence(evidence) && evidence.Status == "verified" &&
		fingerprint.Authorization != "" && fingerprint.Binding != "" && fingerprint.Configuration != "" &&
		evidence.AuthorizationFingerprint == fingerprint.Authorization && evidence.BindingFingerprint == fingerprint.Binding && evidence.ConfigFingerprint == fingerprint.Configuration
}

// Legacy entry points retain the Carbon metadata contract.
func ReadCarbonEnabled(metadata map[string]any) (bool, error) {
	return CarbonAdmissionModel.ReadEnabled(metadata)
}
func ReadCarbonSettings(metadata map[string]any) (CarbonSettings, error) {
	return CarbonAdmissionModel.ReadSettings(metadata)
}
func ReadCarbonEvidenceStore(metadata map[string]any) map[string]any {
	return CarbonAdmissionModel.ReadEvidenceStore(metadata)
}
func ValidCarbonEvidence(evidence *CarbonTestEvidence) bool {
	return CarbonAdmissionModel.ValidEvidence(evidence)
}
func CarbonEvidenceMatches(evidence *CarbonTestEvidence, fingerprint CarbonEvidenceFingerprint) bool {
	return CarbonAdmissionModel.EvidenceMatches(evidence, fingerprint)
}
