package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"ai-foundation/gateway/internal/config"
)

// jsonEq compares two JSON texts by value.
func jsonEq(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got is not JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want is not JSON: %v\n%s", err, want)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestToMessages(t *testing.T) {
	cases := []struct {
		name, in, want string
		cache          bool
	}{
		{
			name: "system hoisting, images, tools and tool results",
			in: `{"model":"claude-x","messages":[
				{"role":"system","content":"be brief"},
				{"role":"user","content":[{"type":"text","text":"look"},
					{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA","detail":"high"}},
					{"type":"image_url","image_url":{"url":"https://x.example/a.jpg"}}]},
				{"role":"developer","content":[{"type":"text","text":"use tools"}]},
				{"role":"assistant","content":null,"tool_calls":[
					{"id":"call_1","type":"function","function":{"name":"get","arguments":"{\"q\":1}"}},
					{"id":"call_2","type":"function","function":{"name":"put","arguments":""}}]},
				{"role":"tool","tool_call_id":"call_1","content":"one"},
				{"role":"tool","tool_call_id":"call_2","content":[{"type":"text","text":"two"}]},
				{"role":"user","content":"thanks"},
				{"role":"assistant","content":""}],
				"tools":[{"type":"function","function":{"name":"get","description":"Get it","parameters":{"type":"object","properties":{"q":{"type":"integer"}}},"strict":true}},
					{"type":"function","function":{"name":"put"}}],
				"tool_choice":"required","parallel_tool_calls":false,
				"max_tokens":100,"max_completion_tokens":200,"stop":"END","temperature":0.5,"top_p":null,
				"user":"u-1","seed":7,"stream_options":{"include_usage":true}}`,
			cache: true,
			want: `{"model":"claude-x","max_tokens":200,
				"system":[{"type":"text","text":"be brief"},{"type":"text","text":"use tools"}],
				"messages":[
					{"role":"user","content":[{"type":"text","text":"look"},
						{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},
						{"type":"image","source":{"type":"url","url":"https://x.example/a.jpg"}}]},
					{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"get","input":{"q":1}},
						{"type":"tool_use","id":"call_2","name":"put","input":{}}]},
					{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"one"}]},
						{"type":"tool_result","tool_use_id":"call_2","content":[{"type":"text","text":"two"}]},
						{"type":"text","text":"thanks"}]}],
				"stop_sequences":["END"],"temperature":0.5,
				"tools":[{"name":"get","description":"Get it","input_schema":{"type":"object","properties":{"q":{"type":"integer"}}},"strict":true},
					{"name":"put","input_schema":{"type":"object","properties":{}}}],
				"tool_choice":{"type":"any","disable_parallel_tool_use":true},
				"metadata":{"user_id":"u-1"},"cache_control":{"type":"ephemeral"}}`,
		},
		{
			name: "defaults, stream, named tool, structured output, effort",
			in: `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}],"stop":["a","b"],
				"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],
				"tool_choice":{"type":"function","function":{"name":"f"}},
				"response_format":{"type":"json_schema","json_schema":{"name":"x","strict":true,"schema":{"type":"object"}}},
				"reasoning_effort":"minimal"}`,
			want: `{"model":"m","max_tokens":16000,"stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],
				"stop_sequences":["a","b"],
				"tools":[{"name":"f","input_schema":{"type":"object"}}],
				"tool_choice":{"type":"tool","name":"f"},
				"output_config":{"effort":"low","format":{"type":"json_schema","schema":{"type":"object"}}}}`,
		},
		{
			name: "tool_choice without tools is dropped; none keeps parallel use",
			in:   `{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":"auto","response_format":{"type":"text"},"reasoning_effort":"high"}`,
			want: `{"model":"m","max_tokens":16000,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"output_config":{"effort":"high"}}`,
		},
		{
			name: "tool_choice none with parallel_tool_calls false",
			in:   `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"none","parallel_tool_calls":false}`,
			want: `{"model":"m","max_tokens":16000,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],
				"tools":[{"name":"f","input_schema":{"type":"object","properties":{}}}],"tool_choice":{"type":"none"}}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _, err := toMessages([]byte(c.in), c.cache)
			if err != nil {
				t.Fatal(err)
			}
			jsonEq(t, got, c.want)
		})
	}
}

func TestToMessagesRefuses(t *testing.T) {
	for name, in := range map[string]string{
		"n":               `{"model":"m","n":2,"messages":[]}`,
		"logprobs":        `{"model":"m","logprobs":true,"messages":[]}`,
		"json_object":     `{"model":"m","response_format":{"type":"json_object"},"messages":[]}`,
		"audio output":    `{"model":"m","modalities":["text","audio"],"messages":[]}`,
		"audio input":     `{"model":"m","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{}}]}]}`,
		"file input":      `{"model":"m","messages":[{"role":"user","content":[{"type":"file","file":{}}]}]}`,
		"function role":   `{"model":"m","messages":[{"role":"function","name":"f","content":"x"}]}`,
		"functions":       `{"model":"m","functions":[{"name":"f"}],"messages":[]}`,
		"bad arguments":   `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c","function":{"name":"f","arguments":"{nope"}}]}]}`,
		"bad data url":    `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png,xx"}}]}]}`,
		"image in system": `{"model":"m","messages":[{"role":"system","content":[{"type":"image_url","image_url":{"url":"https://x"}}]}]}`,
		"unknown role":    `{"model":"m","messages":[{"role":"robot","content":"x"}]}`,
		"bad stop":        `{"model":"m","stop":5,"messages":[]}`,
		"bad tool type":   `{"model":"m","tools":[{"type":"web_search"}],"messages":[]}`,
		"not json":        `{"model":`,
	} {
		_, _, err := toMessages([]byte(in), false)
		var bad *badRequest
		if !errors.As(err, &bad) || bad.code == "" || bad.msg == "" {
			t.Errorf("%s: want a gateway 400, got %v", name, err)
		}
	}
}

func TestFromMessage(t *testing.T) {
	now := time.Unix(1700000000, 0)
	got, err := fromMessage([]byte(`{"id":"msg_1","model":"claude-x","stop_reason":"tool_use","content":[
		{"type":"thinking","thinking":"hmm","signature":"sig"},
		{"type":"text","text":"Let me "},{"type":"text","text":"check."},
		{"type":"tool_use","id":"toolu_1","name":"get","input":{"q":1}},
		{"type":"server_tool_use","id":"x"}],
		"usage":{"input_tokens":10,"cache_read_input_tokens":1000,"cache_creation_input_tokens":50,"output_tokens":20,"output_tokens_details":{"thinking_tokens":5}}}`), now)
	if err != nil {
		t.Fatal(err)
	}
	jsonEq(t, got, `{"id":"msg_1","object":"chat.completion","created":1700000000,"model":"claude-x",
		"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"Let me check.","reasoning_content":"hmm",
			"tool_calls":[{"id":"toolu_1","type":"function","function":{"name":"get","arguments":"{\"q\":1}"}}]}}],
		"usage":{"prompt_tokens":1060,"completion_tokens":20,"total_tokens":1080,
			"prompt_tokens_details":{"cached_tokens":1000,"cache_write_tokens":50},"completion_tokens_details":{"reasoning_tokens":5}}}`)

	// Tool calls only: content is null, as OpenAI sends it.
	got, _ = fromMessage([]byte(`{"id":"m","model":"x","stop_reason":"refusal","content":[]}`), now)
	if !strings.Contains(string(got), `"content":null`) || !strings.Contains(string(got), `"finish_reason":"content_filter"`) {
		t.Fatalf("no-text response: %s", got)
	}
	for stop, want := range map[string]string{"end_turn": "stop", "stop_sequence": "stop", "pause_turn": "stop",
		"max_tokens": "length", "model_context_window_exceeded": "length", "tool_use": "tool_calls", "refusal": "content_filter"} {
		if got := finishReason(stop); got != want {
			t.Errorf("finishReason(%s) = %s, want %s", stop, got, want)
		}
	}
}

const anthSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_9","model":"claude-x","usage":{"input_tokens":10,"cache_creation_input_tokens":40,"cache_read_input_tokens":200,"output_tokens":1}}}` + "\n\n" +
	"event: ping\ndata: {\"type\": \"ping\"}\n\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"plan"}}` + "\n\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}` + "\n\n" +
	`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}` + "\n\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hi"}}` + "\n\n" +
	`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"get","input":{}}}` + "\n\n" +
	`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":""}}` + "\n\n" +
	`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}` + "\n\n" +
	`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"1}"}}` + "\n\n" +
	`data: {"type":"content_block_stop","index":2}` + "\n\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":30,"output_tokens_details":{"thinking_tokens":12}}}` + "\n\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// streamOut runs the stream translation and returns the data payloads it wrote.
func streamOut(t *testing.T, in string) ([]string, error) {
	t.Helper()
	pr, pw := io.Pipe()
	go translateStream(io.NopCloser(strings.NewReader(in)), pw, time.Unix(1700000000, 0))
	b, err := io.ReadAll(pr)
	var out []string
	for _, ev := range strings.Split(strings.TrimSpace(string(b)), "\n\n") {
		if ev != "" {
			out = append(out, strings.TrimPrefix(ev, "data: "))
		}
	}
	return out, err
}

func TestTranslateStream(t *testing.T) {
	out, err := streamOut(t, anthSSE)
	if err != nil {
		t.Fatal(err)
	}
	head := `"id":"msg_9","object":"chat.completion.chunk","created":1700000000,"model":"claude-x"`
	want := []string{
		`{` + head + `,"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		`{` + head + `,"choices":[{"index":0,"delta":{"reasoning_content":"plan"},"finish_reason":null}]}`,
		`{` + head + `,"choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`,
		`{` + head + `,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"toolu_1","type":"function","function":{"name":"get","arguments":""}}]},"finish_reason":null}]}`,
		`{` + head + `,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":"}}]},"finish_reason":null}]}`,
		`{` + head + `,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]},"finish_reason":null}]}`,
		`{` + head + `,"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{` + head + `,"choices":[],"usage":{"prompt_tokens":250,"completion_tokens":30,"total_tokens":280,` +
			`"prompt_tokens_details":{"cached_tokens":200,"cache_write_tokens":40},"completion_tokens_details":{"reasoning_tokens":12}}}`,
		`[DONE]`,
	}
	if len(out) != len(want) {
		t.Fatalf("got %d events, want %d:\n%s", len(out), len(want), strings.Join(out, "\n"))
	}
	for i := range want {
		if want[i] == "[DONE]" {
			if out[i] != want[i] {
				t.Fatalf("event %d: %s", i, out[i])
			}
			continue
		}
		jsonEq(t, []byte(out[i]), want[i])
	}
}

func TestTranslateStreamEnds(t *testing.T) {
	// Cut before message_stop: the reader sees an error, not a clean end.
	cut := anthSSE[:strings.Index(anthSSE, `data: {"type":"message_stop"}`)]
	if _, err := streamOut(t, cut); err == nil || !strings.Contains(err.Error(), "message_stop") {
		t.Fatalf("cut stream: err = %v", err)
	}
	// An error event becomes an OpenAI error payload and ends the stream cleanly.
	out, err := streamOut(t, strings.SplitAfter(anthSSE, "\n\n")[0]+
		`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`+"\n\n")
	if err != nil || len(out) != 2 {
		t.Fatalf("error event: %v %q", err, out)
	}
	jsonEq(t, []byte(out[1]), `{"error":{"message":"Overloaded","type":"overloaded_error","param":null,"code":null}}`)
}

func TestNativeDo(t *testing.T) {
	var seen struct {
		path, version, key, auth string
		body                     map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.path, seen.version = r.URL.Path, r.Header.Get("Anthropic-Version")
		seen.key, seen.auth = r.Header.Get("X-Api-Key"), r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		seen.body = nil
		_ = json.Unmarshal(b, &seen.body)
		switch seen.body["model"] {
		case "slow":
			w.Header().Set("Retry-After", "7")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
		case "stream":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, anthSSE)
		default:
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"msg_1","model":"claude-x","stop_reason":"end_turn","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3,"output_tokens":1}}`)
		}
	}))
	defer srv.Close()
	mk := func(cache string) *Anthropic {
		return NewAnthropic(config.Provider{Name: "a", Type: config.TypeAnthropic, BaseURL: srv.URL + "/v1", APIKey: "k",
			OpenAIFormat: config.OpenAIFormatNative, PromptCache: cache}, srv.Client())
	}
	ctx := context.Background()

	resp, err := mk("").Do(ctx, "/chat/completions", []byte(`{"model":"claude-x","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" || !strings.Contains(string(b), `"content":"hi"`) {
		t.Fatalf("got %d %s", resp.StatusCode, b)
	}
	if seen.path != "/v1/messages" || seen.version != anthropicVersion || seen.key != "k" || seen.auth != "" {
		t.Fatalf("upstream saw %+v", seen)
	}
	if cc, _ := seen.body["cache_control"].(map[string]any); cc["type"] != "ephemeral" {
		t.Fatalf("prompt_cache auto: body %v", seen.body)
	}

	if _, err := mk(config.PromptCacheOff).Do(ctx, "/chat/completions", []byte(`{"model":"claude-x","messages":[]}`)); err != nil {
		t.Fatal(err)
	}
	if _, ok := seen.body["cache_control"]; ok {
		t.Fatalf("prompt_cache off: body %v", seen.body)
	}

	// Upstream errors keep status and headers, in the OpenAI shape.
	resp, _ = mk("").Do(ctx, "/chat/completions", []byte(`{"model":"slow","messages":[]}`))
	b, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "7" {
		t.Fatalf("rate limit: %d %v", resp.StatusCode, resp.Header)
	}
	jsonEq(t, b, `{"error":{"message":"slow down","type":"rate_limit_error","param":null,"code":null}}`)

	// Streams come back as OpenAI SSE.
	resp, _ = mk("").Do(ctx, "/chat/completions", []byte(`{"model":"stream","stream":true,"messages":[]}`))
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") ||
		!strings.Contains(string(b), `"finish_reason":"tool_calls"`) || !strings.HasSuffix(string(b), "data: [DONE]\n\n") {
		t.Fatalf("stream: %d %s", resp.StatusCode, b)
	}

	// Gateway refusals never reach the upstream.
	seen.path = ""
	for endpoint, body := range map[string]string{"/embeddings": `{"model":"m","input":"x"}`, "/chat/completions": `{"model":"m","n":3,"messages":[]}`} {
		resp, err := mk("").Do(ctx, endpoint, []byte(body))
		if err != nil || resp.StatusCode != 400 {
			t.Fatalf("%s: %v %v", endpoint, resp, err)
		}
	}
	if seen.path != "" {
		t.Fatal("a refused request reached the upstream")
	}
}

func TestErrorBodyKeepsCreditText(t *testing.T) {
	// The pool finds "credit balance" in the translated body (spec-0002 section 8).
	b := errorBody([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API."}}`), 400)
	if !strings.Contains(string(b), "credit balance") || !strings.Contains(string(b), `"type":"invalid_request_error"`) {
		t.Fatalf("got %s", b)
	}
	if b := errorBody([]byte("upstream exploded"), 502); !strings.Contains(string(b), `"message":"upstream exploded"`) {
		t.Fatalf("plain text error: %s", b)
	}
}
