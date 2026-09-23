// mock_openai 是一个最小的 OpenAI 兼容端点，仅用于本地验证 taiji chat 的
// 流式与多轮行为，不依赖外部 API key。
//
// 用法：go run ./testdata/mockopenai -addr 127.0.0.1:18080
//
// 行为：
//   - POST /v1/chat/completions → SSE 流式返回，分多个 chunk
//   - 记录每次请求收到的消息条数到 stderr（供多轮验证观察）
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18080", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handleChat)
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"mock-model","object":"model"}]}`))
	})

	log.Printf("mock openai listening on %s", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	Tools    []toolDef     `json:"tools"`
}

type toolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

type chatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 多轮验证的观察点：把收到的消息条数写到 stderr。
	// 第二句应比第一句多（含历史）。
	fmt.Fprintf(os.Stderr, "[mock] model=%s messages=%d stream=%v tools=%d roles=%s\n",
		req.Model, len(req.Messages), req.Stream, len(req.Tools), rolesOf(req.Messages))

	// 工具调用链路（issue #3 AC-1）：
	//   - 有可用工具 且 尚未收到工具结果 → 回一个 tool_call
	//   - 已收到工具结果 → 把结果并入最终回答
	if tc, ok := decideToolCall(req); ok {
		fmt.Fprintf(os.Stderr, "[mock] → tool_call %s args=%s\n", tc.Function.Name, tc.Function.Arguments)
		writeToolCallStream(w, req, tc)
		return
	}

	if !req.Stream {
		writeNonStream(w, req)
		return
	}
	writeStream(w, req)
}

// decideToolCall 判断是否应发起工具调用。
// 规则：请求带 tools、且历史中没有 tool 角色消息（即还没拿到工具结果）。
func decideToolCall(req chatRequest) (toolCall, bool) {
	if len(req.Tools) == 0 {
		return toolCall{}, false
	}
	for _, m := range req.Messages {
		if m.Role == "tool" {
			return toolCall{}, false // 已有工具结果，该给最终回答了
		}
	}
	// 选第一个工具，参数按约定填 message（echo 工具需要）
	name := req.Tools[0].Function.Name
	var tc toolCall
	tc.ID = "call_mock_1"
	tc.Type = "function"
	tc.Function.Name = name
	tc.Function.Arguments = `{"message":"hello from mock"}`
	return tc, true
}

// writeToolCallStream 以流式返回一个 tool_call。
func writeToolCallStream(w http.ResponseWriter, req chatRequest, tc toolCall) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)

	// 工具调用通常一次性给出（非逐字），但仍是 chunk 格式
	payload := map[string]any{
		"id": "chatcmpl-mock", "object": "chat.completion.chunk",
		"created": time.Now().Unix(), "model": req.Model,
		"choices": []map[string]any{{
			"index": 0,
			"delta": map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{{
					"index": 0,
					"id":    tc.ID,
					"type":  "function",
					"function": map[string]string{
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
	}
	b, _ := json.Marshal(payload)
	fmt.Fprintf(w, "data: %s\n\n", b)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func rolesOf(msgs []chatMessage) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		parts = append(parts, m.Role)
	}
	return strings.Join(parts, ",")
}

func writeStream(w http.ResponseWriter, req chatRequest) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	// 回显最后一条用户消息，分块发送以证明"逐块输出"。
	// 若历史中含工具结果，则把它并入回答（issue #3 AC-1：
	// 工具结果必须出现在最终回答里）。
	reply := "收到：" + lastUser(req.Messages)
	if toolResult := lastToolContent(req.Messages); toolResult != "" {
		reply = "工具返回：" + toolResult
	}
	chunks := chunkRunes(reply, 3)

	for _, c := range chunks {
		payload := map[string]any{
			"id":      "chatcmpl-mock",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   req.Model,
			"choices": []map[string]any{{
				"index": 0,
				"delta": map[string]string{"content": c},
			}},
		}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(30 * time.Millisecond) // 让"逐块"在时间上可观测
	}

	// 终止块 + [DONE]
	final := map[string]any{
		"id": "chatcmpl-mock", "object": "chat.completion.chunk",
		"created": time.Now().Unix(), "model": req.Model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]string{}, "finish_reason": "stop"}},
	}
	b, _ := json.Marshal(final)
	fmt.Fprintf(w, "data: %s\n\n", b)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func writeNonStream(w http.ResponseWriter, req chatRequest) {
	w.Header().Set("Content-Type", "application/json")
	reply := "收到：" + lastUser(req.Messages)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "chatcmpl-mock", "object": "chat.completion",
		"created": time.Now().Unix(), "model": req.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]string{"role": "assistant", "content": reply},
			"finish_reason": "stop",
		}},
	})
}

func lastUser(msgs []chatMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}

// lastToolContent 返回最后一条 tool 角色消息的内容（工具执行结果）。
func lastToolContent(msgs []chatMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "tool" {
			return msgs[i].Content
		}
	}
	return ""
}

func chunkRunes(s string, n int) []string {
	rs := []rune(s)
	var out []string
	for i := 0; i < len(rs); i += n {
		end := i + n
		if end > len(rs) {
			end = len(rs)
		}
		out = append(out, string(rs[i:end]))
	}
	return out
}
