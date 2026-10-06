// Package relay pipes worker SSE frames to the HTTP client while scraping
// usage and detecting mid-stream errors. Worker frames are already
// OpenAI-shaped (chat.completion.chunk + data: [DONE]), so frames pass
// through unmodified.
package relay

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type Usage struct {
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	Source           string `json:"source"`
	Credits          any    `json:"credits,omitempty"`
}

// MidStreamError reports an error frame observed after the stream started.
type MidStreamError struct {
	Status  int
	Code    string
	Message string
}

func (e *MidStreamError) Error() string {
	if e.Code != "" {
		return e.Code + ": " + e.Message
	}
	return e.Message
}

func SetSSEHeaders(w http.ResponseWriter) {
	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
}

// EmitDone 补一个 [DONE] 终止符。SSE 流不该裸结束，否则客户端只能猜。
func EmitDone(w http.ResponseWriter) {
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// EmitStreamError 在「流已经开始、但又突然断了」时补一个带内错误帧。
//
// 不补的话，客户端只会看到流凭空结束，只能猜（例如 DSH 的
// 「upstream stream ended before a completion event」——它连原因都拿不到）。
// 补了，OpenAI 兼容客户端能在 `parsed.error` 里读到真实原因，之后照常收尾。
// 注意帧后必须带空行，理由同 RelayStream。
func EmitStreamError(w http.ResponseWriter, code, message string) {
	if code == "" {
		code = "upstream_stream_interrupted"
	}
	if message == "" {
		message = "upstream stream ended unexpectedly"
	}
	payload, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "api_error",
			"code":    code,
		},
	})
	if err != nil {
		return
	}
	SetSSEHeaders(w)
	_, _ = io.WriteString(w, "data: "+string(payload)+"\n\n")
	EmitDone(w)
}

// RelayStream copies SSE frames from body to w until [DONE] or EOF.
// It returns the usage scraped from the final chunk, if any.
//
// 上游字节**原样透传**（含帧结束的空行），只旁路解析 data: 行来抓 usage/错误。
//
// 注意不要把空行剥掉再自己拼 `data: ...\n`：SSE 的帧边界就是那个空行
// （text/event-stream 规范），下游解析器按 `\n\n` 切帧。
// 少了空行，整条流会粘成一块，标准解析器只能切出 1 个事件 ——
// 表现为「upstream stream ended before a completion event」，整轮对话作废。
func RelayStream(w http.ResponseWriter, body io.Reader) (Usage, error) {
	SetSSEHeaders(w)
	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReaderSize(body, 64*1024)
	var usage Usage
	var streamErr *MidStreamError

	// 只转发、不重排：line 保留了它自己的行尾（\n / \r\n），因此帧分隔空行会被原样带出去。
	writeRaw := func(line string) error {
		if line == "" {
			return nil
		}
		if _, err := io.WriteString(w, line); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	// 旁路累积本帧的 data 行，仅用于抓 usage 与识别流中错误；不参与写出。
	var frame []string
	sawDone := false
	sawData := false
	flushFrame := func() error {
		data := dataPayload(frame)
		frame = frame[:0]
		if data == "" {
			return nil
		}
		sawData = true
		if data == "[DONE]" {
			sawDone = true
			return nil
		}
		if parsed, ok := parseChunk(data); ok {
			if err := looksLikeError(parsed); err != nil && streamErr == nil {
				streamErr = err
			}
			if u, ok := extractUsage(parsed); ok {
				usage = u
			}
		}
		return nil
	}

	for {
		line, err := reader.ReadString('\n')
		// 先原样转发这一段（可能是 `data: {...}\n`、`event: x\n`、`\n`（帧分隔）或注释 `: keepalive\n`）
		if writeErr := writeRaw(line); writeErr != nil {
			return usage, writeErr
		}
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case trimmed == "":
			// 空行 = 一帧结束
			if flushErr := flushFrame(); flushErr != nil {
				return usage, flushErr
			}
		case strings.HasPrefix(trimmed, "data:"):
			frame = append(frame, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
		}
		if err != nil {
			if flushErr := flushFrame(); flushErr != nil {
				return usage, flushErr
			}
			if err != io.EOF {
				// 读上游就炸了（超时、连接重置…）：同样补带内错误帧再收尾。
				EmitStreamError(w, "upstream_stream_interrupted", "read upstream: "+err.Error())
				return usage, err
			}
			break
		}
	}
	if usage.TotalTokens == 0 && usage.PromptTokens == 0 && usage.CompletionTokens == 0 {
		usage.Source = "none"
	}
	// SSE 流绝不能裸结束：结束必须有 [DONE]，否则客户端只能报
	// 「stream ended before a completion event」，连原因都读不到。
	if !sawDone {
		switch {
		case streamErr != nil:
			// 上游已经吐过带内错误帧（worker 自己写完 error 就 res.end() 了）：
			// 我们补上终止符，让它成为一个「带错误、但格式完整」的流。
			EmitDone(w)
		case sawData:
			// 吐过内容却直接 EOF —— 真断流，补带内错误帧 + 终止符，并记为失败。
			streamErr = &MidStreamError{Status: http.StatusBadGateway, Code: "upstream_stream_incomplete",
				Message: "upstream stream ended before [DONE]"}
			EmitStreamError(w, streamErr.Code,
				"upstream stream ended before [DONE] (truncated by worker or provider)")
		default:
			// 上游一个内容帧都没吐就 EOF：同样不能当成正常的空流。
			streamErr = &MidStreamError{Status: http.StatusBadGateway, Code: "upstream_empty",
				Message: "upstream returned no content frames"}
			EmitStreamError(w, streamErr.Code,
				"upstream returned no content frames (possible context overflow / silent reject)")
		}
	}
	// 注意不要写成 `return usage, streamErr`：streamErr 是 *MidStreamError，
	// 值为 nil 时装箱进 error 接口会得到**非 nil** 的接口，调用方误判为出错。
	if streamErr != nil {
		return usage, streamErr
	}
	return usage, nil
}

func dataPayload(lines []string) string {
	for _, line := range lines {
		if line != "" {
			return line
		}
	}
	return ""
}

func parseChunk(data string) (map[string]any, bool) {
	var parsed map[string]any
	if json.Unmarshal([]byte(data), &parsed) != nil || parsed == nil {
		return nil, false
	}
	return parsed, true
}

func looksLikeError(parsed map[string]any) *MidStreamError {
	raw, ok := parsed["error"]
	if !ok {
		return nil
	}
	err := &MidStreamError{Status: 502, Message: fmt.Sprint(raw)}
	if obj, ok := raw.(map[string]any); ok {
		if code, ok := obj["code"].(string); ok {
			err.Code = code
		}
		if msg, ok := obj["message"].(string); ok {
			err.Message = msg
		}
		if status, ok := obj["status"].(float64); ok {
			err.Status = int(status)
		}
	}
	return err
}

func extractUsage(parsed map[string]any) (Usage, bool) {
	raw, ok := parsed["usage"]
	if !ok {
		return Usage{}, false
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return Usage{}, false
	}
	toInt := func(v any) int64 {
		if f, ok := v.(float64); ok {
			return int64(f)
		}
		return 0
	}
	usage := Usage{
		PromptTokens:     toInt(obj["prompt_tokens"]),
		CompletionTokens: toInt(obj["completion_tokens"]),
		TotalTokens:      toInt(obj["total_tokens"]),
	}
	if source, ok := obj["source"].(string); ok {
		usage.Source = source
	}
	if credits, ok := obj["credits"]; ok {
		usage.Credits = credits
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	return usage, true
}
