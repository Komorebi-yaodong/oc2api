package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"oc2api/internal/chat"
	"oc2api/internal/config"
	"oc2api/internal/zen"
)

func callRPC(t *testing.T, handler *Handler, method string, params any) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	var messages []map[string]any
	handler.Handle(context.Background(), raw, func(msg map[string]any) {
		messages = append(messages, msg)
	})
	return messages
}

func newTestHandler(t *testing.T, upstream http.HandlerFunc) *Handler {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	cfg := config.Config{ZenAPIBaseURL: server.URL, ZenConnectTimeout: time.Second}
	client := zen.New(cfg)
	return New(cfg, client, chat.New(cfg, client))
}

func TestRPCRequiresRequestKey(t *testing.T) {
	handler := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("missing/invalid key must not reach upstream")
	})
	for _, method := range []string{"models", "chat"} {
		for _, key := range []string{"", "invalid key", "sk-\r\n"} {
			messages := callRPC(t, handler, method, map[string]any{"apiKey": key})
			if len(messages) != 1 {
				t.Fatalf("unexpected messages: %v", messages)
			}
			result, _ := messages[0]["result"].(map[string]any)
			if result["status"] != http.StatusUnauthorized {
				t.Errorf("method=%s result=%v", method, result)
			}
		}
	}
	messages := callRPC(t, handler, "health", map[string]any{})
	result, _ := messages[0]["result"].(map[string]any)
	if result["status"] != "ok" {
		t.Errorf("health should remain public: %v", result)
	}
}

func TestRPCForwardsKeyForModelsAndBothChatModes(t *testing.T) {
	handler := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-rpc-user" {
			t.Error("RPC key did not reach upstream")
		}
		if r.URL.Path == "/inference/v1/models" {
			_, _ = io.WriteString(w, `{"data":[{"id":"example-paid"}]}`)
			return
		}
		_, _ = io.WriteString(w, "data: {\"id\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	})
	messages := callRPC(t, handler, "models", map[string]any{"apiKey": "sk-rpc-user"})
	encoded, _ := json.Marshal(messages)
	if !strings.Contains(string(encoded), "example-paid") {
		t.Fatalf("paid model missing: %s", encoded)
	}
	for _, stream := range []bool{false, true} {
		messages = callRPC(t, handler, "chat", map[string]any{
			"apiKey": "sk-rpc-user",
			"stream": stream,
			"body": map[string]any{
				"model":    "example-paid",
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			},
		})
		encoded, _ = json.Marshal(messages)
		if !strings.Contains(string(encoded), "hello") {
			t.Errorf("stream=%t unexpected response: %s", stream, encoded)
		}
		if stream && messages[len(messages)-1]["method"] != "end" {
			t.Errorf("missing stream end notification: %s", encoded)
		}
		if strings.Contains(string(encoded), "sk-rpc-user") {
			t.Error("RPC response must not contain request credentials")
		}
	}
}

func TestRPCUpstreamErrorsPreserveStatus(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		for _, method := range []string{"models", "chat"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%d/stream=%t", method, status, stream), func(t *testing.T) {
					handler := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Retry-After", "60")
						w.WriteHeader(status)
						_, _ = io.WriteString(w, `{"error":"rejected"}`)
					})
					messages := callRPC(t, handler, method, map[string]any{
						"apiKey": "sk-rpc-user", "stream": stream,
						"body": map[string]any{"model": "example-paid", "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
					})
					if len(messages) != 1 {
						t.Fatalf("unexpected messages: %v", messages)
					}
					result, _ := messages[0]["result"].(map[string]any)
					if result["status"] != status || result["retryAfter"] != "60" {
						t.Errorf("unexpected messages: %v", messages)
					}
				})
			}
		}
	}
}
