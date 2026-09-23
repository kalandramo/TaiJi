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
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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
	fmt.Fprintf(os.Stderr, "[mock] model=%s messages=%d stream=%v roles=%s\n",
		req.Model, len(req.Messages), req.Stream, rolesOf(req.Messages))

	if !req.Stream {
		writeNonStream(w, req)
		return
	}
	writeStream(w, req)
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

	// 回显最后一条用户消息，分块发送以证明"逐块输出"
	last := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			last = req.Messages[i].Content
			break
		}
	}
	reply := "收到：" + last
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
