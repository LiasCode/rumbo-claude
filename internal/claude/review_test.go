package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"rumbo-claude/internal/credentials"
	"rumbo-claude/protocol"
)

const messageStart = `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"model","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":1}}}`
const messageEnd = `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`
const messageStop = `{"type":"message_stop"}`

func writeEvents(w io.Writer, events ...string) {
	for _, event := range events {
		var kind struct {
			Type string `json:"type"`
		}
		json.Unmarshal([]byte(event), &kind)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind.Type, event)
	}
}

func TestUnauthorizedRenewsAndRetriesOnce(t *testing.T) {
	for _, operation := range []string{"complete", "models", "still-unauthorized"} {
		t.Run(operation, func(t *testing.T) {
			var requests, renewals atomic.Int32
			var bodies []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("x-should-retry", "false")
				if r.URL.Path == "/token" {
					renewals.Add(1)
					fmt.Fprint(w, `{"access_token":"renewed","refresh_token":"rotated","expires_in":3600}`)
					return
				}
				if r.URL.Path == "/v1/models/model" {
					fmt.Fprint(w, `{"id":"model","type":"model","max_input_tokens":200000}`)
					return
				}
				requests.Add(1)
				if operation == "complete" {
					body, _ := io.ReadAll(r.Body)
					bodies = append(bodies, string(body))
				}
				if r.Header.Get("Authorization") != "Bearer renewed" || operation == "still-unauthorized" {
					w.WriteHeader(401)
					fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"not authorized"}}`)
					return
				}
				if operation == "models" {
					fmt.Fprint(w, `{"data":[{"id":"model","display_name":"Model","type":"model"}],"has_more":false}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				writeEvents(w, messageStart, messageEnd, messageStop)
			}))
			defer server.Close()
			h := testHandler(t)
			h.apiURL, h.tokenURL = server.URL, server.URL+"/token"
			if _, err := h.Initialize(context.Background(), protocol.InitializeParams{}); err != nil {
				t.Fatal(err)
			}
			var err error
			if operation == "models" {
				_, err = h.models(context.Background())
			} else {
				_, err = h.Complete(context.Background(), protocol.CompleteParams{Model: "model", System: "stable", Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.Block{{Type: protocol.BlockText, Text: "hi"}}}}}, func(string) {})
			}
			if operation == "still-unauthorized" {
				if err == nil || protocol.IsTransient(err) || !strings.Contains(err.Error(), "/login") {
					t.Fatalf("expected bounded auth failure: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if renewals.Load() != 1 || requests.Load() != 2 {
				t.Fatalf("requests=%d renewals=%d", requests.Load(), renewals.Load())
			}
			if operation == "complete" && (len(bodies) != 2 || bodies[0] != bodies[1]) {
				t.Fatal("authentication retry changed request")
			}
		})
	}
}

func TestRejectedTokenRenewalSharedAcrossHandlers(t *testing.T) {
	h := testHandler(t) // Still unexpired; rejection, not expiry, must force renewal.
	var renewals atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		renewals.Add(1)
		fmt.Fprint(w, `{"access_token":"renewed","refresh_token":"rotated","expires_in":3600}`)
	}))
	defer server.Close()
	var wait sync.WaitGroup
	for range 8 {
		wait.Go(func() {
			other := New()
			other.keys, other.tokenURL = h.keys, server.URL
			token, err := other.token(context.Background(), "test-access")
			if err != nil || token != "renewed" {
				t.Errorf("token=%q err=%v", token, err)
			}
		})
	}
	wait.Wait()
	if renewals.Load() != 1 {
		t.Fatalf("renewed %d times", renewals.Load())
	}
}

func TestOAuthTemporaryFailuresAndCancellation(t *testing.T) {
	for _, status := range []int{400, 401, 408, 409, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			h := testHandler(t)
			before := &credentials.Grant{AccessToken: "expired", RefreshToken: "keep", ExpiresAt: time.Now().Add(-time.Hour)}
			h.keys.SetGrant(credentialServer, before)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				fmt.Fprint(w, `{"error":"DO-NOT-EXPOSE"}`)
			}))
			defer server.Close()
			h.tokenURL = server.URL
			_, err := h.token(context.Background(), "")
			transient := status == 408 || status == 409 || status == 429 || status >= 500
			if err == nil || protocol.IsTransient(err) != transient || strings.Contains(err.Error(), "DO-NOT-EXPOSE") {
				t.Fatalf("HTTP %d: %v", status, err)
			}
			kept, _ := h.keys.Grant(credentialServer)
			if kept.RefreshToken != "keep" {
				t.Fatal("failed renewal destroyed grant")
			}
		})
	}
	h := testHandler(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := h.exchange(ctx, map[string]any{}, "")
	if !errors.Is(err, context.Canceled) || protocol.IsTransient(err) {
		t.Fatalf("cancellation: %v", err)
	}
	// Failure to connect is temporary, not an invalid account.
	if _, err := h.exchange(context.Background(), map[string]any{}, ""); !protocol.IsTransient(err) {
		t.Fatalf("network failure: %v", err)
	}
}

func TestIncompleteStreamsAreNotSuccessfulAnswers(t *testing.T) {
	cases := []struct {
		name      string
		events    []string
		transient bool
	}{
		{"empty", nil, true},
		{"partial", []string{messageStart}, true},
		{"missing-stop", []string{messageStart, messageEnd}, true},
		{"missing-reason", []string{messageStart, messageStop}, true},
		{"overloaded-in-sse", []string{messageStart, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`}, true},
		{"rate-limited-in-sse", []string{messageStart, `{"type":"error","error":{"type":"rate_limit_error","message":"busy"}}`}, true},
		{"invalid-in-sse", []string{messageStart, `{"type":"error","error":{"type":"invalid_request_error","message":"invalid"}}`}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				writeEvents(w, tc.events...)
			}))
			defer server.Close()
			h := testHandler(t)
			h.apiURL = server.URL
			h.Initialize(context.Background(), protocol.InitializeParams{})
			result, err := h.Complete(context.Background(), protocol.CompleteParams{Model: "model"}, func(string) {})
			if err == nil || protocol.IsTransient(err) != tc.transient || len(result.Message.Content) != 0 {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type trackedBody struct {
	io.Reader
	closed bool
}

func (b *trackedBody) Close() error { b.closed = true; return nil }

func TestStreamBodyClosedOnEveryExit(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprint(broken), func(t *testing.T) {
			var data strings.Builder
			if broken {
				fmt.Fprint(&data, "event: message_start\ndata: not-json\n\n")
			} else {
				writeEvents(&data, messageStart, messageEnd, messageStop)
			}
			body := &trackedBody{Reader: strings.NewReader(data.String())}
			h := testHandler(t)
			h.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body, Request: r}, nil
			})
			h.Initialize(context.Background(), protocol.InitializeParams{})
			h.windows["model"] = 200000 // No unrelated metadata call in this test.
			_, err := h.Complete(context.Background(), protocol.CompleteParams{Model: "model"}, func(string) {})
			if (err != nil) != broken {
				t.Fatalf("broken=%v err=%v", broken, err)
			}
			if !body.closed {
				t.Fatal("response body not closed")
			}
		})
	}
}

func TestMetadataFailureDoesNotPoisonCache(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-should-retry", "false")
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			fmt.Fprint(w, `{"type":"error","error":{"type":"api_error","message":"retry later"}}`)
			return
		}
		fmt.Fprint(w, `{"id":"model","type":"model","max_input_tokens":200000}`)
	}))
	defer server.Close()
	h := testHandler(t)
	h.apiURL = server.URL
	h.Initialize(context.Background(), protocol.InitializeParams{})
	if size := h.window(context.Background(), "model"); size != 0 {
		t.Fatalf("initial size=%d", size)
	}
	for range 2 {
		if size := h.window(context.Background(), "model"); size != 200000 {
			t.Fatalf("size=%d", size)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("asked %d times", calls.Load())
	}
	// A cancelled optional lookup must return immediately without caching zero.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if size := h.window(ctx, "other"); size != 0 {
		t.Fatalf("cancelled size=%d", size)
	}
}

func TestFullToolSchemaPreserved(t *testing.T) {
	h := testHandler(t)
	schema := json.RawMessage(`{"type":"object","properties":{"value":{"$ref":"#/$defs/value"}},"required":["value"],"additionalProperties":false,"$defs":{"value":{"oneOf":[{"type":"integer","minimum":9007199254740993},{"type":"string"}]}}}`)
	request, err := h.request(protocol.CompleteParams{Tools: []protocol.ToolDef{{Name: "custom", Schema: schema}}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(request.Tools[0])
	if err != nil {
		t.Fatal(err)
	}
	var tool struct {
		Schema json.RawMessage `json:"input_schema"`
	}
	json.Unmarshal(raw, &tool)
	// Compare raw-key maps so key ordering is irrelevant and integers stay exact.
	var want, got map[string]json.RawMessage
	json.Unmarshal(schema, &want)
	json.Unmarshal(tool.Schema, &got)
	for key, value := range want {
		if string(got[key]) != string(value) {
			t.Errorf("%s: got %s want %s", key, got[key], value)
		}
	}
}

func TestRenewalPreservesOmittedScopesAndRejectsRemovedInference(t *testing.T) {
	for _, scope := range []string{"omitted", "user:profile", "user:inference user:profile"} {
		t.Run(scope, func(t *testing.T) {
			h := testHandler(t)
			before := &credentials.Grant{AccessToken: "expired", RefreshToken: "keep", ExpiresAt: time.Now().Add(-time.Hour), Scopes: []string{"user:inference", "user:profile"}}
			h.keys.SetGrant(credentialServer, before)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				token := map[string]any{"access_token": "new", "expires_in": 3600}
				if scope != "omitted" {
					token["scope"] = scope
				}
				json.NewEncoder(w).Encode(token)
			}))
			defer server.Close()
			h.tokenURL = server.URL
			_, err := h.token(context.Background(), "")
			kept, _ := h.keys.Grant(credentialServer)
			if scope == "user:profile" {
				if err == nil || protocol.IsTransient(err) || kept.AccessToken != "expired" {
					t.Fatalf("missing inference: grant=%+v err=%v", kept, err)
				}
			} else {
				if err != nil || kept.AccessToken != "new" || kept.RefreshToken != "keep" || strings.Join(kept.Scopes, " ") != "user:inference user:profile" {
					t.Fatalf("renewal: grant=%+v err=%v", kept, err)
				}
			}
		})
	}
}

func TestCancellationWhileReadingOAuthBodyIsNotRetried(t *testing.T) {
	h := testHandler(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":`)
		w.(http.Flusher).Flush()
		cancel()
		<-r.Context().Done()
	}))
	defer server.Close()
	h.tokenURL = server.URL
	_, err := h.exchange(ctx, map[string]any{}, "")
	if !errors.Is(err, context.Canceled) || protocol.IsTransient(err) {
		t.Fatalf("body cancellation: %v", err)
	}
}

func TestAuthRetryDoesNotRetryOtherFailures(t *testing.T) {
	h := testHandler(t)
	var calls int
	_, err := withAuth(h, context.Background(), func([]option.RequestOption) (int, error) { calls++; return 0, &sdk.Error{StatusCode: 403} })
	if err == nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
