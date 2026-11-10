package config

import (
	"fmt"

	log "github.com/sirupsen/logrus"
)

// GeminiEnterpriseWebInputLimits bounds request parsing independently of upstream quotas.
// Zero values select defaults; these limits cannot disable admission accounting.
type GeminiEnterpriseWebInputLimits struct {
	// Retained for old YAML and configuration updates. Admission ignores these.
	MaxContents              int   `yaml:"max-contents,omitempty" json:"-"`
	MaxSegments              int   `yaml:"max-segments,omitempty" json:"-"`
	MaxTextBytes             int64 `yaml:"max-text-bytes" json:"max-text-bytes"`
	MaxUserParts             int   `yaml:"max-user-parts" json:"max-user-parts"`
	MaxModelParts            int   `yaml:"max-model-parts" json:"max-model-parts"`
	MaxSystemParts           int   `yaml:"max-system-parts" json:"max-system-parts"`
	MaxAttachments           int   `yaml:"max-attachments" json:"max-attachments"`
	MaxAttachmentBytes       int64 `yaml:"max-attachment-bytes" json:"max-attachment-bytes"`
	MaxRequestBytes          int64 `yaml:"max-request-bytes" json:"max-request-bytes"`
	MaxJSONObjectFields      int   `yaml:"max-json-object-fields" json:"max-json-object-fields"`
	MaxToolDepth             int   `yaml:"max-tool-depth" json:"max-tool-depth"`
	MaxToolNodes             int   `yaml:"max-tool-nodes" json:"max-tool-nodes"`
	MaxToolArrayEntries      int   `yaml:"max-tool-array-entries" json:"max-tool-array-entries"`
	MaxGenerationConfigBytes int64 `yaml:"max-generation-config-bytes" json:"max-generation-config-bytes"`
}

func (l GeminiEnterpriseWebInputLimits) WithDefaults() GeminiEnterpriseWebInputLimits {
	l.MaxContents, l.MaxSegments = 0, 0
	if l.MaxTextBytes == 0 {
		l.MaxTextBytes = 4 << 20
	}
	if l.MaxUserParts == 0 {
		l.MaxUserParts = 256
	}
	if l.MaxModelParts == 0 {
		l.MaxModelParts = 4096
	}
	if l.MaxSystemParts == 0 {
		l.MaxSystemParts = 256
	}
	if l.MaxAttachments == 0 {
		l.MaxAttachments = 12
	}
	if l.MaxAttachmentBytes == 0 {
		l.MaxAttachmentBytes = 50 << 20
	}
	if l.MaxRequestBytes == 0 {
		l.MaxRequestBytes = 68 << 20
	}
	if l.MaxJSONObjectFields == 0 {
		l.MaxJSONObjectFields = 256
	}
	if l.MaxToolDepth == 0 {
		l.MaxToolDepth = 64
	}
	if l.MaxToolNodes == 0 {
		l.MaxToolNodes = 100000
	}
	if l.MaxToolArrayEntries == 0 {
		l.MaxToolArrayEntries = 4096
	}
	if l.MaxGenerationConfigBytes == 0 {
		l.MaxGenerationConfigBytes = 1 << 20
	}
	return l
}

func (l GeminiEnterpriseWebInputLimits) Validate() error {
	l = l.WithDefaults()
	for _, bound := range []struct {
		name           string
		value, maximum int64
	}{
		{"max-text-bytes", l.MaxTextBytes, 64 << 20},
		{"max-user-parts", int64(l.MaxUserParts), 65536},
		{"max-model-parts", int64(l.MaxModelParts), 65536},
		{"max-system-parts", int64(l.MaxSystemParts), 65536},
		{"max-attachments", int64(l.MaxAttachments), 128},
		{"max-attachment-bytes", l.MaxAttachmentBytes, 512 << 20},
		{"max-request-bytes", l.MaxRequestBytes, 1 << 30},
		{"max-json-object-fields", int64(l.MaxJSONObjectFields), 4096},
		{"max-tool-depth", int64(l.MaxToolDepth), 256},
		{"max-tool-nodes", int64(l.MaxToolNodes), 1000000},
		{"max-tool-array-entries", int64(l.MaxToolArrayEntries), 65536},
		{"max-generation-config-bytes", l.MaxGenerationConfigBytes, 64 << 20},
	} {
		if bound.value < 1 || bound.value > bound.maximum {
			return fmt.Errorf("gemini-enterprise-web input-limits.%s must be within 1..%d", bound.name, bound.maximum)
		}
	}
	return nil
}

func (l GeminiEnterpriseWebInputLimits) warnDeprecatedHistoryLimits() {
	if l.MaxContents != 0 || l.MaxSegments != 0 {
		log.Warn("gemini-enterprise-web ignores deprecated input-limits.max-contents and max-segments")
	}
}
