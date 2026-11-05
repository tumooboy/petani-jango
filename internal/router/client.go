// Package router talks to a 9Router instance: the dashboard/admin API (wiring a
// provider node, its connection, a custom model and an alias) and the OpenAI
// compatible /v1 surface used to verify a model answers.
package router

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	DefaultBase = "http://127.0.0.1:20128"
	SALT        = "9r-cli-auth"
)

// Client is a 9Router dashboard client.
type Client struct {
	Base     string
	CLIToken string
	HTTP     *http.Client
}

func New(base, cliToken string) *Client {
	if base == "" {
		base = DefaultBase
	}
	return &Client{
		Base:     strings.TrimRight(base, "/"),
		CLIToken: cliToken,
		HTTP:     &http.Client{Timeout: 30 * time.Second},
	}
}

// CLITokenFromHome derives the x-9r-cli-token from $HOME/.9router like the 9Router CLI does.
func CLITokenFromHome(home, machineID string) string {
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	dir := filepath.Join(home, ".9router")
	raw := machineID
	if raw == "" {
		if b, err := os.ReadFile(filepath.Join(dir, "machine-id")); err == nil {
			raw = strings.TrimSpace(string(b))
		}
	}
	secret := ""
	if b, err := os.ReadFile(filepath.Join(dir, "auth", "cli-secret")); err == nil {
		secret = strings.TrimSpace(string(b))
	}
	if raw == "" || secret == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw + SALT + secret))
	return hex.EncodeToString(sum[:])[:16]
}

func (c *Client) call(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.CLIToken != "" {
		req.Header.Set("x-9r-cli-token", c.CLIToken)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("9router %s %s: %d %.200s", method, path, resp.StatusCode, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("9router %s %s: decode: %w", method, path, err)
		}
	}
	return nil
}

// Node is a provider node.
type Node struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Prefix  string          `json:"prefix"`
	Type    string          `json:"type"`
	BaseURL string          `json:"baseUrl"`
	Data    json.RawMessage `json:"data"`
}

// Models lists /v1/models ids.
func (c *Client) Models(ctx context.Context, apiKey string) ([]string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("9router /v1/models: %d %.200s", resp.StatusCode, raw)
	}
	var d struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	var out []string
	for _, m := range d.Data {
		out = append(out, m.ID)
	}
	return out, nil
}

// Verify sends one tiny chat completion and returns the answer.
func (c *Client) Verify(ctx context.Context, apiKey, model string) (string, error) {
	body := map[string]any{
		"model":      model,
		"max_tokens": 32,
		"messages":   []map[string]string{{"role": "user", "content": "Reply with OK"}},
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("9router verify: %d %.300s", resp.StatusCode, raw)
	}
	var d struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return "", err
	}
	if len(d.Choices) == 0 {
		return "", fmt.Errorf("9router verify: empty choices (%.200s)", raw)
	}
	return d.Choices[0].Message.Content, nil
}

// Key is a router api key.
type Key struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Key    string `json:"key"`
	Active *bool  `json:"isActive"`
}

// EnsureKey returns the key called name, creating it if needed.
func (c *Client) EnsureKey(ctx context.Context, name string) (string, error) {
	var keys []Key
	err := c.call(ctx, http.MethodGet, "/api/keys", nil, &keys)
	if err != nil {
		var wrap struct {
			Keys []Key `json:"keys"`
			Data []Key `json:"data"`
		}
		if err2 := c.call(ctx, http.MethodGet, "/api/keys", nil, &wrap); err2 != nil {
			return "", err
		}
		keys = append(wrap.Keys, wrap.Data...)
	}
	for _, k := range keys {
		if k.Name == name && k.Key != "" {
			return k.Key, nil
		}
	}
	var created Key
	if err := c.call(ctx, http.MethodPost, "/api/keys", map[string]any{"name": name}, &created); err != nil {
		return "", err
	}
	if created.Key == "" {
		return "", fmt.Errorf("9router: key %q created without a value", name)
	}
	return created.Key, nil
}

// Providers lists provider connections.
type Providers struct {
	Connections []struct {
		ID       string `json:"id"`
		Provider string `json:"provider"`
		Name     string `json:"name"`
	} `json:"connections"`
}

// EnsureNode creates or updates an openai-compatible provider node pointing at baseURL.
func (c *Client) EnsureNode(ctx context.Context, prefix, name, baseURL string) (string, error) {
	var list struct {
		Nodes []Node `json:"nodes"`
	}
	if err := c.call(ctx, http.MethodGet, "/api/provider-nodes", nil, &list); err != nil {
		return "", err
	}
	for _, n := range list.Nodes {
		if n.Prefix == prefix && n.Type == "openai-compatible" {
			cur := n.BaseURL
			if cur == "" && len(n.Data) > 0 {
				var d struct {
					BaseURL string `json:"baseUrl"`
				}
				_ = json.Unmarshal(n.Data, &d)
				cur = d.BaseURL
			}
			if strings.TrimRight(cur, "/") != strings.TrimRight(baseURL, "/") {
				patch := map[string]any{"name": n.Name, "prefix": prefix,
					"type": "openai-compatible", "baseUrl": baseURL, "apiType": "chat"}
				if err := c.call(ctx, http.MethodPut, "/api/provider-nodes/"+n.ID, patch, nil); err != nil {
					return "", err
				}
			}
			return n.ID, nil
		}
	}
	var created struct {
		Node Node `json:"node"`
	}
	err := c.call(ctx, http.MethodPost, "/api/provider-nodes", map[string]any{
		"name": name, "prefix": prefix, "type": "openai-compatible",
		"baseUrl": baseURL, "apiType": "chat"}, &created)
	if err != nil {
		return "", err
	}
	if created.Node.ID == "" {
		return "", fmt.Errorf("9router: provider node %q was not created", prefix)
	}
	return created.Node.ID, nil
}

// EnsureConnection makes sure the node has exactly one credential.
func (c *Client) EnsureConnection(ctx context.Context, nodeID, apiKey, name string) error {
	var p Providers
	if err := c.call(ctx, http.MethodGet, "/api/providers", nil, &p); err != nil {
		return err
	}
	for _, conn := range p.Connections {
		if conn.Provider == nodeID {
			return nil
		}
	}
	return c.call(ctx, http.MethodPost, "/api/providers", map[string]any{
		"provider": nodeID, "apiKey": apiKey, "name": name}, nil)
}

// EnsureModel registers the custom model and aliases it to prefix/model.
func (c *Client) EnsureModel(ctx context.Context, prefix, model, display string) error {
	if err := c.call(ctx, http.MethodPost, "/api/models/custom", map[string]any{
		"providerAlias": prefix, "id": model, "name": display,
		"caps": map[string]bool{"vision": false, "pdf": false, "reasoning": false, "toolCall": false},
	}, nil); err != nil {
		return err
	}
	return c.call(ctx, http.MethodPut, "/api/models/alias", map[string]any{
		"model": prefix + "/" + model, "alias": prefix + "/" + model}, nil)
}
