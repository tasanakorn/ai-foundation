package provider

// Native adapter: OpenAI chat completions <-> Anthropic Messages API (spec-0002 section 7).
// It uses its own small JSON types (D44): tool schemas and values pass through as raw JSON.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"ai-foundation/gateway/internal/metering"
)

const (
	anthropicVersion  = "2023-06-01"
	defaultMaxTokens  = 16000
	maxNativeResponse = 64 << 20
	maxNativeError    = 64 << 10
)

// ---- OpenAI request ----

type oaRequest struct {
	Model               string          `json:"model"`
	Messages            []oaMessage     `json:"messages"`
	MaxTokens           *int64          `json:"max_tokens"`
	MaxCompletionTokens *int64          `json:"max_completion_tokens"`
	Stop                json.RawMessage `json:"stop"`
	Temperature         json.RawMessage `json:"temperature"`
	TopP                json.RawMessage `json:"top_p"`
	Stream              bool            `json:"stream"`
	Tools               []oaTool        `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls"`
	ResponseFormat      *struct {
		Type       string `json:"type"`
		JSONSchema *struct {
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	} `json:"response_format"`
	ReasoningEffort string          `json:"reasoning_effort"`
	User            string          `json:"user"`
	N               *int            `json:"n"`
	Logprobs        bool            `json:"logprobs"`
	Modalities      []string        `json:"modalities"`
	Audio           json.RawMessage `json:"audio"`
	Functions       json.RawMessage `json:"functions"`
	FunctionCall    json.RawMessage `json:"function_call"`
}

type oaMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"` // a string, an array of parts, or null
	ToolCalls  []oaToolCall    `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

type oaPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
		Strict      *bool           `json:"strict"`
	} `json:"function"`
}

// ---- Messages API request ----

type msgRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int64           `json:"max_tokens"`
	System        []msgBlock      `json:"system,omitempty"`
	Messages      []msgMessage    `json:"messages"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Temperature   json.RawMessage `json:"temperature,omitempty"`
	TopP          json.RawMessage `json:"top_p,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Tools         []msgTool       `json:"tools,omitempty"`
	ToolChoice    *msgToolChoice  `json:"tool_choice,omitempty"`
	OutputConfig  *msgOutput      `json:"output_config,omitempty"`
	Metadata      *msgMetadata    `json:"metadata,omitempty"`
	CacheControl  *cacheControl   `json:"cache_control,omitempty"`
}

type msgMessage struct {
	Role    string     `json:"role"`
	Content []msgBlock `json:"content"`
}

type msgBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Source    *imageSource    `json:"source,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   []msgBlock      `json:"content,omitempty"` // tool_result
}

type imageSource struct {
	Type      string `json:"type"` // base64 or url
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type msgTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
	Strict      *bool           `json:"strict,omitempty"`
}

type msgToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
}

type msgOutput struct {
	Effort string     `json:"effort,omitempty"`
	Format *msgFormat `json:"format,omitempty"`
}

type msgFormat struct {
	Type   string          `json:"type"`
	Schema json.RawMessage `json:"schema"`
}

type msgMetadata struct {
	UserID string `json:"user_id"`
}

type cacheControl struct {
	Type string `json:"type"`
}

// badRequest is a request the gateway cannot carry to the Messages API.
type badRequest struct{ code, msg string }

func (e *badRequest) Error() string { return e.msg }

func invalid(code, format string, a ...any) *badRequest {
	return &badRequest{code, fmt.Sprintf(format, a...)}
}

// toMessages turns an OpenAI chat completions body into a Messages API body.
func toMessages(body []byte, promptCache bool) ([]byte, bool, error) {
	var in oaRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, false, invalid("invalid_json", "request body is not a valid chat completions request: %v", err)
	}
	switch {
	case in.N != nil && *in.N != 1:
		return nil, false, invalid("unsupported_parameter", "n must be 1 for this model")
	case in.Logprobs:
		return nil, false, invalid("unsupported_parameter", "logprobs is not supported for this model")
	case slices.Contains(in.Modalities, "audio") || !isNull(in.Audio):
		return nil, false, invalid("unsupported_parameter", "audio output is not supported for this model")
	case !isNull(in.Functions) || !isNull(in.FunctionCall):
		return nil, false, invalid("unsupported_parameter", "functions and function_call are not supported; use tools and tool_choice")
	}
	out := msgRequest{Model: in.Model, MaxTokens: defaultMaxTokens, Stream: in.Stream, Messages: []msgMessage{}}
	switch {
	case in.MaxCompletionTokens != nil:
		out.MaxTokens = *in.MaxCompletionTokens
	case in.MaxTokens != nil:
		out.MaxTokens = *in.MaxTokens
	}
	if !isNull(in.Temperature) {
		out.Temperature = in.Temperature
	}
	if !isNull(in.TopP) {
		out.TopP = in.TopP
	}
	if !isNull(in.Stop) {
		var one string
		if json.Unmarshal(in.Stop, &one) == nil {
			out.StopSequences = []string{one}
		} else if err := json.Unmarshal(in.Stop, &out.StopSequences); err != nil {
			return nil, false, invalid("invalid_parameter", "stop must be a string or an array of strings")
		}
	}
	if in.User != "" {
		out.Metadata = &msgMetadata{UserID: in.User}
	}
	if promptCache {
		out.CacheControl = &cacheControl{Type: "ephemeral"}
	}
	if err := convertMessages(in.Messages, &out); err != nil {
		return nil, false, err
	}
	if err := convertTools(&in, &out); err != nil {
		return nil, false, err
	}
	if err := convertOutput(&in, &out); err != nil {
		return nil, false, err
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, false, err
	}
	return b, in.Stream, nil
}

func convertMessages(msgs []oaMessage, out *msgRequest) error {
	add := func(role string, blocks []msgBlock) {
		if len(blocks) == 0 {
			return
		}
		if n := len(out.Messages); n > 0 && out.Messages[n-1].Role == role {
			last := &out.Messages[n-1]
			if blocks[0].Type == "tool_result" { // tool results go before other user content
				lead := 0
				for lead < len(last.Content) && last.Content[lead].Type == "tool_result" {
					lead++
				}
				merged := append(append(append([]msgBlock{}, last.Content[:lead]...), blocks...), last.Content[lead:]...)
				last.Content = merged
				return
			}
			last.Content = append(last.Content, blocks...)
			return
		}
		out.Messages = append(out.Messages, msgMessage{Role: role, Content: blocks})
	}
	for i, m := range msgs {
		switch m.Role {
		case "system", "developer":
			blocks, err := contentBlocks(m.Content, i, false)
			if err != nil {
				return err
			}
			out.System = append(out.System, blocks...)
		case "user":
			blocks, err := contentBlocks(m.Content, i, true)
			if err != nil {
				return err
			}
			add("user", blocks)
		case "assistant":
			blocks, err := contentBlocks(m.Content, i, false)
			if err != nil {
				return err
			}
			for _, tc := range m.ToolCalls {
				args := json.RawMessage(strings.TrimSpace(tc.Function.Arguments))
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				if !json.Valid(args) {
					return invalid("invalid_parameter", "messages[%d].tool_calls: arguments of %q is not valid JSON", i, tc.Function.Name)
				}
				blocks = append(blocks, msgBlock{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: args})
			}
			add("assistant", blocks)
		case "tool":
			blocks, err := contentBlocks(m.Content, i, true)
			if err != nil {
				return err
			}
			add("user", []msgBlock{{Type: "tool_result", ToolUseID: m.ToolCallID, Content: blocks}})
		case "function":
			return invalid("unsupported_parameter", "messages[%d]: the function role is not supported; use tool messages", i)
		default:
			return invalid("invalid_parameter", "messages[%d]: unknown role %q", i, m.Role)
		}
	}
	return nil
}

// contentBlocks turns OpenAI message content into text (and, when images is true, image) blocks.
// Empty text is dropped: the Messages API refuses empty text blocks.
func contentBlocks(raw json.RawMessage, i int, images bool) ([]msgBlock, error) {
	if isNull(raw) {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil, nil
		}
		return []msgBlock{{Type: "text", Text: s}}, nil
	}
	var parts []oaPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, invalid("invalid_parameter", "messages[%d].content must be a string or an array of parts", i)
	}
	var out []msgBlock
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				out = append(out, msgBlock{Type: "text", Text: p.Text})
			}
		case "refusal": // an earlier refusal from the assistant: nothing to send back
		case "image_url":
			if !images || p.ImageURL == nil || p.ImageURL.URL == "" {
				return nil, invalid("invalid_parameter", "messages[%d]: image_url is allowed only in user and tool content, with a url", i)
			}
			src, err := imageFrom(p.ImageURL.URL, i)
			if err != nil {
				return nil, err
			}
			out = append(out, msgBlock{Type: "image", Source: src})
		default: // input_audio, file, ...
			return nil, invalid("unsupported_parameter", "messages[%d]: content part %q is not supported for this model", i, p.Type)
		}
	}
	return out, nil
}

// imageFrom reads an OpenAI image URL: a base64 data URL or a web URL.
func imageFrom(u string, i int) (*imageSource, error) {
	if rest, ok := strings.CutPrefix(u, "data:"); ok {
		meta, data, found := strings.Cut(rest, ",")
		media, enc, _ := strings.Cut(meta, ";")
		if !found || enc != "base64" || media == "" {
			return nil, invalid("invalid_parameter", "messages[%d]: an image data URL must look like data:image/png;base64,...", i)
		}
		return &imageSource{Type: "base64", MediaType: media, Data: data}, nil
	}
	return &imageSource{Type: "url", URL: u}, nil
}

func convertTools(in *oaRequest, out *msgRequest) error {
	for i, t := range in.Tools {
		if t.Type != "" && t.Type != "function" {
			return invalid("unsupported_parameter", "tools[%d]: type %q is not supported; use function", i, t.Type)
		}
		schema := t.Function.Parameters
		if isNull(schema) {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, msgTool{Name: t.Function.Name, Description: t.Function.Description, InputSchema: schema, Strict: t.Function.Strict})
	}
	if len(out.Tools) == 0 {
		return nil // no tools: nothing for tool_choice to steer
	}
	var tc *msgToolChoice
	if !isNull(in.ToolChoice) {
		var s string
		if json.Unmarshal(in.ToolChoice, &s) == nil {
			switch s {
			case "auto":
				tc = &msgToolChoice{Type: "auto"}
			case "none":
				tc = &msgToolChoice{Type: "none"}
			case "required":
				tc = &msgToolChoice{Type: "any"}
			default:
				return invalid("invalid_parameter", "tool_choice %q is not supported (use auto, none, required or a function)", s)
			}
		} else {
			var obj struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if json.Unmarshal(in.ToolChoice, &obj) != nil || obj.Function.Name == "" {
				return invalid("invalid_parameter", "tool_choice must be a string or {\"type\": \"function\", \"function\": {\"name\": ...}}")
			}
			tc = &msgToolChoice{Type: "tool", Name: obj.Function.Name}
		}
	}
	if in.ParallelToolCalls != nil && !*in.ParallelToolCalls {
		if tc == nil {
			tc = &msgToolChoice{Type: "auto"}
		}
		if tc.Type != "none" {
			off := true
			tc.DisableParallelToolUse = &off
		}
	}
	out.ToolChoice = tc
	return nil
}

func convertOutput(in *oaRequest, out *msgRequest) error {
	var oc msgOutput
	if rf := in.ResponseFormat; rf != nil {
		switch rf.Type {
		case "", "text":
		case "json_schema":
			if rf.JSONSchema == nil || isNull(rf.JSONSchema.Schema) {
				return invalid("invalid_parameter", "response_format json_schema needs json_schema.schema")
			}
			oc.Format = &msgFormat{Type: "json_schema", Schema: rf.JSONSchema.Schema}
		default:
			return invalid("unsupported_parameter", "response_format %q is not supported for this model; use json_schema", rf.Type)
		}
	}
	switch e := in.ReasoningEffort; e {
	case "":
	case "minimal":
		oc.Effort = "low"
	default:
		oc.Effort = e
	}
	if oc.Effort != "" || oc.Format != nil {
		out.OutputConfig = &oc
	}
	return nil
}

// ---- responses ----

type msgContent struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
}

type msgResponse struct {
	ID         string                  `json:"id"`
	Model      string                  `json:"model"`
	Content    []msgContent            `json:"content"`
	StopReason string                  `json:"stop_reason"`
	Usage      metering.AnthropicUsage `json:"usage"`
}

type oaUsageOut struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// usageOut keeps the D20 meaning: prompt tokens count all input, cache included.
func usageOut(u metering.AnthropicUsage) *oaUsageOut {
	var o oaUsageOut
	o.PromptTokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	o.CompletionTokens = u.OutputTokens
	o.TotalTokens = o.PromptTokens + o.CompletionTokens
	o.PromptTokensDetails.CachedTokens = u.CacheReadInputTokens
	o.PromptTokensDetails.CacheWriteTokens = u.CacheCreationInputTokens
	if u.OutputTokensDetails != nil {
		o.CompletionTokensDetails.ReasoningTokens = u.OutputTokensDetails.ThinkingTokens
	}
	return &o
}

type oaFunctionOut struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type oaToolCallOut struct {
	Index    *int          `json:"index,omitempty"`
	ID       string        `json:"id,omitempty"`
	Type     string        `json:"type,omitempty"`
	Function oaFunctionOut `json:"function"`
}

type oaMessageOut struct {
	Role             string          `json:"role,omitempty"`
	Content          *string         `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ToolCalls        []oaToolCallOut `json:"tool_calls,omitempty"`
}

// oaFullMessage is the non-stream message: content is null when there is no text.
type oaFullMessage struct {
	Role             string          `json:"role"`
	Content          *string         `json:"content"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ToolCalls        []oaToolCallOut `json:"tool_calls,omitempty"`
}

type oaChoiceOut struct {
	Index        int            `json:"index"`
	Message      *oaFullMessage `json:"message,omitempty"`
	Delta        *oaMessageOut  `json:"delta,omitempty"`
	FinishReason *string        `json:"finish_reason"`
}

type oaResponseOut struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []oaChoiceOut `json:"choices"`
	Usage   *oaUsageOut   `json:"usage,omitempty"`
}

func finishReason(stop string) string {
	switch stop {
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	}
	return "stop" // end_turn, stop_sequence, pause_turn
}

// fromMessage turns a Messages API response body into a chat.completion body.
func fromMessage(data []byte, now time.Time) ([]byte, error) {
	var m msgResponse
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	msg := oaFullMessage{Role: "assistant"}
	var text, thinking strings.Builder
	hasText := false
	for _, c := range m.Content {
		switch c.Type {
		case "text":
			text.WriteString(c.Text)
			hasText = true
		case "thinking":
			thinking.WriteString(c.Thinking)
		case "tool_use":
			args := string(c.Input)
			if args == "" {
				args = "{}"
			}
			msg.ToolCalls = append(msg.ToolCalls, oaToolCallOut{ID: c.ID, Type: "function", Function: oaFunctionOut{Name: c.Name, Arguments: args}})
		}
	}
	if hasText {
		s := text.String()
		msg.Content = &s
	}
	msg.ReasoningContent = thinking.String()
	fr := finishReason(m.StopReason)
	out := oaResponseOut{ID: m.ID, Object: "chat.completion", Created: now.Unix(), Model: m.Model,
		Choices: []oaChoiceOut{{Message: &msg, FinishReason: &fr}}, Usage: usageOut(m.Usage)}
	return json.Marshal(out)
}

// errorBody turns an Anthropic error body into the OpenAI error shape, keeping the message.
func errorBody(data []byte, status int) []byte {
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	typ, msg := "upstream_error", strings.TrimSpace(string(data))
	if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
		typ, msg = e.Error.Type, e.Error.Message
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": typ, "param": nil, "code": nil}})
	return b
}

func jsonResponse(status int, header http.Header, body []byte) *http.Response {
	h := http.Header{}
	for k, vs := range header {
		h[k] = vs
	}
	h.Del("Content-Length")
	h.Del("Content-Encoding")
	h.Set("Content-Type", "application/json")
	return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header: h, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}
}

// gatewayError is a 400 the gateway makes itself, in the OpenAI shape.
func gatewayError(status int, code, msg string) *http.Response {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "invalid_request_error", "param": nil, "code": code}})
	return jsonResponse(status, nil, b)
}

// fromMessagesResponse translates an upstream Messages response into an OpenAI one.
func fromMessagesResponse(resp *http.Response, stream bool, now func() time.Time) *http.Response {
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxNativeError))
		resp.Body.Close()
		return jsonResponse(resp.StatusCode, resp.Header, errorBody(data, resp.StatusCode))
	}
	if stream && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		pr, pw := io.Pipe()
		src := resp.Body
		go translateStream(src, pw, now())
		h := resp.Header.Clone()
		h.Del("Content-Length")
		return &http.Response{StatusCode: http.StatusOK, Status: resp.Status, Header: h, ContentLength: -1,
			Body: struct {
				io.Reader
				io.Closer
			}{pr, closers{pr, src}}}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxNativeResponse))
	resp.Body.Close()
	if err == nil {
		var b []byte
		if b, err = fromMessage(data, now()); err == nil {
			return jsonResponse(http.StatusOK, resp.Header, b)
		}
	}
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": "cannot read the Anthropic response: " + err.Error(), "type": "upstream_error", "param": nil, "code": "upstream_bad_response"}})
	return jsonResponse(http.StatusBadGateway, nil, b)
}

type closers []io.Closer

func (c closers) Close() error {
	var errs []error
	for _, x := range c {
		errs = append(errs, x.Close())
	}
	return errors.Join(errs...)
}

// ---- streams ----

type streamEvent struct {
	Type    string `json:"type"`
	Message struct {
		ID    string                  `json:"id"`
		Model string                  `json:"model"`
		Usage metering.AnthropicUsage `json:"usage"`
	} `json:"message"`
	Index        int        `json:"index"`
	ContentBlock msgContent `json:"content_block"`
	Delta        struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *metering.AnthropicUsage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type streamState struct {
	w       io.Writer
	id      string
	model   string
	created int64
	usage   metering.AnthropicUsage
	stop    string
	tools   map[int]int // content block index -> tool call index
	done    bool
}

func (s *streamState) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.w, "data: %s\n\n", b)
	return err
}

func (s *streamState) chunk(d oaMessageOut, finish *string) error {
	return s.send(oaResponseOut{ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model,
		Choices: []oaChoiceOut{{Delta: &d, FinishReason: finish}}})
}

func (s *streamState) event(ev *streamEvent) error {
	switch ev.Type {
	case "message_start":
		s.id, s.model, s.usage = ev.Message.ID, ev.Message.Model, ev.Message.Usage
		empty := ""
		return s.chunk(oaMessageOut{Role: "assistant", Content: &empty}, nil)
	case "content_block_start":
		switch ev.ContentBlock.Type {
		case "tool_use":
			k := len(s.tools)
			s.tools[ev.Index] = k
			return s.chunk(oaMessageOut{ToolCalls: []oaToolCallOut{{Index: &k, ID: ev.ContentBlock.ID, Type: "function",
				Function: oaFunctionOut{Name: ev.ContentBlock.Name}}}}, nil)
		case "text":
			if t := ev.ContentBlock.Text; t != "" {
				return s.chunk(oaMessageOut{Content: &t}, nil)
			}
		}
	case "content_block_delta":
		switch ev.Delta.Type {
		case "text_delta":
			if t := ev.Delta.Text; t != "" {
				return s.chunk(oaMessageOut{Content: &t}, nil)
			}
		case "thinking_delta":
			if ev.Delta.Thinking != "" {
				return s.chunk(oaMessageOut{ReasoningContent: ev.Delta.Thinking}, nil)
			}
		case "input_json_delta":
			if k, ok := s.tools[ev.Index]; ok && ev.Delta.PartialJSON != "" {
				return s.chunk(oaMessageOut{ToolCalls: []oaToolCallOut{{Index: &k, Function: oaFunctionOut{Arguments: ev.Delta.PartialJSON}}}}, nil)
			}
		}
	case "message_delta":
		if ev.Delta.StopReason != "" {
			s.stop = ev.Delta.StopReason
		}
		if ev.Usage != nil {
			s.usage.Merge(*ev.Usage)
		}
	case "message_stop":
		s.done = true
		fr := finishReason(s.stop)
		if err := s.chunk(oaMessageOut{}, &fr); err != nil {
			return err
		}
		if err := s.send(oaResponseOut{ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model,
			Choices: []oaChoiceOut{}, Usage: usageOut(s.usage)}); err != nil {
			return err
		}
		_, err := io.WriteString(s.w, "data: [DONE]\n\n")
		return err
	case "error":
		s.done = true
		typ, msg := "upstream_error", "the Anthropic stream failed"
		if ev.Error != nil {
			typ, msg = ev.Error.Type, ev.Error.Message
		}
		return s.send(map[string]any{"error": map[string]any{"message": msg, "type": typ, "param": nil, "code": nil}})
	}
	return nil
}

// translateStream reads Anthropic SSE from src and writes OpenAI SSE chunks to pw.
// A stream that ends before message_stop ends pw with an error, so the gateway logs it.
func translateStream(src io.ReadCloser, pw *io.PipeWriter, now time.Time) {
	defer src.Close()
	s := &streamState{w: pw, created: now.Unix(), tools: map[int]int{}}
	br := bufio.NewReaderSize(src, 64<<10)
	for !s.done {
		line, err := br.ReadBytes('\n')
		if payload, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:")); ok {
			var ev streamEvent
			if json.Unmarshal(bytes.TrimSpace(payload), &ev) == nil {
				if werr := s.event(&ev); werr != nil {
					pw.CloseWithError(werr)
					return
				}
			}
		}
		if err != nil {
			if !s.done {
				if errors.Is(err, io.EOF) {
					err = errors.New("the Anthropic stream ended before message_stop")
				}
				pw.CloseWithError(err)
				return
			}
			break
		}
	}
	pw.Close()
}

func isNull(r json.RawMessage) bool {
	t := bytes.TrimSpace(r)
	return len(t) == 0 || bytes.Equal(t, []byte("null"))
}
