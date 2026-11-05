// Package mailtm is a minimal mail.tm client: create disposable inboxes and wait for
// verification mail. mail.tm is a free, token-authenticated temp-mail service.
package mailtm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const DefaultBase = "https://api.mail.tm"

// Client talks to a mail.tm compatible service.
type Client struct {
	Base string
	HTTP *http.Client
}

// Inbox is one disposable account.
type Inbox struct {
	Address  string `json:"address"`
	Password string `json:"password"`
	ID       string `json:"id"`
	Token    string `json:"token"`
}

// Message is the subset of a mail.tm message we care about.
type Message struct {
	ID   string `json:"id"`
	From struct {
		Address string `json:"address"`
		Name    string `json:"name"`
	} `json:"from"`
	Subject string   `json:"subject"`
	Intro   string   `json:"intro"`
	Text    string   `json:"text"`
	HTML    []string `json:"html"`
}

func New(base string) *Client {
	if base == "" {
		base = DefaultBase
	}
	return &Client{Base: strings.TrimRight(base, "/"), HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, path, token string, body any, out any) error {
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
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("mailtm %s %s: %d %.200s", method, path, resp.StatusCode, raw)
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// domainRow is one /domains entry.
type domainRow struct {
	Domain  string `json:"domain"`
	Active  bool   `json:"isActive"`
	Private bool   `json:"isPrivate"`
}

// decodeCollection accepts both payload shapes mail.tm serves for a collection:
// the hydra JSON-LD object ({"hydra:member":[...]}) and the bare array returned
// when the caller asks for application/json.
func decodeCollection(raw json.RawMessage) ([]json.RawMessage, error) {
	var obj struct {
		Members []json.RawMessage `json:"hydra:member"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Members != nil {
		return obj.Members, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr, nil
	}
	return nil, fmt.Errorf("mailtm: unexpected collection payload %.120s", raw)
}

// Domains lists the active public domains.
func (c *Client) Domains(ctx context.Context) ([]string, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/domains", "", nil, &raw); err != nil {
		return nil, err
	}
	items, err := decodeCollection(raw)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, item := range items {
		var m domainRow
		if json.Unmarshal(item, &m) != nil {
			continue
		}
		if m.Active && !m.Private && m.Domain != "" {
			out = append(out, m.Domain)
		}
	}
	return out, nil
}

const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// RandomLocal generates a random mailbox local part.
func RandomLocal(prefix string, n int) string {
	if n <= 0 {
		n = 10
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[rand.Intn(len(alphabet))]
	}
	return prefix + string(b)
}

// Create makes a new inbox and logs in.
func (c *Client) Create(ctx context.Context, local, password string) (*Inbox, error) {
	domains, err := c.Domains(ctx)
	if err != nil {
		return nil, err
	}
	if len(domains) == 0 {
		return nil, errors.New("mailtm: no public domains available")
	}
	addr := local + "@" + domains[0]
	var acc struct {
		ID      string `json:"id"`
		Address string `json:"address"`
	}
	err = c.do(ctx, http.MethodPost, "/accounts", "",
		map[string]string{"address": addr, "password": password}, &acc)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return nil, err
	}
	ib := &Inbox{Address: addr, Password: password, ID: acc.ID}
	if err := c.Login(ctx, ib); err != nil {
		return nil, err
	}
	return ib, nil
}

// Login fetches an access token for an inbox.
func (c *Client) Login(ctx context.Context, ib *Inbox) error {
	var t struct {
		Token string `json:"token"`
	}
	if err := c.do(ctx, http.MethodPost, "/token", "",
		map[string]string{"address": ib.Address, "password": ib.Password}, &t); err != nil {
		return err
	}
	ib.Token = t.Token
	return nil
}

// Messages lists the inbox.
func (c *Client) Messages(ctx context.Context, ib *Inbox) ([]Message, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/messages", ib.Token, nil, &raw); err != nil {
		return nil, err
	}
	items, err := decodeCollection(raw)
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(items))
	for _, item := range items {
		var m Message
		if json.Unmarshal(item, &m) != nil {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// Message fetches one message with its body.
func (c *Client) Message(ctx context.Context, ib *Inbox, id string) (*Message, error) {
	var m Message
	if err := c.do(ctx, http.MethodGet, "/messages/"+url.PathEscape(id), ib.Token, nil, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

var linkRe = regexp.MustCompile(`https?://[^\s"'<>]+`)

// Links extracts every http(s) URL from the message body, decoding &amp;.
func (m *Message) Links() []string {
	var seen = map[string]bool{}
	var out []string
	for _, s := range append([]string{m.Text, m.Intro}, m.HTML...) {
		for _, u := range linkRe.FindAllString(strings.ReplaceAll(s, "&amp;", "&"), -1) {
			u = strings.TrimRight(u, ".,);")
			if !seen[u] {
				seen[u] = true
				out = append(out, u)
			}
		}
	}
	return out
}

// WaitLink polls the inbox until a message arrives whose links match keep.
func (c *Client) WaitLink(ctx context.Context, ib *Inbox, keep func(string) bool, timeout, interval time.Duration) (string, *Message, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		msgs, err := c.Messages(ctx, ib)
		if err != nil {
			lastErr = err
		}
		for _, m := range msgs {
			full, err := c.Message(ctx, ib, m.ID)
			if err != nil {
				lastErr = err
				continue
			}
			if os.Getenv("MAILTM_DEBUG") != "" {
				fmt.Fprintf(os.Stderr, "debug: %d links in %q\n", len(full.Links()), full.Subject)
			}
			for _, u := range full.Links() {
				if keep == nil || keep(u) {
					return u, full, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		case <-time.After(interval):
		}
	}
	if lastErr != nil {
		return "", nil, fmt.Errorf("mailtm: timed out waiting for mail (%v)", lastErr)
	}
	return "", nil, errors.New("mailtm: timed out waiting for mail")
}
