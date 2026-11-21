package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"rumbo-claude/protocol"
)

// Exercise the actual stdin/stdout entry point, including a cancelled login.
func TestProviderProcess(t *testing.T) {
	if os.Getenv("RUMBO_CLAUDE_TEST_PROCESS") == "1" {
		main()
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProviderProcess$")
	cmd.Env = append(os.Environ(), "RUMBO_CLAUDE_TEST_PROCESS=1", "XDG_CONFIG_HOME="+t.TempDir())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { stdin.Close(); cmd.Process.Kill(); cmd.Wait() }()
	encoder, decoder := json.NewEncoder(stdin), json.NewDecoder(stdout)
	send := func(id int64, method string, params any) {
		t.Helper()
		raw, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		if err := encoder.Encode(protocol.Frame{JSONRPC: "2.0", ID: &id, Method: method, Params: raw}); err != nil {
			t.Fatal(err)
		}
	}
	receive := func() protocol.Frame {
		t.Helper()
		var frame protocol.Frame
		if err := decoder.Decode(&frame); err != nil {
			t.Fatal(err)
		}
		if frame.JSONRPC != "2.0" {
			t.Fatal("not JSON-RPC")
		}
		return frame
	}
	send(1, "initialize", protocol.InitializeParams{Protocol: 1})
	frame := receive()
	var initialized protocol.InitializeResult
	json.Unmarshal(frame.Result, &initialized)
	if frame.Error != nil || initialized.Name != "claude" || initialized.Protocol != 1 || len(initialized.Functions) != 4 {
		t.Fatalf("initialize: %+v", frame)
	}
	send(2, "invoke", protocol.InvokeParams{Name: "account"})
	frame = receive()
	if frame.Error != nil || !strings.Contains(string(frame.Result), "Not signed in") {
		t.Fatalf("account: %+v", frame)
	}
	send(3, "invoke", protocol.InvokeParams{Name: "login"})
	frame = receive()
	if frame.Method != "delta" || !strings.Contains(string(frame.Params), "claude.ai/oauth/authorize") {
		t.Fatalf("login URL: %+v", frame)
	}
	frame = receive()
	var ask protocol.AskParams
	json.Unmarshal(frame.Params, &ask)
	if frame.Method != "ask" || !ask.Secret || ask.ID != 3 {
		t.Fatalf("login question: %+v", frame)
	}
	send(0, "cancel", protocol.CancelParams{ID: 3})
	frame = receive()
	if frame.ID == nil || *frame.ID != 3 || frame.Error == nil {
		t.Fatalf("cancel login: %+v", frame)
	}
	send(4, "shutdown", struct{}{})
	frame = receive()
	if frame.ID == nil || *frame.ID != 4 || frame.Error != nil {
		t.Fatalf("shutdown: %+v", frame)
	}
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}
