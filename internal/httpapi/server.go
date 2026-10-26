// Package httpapi 是本地与 Docker 的 HTTP 入口，端点与 Node 版保持一致。
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"oc2api/internal/chat"
	"oc2api/internal/config"
	"oc2api/internal/ipinfo"
	"oc2api/internal/protocol"
	"oc2api/internal/zen"
)

const maxBodyBytes = 32 << 20

var endpointList = []string{
	"/v1/chat/completions", "/chat/completions", "/v1/models", "/models", "/health", "/ip",
}

func writeJSON(w http.ResponseWriter, status int, payload any, headers map[string]string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	for key, value := range headers {
		w.Header().Set(key, value)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeOpenAIError(w http.ResponseWriter, status int, message, kind, code string) {
	body := map[string]any{"message": message, "type": kind}
	if code != "" {
		body["code"] = code
	}
	writeJSON(w, status, map[string]any{"error": body}, nil)
}

// New 组装路由：健康检查与 /ip 公开，模型和对话请求必须提供 OpenCode Key。
// 凭据按请求传递，不保存在共享客户端或服务中。
func New(client *zen.Client, chats *chat.Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", withCORS(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/health", "":
			health(w)
		case "/ip":
			ip(w)
		case "/v1/models", "/models":
			apiKey := requestAPIKey(r)
			if apiKey == "" {
				writeOpenAIError(w, http.StatusUnauthorized, "A valid OpenCode API key is required", "authentication_error", "")
				return
			}
			models(w, r, client, apiKey)
		case "/v1/chat/completions", "/chat/completions":
			apiKey := requestAPIKey(r)
			if apiKey == "" {
				writeOpenAIError(w, http.StatusUnauthorized, "A valid OpenCode API key is required", "authentication_error", "")
				return
			}
			completions(w, r, chats, apiKey)
		default:
			writeOpenAIError(w, http.StatusNotFound, "Not found", "invalid_request_error", "")
		}
	}))
	return mux
}

func health(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"version":   config.Version,
		"endpoints": endpointList,
	}, nil)
}

// requestAPIKey 与 Vercel 薄层一致：Authorization 优先，回退 X-API-Key。
// 兼容 Bearer 前缀和原始 Key；无效 Authorization 不使用另一个头绕过。
func requestAPIKey(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if header == "" {
		header = r.Header.Get("X-API-Key")
	}
	token := strings.TrimSpace(header)
	parts := strings.Fields(token)
	if len(parts) > 0 && strings.EqualFold(parts[0], "bearer") {
		if len(parts) != 2 {
			return ""
		}
		token = parts[1]
	}
	if !zen.ValidAPIKey(token) {
		return ""
	}
	return token
}

func models(w http.ResponseWriter, r *http.Request, client *zen.Client, apiKey string) {
	list, err := client.Models(r.Context(), apiKey)
	if err != nil {
		var upstream *zen.UpstreamError
		if errors.As(err, &upstream) {
			headers := map[string]string{}
			if upstream.RetryAfter != "" {
				headers["Retry-After"] = upstream.RetryAfter
			}
			writeJSON(w, upstream.Status, protocol.ErrorBody(upstream.Status, string(upstream.Body)), headers)
			return
		}
		writeOpenAIError(w, http.StatusBadGateway, "Upstream error: "+err.Error(), "upstream_error", "upstream_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": list}, nil)
}

// ip 与 Node 版一致：依次尝试两个供应商，返回 {ip, source}。
func ip(w http.ResponseWriter) {
	found, source := ipinfo.Lookup(&http.Client{})
	if found == "" {
		writeOpenAIError(w, http.StatusBadGateway, "Upstream error: all IP providers failed", "upstream_error", "upstream_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ip": found, "source": source}, nil)
}

func completions(w http.ResponseWriter, r *http.Request, chats *chat.Service, apiKey string) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "Not found", "invalid_request_error", "")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "Invalid JSON body", "invalid_request_error", "")
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "Invalid JSON body", "invalid_request_error", "")
		return
	}
	model, reason := chat.Validate(payload)
	if reason != "" {
		writeOpenAIError(w, http.StatusBadRequest, reason, "invalid_request_error", "")
		return
	}
	stream, _ := payload["stream"].(bool)

	if !stream {
		result, err := chats.Complete(r.Context(), payload, model, apiKey)
		if err != nil {
			var upstream *chat.UpstreamError
			if errors.As(err, &upstream) {
				headers := map[string]string{}
				if upstream.RetryAfter != "" {
					headers["Retry-After"] = upstream.RetryAfter
				}
				writeJSON(w, upstream.Status, protocol.ErrorBody(upstream.Status, string(upstream.Body)), headers)
				return
			}
			writeOpenAIError(w, http.StatusBadGateway, "Upstream error: "+err.Error(), "upstream_error", "upstream_error")
			return
		}
		writeJSON(w, http.StatusOK, result, nil)
		return
	}

	resp, renames, err := chats.Upstream(r.Context(), payload, model, apiKey)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "Upstream error: "+err.Error(), "upstream_error", "upstream_error")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		headers := map[string]string{}
		if retry := resp.Header.Get("Retry-After"); retry != "" {
			headers["Retry-After"] = retry
		}
		writeJSON(w, resp.StatusCode, protocol.ErrorBody(resp.StatusCode, string(text)), headers)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	_ = protocol.Stream(w, resp, renames)
}

func withCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, X-API-Key, x-api-key, Content-Type, Anthropic-Version, Anthropic-Beta")
		w.Header().Set("Access-Control-Expose-Headers", "X-Request-Id, Retry-After, Retry-After-Ms")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}
