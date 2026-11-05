// Package wire joins the account pool to 9Router: it exports the bridge session
// file, starts the bridge, registers the provider node/model in 9Router and then
// proves the model answers through 9Router's own /v1 API.
package wire

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tumooboy/petani-jango/internal/bridge"
	"github.com/tumooboy/petani-jango/internal/jango"
	"github.com/tumooboy/petani-jango/internal/router"
	"github.com/tumooboy/petani-jango/internal/store"
)

// Options configures one wiring run.
type Options struct {
	RouterBase  string // http://127.0.0.1:20128
	CLIToken    string // x-9r-cli-token (derived when empty)
	Prefix      string // model prefix, default jango
	BridgeAddr  string // 127.0.0.1:20581
	SessionFile string // bridge session file
	KeyName     string // router key name, default jango-bulk
}

func (o *Options) defaults() {
	if o.RouterBase == "" {
		o.RouterBase = router.DefaultBase
	}
	if o.Prefix == "" {
		o.Prefix = "jango"
	}
	if o.BridgeAddr == "" {
		o.BridgeAddr = "127.0.0.1:20581"
	}
	if o.SessionFile == "" {
		o.SessionFile = filepath.Join("data", "bridge-sessions.json")
	}
	if o.KeyName == "" {
		o.KeyName = "jango-bulk"
	}
}

// Export writes the bridge session file and returns how many accounts went in.
func Export(st *store.Store, path string) (int, error) {
	accs := st.Accounts()
	type row struct {
		ID            string `json:"id"`
		Email         string `json:"email"`
		RefreshCookie string `json:"refreshCookie"`
		AccessToken   string `json:"accessToken"`
		Credits       int    `json:"credits"`
	}
	var rows []row
	for _, a := range accs {
		if a.RefreshCookie == "" {
			continue
		}
		rows = append(rows, row{a.ID, a.Email, a.RefreshCookie, a.AccessToken, a.Credits})
	}
	b, err := json.MarshalIndent(map[string]any{
		"version": 1, "generatedAt": time.Now().UTC(), "accounts": rows}, "", "  ")
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, err
	}
	return len(rows), os.WriteFile(path, b, 0o600)
}

// Run does the whole wiring and returns the client base URL, key and model.
func Run(ctx context.Context, st *store.Store, o Options) (baseURL, apiKey, model string, err error) {
	o.defaults()
	if n, err := Export(st, o.SessionFile); err != nil {
		return "", "", "", fmt.Errorf("export sessions: %w", err)
	} else if n == 0 {
		return "", "", "", fmt.Errorf("no accounts with a refresh cookie in the store")
	}

	tok := o.CLIToken
	if tok == "" {
		tok = router.CLITokenFromHome("", "")
	}
	if tok == "" {
		return "", "", "", fmt.Errorf("cannot derive the 9Router CLI token; pass --cli-token or set up ~/.9router")
	}
	rc := router.New(o.RouterBase, tok)

	nodeID, err := rc.EnsureNode(ctx, o.Prefix, "Jango AI", "http://"+o.BridgeAddr)
	if err != nil {
		return "", "", "", err
	}
	if err := rc.EnsureConnection(ctx, nodeID, tok, "jango-bulk"); err != nil {
		return "", "", "", err
	}
	if err := rc.EnsureModel(ctx, o.Prefix, jango.UpstreamModel, "Jango AI (DeepSeek V4.1 Flash Uncensored)"); err != nil {
		return "", "", "", err
	}
	apiKey, err = rc.EnsureKey(ctx, o.KeyName)
	if err != nil {
		return "", "", "", err
	}
	model = o.Prefix + "/" + jango.UpstreamModel
	baseURL = strings.TrimRight(o.RouterBase, "/") + "/v1"

	// live verification through 9Router
	vctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	ids, err := rc.Models(vctx, apiKey)
	if err != nil {
		return baseURL, apiKey, model, fmt.Errorf("models list: %w", err)
	}
	found := false
	for _, id := range ids {
		if id == model {
			found = true
		}
	}
	if !found {
		return baseURL, apiKey, model, fmt.Errorf("model %s missing from /v1/models", model)
	}
	if _, err := rc.Verify(vctx, apiKey, model); err != nil {
		return baseURL, apiKey, model, fmt.Errorf("verify: %w", err)
	}
	return baseURL, apiKey, model, nil
}

// Bridge loads the exported session file into a bridge server. Every pooled
// account gets a cookie hook: the API revokes the refresh cookie on each use, so
// the rotated value has to be written back or the account dies at the next
// restart.
func Bridge(st *store.Store, path string, cfg bridge.Config) (*bridge.Server, int, error) {
	if _, err := Export(st, path); err != nil {
		return nil, 0, err
	}
	srv := bridge.New(cfg)
	n, err := srv.LoadSessions(path)
	if err != nil {
		return srv, n, err
	}
	persist := persistSessions(st, path)
	for _, a := range srv.Pool.Snapshot() {
		email := a.Email
		a.Client.SetOnCookie(func(cookie string) { persist(email, cookie) })
	}
	return srv, n, nil
}

// persistSessions writes a rotated refresh cookie back to the store and to the
// session file, serialised because the bridge refreshes from many goroutines.
func persistSessions(st *store.Store, path string) func(email, cookie string) {
	var mu sync.Mutex
	return func(email, cookie string) {
		mu.Lock()
		defer mu.Unlock()
		for _, a := range st.Accounts() {
			if a.Email == email {
				a.RefreshCookie = cookie
				a.UpdatedAt = time.Now().UTC()
				_ = st.Upsert(a)
				break
			}
		}
		_, _ = Export(st, path)
	}
}
