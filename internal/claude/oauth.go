package claude

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"rumbo-claude/internal/credentials"
	"rumbo-claude/protocol"
)

// OAuth endpoints, public client and scopes follow pi's Anthropic copy-code
// flow. This is not a client secret or an API key.
const clientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
const authorizeURL = "https://claude.ai/oauth/authorize"
const redirectURI = "https://platform.claude.com/oauth/code/callback"
const scopes = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
const credentialServer = "https://api.anthropic.com"

func (h *Handler) login(ctx context.Context, dialog protocol.Dialog) (string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	verifier := base64.RawURLEncoding.EncodeToString(random)
	digest := sha256.Sum256([]byte(verifier))
	query := url.Values{
		"code": {"true"}, "client_id": {clientID}, "response_type": {"code"},
		"redirect_uri": {redirectURI}, "scope": {scopes},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(digest[:])},
		"code_challenge_method": {"S256"}, "state": {verifier},
	}
	dialog.Say("Open this URL in your browser, sign in to Claude, then paste the code shown:\n" + authorizeURL + "?" + query.Encode())
	input, err := dialog.Ask(ctx, "Authorization code (code#state), or final redirect URL:", true)
	if err != nil {
		return "", err
	}
	code, state, err := parseCode(input)
	if err != nil {
		return "", err
	}
	if state != "" && state != verifier {
		return "", errors.New("OAuth state mismatch; nothing changed")
	}
	grant, err := h.exchange(ctx, map[string]any{
		"grant_type": "authorization_code", "client_id": clientID,
		"code": code, "state": verifier, "redirect_uri": redirectURI, "code_verifier": verifier,
	}, "")
	if err != nil {
		return "", err
	}
	if err := h.keys.Locked(ctx, func() error { return h.keys.SetGrant(credentialServer, grant) }); err != nil {
		return "", err
	}
	return "Signed in with Claude. Tokens are stored privately in " + h.keys.Path + ". Requests count against your subscription.", nil
}

func parseCode(input string) (string, string, error) {
	value := strings.TrimSpace(input)
	var code, state string
	switch {
	case strings.HasPrefix(value, "https://"), strings.HasPrefix(value, "http://"):
		u, err := url.Parse(value)
		if err != nil {
			return "", "", errors.New("invalid authorization redirect URL")
		}
		if u.Query().Get("error") != "" {
			return "", "", errors.New("authorization was denied")
		}
		code, state = u.Query().Get("code"), u.Query().Get("state")
	case strings.Contains(value, "#"):
		code, state, _ = strings.Cut(value, "#")
	case strings.Contains(value, "code="):
		query, err := url.ParseQuery(value)
		if err != nil {
			return "", "", errors.New("invalid authorization parameters")
		}
		code, state = query.Get("code"), query.Get("state")
	default:
		code = value
	}
	if code == "" {
		return "", "", errors.New("no authorization code; nothing changed")
	}
	return code, state, nil
}

// exchange never includes response bodies in errors: they may contain tokens.
func (h *Handler) exchange(ctx context.Context, body map[string]any, previousRefresh string) (*credentials.Grant, error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.tokenURL, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := h.http.Do(req)
	if err != nil {
		if parent.Err() != nil {
			return nil, parent.Err()
		}
		return nil, protocol.Transient(fmt.Errorf("OAuth token request failed: %w", err))
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		err := fmt.Errorf("OAuth token request failed (HTTP %d); use /login if the sign-in expired or was revoked", res.StatusCode)
		if res.StatusCode == 408 || res.StatusCode == 409 || res.StatusCode == 429 || res.StatusCode >= 500 {
			return nil, protocol.Transient(err)
		}
		return nil, err
	}
	var token struct {
		Access  string  `json:"access_token"`
		Refresh string  `json:"refresh_token"`
		Expires int64   `json:"expires_in"`
		Scope   *string `json:"scope"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&token); err != nil {
		if parent.Err() != nil {
			return nil, parent.Err()
		}
		var network net.Error
		if ctx.Err() != nil || errors.As(err, &network) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, protocol.Transient(errors.New("OAuth token response was interrupted; try again"))
		}
		return nil, errors.New("invalid OAuth token response")
	}
	var granted []string
	if token.Scope != nil {
		granted = strings.Fields(*token.Scope)
		if !slices.Contains(granted, "user:inference") {
			return nil, errors.New("Claude did not grant inference access; use /login to authorize it; nothing changed")
		}
	}
	if token.Refresh == "" {
		token.Refresh = previousRefresh
	}
	if token.Access == "" || token.Refresh == "" || token.Expires <= 0 || token.Expires > 365*24*60*60 {
		return nil, errors.New("OAuth response missing valid tokens or expiry; nothing changed")
	}
	return &credentials.Grant{
		AccessToken: token.Access, RefreshToken: token.Refresh,
		ExpiresAt: time.Now().Add(time.Duration(token.Expires) * time.Second), Scopes: granted,
	}, nil
}

// Lock covers read, refresh and atomic write: processes in rumbo's provider
// pool must not exchange the same rotating refresh token simultaneously.
// token also accepts a token rejected with HTTP 401. Under the lock,
// reuse another process's renewal rather than rotating its refresh token again.
func (h *Handler) token(ctx context.Context, failed string) (string, error) {
	var access string
	err := h.keys.Locked(ctx, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		grant, err := h.keys.Grant(credentialServer)
		if err != nil {
			return err
		}
		if grant == nil {
			return errors.New("not signed in to Claude; use /login")
		}
		if grant.AccessToken == "" || grant.RefreshToken == "" {
			return errors.New("incomplete Claude sign-in; use /login")
		}
		if grant.AccessToken == failed || time.Until(grant.ExpiresAt) <= 5*time.Minute {
			renewed, err := h.exchange(ctx, map[string]any{
				"grant_type": "refresh_token", "client_id": clientID, "refresh_token": grant.RefreshToken,
			}, grant.RefreshToken)
			if err != nil {
				return err
			}
			if len(renewed.Scopes) == 0 {
				renewed.Scopes = grant.Scopes
			}
			if err := h.keys.SetGrant(credentialServer, renewed); err != nil {
				return err
			}
			grant = renewed
		}
		access = grant.AccessToken
		return nil
	})
	return access, err
}

// withAuth retries authentication once, before any stream text is emitted.
// Network/server retries remain the SDK's and the host's responsibility.
func withAuth[T any](h *Handler, ctx context.Context, call func([]option.RequestOption) (T, error)) (T, error) {
	var zero T
	token, err := h.token(ctx, "")
	if err != nil {
		return zero, err
	}
	result, err := call([]option.RequestOption{option.WithAuthToken(token)})
	var failure *sdk.Error
	if errors.As(err, &failure) && failure.StatusCode == http.StatusUnauthorized && ctx.Err() == nil {
		token, err = h.token(ctx, token)
		if err != nil {
			return zero, err
		}
		result, err = call([]option.RequestOption{option.WithAuthToken(token)})
		if errors.As(err, &failure) && failure.StatusCode == http.StatusUnauthorized {
			return zero, errors.New("Claude rejected the renewed sign-in; use /login")
		}
	}
	if err != nil {
		return zero, classify(ctx, err)
	}
	return result, nil
}

func (h *Handler) logout(ctx context.Context) (string, error) {
	err := h.keys.Locked(ctx, func() error { return h.keys.SetGrant(credentialServer, nil) })
	if err != nil {
		return "", err
	}
	return "Signed out locally. No other application's credentials were changed. Tokens were not revoked at Anthropic.", nil
}

func (h *Handler) account() (string, error) {
	grant, err := h.keys.Grant(credentialServer)
	if err != nil {
		return "", err
	}
	if grant == nil {
		return "Not signed in to Claude; use /login.", nil
	}
	return "Claude subscription sign-in stored in " + h.keys.Path + ". Usage: https://claude.ai/settings/usage", nil
}
