// Command jango-bulk creates Jango AI accounts in bulk, captures their tokens and
// session cookies, and wires the pool into 9Router as one OpenAI-compatible model.
//
//	jango-bulk signup -n 5                  create 5 verified accounts
//	jango-bulk list                         show the pool with credit balances
//	jango-bulk export --format bridge       write the bridge session file
//	jango-bulk serve                        run the OpenAI-compatible bridge
//	jango-bulk connect --router http://127.0.0.1:20128
//	jango-bulk run -n 5                     signup -> export -> serve check -> connect
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tumooboy/petani-jango/internal/bridge"
	"github.com/tumooboy/petani-jango/internal/creator"
	"github.com/tumooboy/petani-jango/internal/jango"
	"github.com/tumooboy/petani-jango/internal/router"
	"github.com/tumooboy/petani-jango/internal/store"
	"github.com/tumooboy/petani-jango/internal/wire"
)

const version = "1.0.0"

type settings struct {
	storePath string
	jangoAPI  string
	json      bool
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	cmd := args[0]
	flags, pos := parse(args[1:])
	s := settings{storePath: flag(flags, "store", "data/accounts.json"), jangoAPI: flag(flags, "jango-api", ""),
		json: has(flags, "json")}

	var err error
	switch cmd {
	case "signup":
		err = cmdSignup(ctx, s, flags)
	case "list", "status":
		err = cmdList(s)
	case "check":
		err = cmdCheck(ctx, s)
	case "export":
		err = cmdExport(s, flags)
	case "serve":
		err = cmdServe(ctx, s, flags)
	case "connect":
		err = cmdConnect(ctx, s, flags)
	case "run":
		err = cmdRun(ctx, s, flags)
	case "verify-models":
		err = cmdVerifyModels(s, flags)
	case "version", "-v", "--version":
		fmt.Printf("jango-bulk %s (upstream model %s)\n", version, jango.UpstreamModel)
	case "help", "-h", "--help":
		usage()
	default:
		if fmt.Sprint(pos) != "[]" {
			fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		}
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`jango-bulk ` + version + ` - bulk Jango AI accounts -> 9Router

Usage:
  jango-bulk <command> [flags]

Commands:
  signup        create accounts (inbox, signup, email verification, session capture)
  list          table of accounts with credits and cookie health
  check         refresh every token and read its credit balance
  export        write sessions as JSON / CSV / bridge format
  serve         run the OpenAI-compatible bridge for the pool
  connect       register the pool in 9Router and verify a reply end to end
  run           signup -> export -> background serve -> connect (one shot)
  verify-models list the models available through 9Router
  version       print the version

Global flags:
  --store PATH  account store (default data/accounts.json)
  --json        machine readable output

Signup flags:
  -n, --count N           how many accounts (default 1)
  -c, --concurrency N     parallel workers (default 4)
  --name PREFIX           display name prefix (default "Jango User")
  --email-prefix P        mailbox prefix (default "jango")
  --mail-api URL          mail.tm base URL
  --jango-api URL         Jango API base URL
  --verify-timeout D      email wait, e.g. 180s
  --no-verify             skip the verification mail step

Serve flags:
  --addr HOST:PORT        bridge listen address (default 127.0.0.1:20581)
  --prefix P              model prefix (default jango)
  --api-key KEY           require this bearer key on the bridge

Connect flags:
  --router URL            9Router base (default http://127.0.0.1:20128)
  --cli-token TOKEN       x-9r-cli-token (default: derived from ~/.9router)
  --bridge ADDR           bridge address registered in 9Router
  --prefix P              model prefix (default jango)
  --key-name NAME         router API key name (default jango-bulk)
`)
}

func parse(args []string) (map[string][]string, []string) {
	m := map[string][]string{}
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		if k, v, ok := strings.Cut(name, "="); ok {
			m[k] = append(m[k], v)
			continue
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && !isBool(name) {
			m[name] = append(m[name], args[i+1])
			i++
			continue
		}
		m[name] = append(m[name], "true")
	}
	return m, pos
}

func isBool(name string) bool {
	switch name {
	case "json", "no-verify", "help", "version", "h", "v":
		return true
	}
	return false
}

func has(f map[string][]string, k string) bool {
	v, ok := f[k]
	return ok && (len(v) == 0 || v[len(v)-1] != "false")
}

func flag(f map[string][]string, k, def string) string {
	if v, ok := f[k]; ok && len(v) > 0 && v[len(v)-1] != "" {
		return v[len(v)-1]
	}
	return def
}

func intFlag(f map[string][]string, k string, def int) int {
	if v := flag(f, k, ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func openStore(s settings) (*store.Store, error) { return store.Open(s.storePath) }

func cmdSignup(ctx context.Context, s settings, f map[string][]string) error {
	st, err := openStore(s)
	if err != nil {
		return err
	}
	n := intFlag(f, "n", intFlag(f, "count", 1))
	d, _ := time.ParseDuration(flag(f, "verify-timeout", "180s"))
	c := creator.New(creator.Options{
		Concurrency:      intFlag(f, "c", intFlag(f, "concurrency", 4)),
		NamePrefix:       flag(f, "name", "Jango User"),
		EmailPrefix:      flag(f, "email-prefix", "jango"),
		MailAPI:          flag(f, "mail-api", ""),
		JangoAPI:         flag(f, "jango-api", ""),
		AppURL:           flag(f, "app-url", creator.DefaultAppURL),
		FirebaseKey:      flag(f, "firebase-key", ""),
		CaptchaKey:       flag(f, "captcha-key", ""),
		TurnstileSiteKey: flag(f, "turnstile-sitekey", creator.DefaultSiteKey),
		AccountPassword:  flag(f, "password", ""),
		VerifyTimeout:    d,
		SkipVerification: has(f, "no-verify"),
		OnProgress: func(e creator.Event) {
			status := "ok"
			if e.Err != "" {
				status = "fail"
			}
			fmt.Fprintf(os.Stderr, "[%s] %-28s %s %s\n", status, e.Email, e.Stage, e.Err)
		},
	})
	fmt.Printf("creating %d account(s) with %d worker(s)\n", n, intFlag(f, "c", intFlag(f, "concurrency", 4)))
	var okCount int
	c.Run(ctx, st, n, func(a store.Account) {
		if a.Error == "" {
			okCount++
		}
		if s.json {
			b, _ := json.Marshal(a)
			fmt.Println(string(b))
			return
		}
		if a.Error != "" {
			fmt.Printf("  FAIL %s - %s\n", a.Email, a.Error)
			return
		}
		fmt.Printf("  OK   %s  credits=%d  id=%s\n", a.Email, a.Credits, a.ID)
	})
	fmt.Printf("done: %d/%d succeeded -> %s\n", okCount, n, s.storePath)
	return nil
}

func cmdList(s settings) error {
	st, err := openStore(s)
	if err != nil {
		return err
	}
	accs := st.Accounts()
	if s.json {
		return json.NewEncoder(os.Stdout).Encode(accs)
	}
	fmt.Printf("%-34s %-8s %-9s %-8s %-20s %s\n", "EMAIL", "CREDITS", "VERIFIED", "TOKEN", "COOKIE UNTIL", "LAST ERROR")
	for _, a := range accs {
		tok := "expired"
		if time.Now().Before(a.TokenExpiry) {
			tok = "valid"
		}
		ver := "no"
		if a.Verified {
			ver = "yes"
		}
		fmt.Printf("%-34s %-8d %-9s %-8s %-20s %s\n", a.Email, a.Credits, ver, tok,
			a.CookieExpiry.Format("2006-01-02 15:04"), truncate(a.Error, 40))
	}
	fmt.Printf("\n%d account(s)\n", len(accs))
	return nil
}

func cmdCheck(ctx context.Context, s settings) error {
	st, err := openStore(s)
	if err != nil {
		return err
	}
	for _, a := range st.Accounts() {
		c := jango.New(s.jangoAPI)
		if err := c.SetSession(a.RefreshCookie, a.AccessToken); err != nil {
			fmt.Printf("%-34s FAIL set-session: %v\n", a.Email, err)
			continue
		}
		if _, err := c.Token(ctx); err != nil {
			fmt.Printf("%-34s FAIL token: %v\n", a.Email, truncateAll(err.Error(), 80))
			a.Error = err.Error()
			_ = st.Upsert(a)
			continue
		}
		cr, err := c.Balance(ctx)
		if err != nil {
			fmt.Printf("%-34s FAIL balance: %v\n", a.Email, truncateAll(err.Error(), 80))
			continue
		}
		a.Credits, a.AccessToken, a.TokenExpiry, a.Error = cr.Balance, c.AccessToken(), c.AccessExpiry(), ""
		_ = st.Upsert(a)
		fmt.Printf("%-34s credits=%d\n", a.Email, cr.Balance)
	}
	return nil
}

func cmdExport(s settings, f map[string][]string) error {
	st, err := openStore(s)
	if err != nil {
		return err
	}
	format := flag(f, "format", "json")
	out := flag(f, "out", "")
	switch format {
	case "bridge":
		if out == "" {
			out = filepath.Join(filepath.Dir(s.storePath), "bridge-sessions.json")
		}
		n, err := wire.Export(st, out)
		if err != nil {
			return err
		}
		fmt.Printf("exported %d session(s) -> %s\n", n, out)
	case "csv":
		if out == "" {
			out = filepath.Join(filepath.Dir(s.storePath), "accounts.csv")
		}
		fh, err := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer fh.Close()
		wr := csv.NewWriter(fh)
		_ = wr.Write([]string{"id", "email", "password", "accessToken", "tokenExpiry", "refreshCookie", "credits"})
		for _, a := range st.Accounts() {
			_ = wr.Write([]string{a.ID, a.Email, a.Password, a.AccessToken,
				a.TokenExpiry.Format(time.RFC3339), a.RefreshCookie, fmt.Sprint(a.Credits)})
		}
		wr.Flush()
		fmt.Printf("exported CSV -> %s\n", out)
	default:
		var dst = os.Stdout
		if out != "" {
			fh, err := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			defer fh.Close()
			dst = fh
		}
		enc := json.NewEncoder(dst)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"version": 1, "accounts": st.Accounts()})
	}
	return nil
}

func cmdServe(ctx context.Context, s settings, f map[string][]string) error {
	st, err := openStore(s)
	if err != nil {
		return err
	}
	cfg := bridge.Config{
		Addr:      flag(f, "addr", "127.0.0.1:20581"),
		Prefix:    flag(f, "prefix", "jango"),
		Model:     jango.UpstreamModel,
		VerifyKey: flag(f, "api-key", ""),
	}
	srv, n, err := wire.Bridge(st, flag(f, "sessions", filepath.Join(filepath.Dir(s.storePath), "bridge-sessions.json")), cfg)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no usable sessions in %s", s.storePath)
	}
	fmt.Printf("loaded %d account(s); model %s/%s\n", n, cfg.Prefix, cfg.Model)
	return srv.ListenAndServe(ctx)
}

func cmdConnect(ctx context.Context, s settings, f map[string][]string) error {
	st, err := openStore(s)
	if err != nil {
		return err
	}
	base, key, model, err := wire.Run(ctx, st, wire.Options{
		RouterBase:  flag(f, "router", router.DefaultBase),
		CLIToken:    flag(f, "cli-token", ""),
		Prefix:      flag(f, "prefix", "jango"),
		BridgeAddr:  flag(f, "bridge", "127.0.0.1:20581"),
		SessionFile: flag(f, "sessions", filepath.Join(filepath.Dir(s.storePath), "bridge-sessions.json")),
		KeyName:     flag(f, "key-name", "jango-bulk"),
	})
	if err != nil {
		return err
	}
	if s.json {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"base_url": base, "api_key": key, "model": model})
	}
	fmt.Printf("connected to 9Router\n  base_url: %s\n  api_key:  %s\n  model:    %s\n", base, key, model)
	return nil
}

func cmdRun(ctx context.Context, s settings, f map[string][]string) error {
	n := intFlag(f, "n", intFlag(f, "count", 1))
	if n > 0 {
		if err := cmdSignup(ctx, s, f); err != nil {
			return err
		}
	}
	st, err := openStore(s)
	if err != nil {
		return err
	}
	if _, err := wire.Export(st, flag(f, "sessions", filepath.Join(filepath.Dir(s.storePath), "bridge-sessions.json"))); err != nil {
		return err
	}
	// start the bridge in-process unless told to connect to a running one
	if !has(f, "external-bridge") {
		go func() {
			cfg := bridge.Config{Addr: flag(f, "addr", "127.0.0.1:20581"),
				Prefix: flag(f, "prefix", "jango"), Model: jango.UpstreamModel}
			srv, cnt, err := wire.Bridge(st, flag(f, "sessions", filepath.Join(filepath.Dir(s.storePath), "bridge-sessions.json")), cfg)
			if err != nil || cnt == 0 {
				fmt.Fprintln(os.Stderr, "bridge:", err)
				return
			}
			_ = srv.ListenAndServe(ctx)
		}()
		time.Sleep(800 * time.Millisecond)
	}
	if len(st.Accounts()) == 0 {
		return fmt.Errorf("store is empty")
	}
	return cmdConnect(ctx, s, f)
}

func cmdVerifyModels(s settings, f map[string][]string) error {
	key := flag(f, "api-key", flag(f, "key", ""))
	base := strings.TrimRight(flag(f, "router", router.DefaultBase), "/")
	rc := router.New(base, "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ids, err := rc.Models(ctx, key)
	if err != nil {
		return err
	}
	for _, id := range ids {
		fmt.Println(id)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func truncateAll(s string, n int) string { return truncate(s, n) }
