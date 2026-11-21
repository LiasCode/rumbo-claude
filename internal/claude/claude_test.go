package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"rumbo-claude/internal/credentials"
	"strings"
	"testing"
	"time"

	"rumbo-claude/protocol"
)

// The handler is run against a server that replays a recorded-shape stream:
// a thinking block, text, and a tool call.
func TestCompleteRoundTripsThinkingAndToolCalls(t *testing.T) {
	var received map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-access" || r.Header.Get("x-api-key") != "" {
			t.Errorf("wrong authentication headers: %v", r.Header)
		}
		if r.Header.Get("User-Agent") != "claude-cli/2.1.322" || !strings.Contains(r.Header.Get("anthropic-beta"), "oauth-2025-04-20") {
			t.Errorf("missing OAuth identity headers: %v", r.Header)
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &received)

		w.Header().Set("Content-Type", "text/event-stream")

		events := []string{
			`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":12,"output_tokens":1,"cache_read_input_tokens":300,"cache_creation_input_tokens":20}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-abc"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Checking."}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}}`,
			`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}`,
			`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}`,
			`{"type":"content_block_stop","index":2}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":25}}`,
			`{"type":"message_stop"}`,
		}

		for _, event := range events {
			var header struct {
				Type string `json:"type"`
			}

			json.Unmarshal([]byte(event), &header)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", header.Type, event)
		}
	}))

	defer server.Close()

	t.Setenv("ANTHROPIC_API_KEY", "test-key")

	handler := testHandler(t)
	handler.apiURL = server.URL
	config, _ := json.Marshal(Config{Effort: "high"})

	if _, err := handler.Initialize(context.Background(), protocol.InitializeParams{Config: config}); err != nil {
		t.Fatal(err)
	}

	earlier := opaque(reasoning{Type: "thinking", Signature: "sig-old"})

	params := protocol.CompleteParams{
		Model:  "claude-opus-5-5",
		System: "be brief",
		Tools: []protocol.ToolDef{{
			Name: "bash", Description: "run",
			Schema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
		}},
		Messages: []protocol.Message{
			{Role: protocol.RoleUser, Content: []protocol.Block{{Type: protocol.BlockText, Text: "list files"}}},
			{Role: protocol.RoleAssistant, Content: []protocol.Block{
				earlier,
				{Type: protocol.BlockToolCall, ID: "toolu_0", Name: "bash", Args: json.RawMessage(`{"command":"pwd"}`)},
			}},
			{Role: protocol.RoleUser, Content: []protocol.Block{{Type: protocol.BlockToolResult, ID: "toolu_0", Text: "/tmp"}}},
		},
	}

	var streamed strings.Builder
	result, err := handler.Complete(context.Background(), params, func(text string) { streamed.WriteString(text) })

	if err != nil {
		t.Fatal(err)
	}

	sent, _ := json.Marshal(received)

	for _, fragment := range []string{
		`"cache_control":{"type":"ephemeral"}`,
		`"effort":"high"`,
		`"signature":"sig-old"`,
		`"tool_use_id":"toolu_0"`,
		`"eager_input_streaming":true`,
		`"max_tokens":64000`,
		`"stream":true`,
		`"name":"Bash"`,
		`You are Claude Code, Anthropic's official CLI for Claude.`,
		`be brief`,
	} {
		if !strings.Contains(string(sent), fragment) {
			t.Errorf("request is missing %s\n%s", fragment, sent)
		}
	}

	content := result.Message.Content

	if len(content) != 3 || streamed.String() != "Checking." {
		t.Fatalf("content = %+v, streamed %q", content, streamed.String())
	}

	if content[0].Type != protocol.BlockOpaque || !strings.Contains(string(content[0].Data), "sig-abc") {
		t.Fatalf("thinking block not kept as opaque: %+v", content[0])
	}

	if content[2].Name != "bash" || content[2].ID != "toolu_1" || string(content[2].Args) != `{"command":"ls"}` || result.Stop != protocol.StopToolCalls {
		t.Fatalf("tool call: %+v stop %s", content[2], result.Stop)
	}

	if result.Usage != (protocol.Usage{Input: 12, Output: 25, CacheRead: 300, CacheWrite: 20}) {
		t.Fatalf("usage = %+v", result.Usage)
	}
}

func TestTemporaryFailuresAreMarkedTransient(t *testing.T) {
	status := 529

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Tell the SDK not to retry by itself, so the test sees the first answer.
		w.Header().Set("x-should-retry", "false")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	}))

	defer server.Close()

	t.Setenv("ANTHROPIC_API_KEY", "test-key")

	handler := testHandler(t)
	handler.apiURL = server.URL
	config, _ := json.Marshal(Config{})
	handler.Initialize(context.Background(), protocol.InitializeParams{Config: config})

	params := protocol.CompleteParams{
		Model:    "claude-opus-5-5",
		Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Block{{Type: protocol.BlockText, Text: "hi"}}}},
	}

	if _, err := handler.Complete(context.Background(), params, func(string) {}); !protocol.IsTransient(err) {
		t.Fatalf("529: err = %v, want transient", err)
	}

	status = 401
	handler.tokenURL = server.URL // Both the API and the renewal reject this sign-in.

	if _, err := handler.Complete(context.Background(), params, func(string) {}); err == nil || protocol.IsTransient(err) {
		t.Fatalf("401: err = %v, want a permanent failure", err)
	}
}

// The provider says what can be chosen, lists its models through a
// function, and applies an option sent with a request.
func TestDescriptionFunctionsAndOptions(t *testing.T) {
	var completion map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if strings.HasSuffix(r.URL.Path, "/models") {
			fmt.Fprint(w, `{"data":[
				{"type":"model","id":"claude-opus-5-5","display_name":"Claude Opus 5.5","created_at":"2026-01-01T00:00:00Z"},
				{"type":"model","id":"claude-haiku-5-5","display_name":"Claude Haiku 5.5","created_at":"2026-01-01T00:00:00Z"}
			],"has_more":false,"first_id":"claude-opus-5-5","last_id":"claude-haiku-5-5"}`)

			return
		}

		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &completion)

		w.Header().Set("x-should-retry", "false")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"stop here"}}`)
	}))

	defer server.Close()

	t.Setenv("ANTHROPIC_API_KEY", "test-key")

	handler := testHandler(t)
	handler.apiURL = server.URL
	config, _ := json.Marshal(Config{Effort: "low"})
	described, err := handler.Initialize(context.Background(), protocol.InitializeParams{Config: config})

	if err != nil || described.Model == nil || described.Model.Values != "models" || described.Options[0].Name != "effort" || described.Functions[0].Name != "models" {
		t.Fatalf("described = %+v, err = %v", described, err)
	}

	result, err := handler.InvokeDialog(context.Background(), "models", nil, nil)

	if err != nil {
		t.Fatal(err)
	}

	if listed := result.(protocol.ValuesResult); len(listed.Values) != 2 || listed.Values[0] != (protocol.Value{Value: "claude-opus-5-5", Label: "Claude Opus 5.5"}) {
		t.Fatalf("models = %+v", listed)
	}

	params := protocol.CompleteParams{
		Model:    "claude-opus-5-5",
		Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Block{{Type: protocol.BlockText, Text: "hi"}}}},
		Options:  json.RawMessage(`{"effort":"high"}`),
	}

	handler.Complete(context.Background(), params, func(string) {})

	// The option sent with the request wins over the configured one.
	if sent, _ := json.Marshal(completion["output_config"]); string(sent) != `{"effort":"high"}` {
		t.Fatalf("output_config = %s", sent)
	}

	// Both omitted options and the explicit empty default sent by older
	// hosts inherit the configured effort.
	for _, options := range []string{`{"effort":""}`, `{}`, ``} {
		params.Options = json.RawMessage(options)
		handler.Complete(context.Background(), params, func(string) {})

		if sent, _ := json.Marshal(completion["output_config"]); string(sent) != `{"effort":"low"}` {
			t.Fatalf("options %s: output_config = %s", options, sent)
		}
	}

	params.Options = json.RawMessage(`{"temperature":2}`)

	if _, err := handler.Complete(context.Background(), params, func(string) {}); err == nil || !strings.Contains(err.Error(), "invalid options") {
		t.Fatalf("an option the provider did not describe: err = %v", err)
	}
}

// The provider reports the model's context size, asking the API once per
// model, and marks a prompt that does not fit so the host can recover.
func TestContextWindowAndOverflow(t *testing.T) {
	asked := 0
	tooLong := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/models/") {
			asked++

			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"type":"model","id":"claude-opus-5-5","display_name":"Claude Opus 5.5","created_at":"2026-01-01T00:00:00Z","max_input_tokens":1000000,"max_tokens":128000}`)

			return
		}

		if tooLong {
			w.Header().Set("x-should-retry", "false")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 1200000 tokens > 1000000 maximum"}}`)

			return
		}

		w.Header().Set("Content-Type", "text/event-stream")

		for _, event := range []string{
			`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":1}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi."}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`,
			`{"type":"message_stop"}`,
		} {
			var header struct {
				Type string `json:"type"`
			}

			json.Unmarshal([]byte(event), &header)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", header.Type, event)
		}
	}))

	defer server.Close()

	t.Setenv("ANTHROPIC_API_KEY", "test-key")

	handler := testHandler(t)
	handler.apiURL = server.URL
	config, _ := json.Marshal(Config{})
	handler.Initialize(context.Background(), protocol.InitializeParams{Config: config})

	params := protocol.CompleteParams{
		Model:    "claude-opus-5-5",
		Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Block{{Type: protocol.BlockText, Text: "hi"}}}},
	}

	for range 2 {
		result, err := handler.Complete(context.Background(), params, func(string) {})

		if err != nil || result.ContextWindow != 1_000_000 {
			t.Fatalf("window = %d, err = %v", result.ContextWindow, err)
		}
	}

	if asked != 1 {
		t.Fatalf("the API was asked for the model %d times, want once", asked)
	}

	tooLong = true

	if _, err := handler.Complete(context.Background(), params, func(string) {}); !protocol.IsOverflow(err) {
		t.Fatalf("a prompt that does not fit: err = %v, want an overflow", err)
	}
}

// The host can show the effective configuration and each option's default.
func TestInitializeSaysEveryDefault(t *testing.T) {
	if _, err := testHandler(t).Initialize(context.Background(), protocol.InitializeParams{Config: json.RawMessage(`{"effort":"huge"}`)}); err == nil || !strings.Contains(err.Error(), "effort") {
		t.Fatalf("an unknown effort in the config: err = %v", err)
	}

	said, err := testHandler(t).Initialize(context.Background(), protocol.InitializeParams{Config: json.RawMessage(`{"effort":"high"}`)})

	if err != nil || string(said.Config) != `{"effort":"high"}` {
		t.Fatalf("config = %s, err = %v", said.Config, err)
	}

	for _, option := range said.Options {
		if len(option.Default) == 0 {
			t.Errorf("option %s has no default", option.Name)
		}
	}
}

func testHandler(t *testing.T) *Handler {
	t.Helper()
	h := New()
	h.tokenURL = "http://127.0.0.1:1" // A forgotten local mock must never call real OAuth.
	h.keys = credentials.Store{Path: filepath.Join(t.TempDir(), "claude.json")}
	if err := h.keys.SetGrant(credentialServer, &credentials.Grant{AccessToken: "test-access", RefreshToken: "test-refresh", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return h
}
