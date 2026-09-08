package auth

import (
	"context"
	"net/http"
	"testing"
)

func TestRuntimeResultsRetainGenerationWithoutCredentialWrites(t *testing.T) {
	for _, neutral := range []bool{false, true} {
		for _, success := range []bool{false, true} {
			store := &countingStore{}
			manager := NewManager(store, nil, nil)
			credential := &Auth{ID: "sync-credential", Provider: "antigravity", Metadata: map[string]any{"type": "antigravity"}}
			if _, err := manager.Register(context.Background(), credential); err != nil {
				t.Fatal(err)
			}
			before, _ := manager.GetByID(credential.ID)
			saves := store.saveCount.Load()
			result := Result{AuthID: credential.ID, Provider: credential.Provider, Model: "test-model", Success: success}
			if !success {
				result.Error = &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota exhausted"}
			}
			if neutral {
				manager.recordAvailabilityNeutralResult(context.Background(), result)
			} else {
				manager.MarkResult(context.Background(), result)
			}
			after, _ := manager.GetByID(credential.ID)
			if after.Generation <= before.Generation || after.UpdatedAt.IsZero() {
				t.Fatalf("neutral=%v success=%v: result did not advance runtime generation", neutral, success)
			}
			if got := store.saveCount.Load(); got != saves {
				t.Fatalf("neutral=%v success=%v: credential saves=%d, want %d", neutral, success, got, saves)
			}
			if after.Success+after.Failed != before.Success+before.Failed+1 {
				t.Fatalf("neutral=%v success=%v: request accounting was lost", neutral, success)
			}
		}
	}
}
