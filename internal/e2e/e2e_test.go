// Package e2e proves the whole pipeline without network access: fake mail.tm,
// fake Jango AI, the real creator + store + bridge, and a fake 9Router that
// delegates /v1 to the bridge -- the same hop 9Router performs in production.
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tumooboy/petani-jango/internal/bridge"
	"github.com/tumooboy/petani-jango/internal/creator"
	"github.com/tumooboy/petani-jango/internal/jango"
	"github.com/tumooboy/petani-jango/internal/mock"
	"github.com/tumooboy/petani-jango/internal/router"
	"github.com/tumooboy/petani-jango/internal/store"
	"github.com/tumooboy/petani-jango/internal/wire"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestSignupCaptureAndRouter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	mail := mock.NewMailTM()
	defer mail.Srv.Close()
	jg := mock.NewJango(mail, mail.Srv.URL)
	defer jg.Close()

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}

	cr := creator.New(creator.Options{Concurrency: 3, JangoAPI: jg.Srv.URL,
		MailAPI: mail.Srv.URL, VerifyTimeout: 20 * time.Second, LegacySignup: true})
	var ok int
	cr.Run(ctx, st, 3, func(a store.Account) {
		switch {
		case a.Error != "":
			t.Errorf("%s failed: %s", a.Email, a.Error)
		default:
			ok++
			if a.AccessToken == "" || a.RefreshCookie == "" {
				t.Errorf("%s missing credentials", a.Email)
			}
			if !a.Verified {
				t.Errorf("%s not verified", a.Email)
			}
			if a.Credits != 60 {
				t.Errorf("%s credits=%d want 60", a.Email, a.Credits)
			}
		}
	})
	if ok != 3 {
		t.Fatalf("only %d/3 accounts created", ok)
	}

	// refresh a token from the cookie and chat with the fake upstream
	acc := st.Accounts()[0]
	c := jango.New(jg.Srv.URL)
	if err := c.SetSession(acc.RefreshCookie, ""); err != nil {
		t.Fatal(err)
	}
	text, chatID, model, err := c.Ask(ctx, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "hello") || chatID == "" || model != jango.UpstreamModel {
		t.Fatalf("bad answer %q chat=%q model=%q", text, chatID, model)
	}

	// bridge + fake 9Router
	addr := freeAddr(t)
	sessFile := filepath.Join(dir, "bridge-sessions.json")
	srv, n, err := wire.Bridge(st, sessFile, bridge.Config{Addr: addr, APIBase: jg.Srv.URL})
	if err != nil || n != 3 {
		t.Fatalf("bridge load: n=%d err=%v", n, err)
	}
	go func() { _ = srv.ListenAndServe(ctx) }()
	time.Sleep(400 * time.Millisecond)
	mock.BridgeURL = "http://" + addr

	rt := mock.NewRouter("cli-tok")
	defer rt.Srv.Close()
	base, key, model, err := wire.Run(ctx, st, wire.Options{
		RouterBase: rt.Srv.URL, CLIToken: rt.Token, BridgeAddr: addr,
		SessionFile: sessFile, KeyName: "jango-bulk"})
	if err != nil {
		t.Fatalf("wire: %v", err)
	}
	if key == "" || model != "jango/"+jango.UpstreamModel || !strings.HasSuffix(base, "/v1") {
		t.Fatalf("wire returned base=%q key=%q model=%q", base, key, model)
	}

	// one non-streaming answer through the router
	rc := router.New(rt.Srv.URL, "")
	answer, err := rc.Verify(ctx, rt.APIKey(), model)
	if err != nil || !strings.Contains(answer, "Mock reply") {
		t.Fatalf("verify through router: %q %v", answer, err)
	}

	// streaming through the router
	body := strings.NewReader(`{"model":"jango/deepseek-v4-1-flash-uncensored","stream":true,"messages":[{"role":"user","content":"stream ple"}]}`)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, rt.Srv.URL+"/v1/chat/completions", body)
	req.Header.Set("Authorization", "Bearer "+rt.APIKey())
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var pieces []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) == nil && len(ev.Choices) > 0 {
			pieces = append(pieces, ev.Choices[0].Delta.Content)
		}
	}
	if joined := strings.Join(pieces, ""); !strings.Contains(joined, "stream ple") {
		t.Fatalf("stream lost content: %q", joined)
	}

	// audit trail + file permissions
	recs, err := st.ReadLog(0)
	if err != nil || len(recs) != 3 {
		t.Fatalf("audit log: %d records, %v", len(recs), err)
	}
	info, _ := os.Stat(filepath.Join(dir, "accounts.json"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("store perms %v", info.Mode().Perm())
	}
	fmt.Println("e2e ok: 3 accounts, bridge, router, stream, audit log")
}

func TestCreatorRecordsFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, _ := store.Open(filepath.Join(dir, "accounts.json"))
	cr := creator.New(creator.Options{Concurrency: 1, JangoAPI: "http://127.0.0.1:1",
		MailAPI: "http://127.0.0.1:1", VerifyTimeout: 2 * time.Second, LegacySignup: true})
	var errs int
	cr.Run(ctx, st, 1, func(a store.Account) {
		if a.Error != "" {
			errs++
		}
	})
	if errs != 1 {
		t.Fatalf("expected a recorded failure, got %d", errs)
	}
	if len(st.Accounts()) != 1 {
		t.Fatalf("a failed account should still be stored for retry")
	}
}
