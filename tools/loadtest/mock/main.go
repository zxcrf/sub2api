// Command mock 是压测用的假上游：模拟 Anthropic Messages 与 OpenAI
// Responses / Chat Completions 的流式与非流式响应，延迟和分片数可在运行期调整。
//
// 运行期调参：GET /__cfg?ttfb_ms=500&chunks=60&interval_ms=25
// 查看计数：  GET /__stats
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var (
	ttfbMS     atomic.Int64 // 首字节前等待
	chunks     atomic.Int64 // 流式增量事件个数
	intervalMS atomic.Int64 // 增量事件间隔

	reqTotal  atomic.Int64
	inflight  atomic.Int64
	bodyBytes atomic.Int64
)

const deltaText = "Lorem ipsum dolor sit" // 约 5 token 的增量文本

func main() {
	addr := flag.String("addr", "127.0.0.1:9100", "listen address")
	ttfb := flag.Int64("ttfb-ms", 0, "delay before first byte")
	n := flag.Int64("chunks", 60, "number of delta events per stream")
	iv := flag.Int64("interval-ms", 0, "delay between delta events")
	flag.Parse()
	ttfbMS.Store(*ttfb)
	chunks.Store(*n)
	intervalMS.Store(*iv)

	mux := http.NewServeMux()
	mux.HandleFunc("/__cfg", handleCfg)
	mux.HandleFunc("/__stats", handleStats)
	mux.HandleFunc("/", handleAPI)

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("mock upstream listening on %s", *addr)
	log.Fatal(srv.ListenAndServe())
}

func handleCfg(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	set := func(key string, dst *atomic.Int64) {
		if v := q.Get(key); v != "" {
			if i, err := strconv.ParseInt(v, 10, 64); err == nil {
				dst.Store(i)
			}
		}
	}
	set("ttfb_ms", &ttfbMS)
	set("chunks", &chunks)
	set("interval_ms", &intervalMS)
	fmt.Fprintf(w, `{"ttfb_ms":%d,"chunks":%d,"interval_ms":%d}`+"\n", ttfbMS.Load(), chunks.Load(), intervalMS.Load())
}

func handleStats(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprintf(w, `{"requests":%d,"inflight":%d,"body_bytes":%d}`+"\n", reqTotal.Load(), inflight.Load(), bodyBytes.Load())
}

func handleAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// 模型列表、探活之类的请求统一给个空成功，避免网关把账号标记为异常。
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"object":"list","data":[]}`)
		return
	}
	body, _ := io.ReadAll(r.Body)
	reqTotal.Add(1)
	bodyBytes.Add(int64(len(body)))
	inflight.Add(1)
	defer inflight.Add(-1)

	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)
	if req.Model == "" {
		req.Model = "mock-model"
	}

	if d := ttfbMS.Load(); d > 0 {
		select {
		case <-time.After(time.Duration(d) * time.Millisecond):
		case <-r.Context().Done():
			return
		}
	}

	var p protocol
	switch {
	case strings.HasSuffix(r.URL.Path, "/messages"):
		p = anthropicProto{}
	case strings.HasSuffix(r.URL.Path, "/chat/completions"):
		p = chatProto{}
	case strings.HasSuffix(r.URL.Path, "/responses"):
		p = responsesProto{}
	default:
		http.Error(w, `{"error":{"message":"mock: unknown path `+r.URL.Path+`"}}`, http.StatusNotFound)
		return
	}

	n := int(chunks.Load())
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, p.full(req.Model, n))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	io.WriteString(w, p.head(req.Model))
	fl.Flush()
	iv := time.Duration(intervalMS.Load()) * time.Millisecond
	delta := p.delta(req.Model)
	var tick *time.Ticker
	if iv > 0 {
		tick = time.NewTicker(iv)
		defer tick.Stop()
	}
	for i := 0; i < n; i++ {
		if tick != nil {
			select {
			case <-tick.C:
			case <-r.Context().Done():
				return
			}
		}
		if _, err := io.WriteString(w, delta); err != nil {
			return
		}
		// 无间隔模式下攒批刷出，模拟上游一次吐出多段的情况。
		if tick != nil || i%8 == 7 {
			fl.Flush()
		}
	}
	io.WriteString(w, p.tail(req.Model, n))
	fl.Flush()
}

type protocol interface {
	head(model string) string
	delta(model string) string
	tail(model string, n int) string
	full(model string, n int) string
}

const inputTokens = 1200

func outTokens(n int) int { return n * 5 }

// ---- Anthropic Messages ----

type anthropicProto struct{}

func (anthropicProto) head(model string) string {
	return `event: message_start
data: {"type":"message_start","message":{"id":"msg_mock0001","type":"message","role":"assistant","model":"` + model + `","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":` + strconv.Itoa(inputTokens) + `,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

`
}

func (anthropicProto) delta(string) string {
	return `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + deltaText + ` "}}

`
}

func (anthropicProto) tail(_ string, n int) string {
	return `event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":` + strconv.Itoa(outTokens(n)) + `}}

event: message_stop
data: {"type":"message_stop"}

`
}

func (anthropicProto) full(model string, n int) string {
	return `{"id":"msg_mock0001","type":"message","role":"assistant","model":"` + model + `","content":[{"type":"text","text":"` + strings.Repeat(deltaText+" ", n) + `"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":` + strconv.Itoa(inputTokens) + `,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":` + strconv.Itoa(outTokens(n)) + `}}`
}

// ---- OpenAI Chat Completions ----

type chatProto struct{}

func (chatProto) head(model string) string {
	return `data: {"id":"chatcmpl-mock0001","object":"chat.completion.chunk","created":1700000000,"model":"` + model + `","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

`
}

func (chatProto) delta(model string) string {
	return `data: {"id":"chatcmpl-mock0001","object":"chat.completion.chunk","created":1700000000,"model":"` + model + `","choices":[{"index":0,"delta":{"content":"` + deltaText + ` "},"finish_reason":null}]}

`
}

func (chatProto) tail(model string, n int) string {
	return `data: {"id":"chatcmpl-mock0001","object":"chat.completion.chunk","created":1700000000,"model":"` + model + `","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"chatcmpl-mock0001","object":"chat.completion.chunk","created":1700000000,"model":"` + model + `","choices":[],"usage":` + chatUsage(n) + `}

data: [DONE]

`
}

func chatUsage(n int) string {
	return `{"prompt_tokens":` + strconv.Itoa(inputTokens) + `,"completion_tokens":` + strconv.Itoa(outTokens(n)) + `,"total_tokens":` + strconv.Itoa(inputTokens+outTokens(n)) + `}`
}

func (chatProto) full(model string, n int) string {
	return `{"id":"chatcmpl-mock0001","object":"chat.completion","created":1700000000,"model":"` + model + `","choices":[{"index":0,"message":{"role":"assistant","content":"` + strings.Repeat(deltaText+" ", n) + `"},"finish_reason":"stop"}],"usage":` + chatUsage(n) + `}`
}

// ---- OpenAI Responses ----

type responsesProto struct{}

func respObj(model, status, output, usage string) string {
	return `{"id":"resp_mock0001","object":"response","created_at":1700000000,"status":"` + status + `","model":"` + model + `","output":[` + output + `],"usage":` + usage + `}`
}

func respUsage(n int) string {
	return `{"input_tokens":` + strconv.Itoa(inputTokens) + `,"input_tokens_details":{"cached_tokens":0},"output_tokens":` + strconv.Itoa(outTokens(n)) + `,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":` + strconv.Itoa(inputTokens+outTokens(n)) + `}`
}

func respItem(text string) string {
	b, _ := json.Marshal(text)
	return `{"id":"msg_mock0001","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":` + string(b) + `,"annotations":[]}]}`
}

func (responsesProto) head(model string) string {
	return `event: response.created
data: {"type":"response.created","sequence_number":0,"response":` + respObj(model, "in_progress", "", "null") + `}

event: response.output_item.added
data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"msg_mock0001","type":"message","status":"in_progress","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","sequence_number":2,"item_id":"msg_mock0001","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}

`
}

func (responsesProto) delta(string) string {
	return `event: response.output_text.delta
data: {"type":"response.output_text.delta","sequence_number":3,"item_id":"msg_mock0001","output_index":0,"content_index":0,"delta":"` + deltaText + ` "}

`
}

func (responsesProto) tail(model string, n int) string {
	text := strings.Repeat(deltaText+" ", n)
	tb, _ := json.Marshal(text)
	return `event: response.output_text.done
data: {"type":"response.output_text.done","sequence_number":4,"item_id":"msg_mock0001","output_index":0,"content_index":0,"text":` + string(tb) + `}

event: response.output_item.done
data: {"type":"response.output_item.done","sequence_number":5,"output_index":0,"item":` + respItem(text) + `}

event: response.completed
data: {"type":"response.completed","sequence_number":6,"response":` + respObj(model, "completed", respItem(text), respUsage(n)) + `}

`
}

func (responsesProto) full(model string, n int) string {
	return respObj(model, "completed", respItem(strings.Repeat(deltaText+" ", n)), respUsage(n))
}
