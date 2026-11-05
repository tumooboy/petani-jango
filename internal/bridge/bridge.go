// Package bridge exposes every pooled Jango account as one OpenAI-compatible
// endpoint for 9Router (or any OpenAI client):
//
//	GET  /v1/models                  -> jango/<model> plus one entry per account id
//	POST /v1/chat/completions        -> JSON or SSE, load balanced across accounts
//	GET  /healthz                    -> per-account credit and health
//
// Routing: an explicit model id ("jango/acc-3/deepseek-v4-1-flash-uncensored")
// pins an account; the plain "jango/<model>" picks the healthiest account with
// the most credits. Each account keeps its own chat per conversation key, so
// multi-turn context is preserved.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tumooboy/petani-jango/internal/jango"
)

// Account is one pooled Jango account.
type Account struct {
	ID      string `json:"id"`
	Email   string `json:"email"`
	Client  *jango.Client
	Credits int
	Errors  int
	Used    int64
	LastErr string
	At      time.Time
}

// Pool is the set of accounts the bridge can route to.
type Pool struct {
	mu   sync.RWMutex
	accs []*Account
	rr   uint64
}

func NewPool() *Pool { return &Pool{} }

// Add registers an account. Duplicate ids are ignored.
func (p *Pool) Add(a *Account) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, x := range p.accs {
		if x.ID == a.ID {
			return false
		}
	}
	p.accs = append(p.accs, a)
	return true
}

// Snapshot returns a copy of the account list.
func (p *Pool) Snapshot() []*Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Account, len(p.accs))
	copy(out, p.accs)
	return out
}

// Pick chooses an account: explicit id wins, otherwise round-robin over healthy
// accounts with enough credits.
func (p *Pool) Pick(explicit string) (*Account, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.accs) == 0 {
		return nil, errors.New("bridge: no accounts configured")
	}
	if explicit != "" {
		for _, a := range p.accs {
			if a.ID == explicit || a.Email == explicit {
				return a, nil
			}
		}
		return nil, fmt.Errorf("bridge: unknown account %q", explicit)
	}
	// Only accounts that can actually pay for a reply are candidates: a reply
	// costs 5 credits, and a pooled account with 0 credits answers 402 upstream.
	var healthy []*Account
	for _, a := range p.accs {
		if a.Errors < 5 && a.Credits >= 5 {
			healthy = append(healthy, a)
		}
	}
	if len(healthy) == 0 {
		return nil, errors.New("bridge: no pooled account has at least 5 credits")
	}
	sort.SliceStable(healthy, func(i, j int) bool { return healthy[i].Credits > healthy[j].Credits })
	n := atomic.AddUint64(&p.rr, 1)
	return healthy[int(n)%len(healthy)], nil
}

func (p *Pool) touch(a *Account, err error, creditsDelta int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a.At = time.Now()
	a.Used++
	if err != nil {
		a.Errors++
		a.LastErr = err.Error()
	} else {
		a.Errors = 0
		a.LastErr = ""
	}
	if creditsDelta != 0 {
		a.Credits -= creditsDelta
	}
}

// Config is the bridge's runtime configuration.
type Config struct {
	Addr      string
	Prefix    string
	Model     string
	VerifyKey string // optional: require Authorization: Bearer <key>
	APIBase   string // optional: override the Jango API origin (tests/self-hosted)
}

// Server is the HTTP bridge.
type Server struct {
	Cfg  Config
	Pool *Pool
	// Per-conversation upstream chat ids, guarded because accounts are shared.
	chats sync.Map
}

func New(cfg Config) *Server {
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:20581"
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "jango"
	}
	if cfg.Model == "" {
		cfg.Model = jango.UpstreamModel
	}
	return &Server{Cfg: cfg, Pool: NewPool()}
}

// LoadSessions restores pooled accounts from a session file produced by
// `jango-bulk export --format bridge`.
func (s *Server) LoadSessions(path string) (int, error) {
	if path == "" {
		return 0, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var doc struct {
		Accounts []struct {
			ID            string `json:"id"`
			Email         string `json:"email"`
			RefreshCookie string `json:"refreshCookie"`
			AccessToken   string `json:"accessToken"`
			Credits       int    `json:"credits"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0, err
	}
	n := 0
	for _, a := range doc.Accounts {
		c := jango.New(s.Cfg.APIBase)
		if err := c.SetSession(a.RefreshCookie, a.AccessToken); err != nil {
			log.Printf("session %s: %v", a.Email, err)
			continue
		}
		id := a.ID
		if id == "" {
			id = a.Email
		}
		if s.Pool.Add(&Account{ID: id, Email: a.Email, Client: c, Credits: a.Credits}) {
			n++
		}
	}
	return n, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	return mux
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{Addr: s.Cfg.Addr, Handler: s.Handler(), ReadHeaderTimeout: 15 * time.Second}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sh)
	}()
	log.Printf("bridge listening on http://%s/v1 (prefix %s, %d accounts)",
		s.Cfg.Addr, s.Cfg.Prefix, len(s.Pool.Snapshot()))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) authorised(r *http.Request) bool {
	if s.Cfg.VerifyKey == "" {
		return true
	}
	h := r.Header.Get("Authorization")
	return strings.TrimPrefix(h, "Bearer ") == s.Cfg.VerifyKey
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(b)))
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !s.authorised(r) {
		writeJSON(w, 401, map[string]any{"error": map[string]string{"message": "unauthorized"}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	var rows []map[string]any
	for _, a := range s.Pool.Snapshot() {
		row := map[string]any{"id": a.ID, "email": a.Email, "credits": a.Credits,
			"requests": a.Used, "errors": a.Errors, "lastError": a.LastErr}
		if cr, err := a.Client.Balance(ctx); err == nil {
			row["credits"] = cr.Balance
			a.Credits = cr.Balance
		}
		rows = append(rows, row)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "accounts": rows, "model": s.Cfg.Prefix + "/" + s.Cfg.Model})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if !s.authorised(r) {
		writeJSON(w, 401, map[string]any{"error": map[string]string{"message": "unauthorized"}})
		return
	}
	data := []map[string]any{{
		"id": s.Cfg.Prefix + "/" + s.Cfg.Model, "object": "model", "owned_by": "jango",
	}}
	for _, a := range s.Pool.Snapshot() {
		data = append(data, map[string]any{
			"id": s.Cfg.Prefix + "/" + a.ID + "/" + s.Cfg.Model, "object": "model",
			"owned_by": "jango", "account": a.Email,
		})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

type oaiReq struct {
	Model     string `json:"model"`
	Stream    bool   `json:"stream"`
	User      string `json:"user"`
	MaxTokens int    `json:"max_tokens"`
	Messages  []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

func contentToString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

// prompt flattens the OpenAI message list into one Jango turn: system text,
// earlier turns as a transcript, and the final user message on its own.
func (q oaiReq) prompt() (system, transcript, user string) {
	var sys, lines []string
	lastUser := -1
	for i, m := range q.Messages {
		switch m.Role {
		case "system":
			sys = append(sys, contentToString(m.Content))
		case "user":
			lastUser = i
		}
	}
	if lastUser < 0 {
		return strings.Join(sys, "\n"), "", ""
	}
	user = contentToString(q.Messages[lastUser].Content)
	for i, m := range q.Messages {
		if i == lastUser || m.Role == "system" {
			continue
		}
		if i > lastUser {
			continue
		}
		who := "Assistant"
		if m.Role == "user" {
			who = "User"
		}
		lines = append(lines, who+": "+contentToString(m.Content))
	}
	return strings.Join(sys, "\n"), strings.Join(lines, "\n"), user
}

func (q oaiReq) accountID(prefix string) string {
	m := strings.TrimPrefix(q.Model, prefix+"/")
	if i := strings.LastIndex(m, "/"); i >= 0 {
		return m[:i]
	}
	return ""
}

func compose(system, transcript, user string) string {
	var parts []string
	if system != "" {
		parts = append(parts, system)
	}
	if transcript != "" {
		parts = append(parts, "(Previous conversation\n"+transcript+")")
	}
	return strings.Join(append(parts, user), "\n\n")
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if !s.authorised(r) {
		writeJSON(w, 401, map[string]any{"error": map[string]string{"message": "unauthorized"}})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": map[string]string{"message": "read body"}})
		return
	}
	var q oaiReq
	if err := json.Unmarshal(raw, &q); err != nil {
		writeJSON(w, 400, map[string]any{"error": map[string]string{"message": "bad json"}})
		return
	}
	system, transcript, user := q.prompt()
	if user == "" {
		writeJSON(w, 400, map[string]any{"error": map[string]string{"message": "no user message"}})
		return
	}
	acc, err := s.Pool.Pick(q.accountID(s.Cfg.Prefix))
	if err != nil {
		writeJSON(w, 503, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	if q.Stream || strings.Contains(r.URL.RawQuery, "stream=true") {
		s.stream(w, r, acc, q, compose(system, transcript, user))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
	defer cancel()

	key := q.User
	if key == "" {
		key = acc.ID + "|" + transcript[:min(len(transcript), 200)]
	}
	chatID, err := s.chatFor(ctx, acc, key)
	if err != nil {
		s.Pool.touch(acc, err, 0)
		writeJSON(w, 502, map[string]any{"error": map[string]string{"message": "open chat: " + err.Error()}})
		return
	}
	var sb strings.Builder
	var cost int
	model := jango.UpstreamModel
	err = acc.Client.ChatStream(ctx, chatID, compose(system, transcript, user), func(ev jango.StreamEvent) bool {
		switch ev.Type {
		case "chunk":
			sb.WriteString(ev.Content)
		case "done":
			model = orDefault(ev.Model(), model)
			cost = ev.CreditCost()
			return false
		}
		return true
	})
	s.Pool.touch(acc, err, cost)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	writeJSON(w, 200, map[string]any{
		"id": "chatcmpl-" + fmt.Sprint(time.Now().UnixNano()), "object": "chat.completion",
		"created": time.Now().Unix(), "model": orDefault(q.Model, s.Cfg.Prefix+"/"+model),
		"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
			"message": map[string]string{"role": "assistant", "content": sb.String()}}},
		"jango": map[string]any{"account": acc.Email, "chat_id": chatID,
			"upstream_model": model, "credit_cost": cost},
	})
}

func (s *Server) chatFor(ctx context.Context, acc *Account, key string) (string, error) {
	k := acc.ID + "::" + key
	if v, ok := s.chats.Load(k); ok {
		return v.(string), nil
	}
	id, err := acc.Client.NewChat(ctx)
	if err != nil {
		return "", err
	}
	s.chats.Store(k, id)
	return id, nil
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request, acc *Account, q oaiReq, prompt string) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, 500, map[string]any{"error": map[string]string{"message": "no flusher"}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()

	key := q.User
	if key == "" {
		key = acc.ID + "|" + prompt[:min(len(prompt), 200)]
	}
	chatID, chatErr := s.chatFor(ctx, acc, key)
	id := "chatcmpl-" + fmt.Sprint(time.Now().UnixNano())
	created := time.Now().Unix()
	model := orDefault(q.Model, s.Cfg.Prefix+"/"+jango.UpstreamModel)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "close")
	w.WriteHeader(200)
	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
	}
	send(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]string{"role": "assistant"}, "finish_reason": nil}}})
	var cost int
	err := chatErr
	if chatID == "" && err == nil {
		err = errors.New("bridge: could not create upstream chat")
	}
	if err == nil {
		err = acc.Client.ChatStream(ctx, chatID, prompt, func(ev jango.StreamEvent) bool {
			switch ev.Type {
			case "chunk":
				send(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created,
					"model": model, "choices": []map[string]any{{"index": 0,
						"delta": map[string]string{"content": ev.Content}, "finish_reason": nil}}})
			case "done":
				cost = ev.CreditCost()
				return false
			}
			return true
		})
	}
	s.Pool.touch(acc, err, cost)
	if err != nil {
		send(map[string]any{"error": map[string]string{"message": err.Error()}})
	}
	send(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		"jango":   map[string]any{"account": acc.Email, "chat_id": chatID, "credit_cost": cost}})
	fmt.Fprint(w, "data: [DONE]\n\n")
	fl.Flush()
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
