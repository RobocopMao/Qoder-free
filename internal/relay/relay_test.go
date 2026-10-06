package relay

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// worker 实际输出（33100 抓到的真实帧）：每帧 data: 行后跟一个空行。
const workerSSE = "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{\"content\":\"一\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{\"content\":\"二\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

// TestRelayStreamKeepsSSEFrameSeparators 守住 #SSE-SEP：
// SSE 的「帧」由**空行**分隔（text/event-stream 规范）。
// TCP/HTTP 不保证按帧投递，客户端必须靠空行切帧；
// 落掉空行会把整条流粘成一大块，标准 SSE 解析器只会切出 1 个事件。
func TestRelayStreamKeepsSSEFrameSeparators(t *testing.T) {
	rec := httptest.NewRecorder()
	if _, err := RelayStream(rec, strings.NewReader(workerSSE)); err != nil {
		t.Fatalf("RelayStream 返回错误: %v", err)
	}
	got := rec.Body.String()

	// 空行是帧分隔符，不能丢
	if n := strings.Count(got, "\n\n"); n < 4 {
		t.Errorf("输出里空行分隔只有 %d 个，应为 4 个（每帧一个）——\n实际输出:\n%q", n, got)
	}
	// 帧数应等于上游帧数
	if n := strings.Count(got, "data: "); n != 5 {
		t.Errorf("data: 行数 = %d，应为 5", n)
	}
	// [DONE] 必须保留且成帧
	if !strings.Contains(got, "data: [DONE]\n\n") {
		t.Errorf("缺少成帧的 data: [DONE]，实际输出:\n%q", got)
	}
}

// TestRelayStreamMarksTruncatedStream 上游吐了内容却没 [DONE] 就 EOF：
// 必须 (1) 返回 MidStreamError 记为失败，(2) 给下游补一个带内错误帧 + 终止符，
// 而不是让流裸结束（裸结束只会让客户端报「ended before a completion event」）。
func TestRelayStreamMarksTruncatedStream(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"一半\"},\"finish_reason\":null}]}\n\n"
	rec := httptest.NewRecorder()
	_, err := RelayStream(rec, strings.NewReader(body))
	if err == nil {
		t.Fatal("断流被当成了成功")
	}
	got := rec.Body.String()
	if !strings.Contains(got, `"code":"upstream_stream_incomplete"`) {
		t.Errorf("缺少带内错误帧，实际输出:\n%q", got)
	}
	if !strings.HasSuffix(got, "data: [DONE]\n\n") {
		t.Errorf("流没有以 [DONE] 收尾，实际输出:\n%q", got)
	}
}

// TestRelayStreamTerminatesAfterUpstreamErrorFrame worker 自己发完 error 帧就
// res.end()（不带 [DONE]）：relay 要补终止符，让下游拿到「格式完整」的错误流。
func TestRelayStreamTerminatesAfterUpstreamErrorFrame(t *testing.T) {
	body := "data: {\"error\":{\"code\":\"upstream_stream_interrupted\",\"message\":\"boom\",\"status\":502}}\n\n"
	rec := httptest.NewRecorder()
	_, err := RelayStream(rec, strings.NewReader(body))
	if err == nil {
		t.Fatal("上游错误帧未被识别")
	}
	got := rec.Body.String()
	if !strings.Contains(got, "upstream_stream_interrupted") {
		t.Errorf("错误帧没透传，实际输出:\n%q", got)
	}
	if !strings.HasSuffix(got, "data: [DONE]\n\n") {
		t.Errorf("错误流没有终止符，实际输出:\n%q", got)
	}
}

// TestRelayStreamEmptyUpstreamIsFailure 上游 200 但一个帧都没有：不能算成功。
func TestRelayStreamEmptyUpstreamIsFailure(t *testing.T) {
	rec := httptest.NewRecorder()
	_, err := RelayStream(rec, strings.NewReader(""))
	if err == nil {
		t.Fatal("空流被当成了成功")
	}
	if !strings.Contains(rec.Body.String(), `"code":"upstream_empty"`) {
		t.Errorf("缺少空流错误帧，实际输出:\n%q", rec.Body.String())
	}
}

// TestRelayStreamCompleteStreamIsSuccess 正常流必须完全不被打断、也不补多余帧。
func TestRelayStreamCompleteStreamIsSuccess(t *testing.T) {
	rec := httptest.NewRecorder()
	if _, err := RelayStream(rec, strings.NewReader(workerSSE)); err != nil {
		t.Fatalf("完整流被误判为失败: %v", err)
	}
	got := rec.Body.String()
	if got != workerSSE {
		t.Errorf("完整流应逐字节原样透传\n期望: %q\n实际: %q", workerSSE, got)
	}
}
func TestRelayStreamUsageStillScraped(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"好\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":65,\"completion_tokens\":106,\"total_tokens\":171}}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	usage, err := RelayStream(rec, strings.NewReader(body))
	if err != nil {
		t.Fatalf("RelayStream 返回错误: %v", err)
	}
	if usage.PromptTokens != 65 || usage.CompletionTokens != 106 || usage.TotalTokens != 171 {
		t.Errorf("usage 抓取错误: %+v", usage)
	}
}

// TestRelayStreamDetectsMidStreamError 确认流中错误仍能被识别（依赖成帧判断）。
func TestRelayStreamDetectsMidStreamError(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"好\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"error\":{\"code\":\"upstream_stream_incomplete\",\"message\":\"ended before [DONE]\",\"status\":502}}\n\n"
	rec := httptest.NewRecorder()
	_, err := RelayStream(rec, strings.NewReader(body))
	if err == nil {
		t.Fatal("流中错误未被识别")
	}
	mid, ok := err.(*MidStreamError)
	if !ok {
		t.Fatalf("错误类型 = %T，应为 *MidStreamError", err)
	}
	if mid.Code != "upstream_stream_incomplete" || mid.Status != 502 {
		t.Errorf("MidStreamError 字段错误: %+v", mid)
	}
}
