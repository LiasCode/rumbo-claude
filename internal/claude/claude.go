// Package claude implements the external Claude subscription provider.
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/anthropics/anthropic-sdk-go/v2/packages/param"
	"github.com/anthropics/anthropic-sdk-go/v2/packages/ssestream"

	"rumbo-claude/internal/credentials"
	"rumbo-claude/protocol"
)

// Streaming removes the timeout concern, so the default leaves room to think
// and to write whole files.
const defaultMaxTokens = 64_000

// Config contains no secrets. Tokens belong to this provider, not the host.
type Config struct {
	Effort string `json:"effort"`
}

// Handler serves one host connection. Multiple processes share the token store.
type Handler struct {
	client   sdk.Client
	config   Config
	windows  map[string]int64
	mu       sync.Mutex
	keys     credentials.Store
	http     *http.Client
	tokenURL string
	apiURL   string
}

func New() *Handler {
	return &Handler{
		windows: map[string]int64{}, keys: credentials.Default("claude"),
		http:     &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		tokenURL: "https://platform.claude.com/v1/oauth/token",
		apiURL:   "https://api.anthropic.com",
	}
}

// Initialize implements protocol.Handler.
func (h *Handler) Initialize(_ context.Context, params protocol.InitializeParams) (protocol.InitializeResult, error) {
	if len(params.Config) > 0 {
		if err := json.Unmarshal(params.Config, &h.config); err != nil {
			return protocol.InitializeResult{}, fmt.Errorf("invalid config: %w", err)
		}
	}

	if h.config.Effort != "" && !slices.Contains(efforts, h.config.Effort) {
		return protocol.InitializeResult{}, fmt.Errorf("invalid config: effort is one of %s, or empty, not %q", strings.Join(efforts, ", "), h.config.Effort)
	}

	h.client = sdk.NewClient(
		option.WithBaseURL(h.apiURL), option.WithHTTPClient(h.http),
		option.WithAuthToken(""),
		option.WithHeader("User-Agent", "claude-cli/2.1.322"),
		option.WithHeader("x-app", "cli"),
		option.WithHeader("anthropic-beta", "claude-code-20250219,oauth-2025-04-20"),
	)

	// Report the effective configuration for the host's settings view.
	config, _ := json.Marshal(h.config)

	return protocol.InitializeResult{Protocol: protocol.Version, Name: "claude", Config: config, Description: description}, nil
}

// efforts are the reasoning efforts the API accepts. Which of them a model
// takes depends on the model.
var efforts = []string{"low", "medium", "high", "xhigh", "max"}

// description is what this provider lets the user choose and ask.
var description = protocol.Description{
	Model: &protocol.Option{Type: protocol.OptionString, Values: "models", Description: "Claude model to use"},
	Options: []protocol.Option{{
		Name:        "effort",
		Type:        protocol.OptionString,
		Enum:        efforts,
		Description: "How much the model thinks and how thorough it is. Empty, the model's own default applies.",
		Default:     json.RawMessage(`""`),
	}},
	Functions: []protocol.Function{
		{Name: "models", Description: "List the models this account can use"},
		{Name: "login", Description: "Sign in with a Claude Pro/Max subscription (OAuth copy-code flow)"},
		{
			Name:        "logout",
			Description: "Remove the Claude subscription tokens stored on this computer",
			Confirm:     "This signs out this external provider locally. It does not revoke tokens at Anthropic or touch other applications.",
		},
		{Name: "account", Description: "Show whether this provider is signed in, without exposing tokens"},
	},
}

// options are the values a request may carry, as described above.
type options struct {
	Effort string `json:"effort"`
}

// InvokeDialog implements protocol.DialogInvoker.
func (h *Handler) InvokeDialog(ctx context.Context, name string, _ json.RawMessage, dialog protocol.Dialog) (any, error) {
	switch name {
	case "models":
		return h.models(ctx)
	case "login":
		return h.login(ctx, dialog)
	case "logout":
		return h.logout(ctx)
	case "account":
		return h.account()
	}

	return nil, fmt.Errorf("unknown function %q", name)
}

func (h *Handler) models(ctx context.Context) (protocol.ValuesResult, error) {
	return withAuth(h, ctx, func(using []option.RequestOption) (protocol.ValuesResult, error) {
		result := protocol.ValuesResult{Values: []protocol.Value{}}
		pages := h.client.Models.ListAutoPaging(ctx, sdk.ModelListParams{}, using...)
		for pages.Next() {
			model := pages.Current()
			result.Values = append(result.Values, protocol.Value{Value: model.ID, Label: model.DisplayName})
		}
		return result, pages.Err()
	})
}

// Complete implements protocol.Handler.
func (h *Handler) Complete(ctx context.Context, params protocol.CompleteParams, emit func(text string)) (protocol.CompleteResult, error) {
	request, err := h.request(params)

	if err != nil {
		return protocol.CompleteResult{}, err
	}

	stream, err := withAuth(h, ctx, func(using []option.RequestOption) (*ssestream.Stream[sdk.MessageStreamEventUnion], error) {
		stream := h.client.Messages.NewStreaming(ctx, request, using...)
		if err := stream.Err(); err != nil {
			stream.Close()
			return nil, err
		}
		return stream, nil
	})
	if err != nil {
		return protocol.CompleteResult{}, err
	}
	defer stream.Close()
	message := sdk.Message{}
	finished := false

	for stream.Next() {
		event := stream.Current()
		finished = event.Type == "message_stop"

		if err := message.Accumulate(event); err != nil {
			return protocol.CompleteResult{}, err
		}

		if delta, ok := event.AsAny().(sdk.ContentBlockDeltaEvent); ok {
			if text, ok := delta.Delta.AsAny().(sdk.TextDelta); ok {
				emit(text.Text)
			}
		}
		if finished {
			break
		}
	}

	if err := stream.Err(); err != nil {
		return protocol.CompleteResult{}, classify(ctx, err)
	}

	if err := ctx.Err(); err != nil {
		return protocol.CompleteResult{}, err
	}
	if !finished || message.StopReason == "" {
		return protocol.CompleteResult{}, protocol.Transient(errors.New("Claude stream ended before message_stop or stop_reason"))
	}

	completed := result(message)
	for i := range completed.Message.Content {
		block := &completed.Message.Content[i]
		if block.Type == protocol.BlockToolCall {
			block.Name = fromClaudeName(block.Name, params.Tools)
		}
	}
	completed.ContextWindow = h.window(ctx, params.Model)

	return completed, nil
}

// window returns the context size reported by the API. Successful sizes are
// cached per model. A failed lookup returns zero so the host uses its fallback,
// but can be tried again on a later completion.
func (h *Handler) window(ctx context.Context, model string) int64 {
	h.mu.Lock()
	size, known := h.windows[model]
	h.mu.Unlock()
	if known {
		return size
	}

	// Optional metadata must not keep a completed answer waiting indefinitely.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	info, err := withAuth(h, ctx, func(using []option.RequestOption) (*sdk.ModelInfo, error) {
		return h.client.Models.Get(ctx, model, sdk.ModelGetParams{}, using...)
	})
	if err != nil || info.MaxInputTokens <= 0 {
		return 0
	}
	h.mu.Lock()
	h.windows[model] = info.MaxInputTokens
	h.mu.Unlock()
	return info.MaxInputTokens
}

// classify marks the failures worth retrying: the statuses the API documents
// as temporary, and connections that failed or dropped. The SDK has already
// retried these briefly; the host waits longer.
func classify(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return err
	}

	var failure *sdk.Error

	if errors.As(err, &failure) {
		switch status := failure.StatusCode; {
		case status == 408, status == 409, status == 429, status >= 500:
			return protocol.Transient(err)

		// The API has no code of its own for a prompt that does not fit, so
		// its message is what tells it apart from other bad requests.
		case status == 413, status == 400 && strings.Contains(strings.ToLower(failure.Error()), "prompt is too long"):
			return protocol.Overflow(err)
		}

		// SSE errors arrive after HTTP 200; status alone says nothing about
		// overload or rate limiting reported inside the stream.
		switch failure.Type() {
		case sdk.ErrorTypeOverloadedError, sdk.ErrorTypeRateLimitError, sdk.ErrorTypeAPIError:
			return protocol.Transient(err)
		}
		return err
	}

	var network net.Error

	if errors.As(err, &network) || errors.Is(err, io.ErrUnexpectedEOF) {
		return protocol.Transient(err)
	}

	return err
}

func (h *Handler) request(params protocol.CompleteParams) (sdk.MessageNewParams, error) {
	request := sdk.MessageNewParams{
		Model:     params.Model,
		MaxTokens: int64(params.MaxTokens),
		System:    []sdk.TextBlockParam{{Text: "You are Claude Code, Anthropic's official CLI for Claude."}},
		// The host only ever appends to the history, so caching up to the last
		// block makes every request reuse the previous one.
		CacheControl: sdk.NewCacheControlEphemeralParam(),
	}

	if params.MaxTokens == 0 {
		request.MaxTokens = defaultMaxTokens
	}

	if params.System != "" {
		request.System = append(request.System, sdk.TextBlockParam{Text: params.System})
	}

	// Older hosts can send an explicit empty default. It inherits the
	// configured effort just like an omitted option.
	var chosen options

	if len(params.Options) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(params.Options))
		decoder.DisallowUnknownFields()

		if err := decoder.Decode(&chosen); err != nil {
			return request, fmt.Errorf("invalid options: %w", err)
		}
	}

	effort := chosen.Effort

	if effort == "" {
		effort = h.config.Effort
	}

	if effort != "" {
		request.OutputConfig = sdk.OutputConfigParam{Effort: sdk.OutputConfigEffort(effort)}
	}

	for _, def := range params.Tools {
		// Preserve the entire schema, as the OpenAI provider does. Reducing it
		// to properties/required loses $defs, additionalProperties and unions.
		var schema map[string]json.RawMessage
		if err := json.Unmarshal(def.Schema, &schema); err != nil {
			return request, fmt.Errorf("tool %s: invalid schema: %w", def.Name, err)
		}
		if schema == nil {
			return request, fmt.Errorf("tool %s: schema must be an object", def.Name)
		}
		if _, given := schema["type"]; !given {
			schema["type"] = json.RawMessage(`"object"`)
		}

		request.Tools = append(request.Tools, sdk.ToolUnionParam{OfTool: &sdk.ToolParam{
			Name:        toClaudeName(def.Name),
			Description: sdk.String(def.Description),
			InputSchema: param.Override[sdk.ToolInputSchemaParam](schema),
			// Large arguments, such as file contents, stream as they are
			// generated. The host validates the arguments before running a tool.
			EagerInputStreaming: sdk.Bool(true),
		}})
	}

	for _, message := range params.Messages {
		blocks, err := blocks(message)

		if err != nil {
			return request, err
		}

		if message.Role == protocol.RoleAssistant {
			request.Messages = append(request.Messages, sdk.NewAssistantMessage(blocks...))
		} else {
			request.Messages = append(request.Messages, sdk.NewUserMessage(blocks...))
		}
	}

	return request, nil
}

// reasoning is the opaque data kept for a thinking block, which the API
// requires back unchanged on later requests.
type reasoning struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
}

func blocks(message protocol.Message) ([]sdk.ContentBlockParamUnion, error) {
	var out []sdk.ContentBlockParamUnion

	for _, block := range message.Content {
		switch block.Type {
		case protocol.BlockText:
			out = append(out, sdk.NewTextBlock(block.Text))

		case protocol.BlockToolCall:
			// The API only accepts an object here, whatever the model sent.
			input := map[string]any{}
			_ = json.Unmarshal(block.Args, &input)

			out = append(out, sdk.NewToolUseBlock(block.ID, input, toClaudeName(block.Name)))

		case protocol.BlockToolResult:
			out = append(out, sdk.NewToolResultBlock(block.ID, block.Text, block.IsError))

		case protocol.BlockOpaque:
			// Opaque data left by another provider is not ours to read; it
			// decodes to no known type and is skipped.
			var kept reasoning
			_ = json.Unmarshal(block.Data, &kept)

			switch kept.Type {
			case "thinking":
				out = append(out, sdk.NewThinkingBlock(kept.Signature, kept.Thinking))
			case "redacted_thinking":
				out = append(out, sdk.NewRedactedThinkingBlock(kept.Data))
			}
		}
	}

	return out, nil
}

func result(message sdk.Message) protocol.CompleteResult {
	out := protocol.CompleteResult{
		Message: protocol.Message{Role: protocol.RoleAssistant},
		Stop:    stop(message.StopReason),
		Usage: protocol.Usage{
			Input:      message.Usage.InputTokens,
			Output:     message.Usage.OutputTokens,
			CacheRead:  message.Usage.CacheReadInputTokens,
			CacheWrite: message.Usage.CacheCreationInputTokens,
		},
	}

	for _, block := range message.Content {
		switch variant := block.AsAny().(type) {
		case sdk.TextBlock:
			out.Message.Content = append(out.Message.Content, protocol.Block{Type: protocol.BlockText, Text: variant.Text})

		case sdk.ToolUseBlock:
			args := variant.Input

			// Eagerly streamed arguments are not validated by the API. Invalid
			// ones travel as a JSON string, so the host can reject them.
			if !json.Valid(args) {
				args, _ = json.Marshal(string(args))
			}

			out.Message.Content = append(out.Message.Content, protocol.Block{
				Type: protocol.BlockToolCall, ID: variant.ID, Name: variant.Name, Args: args,
			})

		case sdk.ThinkingBlock:
			out.Message.Content = append(out.Message.Content, opaque(reasoning{
				Type: "thinking", Thinking: variant.Thinking, Signature: variant.Signature,
			}))

		case sdk.RedactedThinkingBlock:
			out.Message.Content = append(out.Message.Content, opaque(reasoning{Type: "redacted_thinking", Data: variant.Data}))
		}
	}

	return out
}

func opaque(kept reasoning) protocol.Block {
	raw, _ := json.Marshal(kept)

	return protocol.Block{Type: protocol.BlockOpaque, Data: raw}
}

func stop(reason sdk.StopReason) string {
	switch reason {
	case sdk.StopReasonEndTurn, sdk.StopReasonStopSequence:
		return protocol.StopEnd
	case sdk.StopReasonToolUse:
		return protocol.StopToolCalls
	case sdk.StopReasonMaxTokens:
		return protocol.StopMaxTokens
	case sdk.StopReasonRefusal:
		return protocol.StopRefusal
	}

	return string(reason)
}
