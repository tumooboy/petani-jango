// Package captcha solves Cloudflare Turnstile challenges through YesCaptcha.
//
// Jango's /auth/web/login requires a turnstileToken. The widget is served with
// appearance:always and cannot be rendered from a headless client or a
// datacenter IP, and the proxyless solver also needs the widget's `action`, so
// the action ("web-login") is passed through explicitly.
package captcha

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const endpoint = "https://api.yescaptcha.com"

// Client is a YesCaptcha client.
type Client struct {
	Key  string
	HTTP *http.Client
}

// New builds a client. An empty key is read from YESCAPTCHA_CLIENT_KEY, then
// from ~/.hermes/secrets/yescaptcha.key.
func New(key string) (*Client, error) {
	if key == "" {
		key = strings.TrimSpace(os.Getenv("YESCAPTCHA_CLIENT_KEY"))
	}
	if key == "" {
		home, _ := os.UserHomeDir()
		if b, err := os.ReadFile(filepath.Join(home, ".hermes", "secrets", "yescaptcha.key")); err == nil {
			key = strings.TrimSpace(string(b))
		}
	}
	if key == "" {
		return nil, errors.New("captcha: no YesCaptcha key (set YESCAPTCHA_CLIENT_KEY)")
	}
	return &Client{Key: key, HTTP: &http.Client{Timeout: 45 * time.Second}}, nil
}

type resp struct {
	ErrorID   int    `json:"errorId"`
	ErrorCode string `json:"errorCode"`
	TaskID    string `json:"taskId"`
	Status    string `json:"status"`
	Solution  struct {
		Token string `json:"token"`
	} `json:"solution"`
}

func (c *Client) post(ctx context.Context, path string, body map[string]any) (*resp, error) {
	body["clientKey"] = c.Key
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	r, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var out resp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("captcha %s: decode %.160s", path, raw)
	}
	return &out, nil
}

// Turnstile solves one challenge and returns the token.
func (c *Client) Turnstile(ctx context.Context, pageURL, siteKey, action string) (string, error) {
	task := map[string]any{
		"type":       "TurnstileTaskProxyless",
		"websiteURL": pageURL,
		"websiteKey": siteKey,
	}
	if action != "" {
		task["pageAction"] = action
	}
	created, err := c.post(ctx, "/createTask", map[string]any{"task": task})
	if err != nil {
		return "", err
	}
	if created.ErrorID != 0 || created.TaskID == "" {
		return "", fmt.Errorf("captcha createTask: %s", created.ErrorCode)
	}

	deadline := time.Now().Add(240 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(6 * time.Second):
		}
		r, err := c.post(ctx, "/getTaskResult", map[string]any{"taskId": created.TaskID})
		if err != nil {
			return "", err
		}
		if r.Status == "ready" && r.Solution.Token != "" {
			return r.Solution.Token, nil
		}
		if r.ErrorID != 0 {
			return "", fmt.Errorf("captcha getTaskResult: %s", r.ErrorCode)
		}
	}
	return "", errors.New("captcha: turnstile timed out")
}

// PointsBalance returns the remaining points on the account.
func (c *Client) PointsBalance(ctx context.Context) (int, error) {
	body := map[string]any{"clientKey": c.Key}
	b, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/getBalance", bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	r, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer r.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var out struct {
		ErrorID int `json:"errorId"`
		Balance int `json:"balance"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, err
	}
	if out.ErrorID != 0 {
		return 0, errors.New("captcha: balance query failed")
	}
	return out.Balance, nil
}
