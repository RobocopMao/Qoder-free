package worker

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestChatClientNotCappedOnBody 守住 chat 客户端的超时形状：
// 原来 Chat 复用 &http.Client{Timeout: 130s}，而 http.Client.Timeout
// 是**连响应体一起算**的总时长上限 —— 大上下文 + 深度思考的流式回复
// 一旦超过 130 秒就会被腰斩，下游只看到流凭空结束。
func TestChatClientNotCappedOnBody(t *testing.T) {
	m := NewManager(nil)
	if m.chatClient == nil {
		t.Fatal("chatClient 未初始化")
	}
	if m.chatClient.Timeout != 0 {
		t.Errorf("chatClient.Timeout = %v，必须为 0：整体超时会掐断流式响应体", m.chatClient.Timeout)
	}
	tr, ok := m.chatClient.Transport.(*http.Transport)
	if !ok {
		t.Fatal("chatClient.Transport 不是 *http.Transport，ProxyFromEnvironment 等设置可能已丢失")
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Errorf("ResponseHeaderTimeout = %v，应为正数：否则迟迟不返回响应头的 worker 会永久挂住", tr.ResponseHeaderTimeout)
	}
	// JSON RPC 仍应保留整体封顶（短调用，卡住不如快速失败）
	if m.client.Timeout <= 0 {
		t.Errorf("client.Timeout = %v，JSON RPC 应有整体上限", m.client.Timeout)
	}
}

// TestChatSurvivesSlowBody 端到端验证：一条持续吐字节、总时长超过
// 旧 130s 上限语义的慢流，必须完整读到 [DONE] 而不是被掐断。
// 用「比 header 等待上限更慢的节奏」不现实，所以这里把节奏压缩到
// 秒级，只验证机制：只要字节持续到达，就不该有整体超时把它切断。
func TestChatSurvivesSlowBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := bufio.NewWriter(w)
		for i := 0; i < 60; i++ {
			fmt.Fprintf(f, "data: {\"choices\":[{\"delta\":{\"content\":\"x%d\"},\"finish_reason\":null}]}\n\n", i)
			_ = f.Flush()
			time.Sleep(20 * time.Millisecond) // 共约 1.2s
		}
		_, _ = f.WriteString("data: [DONE]\n\n")
		_ = f.Flush()
	}))
	defer srv.Close()

	m := NewManager(nil)
	c := Client{HTTP: m.client, ChatHTTP: m.chatClient}
	resp, err := c.Chat(context.Background(), srv.URL, "req-1", []byte(`{"model":"x","stream":true}`))
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	defer resp.Body.Close()

	var body strings.Builder
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		body.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	got := body.String()
	if !strings.Contains(got, "x59") || !strings.HasSuffix(strings.TrimSpace(got), "data: [DONE]") {
		t.Errorf("慢流被截断：末尾不是完整的 [DONE]\n实际尾部: %q", got[max(0, len(got)-80):])
	}
	if n := strings.Count(got, "\n\n"); n != 61 {
		t.Errorf("帧数 = %d，应为 61（60 内容帧 + 1 终止帧）", n)
	}
}
