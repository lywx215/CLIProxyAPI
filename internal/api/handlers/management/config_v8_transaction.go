package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"reflect"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"gopkg.in/yaml.v3"
)

// configV8Document keeps the persisted revision separate from runtime ownership
// revisions and auth indexes. Callers hold Handler.mu throughout a transaction.
type configV8Document struct {
	Document yaml.Node
	Root     *yaml.Node
	Before   *yaml.Node
	Revision string
	Data     []byte
}

type configV8Error struct {
	Status   int
	Code     string
	Message  string
	Field    string
	Revision string
}

func (e *configV8Error) Error() string { return e.Code + ": " + e.Message }

func configV8Revision(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Strong entity tags and the opaque revision returned in JSON are accepted.
func configV8RevisionMatches(value, revision string) bool {
	for _, tag := range strings.Split(value, ",") {
		tag = strings.TrimSpace(tag)
		if tag == "*" || tag == revision || tag == `"`+revision+`"` {
			return true
		}
	}
	return false
}

func configV8Conflict(revision string) error {
	return &configV8Error{Status: http.StatusConflict, Code: "config_conflict", Revision: revision}
}

func writeConfigV8Error(c *gin.Context, err error) {
	var problem *configV8Error
	if !errors.As(err, &problem) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid_config"})
		return
	}
	body := gin.H{"error": problem.Code}
	if problem.Message != "" {
		body["message"] = problem.Message
	}
	if problem.Field != "" {
		body["field"] = problem.Field
	}
	if problem.Revision != "" {
		body["revision"] = problem.Revision
		c.Header("ETag", `"`+problem.Revision+`"`)
	}
	c.JSON(problem.Status, body)
}

func (h *Handler) readConfigV8Locked() (*configV8Document, error) {
	raw, err := os.ReadFile(h.configFilePath)
	if err != nil {
		return nil, &configV8Error{Status: http.StatusInternalServerError, Code: "read_failed"}
	}
	data, _, err := config.NormalizeConfigLayout(raw, true)
	if err != nil {
		return nil, &configV8Error{Status: http.StatusInternalServerError, Code: "invalid_config", Message: err.Error()}
	}
	state := &configV8Document{Data: data, Revision: configV8Revision(raw)}
	if err = yaml.Unmarshal(data, &state.Document); err != nil || len(state.Document.Content) == 0 || state.Document.Content[0].Kind != yaml.MappingNode {
		return nil, &configV8Error{Status: http.StatusInternalServerError, Code: "invalid_config"}
	}
	state.Root = state.Document.Content[0]
	state.Before = cloneConfigV8Node(state.Root)
	return state, nil
}

// commitConfigV8Locked validates the entire candidate before saving its canonical
// YAML once. Runtime configuration and reload generation advance only on success.
func (h *Handler) commitConfigV8Locked(ctx context.Context, state *configV8Document) (string, error) {
	if err := config.NormalizeV8ConfigAliases(state.Root); err != nil {
		return "", &configV8Error{Status: http.StatusBadRequest, Code: "invalid_config", Message: err.Error()}
	}
	for _, field := range []string{"credentials/concurrency/lifecycle-config-revision", "credentials/concurrency/observation-barrier-revision", "plugins/auth-revision"} {
		var before, after any
		if old := configV8Node(state.Before, strings.Split(field, "/")); old != nil {
			_ = old.Decode(&before)
		}
		if next := configV8Node(state.Root, strings.Split(field, "/")); next != nil {
			_ = next.Decode(&after)
		}
		if !reflect.DeepEqual(before, after) {
			return "", &configV8Error{Status: http.StatusBadRequest, Code: "read_only_field", Field: field}
		}
	}
	stripAPIKeysAuthIndexesFromRoot(state.Root)
	config.NormalizeCollectionNodeStyles(state.Root)
	data, err := yaml.Marshal(&state.Document)
	if err != nil {
		return "", &configV8Error{Status: http.StatusBadRequest, Code: "invalid_config", Message: err.Error()}
	}
	next, err := config.ParseConfigBytes(data)
	if err != nil {
		return "", &configV8Error{Status: http.StatusUnprocessableEntity, Code: "invalid_config", Message: err.Error()}
	}
	if err = validateConfigV8WithExistingExtensions(state); err != nil {
		return "", &configV8Error{Status: http.StatusBadRequest, Code: "invalid_config", Message: err.Error()}
	}
	data, _, err = config.NormalizeConfigLayout(data, true)
	if err != nil {
		return "", &configV8Error{Status: http.StatusBadRequest, Code: "invalid_config", Message: err.Error()}
	}
	data = config.NormalizeCommentIndentation(data)
	current, err := os.ReadFile(h.configFilePath)
	if err != nil {
		return "", &configV8Error{Status: http.StatusInternalServerError, Code: "read_failed"}
	}
	if revision := configV8Revision(current); revision != state.Revision {
		return "", configV8Conflict(revision)
	}
	// Retain the inode of mounted configuration files and all explicit inheritance
	// values. Runtime defaults must never be projected onto this persisted tree.
	if err = WriteConfig(h.configFilePath, data); err != nil {
		return "", &configV8Error{Status: http.StatusInternalServerError, Code: "write_failed", Message: err.Error()}
	}
	if h.cfg != nil {
		next.Home = h.cfg.Home
	}
	h.cfg = next
	snapshot := h.reloadSnapshotConfigLocked()
	h.reloadConfigAfterManagementSaveAsync(ctx, snapshot)
	state.Data = data
	state.Revision = configV8Revision(data)
	return state.Revision, nil
}
