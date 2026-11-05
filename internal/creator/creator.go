// Package creator runs the signup pipeline for one account:
// disposable inbox -> Firebase user -> Turnstile token -> Jango web session ->
// email verification -> session capture.
//
// The live Jango API no longer accepts email+password at /auth/web/login: it
// wants a Firebase ID token plus a Cloudflare Turnstile token, so the production
// path is driven through identitytoolkit and a YesCaptcha Turnstile solve.
// Options.SignupFunc selects the legacy in-process seam used by the offline e2e
// fakes, which keeps the whole pipeline testable without network access.
package creator

import (
	"context"
	"fmt"
	"math/rand"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tumooboy/petani-jango/internal/captcha"
	"github.com/tumooboy/petani-jango/internal/firebase"
	"github.com/tumooboy/petani-jango/internal/jango"
	"github.com/tumooboy/petani-jango/internal/mailtm"
	"github.com/tumooboy/petani-jango/internal/store"
)

// Browser-facing constants the live flow needs.
const (
	DefaultAppURL    = "https://app.jangoai.chat"
	DefaultSiteKey   = "0x4AAAAAAFAxvhRMZ0pu3lBE"
	DefaultAction    = "web-login"
	DefaultAccountPw = "Str0ng!Pass123"
)

// Options configures the creator.
type Options struct {
	Concurrency      int    // parallel signups (default 4)
	NamePrefix       string // display name prefix (default "Jango User")
	EmailPrefix      string // mailbox local-part prefix (default "jango")
	MailPassword     string // mailbox password (default random)
	AccountPassword  string // Jango password (default DefaultAccountPw)
	MailAPI          string // mail.tm base (default https://api.mail.tm)
	JangoAPI         string // Jango API base
	AppURL           string // web app origin, used as the Turnstile page URL
	FirebaseKey      string // Firebase web API key (default firebase.DefaultKey)
	CaptchaKey       string // YesCaptcha client key
	TurnstileSiteKey string // Turnstile site key
	VerifyTimeout    time.Duration
	SkipVerification bool
	OnProgress       func(Event)
	// LegacySignup drives the old direct-API signup (email+password to
	// /auth/web/login). Only the offline e2e fakes use it, because the live API
	// rejects that payload.
	LegacySignup bool
}

// Event is a progress update.
type Event struct {
	Email string `json:"email"`
	Stage string `json:"stage"`
	Ok    bool   `json:"ok"`
	Err   string `json:"err,omitempty"`
}

func (o *Options) defaults() {
	if o.Concurrency <= 0 {
		o.Concurrency = 4
	}
	if o.NamePrefix == "" {
		o.NamePrefix = "Jango User"
	}
	if o.EmailPrefix == "" {
		o.EmailPrefix = "jango"
	}
	if o.AccountPassword == "" {
		o.AccountPassword = DefaultAccountPw
	}
	if o.AppURL == "" {
		o.AppURL = DefaultAppURL
	}
	if o.TurnstileSiteKey == "" {
		o.TurnstileSiteKey = DefaultSiteKey
	}
	if o.VerifyTimeout <= 0 {
		o.VerifyTimeout = 180 * time.Second
	}
}

// Creator runs batches.
type Creator struct {
	Opts Options
}

// New normalises options and returns a creator.
func New(o Options) *Creator {
	o.defaults()
	return &Creator{Opts: o}
}

const passAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789!@#$%"

func randToken(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = passAlphabet[rand.Intn(len(passAlphabet))]
	}
	return string(b)
}

var counter int64

// Run creates n accounts, writing each result to st as soon as it is ready.
// Errors are per-account: one failure never aborts the batch.
func (c *Creator) Run(ctx context.Context, st *store.Store, n int, report func(store.Account)) {
	jobs := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < c.Opts.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				acc := c.one(ctx)
				if acc != nil {
					if err := st.Upsert(acc); err != nil {
						acc.Error = "store: " + err.Error()
					}
					_ = st.Log(map[bool]string{true: "created", false: "failed"}[acc.Error == ""],
						map[string]any{"email": acc.Email, "id": acc.ID, "credits": acc.Credits,
							"verified": acc.Verified, "error": acc.Error})
					report(*acc)
				}
			}
		}()
	}
	for i := 0; i < n; i++ {
		select {
		case <-ctx.Done():
			i = n
		case jobs <- struct{}{}:
		}
	}
	close(jobs)
	wg.Wait()
}

func (c *Creator) emit(e Event) {
	if c.Opts.OnProgress != nil {
		c.Opts.OnProgress(e)
	}
}

// Signup is a single account (exported for tests and the `signup` command).
func (c *Creator) Signup(ctx context.Context) *store.Account { return c.one(ctx) }

func (c *Creator) one(ctx context.Context) *store.Account {
	ctx, cancel := context.WithTimeout(ctx, c.Opts.VerifyTimeout+320*time.Second)
	defer cancel()

	n := atomic.AddInt64(&counter, 1)
	name := fmt.Sprintf("%s %d", c.Opts.NamePrefix, n)
	mailPass := c.Opts.MailPassword
	if mailPass == "" {
		mailPass = randToken(18)
	}
	acc := &store.Account{
		Name:          name,
		Password:      c.Opts.AccountPassword,
		InboxPassword: mailPass,
		CreatedAt:     time.Now().UTC(),
	}
	fail := func(stage string, err error) *store.Account {
		acc.Error = stage + ": " + err.Error()
		c.emit(Event{Email: acc.Email, Stage: stage, Err: err.Error()})
		return acc
	}

	// 1. disposable inbox
	mail := mailtm.New(c.Opts.MailAPI)
	ib, err := mail.Create(ctx, mailtm.RandomLocal(c.Opts.EmailPrefix, 12), mailPass)
	if err != nil {
		return fail("inbox", err)
	}
	acc.Email = ib.Address
	c.emit(Event{Email: acc.Email, Stage: "inbox", Ok: true})

	jc := jango.New(c.Opts.JangoAPI)
	jc.SetOnCookie(func(v string) { acc.RefreshCookie = v })

	if c.Opts.LegacySignup {
		// offline path used by the in-process fakes
		if _, err := jc.Signup(ctx, name, ib.Address, acc.Password); err != nil && !jango.IsStatus(err, 409) {
			return fail("signup", err)
		}
		c.emit(Event{Email: acc.Email, Stage: "signup", Ok: true})
		if !c.Opts.SkipVerification {
			link, _, err := mail.WaitLink(ctx, ib, func(u string) bool {
				return strings.Contains(u, "verifyEmail") || strings.Contains(u, "/verify")
			}, c.Opts.VerifyTimeout, 4*time.Second)
			if err != nil {
				return fail("verify-mail", err)
			}
			if err := jc.VerifyEmail(ctx, link, baseOrigin(link)); err != nil {
				return fail("verify", err)
			}
			acc.Verified = true
			c.emit(Event{Email: acc.Email, Stage: "verified", Ok: true})
		}
	} else {
		// 2. Firebase user, 3. Turnstile token, 4. Jango web session
		fb := firebase.New(c.Opts.FirebaseKey)
		sess, err := fb.SignUp(ctx, ib.Address, acc.Password)
		if err != nil {
			sess, err = fb.SignIn(ctx, ib.Address, acc.Password)
			if err != nil {
				return fail("firebase", err)
			}
		}
		acc.FirebaseUID = sess.LocalID
		c.emit(Event{Email: acc.Email, Stage: "firebase", Ok: true})

		capClient, err := captcha.New(c.Opts.CaptchaKey)
		if err != nil {
			return fail("captcha", err)
		}
		tsToken, err := capClient.Turnstile(ctx, c.Opts.AppURL+"/login", c.Opts.TurnstileSiteKey, DefaultAction)
		if err != nil {
			return fail("captcha", err)
		}
		c.emit(Event{Email: acc.Email, Stage: "turnstile", Ok: true})

		if err := jc.LoginWeb(ctx, sess.IDToken, tsToken); err != nil {
			return fail("login", err)
		}
		c.emit(Event{Email: acc.Email, Stage: "login", Ok: true})

		// 5. email verification via the Firebase oobCode (no browser needed)
		if !c.Opts.SkipVerification {
			if err := fb.SendVerifyEmail(ctx, sess.IDToken); err != nil {
				return fail("verify-mail-send", err)
			}
			link, _, err := mail.WaitLink(ctx, ib, func(u string) bool {
				return strings.Contains(u, "verifyEmail") || strings.Contains(u, "/verify")
			}, c.Opts.VerifyTimeout, 4*time.Second)
			if err != nil {
				return fail("verify-mail", err)
			}
			code := oobCode(link)
			if code == "" {
				return fail("verify", fmt.Errorf("no oobCode in link"))
			}
			if err := fb.VerifyWithOobCode(ctx, code); err != nil {
				return fail("verify", err)
			}
			verified, err := fb.Lookup(ctx, sess.IDToken)
			if err != nil {
				return fail("verify-check", err)
			}
			acc.Verified = verified
			c.emit(Event{Email: acc.Email, Stage: "verified", Ok: verified})
		}
	}

	// 6. capture profile + session
	if _, err := jc.Token(ctx); err != nil {
		return fail("session", err)
	}
	me, err := jc.Me(ctx)
	if err != nil {
		return fail("profile", err)
	}
	acc.ID = me.ID
	if me.Name != "" {
		acc.Name = me.Name
	}
	acc.FirebaseUID = me.FirebaseUID
	acc.AccessToken = jc.AccessToken()
	acc.TokenExpiry = jc.AccessExpiry()
	if ck := jc.RefreshCookie(); ck != "" {
		acc.RefreshCookie = ck
	}
	if acc.RefreshCookie == "" {
		return fail("session", fmt.Errorf("no %s cookie captured", jango.RefreshCookie))
	}
	acc.CookieExpiry = time.Now().UTC().Add(30 * 24 * time.Hour)
	if me.Credits != "" {
		fmt.Sscan(me.Credits, &acc.Credits)
	}
	// The signup bonus is credited asynchronously (it landed ~40 minutes after
	// the account row appeared in one run), so poll the balance briefly instead
	// of storing the 0 that /users/me reports at creation time.
	for i := 0; i < 12 && acc.Credits == 0; i++ {
		if cr, err := jc.Balance(ctx); err == nil {
			acc.Credits = cr.Balance
			if cr.Balance > 0 {
				c.emit(Event{Email: acc.Email, Stage: "credits", Ok: true})
				break
			}
		}
		time.Sleep(5 * time.Second)
	}
	if me.BlockedAt != nil {
		acc.Error = "account blocked at signup"
	}
	acc.UpdatedAt = time.Now().UTC()
	c.emit(Event{Email: acc.Email, Stage: "session", Ok: true})
	return acc
}

// oobCode pulls the Firebase verification code out of an action link.
func oobCode(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	return u.Query().Get("oobCode")
}

// baseOrigin returns scheme://host of a URL.
func baseOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
