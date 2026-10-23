package management

import (
	"net"
	"net/http"
	"strconv"
	"sync"

	ge "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/geminienterprise"
)

// One loopback receiver can serve concurrent management sessions. An occupied
// port leaves manual callback submission available without stopping its owner.
type enterpriseCallbackReceiver struct {
	mu       sync.Mutex
	server   *http.Server
	sessions map[string]string
}

var enterpriseCallbacks = &enterpriseCallbackReceiver{}

func (r *enterpriseCallbackReceiver) acquire(state, authDir string, port int) (func(), bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.server == nil {
		listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			return func() {}, false
		}
		r.sessions = make(map[string]string)
		r.server = &http.Server{Handler: http.HandlerFunc(r.callback)}
		server := r.server
		go func() { _ = server.Serve(listener) }()
	}
	r.sessions[state] = authDir
	return func() {
		r.mu.Lock()
		delete(r.sessions, state)
		var server *http.Server
		if len(r.sessions) == 0 {
			server, r.server = r.server, nil
		}
		r.mu.Unlock()
		if server != nil {
			_ = server.Close()
		}
	}, true
}

func (r *enterpriseCallbackReceiver) callback(w http.ResponseWriter, request *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if request.Method != http.MethodGet || request.URL.Path != "/oauth-callback" {
		http.NotFound(w, request)
		return
	}
	query := request.URL.Query()
	state := query.Get("state")
	r.mu.Lock()
	authDir := r.sessions[state]
	r.mu.Unlock()
	if authDir == "" || !IsOAuthSessionPending(state, ge.Provider) {
		http.Error(w, "Unknown or expired OAuth state", http.StatusBadRequest)
		return
	}
	if query.Get("code") == "" && query.Get("error") == "" {
		http.Error(w, "Google authorization was not completed", http.StatusBadRequest)
		return
	}
	if _, err := WriteOAuthCallbackFileForPendingSession(authDir, ge.Provider, state, query.Get("code"), query.Get("error")); err != nil {
		http.Error(w, "OAuth callback was not accepted", http.StatusBadRequest)
		return
	}
	_, _ = w.Write([]byte("Authorization received. You may close this tab."))
}
