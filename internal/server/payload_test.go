package server

import (
	"encoding/json"
	"testing"
)

// decoded 解开 buildChatPayload 的输出，方便断言。
func decoded(t *testing.T, raw string, model string, stream bool, defaultContext int) map[string]any {
	t.Helper()
	out := buildChatPayload([]byte(raw), model, stream, defaultContext)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v\n%s", err, out)
	}
	return got
}

// 服务端配置了窗口、客户端没带 context_length -> 必须注入。
// 这是「1M 上下文可调」的核心：以前这个字段被白名单丢掉，
// 上游永远按目录默认的 200000 走（用户 m03576：现在只有几百k，不能调节）。
func TestBuildChatPayloadInjectsDefaultContext(t *testing.T) {
	got := decoded(t, `{"messages":[{"role":"user","content":"hi"}]}`, "qwen3.8-flash", true, 1000000)
	if v, ok := got["context_length"].(float64); !ok || int(v) != 1000000 {
		t.Fatalf("期望注入 context_length=1000000，实际 %v", got["context_length"])
	}
}

// 客户端显式指定的窗口优先级最高，不能被服务端配置覆盖。
func TestBuildChatPayloadClientContextWins(t *testing.T) {
	got := decoded(t, `{"messages":[],"context_length":400000}`, "qwen3.8-flash", true, 1000000)
	if v, ok := got["context_length"].(float64); !ok || int(v) != 400000 {
		t.Fatalf("期望保留客户端 context_length=400000，实际 %v", got["context_length"])
	}
}

// 没配置（0）时**不能**写 context_length，得让上游目录默认值生效，
// 否则面板里把窗口设回「默认」反而会锁死某个具体数值。
func TestBuildChatPayloadNoDefaultMeansOmit(t *testing.T) {
	got := decoded(t, `{"messages":[]}`, "qwen3.8-flash", true, 0)
	if _, present := got["context_length"]; present {
		t.Fatalf("未配置时不应出现 context_length，实际 %v", got["context_length"])
	}
}

// 白名单行为不能被这次改动破坏：未知字段仍然丢弃，已知字段仍然透传。
func TestBuildChatPayloadStillWhitelists(t *testing.T) {
	got := decoded(t,
		`{"messages":[{"role":"user","content":"hi"}],"temperature":0.3,"tools":[{"type":"function"}],"unknown_field":123}`,
		"m", false, 0)
	if _, present := got["unknown_field"]; present {
		t.Fatal("未知字段不应透传给上游")
	}
	if _, present := got["temperature"]; !present {
		t.Fatal("temperature 应透传")
	}
	if _, present := got["tools"]; !present {
		t.Fatal("tools 应透传")
	}
	if got["stream"] != false {
		t.Fatalf("stream 应为 false，实际 %v", got["stream"])
	}
	if got["model"] != "m" {
		t.Fatalf("model 应为 m，实际 %v", got["model"])
	}
}
