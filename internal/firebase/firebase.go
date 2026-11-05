// Package firebase is a minimal Google Identity Toolkit client: it creates and
// signs in Email/Password users for a Firebase project and can send the email
// verification link. Jango's web API only accepts a Firebase ID token, so this
// replaces the old "post email and password to the API" signup.
package firebase

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultKey is the public web API key of the Jango AI Firebase project. Web API
// keys are not secrets (they ship in the site bundle); override with --firebase-key.
const DefaultKey = "AIzaSyAkWa23MmA9S_wWUMLl1QWX5IV1wKGsKPU"

// Client talks to identitytoolkit.googleapis.com.
type Client struct {
	Key  string
	HTTP *http.Client
}

// Session is what a successful signup or sign-in yields.
type Session struct {
	IDToken      string
	RefreshToken string
	LocalID      string
	Email        string
}

// New builds a client with a 45s timeout.
func New(key string) *Client {
	if key == "" {
		key = DefaultKey
	}
	return &Client{Key: key, HTTP: &http.Client{Timeout: 45 * time.Second}}
}

type idpResponse struct {
	IDToken      string `json:"idToken"`
	RefreshToken string `json:"refreshToken"`
	LocalID      string `json:"localId"`
	Email        string `json:"email"`
	Error        *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) call(ctx context.Context, method, path string, body any) (*idpResponse, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	url := "https://identitytoolkit.googleapis.com/v1/" + path + "?key=" + c.Key
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out idpResponse
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	if resp.StatusCode >= 300 {
		msg := string(raw)
		if out.Error != nil && out.Error.Message != "" {
			msg = out.Error.Message
		}
		return nil, fmt.Errorf("firebase %s: %d %s", path, resp.StatusCode, truncate(msg, 200))
	}
	if out.IDToken == "" {
		// accounts:update / sendOobCode legitimately return no token
		if strings.HasSuffix(path, "accounts:update") || strings.HasSuffix(path, "accounts:sendOobCode") {
			return &out, nil
		}
		return nil, fmt.Errorf("firebase %s: empty idToken", path)
	}
	return &out, nil
}

// SignUp creates an Email/Password user.
func (c *Client) SignUp(ctx context.Context, email, password string) (*Session, error) {
	r, err := c.call(ctx, http.MethodPost, "accounts:signUp", map[string]any{
		"email": email, "password": password, "returnSecureToken": true,
	})
	if err != nil {
		return nil, err
	}
	return &Session{r.IDToken, r.RefreshToken, r.LocalID, r.Email}, nil
}

// SignIn exchanges credentials for a fresh ID token.
func (c *Client) SignIn(ctx context.Context, email, password string) (*Session, error) {
	r, err := c.call(ctx, http.MethodPost, "accounts:signInWithPassword", map[string]any{
		"email": email, "password": password, "returnSecureToken": true,
	})
	if err != nil {
		return nil, err
	}
	return &Session{r.IDToken, r.RefreshToken, r.LocalID, r.Email}, nil
}

// SendVerifyEmail asks Firebase to mail the verification link for idToken.
func (c *Client) SendVerifyEmail(ctx context.Context, idToken string) error {
	_, err := c.call(ctx, http.MethodPost, "accounts:sendOobCode", map[string]any{
		"requestType": "VERIFY_EMAIL", "idToken": idToken,
	})
	if err != nil {
		// A user that is already verified returns EMAIL_NOT_FOUND / already verified.
		if errors.Is(err, context.Canceled) {
			return err
		}
		return err
	}
	return nil
}

// VerifyWithOobCode completes an email verification without a browser: the
// oobCode from the verification link is applied through accounts:update.
func (c *Client) VerifyWithOobCode(ctx context.Context, oobCode string) error {
	_, err := c.call(ctx, http.MethodPost, "accounts:update", map[string]any{"oobCode": oobCode})
	return err
}

// Lookup reports whether the account's email is verified.
func (c *Client) Lookup(ctx context.Context, idToken string) (bool, error) {
	b, err := json.Marshal(map[string]any{"idToken": idToken})
	if err != nil {
		return false, err
	}
	url := "https://identitytoolkit.googleapis.com/v1/accounts:lookup?key=" + c.Key
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Users []struct {
			EmailVerified bool `json:"emailVerified"`
		} `json:"users"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return false, err
	}
	if len(out.Users) == 0 {
		return false, fmt.Errorf("firebase lookup: no user")
	}
	return out.Users[0].EmailVerified, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
