# jango-bulk — audit and repair notes

## Verdict

No malware, no backdoor, no exfiltration. The published code is a straight HTTP
client: zero third-party Go dependencies (stdlib only), no `os/exec`, no
`syscall` use beyond `SIGTERM` handling, no `unsafe`, no base64 blobs, no
telemetry, no outbound webhook. The only hardcoded hosts are:

- `api.mail.tm` — disposable inboxes
- `api.jangoai.chat` / `app.jangoai.chat` — the target API and web origin
- `jango-ai.firebaseapp.com` — Firebase auth (used only as a documented constant)
- `127.0.0.1:20128` / `127.0.0.1:20581` — 9Router and the local bridge

Files are written `0600`, directories `0700`, the bridge binds loopback by
default, and the account store is a plain JSON vault rather than anything
sneaky.

## Risk findings (not malware, but worth knowing)

1. **One shared account password.** `Options.AccountPassword` defaults to a
   constant, so every generated account has the same password. Anyone who reads
   `data/accounts.json` or guesses the email can take the account over. Pass
   `--password` with a random value per run.
2. **`--router` can leak the 9Router CLI token.** `router.CLITokenFromHome`
   derives `x-9r-cli-token` from `~/.9router/machine-id` plus
   `~/.9router/auth/cli-secret` and sends it to whatever `--router` points at.
   Safe at the default `127.0.0.1:20128`; never point it at a remote host you
   do not control.
3. **The bridge is unauthenticated unless `--api-key` is set.** It binds
   loopback, so this only matters if you widen `--addr`.
4. **Weak randomness.** Mailbox local parts and mailbox passwords come from
   `math/rand`, not `crypto/rand`.
5. **`EnsureNode` overwrites by prefix.** An existing provider node whose prefix
   is `jango` gets patched in place — pick a different `--prefix` if that
   collides with something in 9Router.

## What was broken in the published binary (and fixed here)

The first published build no longer worked against the live site. Four separate
breakages, in the order they surfaced:

1. **mail.tm collection shape.** `GET /domains` and `GET /messages` return a bare
   JSON array when the request sends `Accept: application/json`, and the hydra
   JSON-LD object (`{"hydra:member": [...]}`) otherwise. The code only handled
   the object form and died with `cannot unmarshal array`. Fixed with
   `decodeCollection`, which accepts both.
2. **Signup payload.** `POST /auth/web/login` now rejects `email`/`name`/
   `password` with `property email should not exist` and wants a
   `firebaseToken`. Added `internal/firebase`: create or sign in the Firebase
   Email/Password user via Identity Toolkit, then exchange the ID token.
3. **Captcha.** The same endpoint answers `403 captcha_failed` without a
   Cloudflare Turnstile token (sitekey `0x4AAAAAAFAxvhRMZ0pu3lBE`, action
   `web-login`). Added `internal/captcha`, a YesCaptcha
   `TurnstileTaskProxyless` solver. The proxyless solve only succeeds when
   `pageAction` is passed.
4. **Refresh cookie rotation.** `__Secure-jango_rt` is single-use: every refresh
   issues a new cookie and revokes the old one, but neither `store` nor the
   bridge wrote the rotated value back, so any account died on the next restart.
   Added `jango.Client.SetOnCookie` plus a `persistSessions` hook in `wire` that
   rewrites the store and the session file on every rotation.

`creator.Options.LegacySignup` keeps the old direct-API path alive for the
in-process e2e fakes; the whole suite still runs offline.

## Verified live

- `jango-bulk signup -n 1` completes: inbox → firebase → turnstile → login →
  email verified → session captured.
- `serve` loads the pool; `/healthz` reports balances; `/v1/models` lists the
  pooled and the pinned model id (`jango/<account-id>/<model>` — the README's
  `acc-` form is wrong).
- Kill and restart the bridge: the pool still authenticates, which proves the
  rotated cookie was persisted.

## Credits: the signup bonus exists, it is just late

The first check right after signup read `credits: 0` and I wrongly concluded the
bonus was gone. It is not: `/credits/transactions` shows

```
type: SIGNUP_BONUS, amount: 60, reason: "Welcome bonus", metadata: {source: "signup"}
```

granted asynchronously — in one run it landed roughly 40 minutes after the user
row appeared, and it is still missing when `/users/me` is first read. One reply
costs 5 credits, so a fresh account buys 12 replies. `/credits/daily-reward`
exists but answers `Daily reward functionality is currently disabled`, and
`adRewards` remains the other source (5 credits per ad, 5/h, 20/day).

Fixes applied for this:

- `creator` polls `/credits/balance` for up to 60s after signup so the stored
  balance is real instead of 0.
- `bridge.Pick` now only routes to accounts with `credits >= 5`. The old filter
  treated `0` as usable, so round-robin handed requests to empty or dead
  accounts and surfaced `401 Refresh token revoked` / `402` to the caller. With
  nothing usable it now returns `503 no pooled account has at least 5 credits`.

## Operational hazard: the refresh cookie has exactly one owner

`__Secure-jango_rt` is single-use. Two clients refreshing the same account — the
bridge and a manual probe, for example — revoke each other and the account looks
dead. Run one owner per account: either the bridge, or a one-off script. The
bridge persists the rotated cookie back to `data/accounts.json` and
`data/bridge-sessions.json` on every refresh, so restarting it is safe as long
as nothing else is refreshing at the same time.

## Verified live (working)

- `jango/deepseek-v4-1-flash-uncensored` returns real completions through the
  pool: Jango AI replies, `credit_cost: 5`,
  `upstream_model: deepseek-v4-1-flash-uncensored`.
- Streaming works: `data: {"choices":[{"delta":{"content":"1"}}]}` frames arrive
  and `[DONE]` closes the stream.
- `/healthz` tracks the live balance (60 → 55 → 50 → 45 across three replies plus
  a stream) and the pool skips the account sitting at 0 credits.
- Kill and restart the bridge: the pool still authenticates.

Cost per account ≈ 25 YesCaptcha points for the Turnstile solve, once.
