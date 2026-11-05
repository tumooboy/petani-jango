// Package mock spins up in-process fakes of mail.tm, Jango AI and 9Router so the
// whole pipeline can be tested end to end with no network and no credits.
package mock

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"
)

// MailTM fakes the three endpoints the creator uses.
type MailTM struct {
	Srv   *httptest.Server
	mu    sync.Mutex
	boxes map[string][]map[string]any
	n     int
}

func NewMailTM() *MailTM {
	m := &MailTM{boxes: map[string][]map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/domains", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"hydra:member": []map[string]any{
			{"domain": "mockmail.test", "isActive": true, "isPrivate": false}}})
	})
	mux.HandleFunc("/accounts", func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Address, Password string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		writeJSON(w, 201, map[string]any{"id": "acc-" + b.Address, "address": b.Address})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Address, Password string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		writeJSON(w, 200, map[string]any{"token": "mailtok:" + b.Address})
	})
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		addr := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer mailtok:")
		m.mu.Lock()
		boxes := append([]map[string]any{}, m.boxes[addr]...)
		m.mu.Unlock()
		if boxes == nil {
			boxes = []map[string]any{}
		}
		writeJSON(w, 200, map[string]any{"hydra:member": boxes})
	})
	mux.HandleFunc("/messages/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/messages/")
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, msgs := range m.boxes {
			for _, x := range msgs {
				if x["id"] == id {
					writeJSON(w, 200, x)
					return
				}
			}
		}
		writeJSON(w, 404, map[string]any{"error": "not found"})
	})
	m.Srv = httptest.NewServer(mux)
	return m
}

// Deliver queues a verification mail for an address.
func (m *MailTM) Deliver(addr, link string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	m.boxes[addr] = append(m.boxes[addr], map[string]any{
		"id":      fmt.Sprintf("msg-%d", m.n),
		"from":    map[string]string{"address": "noreply@jangoai.chat", "name": "Jango AI"},
		"subject": "Verify your email for Jango AI",
		"intro":   "Follow this link to verify your email address.",
		"text":    "Hello,\n\nFollow this link to verify your email address.\n\n" + link + "\n",
		"html":    []string{"<a href='" + link + "'>verify</a>"},
	})
}

// Jango fakes signup, web sessions, verification, chats and the SSE stream.
type Jango struct {
	Srv        *httptest.Server
	mu         sync.Mutex
	sessions   map[string]string // refresh cookie -> email
	verified   map[string]bool
	chats      map[string]bool
	token      map[string]string
	Mail       *MailTM
	MailBase   string
	Blocked    bool
	action     *httptest.Server
	actionBase string
}

// Close shuts the fakes down.
func (j *Jango) Close() {
	j.Srv.Close()
	if j.action != nil {
		j.action.Close()
	}
}

// ActionURL is the verify link a signup mail points at (third-party origin).
// IsVerified reports whether email finished verification (debug/tests).
func (j *Jango) IsVerified(email string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.verified[email]
}

func (j *Jango) ActionURL(email string) string {
	return j.action.URL + "/verify?email=" + url.QueryEscape(email)
}

func NewJango(mail *MailTM, mailBase string) *Jango {
	j := &Jango{sessions: map[string]string{}, verified: map[string]bool{}, token: map[string]string{},
		chats: map[string]bool{}, Mail: mail, MailBase: mailBase}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/web/login", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Name     string `json:"name"`
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b.Email == "" || len(b.Password) < 8 {
			writeJSON(w, 400, map[string]any{"message": "invalid signup"})
			return
		}
		j.mu.Lock()
		sid := "rt-" + fmt.Sprint(len(j.sessions)+1) + "-" + strings.Split(b.Email, "@")[0]
		j.sessions[sid] = b.Email
		j.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "__Secure-jango_rt", Value: sid, Path: "/",
			Secure: strings.HasPrefix(j.Srv.URL, "https://"), HttpOnly: true, SameSite: http.SameSiteLaxMode,
			Expires: time.Now().Add(30 * 24 * time.Hour)})
		j.Mail.Deliver(b.Email, j.ActionURL(b.Email))
		writeJSON(w, 201, map[string]any{"user": user(b.Email, b.Name)})
	})
	mux.HandleFunc("/confirm", func(w http.ResponseWriter, r *http.Request) {
		email := r.URL.Query().Get("email")
		j.mu.Lock()
		j.verified[email] = true
		j.mu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("/auth/web/refresh", func(w http.ResponseWriter, r *http.Request) {
		ck := ""
		for _, x := range r.Cookies() {
			if x.Name == "__Secure-jango_rt" {
				ck = x.Value
			}
		}
		if ck == "" {
			ck = r.Header.Get("Cookie")
		}
		j.mu.Lock()
		email := j.sessions[ck]
		if email == "" {
			for _, v := range j.sessions {
				if strings.Contains(ck, v) {
					email = v
				}
			}
		}
		ok := ck != "" && email != ""
		ver := j.verified[email]
		j.mu.Unlock()
		if !ok {
			writeJSON(w, 401, map[string]any{"message": "No web session"})
			return
		}
		j.mu.Lock()
		j.token[email] = "at-" + email
		j.mu.Unlock()
		writeJSON(w, 200, map[string]any{
			"accessToken": "at-" + email, "accessTokenExpiresAt": "2030-01-01T00:00:00Z",
			"user": user(email, "Jango User"), "verified": ver})
	})
	auth := func(w http.ResponseWriter, r *http.Request) string {
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		email := ""
		j.mu.Lock()
		for e, tok := range j.token {
			if tok == raw {
				email = e
			}
		}
		j.mu.Unlock()
		if email == "" {
			writeJSON(w, 401, map[string]any{"message": "Unauthorized"})
			return ""
		}
		if j.Blocked {
			writeJSON(w, 403, map[string]any{"message": "blocked"})
			return ""
		}
		return email
	}
	mux.HandleFunc("/verify", func(w http.ResponseWriter, r *http.Request) {
		// mirrors the Firebase action page: verify, then bounce to the app with a token
		email := r.URL.Query().Get("email")
		if email == "" {
			writeJSON(w, 400, map[string]any{"error": "missing email"})
			return
		}
		j.mu.Lock()
		j.verified[email] = true
		j.token[email] = "at-" + email
		j.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "fb_at", Value: "at-" + email, Path: "/",
			Secure: strings.HasPrefix(j.Srv.URL, "https://"), Expires: time.Now().Add(time.Hour)})
		http.Redirect(w, r, "/app?verified=1", http.StatusFound)
	})
	mux.HandleFunc("/auth/web/email-verification", func(w http.ResponseWriter, r *http.Request) {
		email := auth(w, r)
		if email == "" {
			return
		}
		j.mu.Lock()
		j.verified[email] = true
		j.mu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true, "email": email})
	})
	mux.HandleFunc("/users/me", func(w http.ResponseWriter, r *http.Request) {
		email := auth(w, r)
		if email == "" {
			return
		}
		j.mu.Lock()
		ver := j.verified[email]
		j.mu.Unlock()
		if !ver {
			writeJSON(w, 403, map[string]any{"message": "verify your email"})
			return
		}
		writeJSON(w, 200, user(email, "Jango User"))
	})
	mux.HandleFunc("/credits/balance", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r) == "" {
			return
		}
		writeJSON(w, 200, map[string]any{"balance": 60, "totalCreditsEarned": 60,
			"totalCreditsUsed": 0, "minCreditsToChat": 5})
	})
	mux.HandleFunc("/chats", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r) == "" {
			return
		}
		j.mu.Lock()
		id := fmt.Sprintf("chat-%d", len(j.chats)+1)
		j.chats[id] = true
		j.mu.Unlock()
		writeJSON(w, 201, map[string]any{"id": id, "title": nil})
	})
	mux.HandleFunc("/chats/", func(w http.ResponseWriter, r *http.Request) {
		if auth(w, r) == "" {
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/messages/stream") {
			writeJSON(w, 404, map[string]any{"message": "not found"})
			return
		}
		var b struct{ Content string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, piece := range []string{"Mock ", "reply ", "to: ", b.Content} {
			fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"type": "chunk", "content": piece,
				"metadata": map[string]any{"model": "deepseek-v4-1-flash-uncensored"}}))
		}
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"type": "done", "content": "",
			"metadata": map[string]any{"model": "deepseek-v4-1-flash-uncensored", "creditCost": 5,
				"usage": map[string]int{"promptTokens": 10, "completionTokens": 4, "totalTokens": 14}}}))
	})
	j.Srv = httptest.NewServer(mux)
	j.mu.Lock()
	for k := range j.verified {
		delete(j.verified, k)
	}
	j.mu.Unlock()
	// action page on a separate origin, like Firebase's verify page
	actionMux := http.NewServeMux()
	actionMux.HandleFunc("/verify", func(w http.ResponseWriter, r *http.Request) {
		email := r.URL.Query().Get("email")
		if email == "" {
			writeJSON(w, 400, map[string]any{"error": "missing email"})
			return
		}
		j.mu.Lock()
		j.verified[email] = true
		j.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "fb_at", Value: "at-" + email, Path: "/",
			Secure: strings.HasPrefix(j.action.URL, "https://"), Expires: time.Now().Add(time.Hour)})
		http.Redirect(w, r, "/done", http.StatusFound)
	})
	actionMux.HandleFunc("/done", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<h1>Your email has been verified</h1>")
	})
	j.action = httptest.NewServer(actionMux)
	return j
}

func (j *Jango) SrvName() string { return j.Srv.URL }

func user(email, name string) map[string]any {
	return map[string]any{"id": "user-" + strings.Split(email, "@")[0], "email": email,
		"name": name, "role": "USER", "provider": "EMAIL", "credits": "60",
		"totalCreditsEarned": 60, "firebaseUid": "fb-" + strings.Split(email, "@")[0]}
}

func c2v(c *http.Cookie) string {
	if c == nil {
		return ""
	}
	return c.Value
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
