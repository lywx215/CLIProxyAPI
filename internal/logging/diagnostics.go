package logging

import (
	"bytes"
	"os"
	"sync"
	"sync/atomic"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/diagnostics"
	log "github.com/sirupsen/logrus"
)

type diagnosticLine []byte

var diagnosticsOnce sync.Once
var diagnosticEngine atomic.Pointer[diagnostics.Engine]

// GinDiagnostics shares the existing INFO access-log gate and logrus writer.
// request-log controls legacy body capture and is deliberately unrelated.
func GinDiagnostics() gin.HandlerFunc {
	diagnosticsOnce.Do(func() {
		diagnosticEngine.Store(diagnostics.NewEngine(diagnostics.EnvironmentConfig(buildinfo.Commit), os.Getenv("DIAG_PEERS"),
			func() bool { return log.IsLevelEnabled(log.InfoLevel) },
			func() bool { return log.IsLevelEnabled(log.DebugLevel) },
			func(line []byte) error {
				entry := log.WithField("diagnostics_line", diagnosticLine(line))
				if bytes.Contains(line, []byte(`"recordKind":"debug"`)) {
					entry.Debug("")
				} else {
					entry.Info("")
				}
				return nil
			}))
		diagnosticEngine.Load().MarkSinkLossUnknown()
	})
	return diagnosticEngine.Load().GinMiddleware(func(c *gin.Context) string {
		if id := GetGinRequestID(c); id != "" {
			return id
		}
		// Non-model routes did not historically have a request ID. Their new
		// diagnostic ID does not change legacy Gin log or management identities.
		return GenerateRequestID()
	})
}

// ReloadDiagnostics refreshes local peer policy on the existing config reload.
// Resource identity remains fixed for the lifetime of the worker.
func ReloadDiagnostics() {
	if engine := diagnosticEngine.Load(); engine != nil {
		engine.ReloadPeers(os.Getenv("DIAG_PEERS"))
	}
}
