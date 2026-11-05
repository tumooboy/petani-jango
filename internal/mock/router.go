package mock

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Router fakes the parts of 9Router the wiring uses: nodes, providers, models,
// alias, keys and the /v1 surface (which forwards to the bridge under test).
type Router struct {
	Srv    *httptest.Server
	Token  string
	mu     sync.Mutex
	keys   map[string]string
	nodes  map[string]map[string]any
	models map[string]bool
	apiKey string
}

func NewRouter(cliToken string) *Router {
	r := &Router{Token: cliToken, keys: map[string]string{}, nodes: map[string]map[string]any{},
		models: map[string]bool{}, apiKey: "sk-test"}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/keys", r.need(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			var out []map[string]any
			r.mu.Lock()
			for name, k := range r.keys {
				out = append(out, map[string]any{"name": name, "key": k})
			}
			r.mu.Unlock()
			if out == nil {
				out = []map[string]any{}
			}
			writeJSON(w, 200, map[string]any{"keys": out})
			return
		}
		var b struct{ Name string }
		_ = json.NewDecoder(req.Body).Decode(&b)
		k := "sk-" + b.Name
		r.mu.Lock()
		r.keys[b.Name] = k
		r.mu.Unlock()
		writeJSON(w, 201, map[string]any{"name": b.Name, "key": k})
	}))
	mux.HandleFunc("/api/provider-nodes", r.need(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if req.Method == http.MethodGet {
			var out []map[string]any
			for _, n := range r.nodes {
				out = append(out, n)
			}
			if out == nil {
				out = []map[string]any{}
			}
			writeJSON(w, 200, map[string]any{"nodes": out})
			return
		}
		var b map[string]any
		_ = json.NewDecoder(req.Body).Decode(&b)
		if req.Method == http.MethodPut {
			id := strings.TrimPrefix(req.URL.Path, "/api/provider-nodes/")
			if n, ok := r.nodes[id]; ok {
				n["data"] = map[string]any{"baseUrl": b["baseUrl"]}
			}
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
		id := fmt.Sprintf("node-%d", len(r.nodes)+1)
		node := map[string]any{"id": id, "name": b["name"], "prefix": b["prefix"],
			"type": b["type"], "data": map[string]any{"baseUrl": b["baseUrl"]}}
		r.nodes[id] = node
		writeJSON(w, 201, map[string]any{"node": node})
	}))
	mux.HandleFunc("/api/providers", r.need(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if req.Method == http.MethodGet {
			var out []map[string]any
			for id := range r.nodes {
				out = append(out, map[string]any{"id": "conn-" + id, "provider": id})
			}
			if out == nil {
				out = []map[string]any{}
			}
			writeJSON(w, 200, map[string]any{"connections": out})
			return
		}
		writeJSON(w, 201, map[string]any{"ok": true})
	}))
	mux.HandleFunc("/api/models/custom", r.need(func(w http.ResponseWriter, req *http.Request) {
		var b struct{ ProviderAlias, ID string }
		_ = json.NewDecoder(req.Body).Decode(&b)
		r.mu.Lock()
		r.models[b.ProviderAlias+"/"+b.ID] = true
		r.mu.Unlock()
		writeJSON(w, 201, map[string]any{"ok": true})
	}))
	mux.HandleFunc("/api/models/alias", r.need(func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, req *http.Request) {
		var data []map[string]any
		r.mu.Lock()
		for m := range r.models {
			data = append(data, map[string]any{"id": m})
		}
		r.mu.Unlock()
		writeJSON(w, 200, map[string]any{"object": "list", "data": data})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, req *http.Request) {
		k := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		r.mu.Lock()
		valid := k == r.apiKey
		for _, v := range r.keys {
			if k == v {
				valid = true
			}
		}
		r.mu.Unlock()
		if !valid {
			writeJSON(w, 401, map[string]any{"error": "bad key"})
			return
		}
		if BridgeURL == "" {
			writeJSON(w, 502, map[string]any{"error": "bridge down"})
			return
		}
		target := BridgeURL + "/v1/chat/completions"
		body := req.Body
		up, err := http.NewRequest(req.Method, target, body)
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		up.Header = req.Header.Clone()
		up.Body = body
		resp, err := http.DefaultClient.Do(up)
		if err != nil {
			writeJSON(w, 502, map[string]any{"error": err.Error()})
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		buf := make([]byte, 32*1024)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
				w.(http.Flusher).Flush()
			}
			if err != nil {
				break
			}
		}
	})
	r.Srv = httptest.NewServer(mux)
	return r
}

func (r *Router) need(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("x-9r-cli-token") != r.Token {
			writeJSON(w, 401, map[string]any{"error": "Unauthorized"})
			return
		}
		h(w, req)
	}
}

// BridgeURL is where 9Router forwards (set by the test).
var BridgeURL string

// Models returns the registered model ids.
func (r *Router) Models() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for m := range r.models {
		out = append(out, m)
	}
	return out
}

// APIKey is the router key the tests use.
func (r *Router) APIKey() string { return r.apiKey }
