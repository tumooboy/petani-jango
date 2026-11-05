// Package jango is a typed client for the unofficial Jango AI web API
// (https://api.jangoai.chat). It covers: email signup, email verification,
// web-session login with automatic JWT rotation, chat management and the chat
// completion SSE stream.
package jango

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	DefaultAPI      = "https://api.jangoai.chat"
	DefaultApp      = "https://app.jangoai.chat"
	DefaultFirebase = "https://jango-ai.firebaseapp.com"
	// UpstreamModel is the single chat model the Jango web app uses.
	UpstreamModel = "deepseek-v4-1-flash-uncensored"
	// RefreshCookie is the long lived (30 day) web session cookie.
	RefreshCookie = "__Secure-jango_rt"
	// AccessTTL is the lifetime of the JWT handed out by /auth/web/refresh.
	AccessTTL = 15 * time.Minute
)

var DefaultUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

// User mirrors the /users/me payload.
type User struct {
	ID                 string `json:"id"`
	FirebaseUID        string `json:"firebaseUid"`
	Email              string `json:"email"`
	Name               string `json:"name"`
	Role               string `json:"role"`
	Provider           string `json:"provider"`
	Credits            string `json:"credits"`
	TotalCreditsEarned int    `json:"totalCreditsEarned"`
	TotalCreditsUsed   int    `json:"totalCreditsUsed"`
	IsPremium          bool   `json:"isPremium"`
	EmailVerified      bool   `json:"emailVerified"`
	Country            string `json:"country"`
	BlockedAt          any    `json:"blockedAt"`
}

// Credits mirrors /credits/balance.
type Credits struct {
	Balance          int `json:"balance"`
	TotalEarned      int `json:"totalCreditsEarned"`
	TotalUsed        int `json:"totalCreditsUsed"`
	MinCreditsToChat int `json:"minCreditsToChat"`
}

// Chat mirrors a /chats row.
type Chat struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	CreatedAt     string `json:"createdAt"`
	LastMessageAt string `json:"lastMessageAt"`
}

// StreamEvent is one SSE frame from /chats/{id}/messages/stream.
type StreamEvent struct {
	Type     string         `json:"type"`
	Content  string         `json:"content"`
	Metadata map[string]any `json:"metadata"`
}

// Model returns metadata.model, the real upstream model id.
func (e StreamEvent) Model() string {
	if s, ok := e.Metadata["model"].(string); ok {
		return s
	}
	return ""
}

// CreditCost returns metadata.creditCost for the done frame.
func (e StreamEvent) CreditCost() int {
	if f, ok := e.Metadata["creditCost"].(float64); ok {
		return int(f)
	}
	return 0
}

// Client is one authenticated (or authenticating) Jango session.
type Client struct {
	API  string
	App  string
	Http *http.Client
	UA   string

	mu        sync.Mutex
	access    string
	accessExp time.Time
	lastUser  *User
	onCookie  func(string)
}

// SetOnCookie registers a callback fired with the rotated refresh cookie after
// every successful token refresh.
func (c *Client) SetOnCookie(fn func(string)) {
	c.mu.Lock()
	c.onCookie = fn
	c.mu.Unlock()
}

// New builds a client with a cookie jar so web sessions survive redirects.
func New(api string) *Client {
	if api == "" {
		api = DefaultAPI
	}
	// Only persistent cookies are stored: session cookies would be evicted when
	// several accounts are created concurrently on one jar.
	jar := newAccountJar()
	return &Client{
		API:  strings.TrimRight(api, "/"),
		App:  DefaultApp,
		Http: &http.Client{Jar: jar, Timeout: 90 * time.Second},
		UA:   DefaultUA,
	}
}

// SetSession restores a previously captured refresh cookie + access token.
func (c *Client) SetSession(refreshCookie, accessToken string) error {
	u, err := url.Parse(c.API)
	if err != nil {
		return err
	}
	c.Http.Jar.SetCookies(u, []*http.Cookie{{
		Name: RefreshCookie, Value: refreshCookie, Domain: u.Hostname(),
		Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	}})
	c.mu.Lock()
	c.access = accessToken
	if accessToken != "" {
		c.accessExp = time.Now().Add(AccessTTL)
	}
	c.mu.Unlock()
	return nil
}

// actionCookie returns a token stored in the action page's cookie, if any.
func (c *Client) actionCookie() string {
	u, _ := url.Parse(c.API)
	for _, ck := range c.Http.Jar.Cookies(u) {
		switch ck.Name {
		case "fb_at", "fb_token", "firebase_token":
			if ck.Value != "" {
				return strings.TrimPrefix(ck.Value, "at-")
			}
		}
	}
	u2 := &url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/"}
	for _, ck := range c.Http.Jar.Cookies(u2) {
		if ck.Name == "fb_at" && ck.Value != "" {
			return strings.TrimPrefix(ck.Value, "at-")
		}
	}
	return ""
}

// RefreshCookie returns the jar's __Secure-jango_rt value ("" if absent).
func (c *Client) RefreshCookie() string {
	u, _ := url.Parse(c.API)
	for _, ck := range c.Http.Jar.Cookies(u) {
		if ck.Name == RefreshCookie {
			return ck.Value
		}
	}
	// the cookie is scoped to /auth/web, so also ask for that path
	u2 := &url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/auth/web"}
	for _, ck := range c.Http.Jar.Cookies(u2) {
		if ck.Name == RefreshCookie {
			return ck.Value
		}
	}
	return ""
}

func (c *Client) baseHeaders(req *http.Request, json bool) {
	req.Header.Set("User-Agent", c.UA)
	req.Header.Set("Origin", c.App)
	req.Header.Set("Referer", c.App+"/")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	if json {
		req.Header.Set("Content-Type", "application/json")
	}
}

// APIError carries a non-2xx response.
type APIError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("jango %s %s: %d %s", e.Method, e.Path, e.Status, truncate(e.Body, 300))
}

func IsStatus(err error, code int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == code
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any, auth bool) error {
	return c.doAt(ctx, c.API, method, path, body, out, auth)
}

func (c *Client) doAt(ctx context.Context, base, method, path string, body any, out any, auth bool) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return err
	}
	c.baseHeaders(req, body != nil)
	req.Header.Set("Accept", "application/json")
	if auth {
		tok, err := c.Token(ctx)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.Http.Do(req)
	if err == nil && auth && resp.StatusCode == http.StatusUnauthorized {
		// the cached token may have been rotated by another login: refresh once
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		tok, _, rerr := c.refresh(ctx, true)
		if rerr != nil {
			return rerr
		}
		req2, err2 := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(bodyBytes(body)))
		if err2 != nil {
			return err2
		}
		c.baseHeaders(req2, body != nil)
		req2.Header.Set("Accept", "application/json")
		req2.Header.Set("Authorization", "Bearer "+tok)
		resp, err = c.Http.Do(req2)
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return &APIError{Method: method, Path: path, Status: resp.StatusCode, Body: string(raw)}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("jango %s %s: decode: %w (%.120s)", method, path, err, raw)
		}
	}
	return nil
}

// Signup creates an account (POST /auth/web/login?mode=sign-up on the web, which is
// what the Supabase-style backend expects). It returns the current user.
func (c *Client) Signup(ctx context.Context, name, email, password string) (*User, error) {
	body := map[string]any{"name": name, "email": email, "password": password}
	var out struct {
		User *User `json:"user"`
	}
	err := c.do(ctx, http.MethodPost, "/auth/web/login", body, &out, false)
	if err != nil {
		return nil, err
	}
	if c.RefreshCookie() == "" {
		// Some deployments hand the session back only on refresh; make sure the jar has it.
		_, _, _ = c.refresh(ctx, true)
	}
	return out.User, nil
}

// LoginWeb exchanges a Firebase ID token plus a Cloudflare Turnstile token for
// the web session cookie. This is the only signup/sign-in path the current API
// accepts: email+password posted directly is rejected with "property email
// should not exist".
func (c *Client) LoginWeb(ctx context.Context, firebaseToken, turnstileToken string) error {
	body := map[string]any{
		"firebaseToken":  firebaseToken,
		"platform":       "web",
		"turnstileToken": turnstileToken,
	}
	if err := c.do(ctx, http.MethodPost, "/auth/web/login", body, nil, false); err != nil {
		return err
	}
	_, _, err := c.refresh(ctx, true)
	return err
}

// Login signs in with email/password.
func (c *Client) Login(ctx context.Context, email, password string) error {
	err := c.do(ctx, http.MethodPost, "/auth/web/login",
		map[string]any{"email": email, "password": password}, nil, false)
	if err != nil {
		return err
	}
	_, _, err = c.refresh(ctx, true)
	return err
}

func (c *Client) refresh(ctx context.Context, force bool) (string, time.Time, error) {
	if !force {
		c.mu.Lock()
		tok, exp := c.access, c.accessExp
		c.mu.Unlock()
		if tok != "" && time.Now().Before(exp.Add(-2*time.Minute)) {
			return tok, exp, nil
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.API+"/auth/web/refresh", nil)
	if err != nil {
		return "", time.Time{}, err
	}
	c.baseHeaders(req, false)
	req.Header.Set("Accept", "application/json")
	resp, err := c.Http.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		// fall back to the short lived access cookie the action page sets
		if ck := c.actionCookie(); ck != "" {
			c.mu.Lock()
			c.access, c.accessExp = ck, time.Now().Add(AccessTTL)
			c.mu.Unlock()
			return ck, c.accessExp, nil
		}
		return "", time.Time{}, &APIError{Method: "POST", Path: "/auth/web/refresh", Status: resp.StatusCode, Body: string(raw)}
	}
	var d struct {
		AccessToken          string `json:"accessToken"`
		AccessTokenExpiresAt string `json:"accessTokenExpiresAt"`
		User                 *User  `json:"user"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return "", time.Time{}, err
	}
	if d.AccessToken == "" {
		return "", time.Time{}, errors.New("jango: refresh returned no accessToken")
	}
	exp := time.Now().Add(AccessTTL)
	if t, err := time.Parse(time.RFC3339, d.AccessTokenExpiresAt); err == nil {
		exp = t
	}
	tok := d.AccessToken
	c.mu.Lock()
	c.access, c.accessExp = tok, exp
	if d.User != nil {
		c.lastUser = d.User
	}
	hook := c.onCookie
	c.mu.Unlock()
	// The refresh cookie rotates on every use: the previous value is revoked, so
	// whoever persists sessions has to be told about the new one immediately.
	if hook != nil {
		if nc := c.RefreshCookie(); nc != "" {
			hook(nc)
		}
	}
	return tok, exp, nil
}

// Token returns a valid access token, refreshing when needed.
func (c *Client) Token(ctx context.Context) (string, error) {
	if c.AccessToken() != "" && time.Now().Before(c.AccessExpiry().Add(-2*time.Minute)) {
		return c.AccessToken(), nil
	}
	tok, _, err := c.refresh(ctx, false)
	return tok, err
}

// AccessToken returns the cached token without refreshing.
func (c *Client) AccessToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.access
}

// AccessExpiry returns when the cached token expires.
func (c *Client) AccessExpiry() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.accessExp
}

// Me returns the profile.
func (c *Client) Me(ctx context.Context) (*User, error) {
	var u User
	if err := c.do(ctx, http.MethodGet, "/users/me", nil, &u, true); err != nil {
		return nil, err
	}
	return &u, nil
}

// Balance returns the credit balance.
func (c *Client) Balance(ctx context.Context) (*Credits, error) {
	var cr Credits
	if err := c.do(ctx, http.MethodGet, "/credits/balance", nil, &cr, true); err != nil {
		return nil, err
	}
	return &cr, nil
}

// VerifyEmail follows a Firebase verifyEmail action link (which finishes the
// verification in its own page JS), then confirms through the REST fallback.
func (c *Client) VerifyEmail(ctx context.Context, actionLink, apiOrigin string) error {
	if actionLink != "" && apiOrigin != "" && strings.Contains(actionLink, apiOrigin) {
		// action page served by the Jango host itself: follow it on this client so
		// the account's own cookies observe every hop
		if err := c.followAction(ctx, actionLink); err != nil {
			return err
		}
	} else if actionLink != "" {
		// third-party action page (Firebase): fetch with a throwaway jar so it can
		// never mix with another account's cookies
		isolated := New(c.API)
		if err := isolated.followAction(ctx, actionLink); err != nil {
			return err
		}
	}
	base := c.API
	if apiOrigin != "" {
		base = strings.TrimRight(apiOrigin, "/")
	}
	err := c.doAt(ctx, base, http.MethodPost, "/auth/web/email-verification", map[string]any{}, nil, true)
	if err != nil && (IsStatus(err, 404) || IsStatus(err, 400) || IsStatus(err, 405)) {
		return nil
	}
	return err
}

// NewChat creates an empty conversation and returns its id.
func (c *Client) NewChat(ctx context.Context) (string, error) {
	var ch Chat
	if err := c.do(ctx, http.MethodPost, "/chats", map[string]any{}, &ch, true); err != nil {
		return "", err
	}
	if ch.ID == "" {
		return "", errors.New("jango: chat create returned no id")
	}
	return ch.ID, nil
}

// Chat sends one user turn and streams the assistant reply. The callback gets every
// frame; returning false stops early (the upstream request is cancelled).
func (c *Client) ChatStream(ctx context.Context, chatID, content string, fn func(StreamEvent) bool) error {
	tok, err := c.Token(ctx)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(map[string]string{"content": content})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.API+"/chats/"+url.PathEscape(chatID)+"/messages/stream", bytes.NewReader(b))
	if err != nil {
		return err
	}
	c.baseHeaders(req, true)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.Http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return &APIError{Method: "POST", Path: "/chats/" + chatID + "/messages/stream",
			Status: resp.StatusCode, Body: string(raw)}
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev StreamEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		if !fn(ev) {
			return nil
		}
	}
	return sc.Err()
}

// Ask is the convenience path: one chat, one prompt, whole answer.
func (c *Client) Ask(ctx context.Context, prompt string) (text, chatID, model string, err error) {
	chatID, err = c.NewChat(ctx)
	if err != nil {
		return "", "", "", err
	}
	var sb strings.Builder
	err = c.ChatStream(ctx, chatID, prompt, func(ev StreamEvent) bool {
		switch ev.Type {
		case "chunk":
			sb.WriteString(ev.Content)
			if m := ev.Model(); m != "" {
				model = m
			}
		case "done":
			if m := ev.Model(); m != "" {
				model = m
			}
			return false
		}
		return true
	})
	return sb.String(), chatID, model, err
}

// followAction fetches an action page, following redirects by hand so every
// Set-Cookie on the way is applied to this client's jar.
func (c *Client) followAction(ctx context.Context, link string) error {
	cl := &http.Client{Jar: c.Http.Jar, Timeout: 60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	next := link
	for i := 0; i < 6 && next != ""; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return err
		}
		c.baseHeaders(req, false)
		req.Header.Set("Accept", "text/html")
		rsp, err := cl.Do(req)
		if err != nil {
			return err
		}
		loc := rsp.Header.Get("Location")
		body, _ := io.ReadAll(io.LimitReader(rsp.Body, 1<<20))
		rsp.Body.Close()
		// some deployments verify on GET without a redirect
		if strings.Contains(string(body), "Email verified") || strings.Contains(string(body), "email has been verified") {
			break
		}
		if loc == "" {
			break
		}
		next = absURL(next, loc)
	}
	return nil
}

// absURL resolves a Location header against the request URL.
func absURL(base, loc string) string {
	u, err := url.Parse(loc)
	if err != nil {
		return loc
	}
	if u.IsAbs() {
		return loc
	}
	b, err := url.Parse(base)
	if err != nil {
		return loc
	}
	return b.ResolveReference(u).String()
}

// accountJar is a minimal cookie jar for a single account. It stores every cookie
// (session or persistent) and sends them according to path, which matters because
// the signup/verification cookies can be session scoped.
type accountJar struct {
	mu sync.Mutex
	cs []storedCookie
}

type storedCookie struct {
	c      *http.Cookie
	domain string
}

func newAccountJar() *accountJar { return &accountJar{} }

func (j *accountJar) SetCookies(u *url.URL, cs []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, c := range cs {
		if c == nil || c.Name == "" {
			continue
		}
		dom := c.Domain
		if dom == "" {
			dom = u.Hostname()
		}
		// replace the same name+domain+path
		out := j.cs[:0]
		for _, ex := range j.cs {
			if ex.c.Name == c.Name && ex.domain == dom && ex.c.Path == c.Path {
				continue
			}
			out = append(out, ex)
		}
		j.cs = append(out, storedCookie{c: c, domain: dom})
	}
}

func (j *accountJar) Cookies(u *url.URL) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []*http.Cookie
	host := u.Hostname()
	for _, sc := range j.cs {
		if host != sc.domain && !strings.HasSuffix(host, "."+sc.domain) {
			continue
		}
		path := sc.c.Path
		if path == "" {
			path = "/"
		}
		if !strings.HasPrefix(u.Path, path) {
			continue
		}
		if !sc.c.Expires.IsZero() && sc.c.Expires.Before(time.Now()) {
			continue
		}
		c := *sc.c
		c.Domain = ""
		out = append(out, &c)
	}
	return out
}

// bodyBytes re-encodes a request body for a retry.
func bodyBytes(body any) []byte {
	if body == nil {
		return nil
	}
	b, _ := json.Marshal(body)
	return b
}
