package management

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
	_ "time/tzdata"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/budget"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type geminiBudgetModelView struct {
	Model     string     `json:"model"`
	Limit     int        `json:"limit"`
	Used      int        `json:"used"`
	Remaining int        `json:"remaining"`
	InFlight  int        `json:"inflight"`
	State     string     `json:"state"`
	RetryAt   *time.Time `json:"retry_at,omitempty"`
	Estimated bool       `json:"estimated"`
}
type geminiBudgetProjectView struct {
	ProjectID string                  `json:"project_id"`
	KeyCount  int                     `json:"key_count"`
	Models    []geminiBudgetModelView `json:"models"`
}

func pacificBudgetDay(now time.Time) (string, time.Time) {
	zone, _ := time.LoadLocation("America/Los_Angeles")
	local := now.In(zone)
	return local.Format("2006-01-02"), time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, zone)
}

func (h *Handler) geminiBudgetView(revision string, cfg *config.Config) gin.H {
	p := cfg.GeminiFreeTierPolicy.WithDefaults()
	day, nextReset := pacificBudgetDay(time.Now())
	snap := budget.Snapshot{Day: day, State: budget.StateUninitialized}
	var engine *budget.Engine
	if h.authManager != nil {
		engine = h.authManager.GeminiBudgetEngine()
	}
	if engine != nil {
		snap = engine.Snapshot()
	}
	state := snap.State
	if p.Enabled && engine == nil {
		state = "storage_error"
	}
	if !p.Enabled && state == budget.StateReady {
		state = "disabled"
	}
	canInit := !p.Enabled && !snap.Initialized && state == budget.StateUninitialized
	if _, err := os.Stat(filepath.Join(p.StoragePath, "init.meta")); !os.IsNotExist(err) {
		canInit = false
	}
	groups := map[string]*geminiBudgetProjectView{}
	scopes := map[string]map[string][]budget.Scope{}
	if h.authManager != nil {
		for _, auth := range h.authManager.List() {
			if !coreauth.IsOfficialGeminiAuth(auth) {
				continue
			}
			project := coreauth.GeminiBudgetProject(auth)
			row := groups[project]
			if row == nil {
				row = &geminiBudgetProjectView{ProjectID: project, Models: []geminiBudgetModelView{}}
				groups[project] = row
				scopes[project] = map[string][]budget.Scope{}
			}
			row.KeyCount++
			if !auth.Disabled {
				for _, registered := range registry.GetGlobalRegistry().GetModelsForClient(auth.ID) {
					real := coreauth.ResolveCanonicalBudgetModel(registered.ID, auth, cfg)
					scopes[project][real] = append(scopes[project][real], coreauth.GeminiBudgetScope(auth, real))
				}
			}
		}
	}
	estimated := false
	for project, row := range groups {
		for model, limit := range p.ManagedModels {
			view := geminiBudgetModelView{Model: model, Limit: limit, Remaining: limit, State: "available"}
			if engine != nil {
				engineView := engine.View(budget.Scope{Project: project, Model: model, Origin: coreauth.GeminiOfficialOrigin})
				// One quarantined credential must not hide other usable keys in its project.
				for i, scope := range scopes[project][model] {
					candidate := engine.View(scope)
					if i == 0 || candidate.Available || geminiBudgetViewRank(candidate.State) > geminiBudgetViewRank(engineView.State) {
						engineView = candidate
					}
					if engineView.Available {
						break
					}
				}
				view.InFlight = engineView.InFlight
				view.Used = engineView.Used
				view.Limit = engineView.Limit
				view.Remaining = max(0, view.Limit-view.Used)
				view.Estimated = engineView.Estimated
				view.State = engineView.State
				if view.State == "ready" {
					view.State = "available"
				}
				if !engineView.RetryAt.IsZero() {
					until := engineView.RetryAt
					view.RetryAt = &until
				}
			}
			estimated = estimated || view.Estimated
			if view.InFlight > 0 && view.State == "available" {
				view.State = "inflight"
			}
			if len(scopes[project][model]) == 0 {
				view.State = "not_routable"
			}
			if !p.Enabled {
				view.State = "disabled"
			} else if state != budget.StateReady {
				view.State = state
				view.Remaining = 0
				if !snap.FreezeUntil.IsZero() {
					until := snap.FreezeUntil
					view.RetryAt = &until
				}
			}
			row.Models = append(row.Models, view)
		}
		sort.Slice(row.Models, func(i, j int) bool { return row.Models[i].Model < row.Models[j].Model })
	}
	projects := make([]geminiBudgetProjectView, 0, len(groups))
	for _, row := range groups {
		projects = append(projects, *row)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].ProjectID < projects[j].ProjectID })
	backupState := "never"
	if !p.COSBackup.Enabled {
		backupState = "disabled"
	} else if snap.Backup.Attempted {
		backupState = "pending"
		if snap.Backup.Success {
			backupState = "success"
		} else if snap.Backup.Message != "" {
			backupState = "failed"
		}
	}
	var completedAt any
	if snap.Backup.Success {
		completedAt = timeOrNil(snap.Backup.At)
	}
	return gin.H{"revision": revision, "enabled": p.Enabled, "engine_state": state, "initialized": snap.Initialized, "can_initialize": canInit, "pacific_date": snap.Day, "next_reset": nextReset, "estimated": estimated, "projects": projects, "backup_enabled": p.COSBackup.Enabled, "last_backup": gin.H{"state": backupState, "date": snap.Backup.Day, "attempted_at": timeOrNil(snap.Backup.At), "completed_at": completedAt}}
}

func geminiBudgetViewRank(state string) int {
	switch state {
	case "ready":
		return 5
	case "busy":
		return 4
	case "cooldown", "daily_exhausted", "circuit_open":
		return 3
	case "model_restricted":
		return 2
	case "quarantined":
		return 1
	default:
		return 0
	}
}

func timeOrNil(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

// GetGeminiBudget is a read-only view; it does not open a new ledger or call Google/COS.
func (h *Handler) GetGeminiBudget(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	state, err := h.readConfigV8Locked()
	if err != nil {
		writeConfigV8Error(c, err)
		return
	}
	cfg, err := config.ParseConfigBytes(state.Data)
	if err != nil {
		c.JSON(503, gin.H{"error": "configuration_unavailable"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("ETag", `"`+state.Revision+`"`)
	c.JSON(200, h.geminiBudgetView(state.Revision, cfg))
}

// InitializeGeminiBudget is revision guarded and cannot reset an existing or disaster ledger.
func (h *Handler) InitializeGeminiBudget(c *gin.Context) {
	var request struct {
		Revision    string `json:"revision"`
		Acknowledge bool   `json:"acknowledge_unknown_usage"`
	}
	if !decodeGeminiRequest(c, &request) {
		return
	}
	h.mu.Lock()
	state, err := h.readConfigV8Locked()
	if err != nil {
		h.mu.Unlock()
		writeConfigV8Error(c, err)
		return
	}
	match := c.GetHeader("If-Match")
	if !geminiRevisionRequired(c, request.Revision, state.Revision) {
		h.mu.Unlock()
		return
	}
	if match == "" || !configV8RevisionMatches(match, state.Revision) {
		h.mu.Unlock()
		writeConfigV8Error(c, configV8Conflict(state.Revision))
		return
	}
	cfg, err := config.ParseConfigBytes(state.Data)
	if err != nil || h.authManager == nil {
		h.mu.Unlock()
		c.JSON(503, gin.H{"error": "budget_manager_unavailable"})
		return
	}
	if cfg.GeminiFreeTierPolicy.Enabled || !request.Acknowledge {
		h.mu.Unlock()
		c.JSON(409, gin.H{"error": "initialization_not_allowed", "message": "Disable the policy and acknowledge unknown prior usage before first initialization."})
		return
	}
	h.authManager.SetConfig(cfg)
	engine, err := h.authManager.EnsureGeminiBudget(c.Request.Context(), true)
	if err != nil {
		h.mu.Unlock()
		c.JSON(503, gin.H{"error": "budget_storage_unavailable"})
		return
	}
	if engine.Snapshot().Initialized {
		h.mu.Unlock()
		c.JSON(409, gin.H{"error": "already_initialized"})
		return
	}
	initial := h.geminiBudgetInitialUsage(cfg)
	if err = engine.Initialize(c.Request.Context(), budget.InitializeOptions{Used: initial}); err != nil {
		h.mu.Unlock()
		c.JSON(409, gin.H{"error": "initialization_failed", "message": "The ledger could not be initialized; inspect its state before retrying."})
		return
	}
	reload := h.reloadSnapshotConfigLocked()
	h.mu.Unlock()
	h.reloadConfigAfterManagementSave(context.WithoutCancel(c.Request.Context()), reload)
	_ = h.authManager.BackupGeminiBudget(context.WithoutCancel(c.Request.Context()))
	h.mu.Lock()
	view := h.geminiBudgetView(state.Revision, cfg)
	h.mu.Unlock()
	c.Header("ETag", `"`+state.Revision+`"`)
	c.JSON(http.StatusOK, view)
}

func (h *Handler) geminiBudgetInitialUsage(cfg *config.Config) []budget.InitialUsage {
	if h.usageStats == nil {
		return nil
	}
	day, _ := pacificBudgetDay(time.Now())
	byIndex := map[string]*coreauth.Auth{}
	for _, auth := range h.authManager.List() {
		if coreauth.IsOfficialGeminiAuth(auth) {
			byIndex[auth.Index] = auth
		}
	}
	counts := map[string]*budget.InitialUsage{}
	seen := map[string]bool{}
	for _, api := range h.usageStats.Snapshot().APIs {
		for model, stats := range api.Models {
			for _, detail := range stats.Details {
				itemDay, _ := pacificBudgetDay(detail.Timestamp)
				if itemDay != day {
					continue
				}
				auth := byIndex[detail.AuthIndex]
				if auth == nil {
					continue
				}
				real := coreauth.ResolveCanonicalBudgetModel(model, auth, cfg)
				if _, managed := cfg.GeminiFreeTierPolicy.WithDefaults().ManagedModels[real]; !managed {
					continue
				}
				encoded, _ := json.Marshal(detail)
				signature := detail.AuthIndex + real + string(encoded)
				if seen[signature] {
					continue
				}
				seen[signature] = true
				scope := coreauth.GeminiBudgetScope(auth, real)
				key := scope.Project + "/" + real
				if counts[key] == nil {
					counts[key] = &budget.InitialUsage{Scope: scope}
				}
				counts[key].Used++
			}
		}
	}
	result := make([]budget.InitialUsage, 0, len(counts))
	for _, v := range counts {
		result = append(result, *v)
	}
	return result
}
