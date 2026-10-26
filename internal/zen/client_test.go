package zen

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"oc2api/internal/config"
)

// 基址只到站点根，路径由包内常量拼：上游换路径只改常量，这里守住拼接结果。
func TestURLJoining(t *testing.T) {
	client := New(config.Config{ZenAPIBaseURL: "https://opencode.ai", ZenConnectTimeout: time.Second})
	if client.modelsURL != "https://opencode.ai/inference/v1/models" {
		t.Errorf("modelsURL = %q", client.modelsURL)
	}
	if client.chatURL != "https://opencode.ai/inference/v1/chat/completions" {
		t.Errorf("chatURL = %q", client.chatURL)
	}
}

// 自建中继只填站点根（可能带反代前缀），拼接时不能重复或丢失前缀路径。
func TestURLJoiningWithProxyPrefix(t *testing.T) {
	client := New(config.Config{ZenAPIBaseURL: "http://relay.local/oc2api", ZenConnectTimeout: time.Second})
	if client.modelsURL != "http://relay.local/oc2api/inference/v1/models" {
		t.Errorf("modelsURL = %q", client.modelsURL)
	}
}

func TestUpstreamHeadersUseRequestKey(t *testing.T) {
	headers := UpstreamHeaders("ses_test", "sk-request-user")
	if headers["Authorization"] != "Bearer sk-request-user" || headers["x-opencode-session"] != "ses_test" {
		t.Errorf("unexpected request headers: %v", headers)
	}
}

func TestInvalidKeyNeverCallsUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid credentials must not reach upstream")
	}))
	defer upstream.Close()
	client := New(config.Config{ZenAPIBaseURL: upstream.URL, ZenConnectTimeout: time.Second})
	for _, key := range []string{"", "invalid key", "sk-\r\n", "sk-\x7f", "sk-中文"} {
		if _, err := client.Models(context.Background(), key); err == nil {
			t.Error("Models accepted an invalid key")
		}
		if _, err := client.Chat(context.Background(), []byte(`{}`), "ses_test", key); err == nil {
			t.Error("Chat accepted an invalid key")
		}
	}
}

// 上游接了连接却迟迟不给响应头时，必须在 ZEN_CONNECT_TIMEOUT_MS 内放弃，
// 而不是挂到平台上限。这里用“永不回包”的本地上游验证超时真的接上了。
func TestModelsTimesOutBeforeHeaders(t *testing.T) {
	for _, timeout := range []time.Duration{150 * time.Millisecond, 600 * time.Millisecond} {
		t.Run(timeout.String(), func(t *testing.T) {
			release := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				<-release
			}))
			defer func() {
				close(release)
				upstream.Close()
			}()

			client := New(config.Config{ZenAPIBaseURL: upstream.URL, ZenConnectTimeout: timeout})

			started := time.Now()
			_, err := client.Models(context.Background(), "sk-test")
			elapsed := time.Since(started)

			if err == nil {
				t.Fatal("上游不回响应头时应该超时失败")
			}
			if elapsed < timeout*9/10 {
				t.Errorf("耗时 %v 早于超时 %v，说明客户端不是被超时打断的", elapsed, timeout)
			}
			// 上限放宽到 3 倍 + 200ms，避免 CI 上偶发的调度抖动把测试打成假失败。
			if elapsed > timeout*3+200*time.Millisecond {
				t.Errorf("耗时 %v 远超超时 %v，说明超时没接上", elapsed, timeout)
			}
		})
	}
}
