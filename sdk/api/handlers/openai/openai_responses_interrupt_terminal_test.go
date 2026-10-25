package openai

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/tidwall/gjson"
)

func TestResponsesTerminalFrameDisablesLocalInterruptBeforeDrain(t *testing.T) {
	for _, event := range []string{"response.completed", "response.done", "response.incomplete"} {
		t.Run(event, func(t *testing.T) {
			local := newResponsesLocalInterrupt()
			data := make(chan []byte, 1)
			errs := make(chan *interfaces.ErrorMessage)
			data <- []byte(fmt.Sprintf(`{"type":%q,"response":{"id":"finished","output":[]}}`, event))
			done := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, errUpgrade := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if errUpgrade != nil {
					done <- errUpgrade
					return
				}
				defer func() { _ = conn.Close() }()
				ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ctx.Request = r
				_, _, _, _, errForward := (*OpenAIResponsesAPIHandler)(nil).forwardResponsesWebsocket(
					ctx, newResponsesWebsocketWriter(conn), func(...interface{}) {}, data, errs,
					newInMemoryWebsocketTimelineLog(), "terminal-drain", responsesWebsocketForwardOptions{localInterrupt: local},
				)
				done <- errForward
			}))
			var stopOnce sync.Once
			stop := func() { stopOnce.Do(func() { close(data); close(errs) }) }
			t.Cleanup(func() { stop(); server.Close() })
			client, _, errDial := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if errDial != nil {
				t.Fatal(errDial)
			}
			defer func() { _ = client.Close() }()
			if errDeadline := client.SetReadDeadline(time.Now().Add(3 * time.Second)); errDeadline != nil {
				t.Fatal(errDeadline)
			}
			_, payload, errRead := client.ReadMessage()
			if errRead != nil {
				t.Fatal(errRead)
			}
			if gjson.GetBytes(payload, "type").String() != event {
				t.Fatalf("unexpected terminal payload: %s", payload)
			}
			if local.deliver([]byte(`{"type":"response.interrupt","response_id":"unknown"}`)) {
				t.Fatal("terminal response accepted a local interrupt before upstream channels drained")
			}
			stop()
			select {
			case errForward := <-done:
				if errForward != nil {
					t.Fatal(errForward)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("forwarder did not finish after upstream drain")
			}
		})
	}
}
