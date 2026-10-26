package chat

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"oc2api/internal/config"
	"oc2api/internal/zen"
)

func TestCompleteRetryKeepsRequestKey(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-retry-user" {
			t.Error("retry did not preserve the request key")
		}
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, "data: {\"id\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"read\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, "data: {\"id\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"retried answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	cfg := config.Config{ZenAPIBaseURL: upstream.URL, ZenConnectTimeout: time.Second, Debug: true}
	service := New(cfg, zen.New(cfg))
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)
	payload := map[string]any{
		"model":    "example-paid",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	result, err := service.Complete(context.Background(), payload, "example-paid", "sk-retry-user")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || !hasContent(result) {
		t.Fatalf("calls=%d result=%v", calls.Load(), result)
	}
	if strings.Contains(logs.String(), "sk-retry-user") {
		t.Error("debug logs must not contain the request key")
	}
}

func TestValidateAcceptsPaidModels(t *testing.T) {
	for _, model := range []string{"big-pickle", "example-free", "example-paid"} {
		got, reason := Validate(map[string]any{
			"model":    model,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		})
		if got != model || reason != "" {
			t.Errorf("model=%s got=%s reason=%s", model, got, reason)
		}
	}
}
