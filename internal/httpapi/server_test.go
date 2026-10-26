package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"oc2api/internal/chat"
	"oc2api/internal/config"
	"oc2api/internal/zen"
)

const testSSE = "data: {\"id\":\"test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

func newTestHandler(t *testing.T, upstream http.HandlerFunc) http.Handler {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	cfg := config.Config{ZenAPIBaseURL: server.URL, ZenConnectTimeout: time.Second}
	client := zen.New(cfg)
	return New(client, chat.New(cfg, client))
}

func TestRequestAPIKey(t *testing.T) {
	for _, tc := range []struct {
		name, authorization, apiKey, want string
	}{
		{"missing", "", "", ""},
		{"bearer", "Bearer sk-user", "", "sk-user"},
		{"case and whitespace", "  bEaReR\tsk-user  ", "", "sk-user"},
		{"raw", "sk-user", "", "sk-user"},
		{"x-api-key", "", "sk-user", "sk-user"},
		{"x-api-key bearer", "", "Bearer sk-user", "sk-user"},
		{"priority", "Bearer sk-first", "sk-second", "sk-first"},
		{"empty bearer", "Bearer ", "sk-second", ""},
		{"bare bearer", "Bearer", "", ""},
		{"whitespace only", "  ", "sk-second", ""},
		{"other scheme", "Basic abc", "", ""},
		{"multiple tokens", "Bearer sk-one sk-two", "", ""},
		{"control character", "sk-\x7f", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			r.Header.Set("Authorization", tc.authorization)
			r.Header.Set("X-API-Key", tc.apiKey)
			if got := requestAPIKey(r); got != tc.want {
				t.Errorf("requestAPIKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMissingKeyNeverCallsUpstream(t *testing.T) {
	t.Setenv("API_KEY", "old-gateway-password")
	handler := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("missing credentials must not reach upstream")
		w.WriteHeader(http.StatusInternalServerError)
	})
	for _, path := range []string{"/v1/models", "/models", "/v1/chat/completions", "/chat/completions"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "authentication_error") {
			t.Errorf("%s: status=%d body=%s", path, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{"/", "/health"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Errorf("%s should stay public, status=%d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodOptions, "/v1/models", nil))
	if w.Code != http.StatusNoContent {
		t.Errorf("CORS preflight status=%d", w.Code)
	}
}

func TestModelsForwardsKeyAndIncludesPaidModels(t *testing.T) {
	t.Setenv("API_KEY", "old-gateway-password")
	handler := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/inference/v1/models" || r.Header.Get("Authorization") != "Bearer sk-user" {
			t.Errorf("unexpected upstream request: path=%s auth=%s", r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"big-pickle"},{"id":"example-free"},{"id":"example-paid"}]}`)
	})
	for _, path := range []string{"/v1/models", "/models"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-API-Key", "sk-user")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "example-paid") {
			t.Errorf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
}

func TestChatForwardsKeyInBothModes(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			handler := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/inference/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer sk-user" {
					t.Errorf("unexpected upstream request: path=%s auth=%s", r.URL.Path, r.Header.Get("Authorization"))
				}
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if payload["model"] != "example-paid" || payload["stream"] != true {
					t.Errorf("unexpected model/stream: %v", payload)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, testSSE)
			})
			for _, path := range []string{"/v1/chat/completions", "/chat/completions"} {
				body := fmt.Sprintf(`{"model":"example-paid","messages":[{"role":"user","content":"hi"}],"stream":%t}`, stream)
				r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer sk-user")
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "hello") {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				if stream && !strings.Contains(w.Body.String(), "data: [DONE]") {
					t.Error("missing stream terminator")
				}
			}
		})
	}
}

func TestUpstreamErrorsKeepStatusAndRetryAfter(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		for _, path := range []string{"/v1/models", "/v1/chat/completions"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%d%s/stream=%t", status, path, stream), func(t *testing.T) {
					handler := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Retry-After", "30")
						w.WriteHeader(status)
						_, _ = io.WriteString(w, `{"error":"upstream rejected request"}`)
					})
					body := fmt.Sprintf(`{"model":"example-paid","messages":[{"role":"user","content":"hi"}],"stream":%t}`, stream)
					r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
					r.Header.Set("Authorization", "Bearer sk-user")
					w := httptest.NewRecorder()
					handler.ServeHTTP(w, r)
					if w.Code != status || w.Header().Get("Retry-After") != "30" {
						t.Errorf("status=%d retry=%s body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
					}
				})
			}
		}
	}
}

func TestConcurrentRequestsKeepKeysIsolated(t *testing.T) {
	handler := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if r.Header.Get("Authorization") != "Bearer sk-"+payload.Model {
			t.Errorf("credentials crossed requests for %s", payload.Model)
		}
		_, _ = io.WriteString(w, testSSE)
	})
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			model := fmt.Sprintf("paid-%d", i)
			body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"stream":%t}`, model, i%2 == 0)
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer sk-"+model)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Errorf("request %d status=%d", i, w.Code)
			}
		}(i)
	}
	wg.Wait()
}
