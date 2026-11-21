package claude

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"rumbo-claude/internal/credentials"
)

type testDialog struct {
	say func(string)
	ask func(context.Context, string, bool) (string, error)
}

func (d testDialog) Say(text string) {
	if d.say != nil {
		d.say(text)
	}
}
func (d testDialog) Ask(ctx context.Context, question string, secret bool) (string, error) {
	return d.ask(ctx, question, secret)
}

func TestLoginPKCEAndPrivateStorage(t *testing.T) {
	var auth url.Values
	var exchanges int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges++
		if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("not a JSON POST")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		digest := sha256.Sum256([]byte(body["code_verifier"]))
		if auth.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(digest[:]) || auth.Get("code_challenge_method") != "S256" {
			t.Error("invalid PKCE")
		}
		if body["state"] != auth.Get("state") || body["code"] != "test-code" || body["grant_type"] != "authorization_code" || body["redirect_uri"] != redirectURI || body["client_id"] != clientID {
			t.Errorf("wrong exchange: %+v", body)
		}
		fmt.Fprint(w, `{"access_token":"secret-access","refresh_token":"secret-refresh","expires_in":3600}`)
	}))
	defer server.Close()
	h := New()
	h.keys = credentials.Store{Path: filepath.Join(t.TempDir(), "private", "claude.json")}
	h.tokenURL = server.URL
	text, err := h.login(context.Background(), testDialog{
		say: func(text string) {
			_, address, _ := strings.Cut(text, "\n")
			u, err := url.Parse(address)
			if err != nil {
				t.Fatal(err)
			}
			auth = u.Query()
			if auth.Get("client_id") != clientID || auth.Get("redirect_uri") != redirectURI || auth.Get("scope") != scopes {
				t.Error("wrong authorize URL")
			}
		},
		ask: func(_ context.Context, _ string, secret bool) (string, error) {
			if !secret {
				t.Error("authorization code must be secret")
			}
			return "test-code#" + auth.Get("state"), nil
		},
	})
	if err != nil || exchanges != 1 {
		t.Fatalf("login: %q, %v (%d exchanges)", text, err, exchanges)
	}
	if strings.Contains(text, "secret-access") || strings.Contains(text, "secret-refresh") {
		t.Fatal("token leaked in result")
	}
	grant, err := h.keys.Grant(credentialServer)
	if err != nil || grant.AccessToken != "secret-access" || grant.RefreshToken != "secret-refresh" || time.Until(grant.ExpiresAt) < 59*time.Minute {
		t.Fatalf("grant = %+v, err = %v", grant, err)
	}
	info, err := os.Stat(h.keys.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("permissions %o", info.Mode().Perm())
	}
	dir, _ := os.Stat(filepath.Dir(h.keys.Path))
	if dir.Mode().Perm() != 0700 {
		t.Errorf("directory permissions %o", dir.Mode().Perm())
	}
	account, err := h.account()
	if err != nil || strings.Contains(account, "secret-access") || !strings.Contains(account, "settings/usage") {
		t.Fatalf("account = %q, %v", account, err)
	}
	if _, err := h.logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.token(context.Background(), ""); err == nil {
		t.Fatal("request authenticated after logout")
	}
}

func TestParseCode(t *testing.T) {
	for _, input := range []string{" code#state \n", "https://platform.claude.com/oauth/code/callback?code=code&state=state", "code=code&state=state"} {
		code, state, err := parseCode(input)
		if err != nil || code != "code" || state != "state" {
			t.Errorf("%q => %q %q %v", input, code, state, err)
		}
	}
	code, state, err := parseCode("bare-code")
	if err != nil || code != "bare-code" || state != "" {
		t.Fatal("bare code not accepted")
	}
	for _, input := range []string{"", "https://example.com/?state=state", "https://example.com/?error=access_denied"} {
		if _, _, err := parseCode(input); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestFailedLoginPreservesSession(t *testing.T) {
	for _, mode := range []string{"state", "cancel", "denied", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			h := testHandler(t)
			called := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				if mode == "denied" {
					w.WriteHeader(400)
				}
				fmt.Fprint(w, `{"access_token":"DO-NOT-LEAK","expires_in":0}`)
			}))
			defer server.Close()
			h.tokenURL = server.URL
			_, err := h.login(context.Background(), testDialog{ask: func(context.Context, string, bool) (string, error) {
				if mode == "cancel" {
					return "", context.Canceled
				}
				if mode == "state" {
					return "code#wrong-state", nil
				}
				return "code", nil
			}})
			if err == nil || strings.Contains(err.Error(), "DO-NOT-LEAK") {
				t.Fatalf("bad error: %v", err)
			}
			if (mode == "state" || mode == "cancel") && called {
				t.Error("exchanged code despite state mismatch/cancel")
			}
			grant, err := h.keys.Grant(credentialServer)
			if err != nil || grant.AccessToken != "test-access" {
				t.Fatal("failed login overwrote existing session")
			}
		})
	}
}

func TestRefreshSerializedAcrossHandlers(t *testing.T) {
	h := testHandler(t)
	if err := h.keys.SetGrant(credentialServer, &credentials.Grant{AccessToken: "expired", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["grant_type"] != "refresh_token" || body["refresh_token"] != "old-refresh" || body["client_id"] != clientID {
			t.Errorf("wrong refresh: %+v", body)
		}
		fmt.Fprint(w, `{"access_token":"new-access","refresh_token":"rotated-refresh","expires_in":3600}`)
	}))
	defer server.Close()
	var wait sync.WaitGroup
	for range 8 {
		wait.Go(func() {
			other := New()
			other.keys, other.tokenURL = h.keys, server.URL
			if _, err := other.token(context.Background(), ""); err != nil {
				t.Error(err)
			}
		})
	}
	wait.Wait()
	if calls.Load() != 1 {
		t.Errorf("%d refreshes, want 1", calls.Load())
	}
	grant, err := h.keys.Grant(credentialServer)
	if err != nil || grant.AccessToken != "new-access" || grant.RefreshToken != "rotated-refresh" {
		t.Fatalf("rotation not stored: %+v, %v", grant, err)
	}
}

func TestRefreshFailureAndCancellation(t *testing.T) {
	h := testHandler(t)
	original := &credentials.Grant{AccessToken: "expired", RefreshToken: "keep-me", ExpiresAt: time.Now().Add(-time.Hour)}
	h.keys.SetGrant(credentialServer, original)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":"DO-NOT-LEAK"}`)
	}))
	defer server.Close()
	h.tokenURL = server.URL
	if _, err := h.token(context.Background(), ""); err == nil || strings.Contains(err.Error(), "DO-NOT-LEAK") {
		t.Fatalf("refresh error: %v", err)
	}
	grant, _ := h.keys.Grant(credentialServer)
	if grant.RefreshToken != "keep-me" {
		t.Fatal("failed refresh destroyed token")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.token(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestTokenEndpointRedirectNotFollowed(t *testing.T) {
	reached := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	h := testHandler(t)
	h.tokenURL = redirect.URL
	if _, err := h.exchange(context.Background(), map[string]any{"refresh_token": "private"}, ""); err == nil {
		t.Fatal("accepted redirect")
	}
	if reached {
		t.Fatal("sent tokens to redirected server")
	}
}
