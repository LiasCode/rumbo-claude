// Package protocol defines the two wire contracts of rumbo. Both are JSON-RPC
// 2.0, one JSON object per line.
//
// The Agent Provider Protocol connects the host to a provider process that
// talks to a model. It is specified in docs/protocol.md; this file holds its
// types and serve.go a server for providers written in Go.
//
// The Agent Frontend Protocol connects the host to a front end. It is
// specified in docs/frontend-protocol.md and lives in frontend.go.
package protocol

import (
	"context"
	"encoding/json"
	"errors"
)

// Version is the protocol version this package implements.
const Version = 1

// Method names.
const (
	MethodInitialize = "initialize"
	MethodComplete   = "complete"
	MethodInvoke     = "invoke"
	MethodCancel     = "cancel"
	MethodDelta      = "delta"
	MethodShutdown   = "shutdown"
	// MethodAsk is in both protocols: a function of the provider asks the
	// host a question for the person, and the host asks the front end.
	MethodAsk = "ask"
)

// Block types.
const (
	BlockText       = "text"
	BlockToolCall   = "tool_call"
	BlockToolResult = "tool_result"
	BlockOpaque     = "opaque"
)

// Stop reasons.
const (
	StopEnd       = "end"
	StopToolCalls = "tool_calls"
	StopMaxTokens = "max_tokens"
	StopRefusal   = "refusal"
)

// Roles.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Block is one piece of message content.
//
//   - text: Text.
//   - tool_call: ID, Name, Args (assistant only).
//   - tool_result: ID of the call it answers, Text, IsError (user only).
//   - opaque: Data the provider needs back verbatim on later requests, such as
//     signed reasoning. The host stores and replays it without reading it.
type Block struct {
	Type    string          `json:"type"`
	Text    string          `json:"text,omitempty"`
	ID      string          `json:"id,omitempty"`
	Name    string          `json:"name,omitempty"`
	Args    json.RawMessage `json:"args,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Message is one turn of the conversation.
type Message struct {
	Role    string  `json:"role"`
	Content []Block `json:"content"`
}

// ToolDef declares a tool the model may call. Schema is a JSON Schema object.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

// Usage reports the tokens of one completion.
type Usage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read,omitempty"`
	CacheWrite int64 `json:"cache_write,omitempty"`
}

// InitializeParams opens the connection. Config is the provider's own
// configuration as registered in the host; the host does not interpret it.
type InitializeParams struct {
	Protocol int             `json:"protocol"`
	Config   json.RawMessage `json:"config,omitempty"`
}

// InitializeResult identifies the provider, or the front end, and carries
// what it says about itself.
type InitializeResult struct {
	Protocol int    `json:"protocol"`
	Name     string `json:"name"`
	// Config is the configuration in force: what initialize gave, with the
	// program's own default for every field it left out. The host shows it
	// to a person who asks, so every field is there to see, and keeps none
	// of it: only what the person chose is stored.
	Config json.RawMessage `json:"config,omitempty"`
	Description
}

// Description is how a provider tells the host and the front end what can be
// chosen and asked of it. The host passes the description to the front end,
// the chosen options to the provider with every request, and function calls
// through, and checks a value a person sets against what describes it. All
// of it is optional.
type Description struct {
	// Model says how a model is chosen, usually by naming the function that
	// lists them. Its Name is ignored.
	Model *Option `json:"model,omitempty"`
	// Options are what the provider accepts with a request, beyond the model.
	Options []Option `json:"options,omitempty"`
	// Fields are the fields of the provider's configuration, besides its
	// options, described as options are, each with its default: what a
	// person can set for it, which it reads when it starts.
	Fields []Option `json:"fields,omitempty"`
	// Functions are what the provider can be asked through invoke.
	Functions []Function `json:"functions,omitempty"`
}

// Option types.
const (
	OptionString  = "string"
	OptionNumber  = "number"
	OptionBoolean = "boolean"
)

// Option describes one value that can be chosen. It is a deliberately small
// subset of JSON Schema, so a front end can offer it without a validator.
type Option struct {
	Name        string `json:"name,omitempty"`
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
	// Enum lists the values a string option accepts, when they are fixed.
	Enum []string `json:"enum,omitempty"`
	// Values names the function that returns the accepted values, when they
	// are not fixed. The function answers with a ValuesResult.
	Values string `json:"values,omitempty"`
	// Default is the value in force when none is chosen, for a person to
	// see; it may be empty when that means the model decides. The host
	// sends an option only when a value was chosen for it.
	Default json.RawMessage `json:"default,omitempty"`
	// Optional says a parameter of a function may be left out. Optional
	// parameters come after the required ones.
	Optional bool `json:"optional,omitempty"`
}

// Function describes one operation of the provider or of the host.
type Function struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Params      []Option `json:"params,omitempty"`
	// Confirm, when set, says the function cannot be undone and what it
	// does. A front end shows it and calls the function only after the
	// person agrees.
	Confirm string `json:"confirm,omitempty"`
	// Group says what a function of the host acts on: one of the Group
	// constants. A front end may list the functions by it. A provider's
	// functions are all the provider's and leave it empty.
	Group string `json:"group,omitempty"`
	// Lists says the function is there to list the values a parameter of
	// another function takes, which names it in Values. A front end calls
	// it for those values, and need not offer it as a command.
	Lists bool `json:"lists,omitempty"`
}

// InvokeParams calls a function of the provider.
type InvokeParams struct {
	Name   string          `json:"name"`
	Params json.RawMessage `json:"params,omitempty"`
	// Target says whose function it is, on the frontend protocol: the
	// provider's, which is the default, or the host's.
	Target string `json:"target,omitempty"`
}

// Targets of an invoke on the frontend protocol.
const (
	TargetProvider = "provider"
	TargetHost     = "host"
)

// ValuesResult is the answer of a function that an Option names in Values.
// Any other function answers with whatever JSON it likes.
type ValuesResult struct {
	Values []Value `json:"values"`
}

// Value is one accepted value of an option, with what a person needs to
// recognize it.
type Value struct {
	Value       string `json:"value"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

// CompleteParams asks for one assistant message. The host always sends the
// full history, and the history only ever grows by appending, so providers can
// cache the prefix. MaxTokens of zero means the provider's default. Options
// is an object with the values chosen for the options the provider described.
type CompleteParams struct {
	Model     string          `json:"model"`
	System    string          `json:"system"`
	Messages  []Message       `json:"messages"`
	Tools     []ToolDef       `json:"tools,omitempty"`
	MaxTokens int             `json:"max_tokens,omitempty"`
	Options   json.RawMessage `json:"options,omitempty"`
}

// CompleteResult is the finished assistant message. ContextWindow is how
// many tokens the model's context holds, when the provider knows it; the
// host uses it to decide when a conversation has to be summarized.
type CompleteResult struct {
	Message       Message `json:"message"`
	Stop          string  `json:"stop"`
	Usage         Usage   `json:"usage"`
	ContextWindow int64   `json:"context_window,omitempty"`
}

// DeltaParams streams visible text while the request with the given id is
// running. For a completion, deltas are pieces of the answer, for display
// only; the CompleteResult is the authoritative message. For a function,
// each delta is something the function says to the person while it works.
type DeltaParams struct {
	ID   int64  `json:"id"`
	Text string `json:"text"`
}

// CancelParams asks the provider to abort the request with the given id: a
// completion, or a function.
type CancelParams struct {
	ID int64 `json:"id"`
}

// AskParams is a question for the person, which a function asks while it
// works. ID is the id of the invoke it belongs to. A Secret answer, such as
// a password, is not shown while it is typed, and nothing on its way
// records it.
type AskParams struct {
	ID     int64  `json:"id"`
	Text   string `json:"text"`
	Secret bool   `json:"secret,omitempty"`
}

// AskResult is the person's answer to a question.
type AskResult struct {
	Text string `json:"text"`
}

// Dialog is how a function talks with the person while it works, such as a
// login that shows where to sign in and asks for the code it gets back.
type Dialog interface {
	// Say shows text to the person.
	Say(text string)
	// Ask shows a question and waits for the answer. It fails when the
	// person declines, or when ctx ends.
	Ask(ctx context.Context, question string, secret bool) (string, error)
}

// Frame is a JSON-RPC 2.0 message: a request (ID and Method), a notification
// (Method only), or a response (ID and Result or Error).
type Frame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	return e.Message
}

// Error codes. CodeTransient is the one a provider chooses: it marks a
// failure worth retrying, such as a rate limit, an overloaded server, or a
// dropped connection. The host waits and sends the request again.
const (
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
	CodeTransient      = 1
	// CodeOverflow marks a request that did not fit in the model's context.
	// The host summarizes the conversation and tries again.
	CodeOverflow = 2
)

// Overflow wraps err as a request that did not fit in the model's context.
func Overflow(err error) error {
	return &Error{Code: CodeOverflow, Message: err.Error()}
}

// IsOverflow reports whether err says the request did not fit.
func IsOverflow(err error) bool {
	var failure *Error

	return errors.As(err, &failure) && failure.Code == CodeOverflow
}

// Transient wraps err as a failure the host should retry.
func Transient(err error) error {
	return &Error{Code: CodeTransient, Message: err.Error()}
}

// IsTransient reports whether err is a failure the host should retry.
func IsTransient(err error) bool {
	var failure *Error

	return errors.As(err, &failure) && failure.Code == CodeTransient
}
