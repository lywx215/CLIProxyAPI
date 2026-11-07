package enterpriseweb

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/tidwall/gjson"
)

func TestModelProbeRequiresBothInputsAndCompletedResponse(t *testing.T) {
	body, code, err := NewModelProbe()
	if err != nil || len(gjson.GetBytes(body, "contents.0.parts").Array()) != 3 {
		t.Fatal("probe inputs missing", err)
	}
	if gjson.GetBytes(body, "contents.0.parts.0.text").String() == code {
		t.Fatal("prompt disclosed attachment answer")
	}
	for _, test := range []struct {
		text, finish string
		want         bool
	}{{code + "|red|blue|green", "STOP", true}, {"OK", "STOP", false}, {code + "|blue|red|green", "STOP", false}, {code + "|red|blue|green", "MAX_TOKENS", false}} {
		payload, _ := json.Marshal(map[string]any{"candidates": []any{map[string]any{"finishReason": test.finish, "content": map[string]any{"parts": []any{map[string]string{"text": test.text}}}}}})
		if ModelProbeMatches(payload, code) != test.want {
			t.Fatal("incorrect media verification", test)
		}
	}
}

func TestModelProbeRejectsChangedCredentialModelAndEpoch(t *testing.T) {
	c := credentialFixture(t)
	current := true
	ctx := WithModelProbe(context.Background(), "account", c.Fingerprint(), "observed-web-id", 7, func() bool { return current })
	if err := ValidateModelProbe(ctx, "account", c, "observed-web-id", 7); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		id, model string
		epoch     uint64
		code      string
	}{{"other", "observed-web-id", 7, "credential_changed"}, {"account", "other-model", 7, "model_changed"}, {"account", "observed-web-id", 8, "credential_changed"}} {
		var changed *ModelProbeError
		if err := ValidateModelProbe(ctx, test.id, c, test.model, test.epoch); !errors.As(err, &changed) || changed.Code != test.code {
			t.Fatal("changed binding accepted", err)
		}
	}
	current = false
	if ValidateModelProbe(ctx, "account", c, "observed-web-id", 7) == nil {
		t.Fatal("late model test survived credential publication")
	}
}
