# petani-jango

Bulk Jango AI account pool behind an OpenAI-compatible bridge, wired into 9Router.

Original tooling by tumooboy. Fixed and hardened for the live site — see [AUDIT.md](AUDIT.md)
for the full breakdown. Zero third-party Go dependencies — stdlib only.

## What it does

```
signup   ->  create N accounts end to end:
            mail.tm inbox -> Firebase Email/Password user -> Cloudflare Turnstile solve
            -> POST /auth/web/login (firebaseToken) -> email verification -> session capture
export   ->  write sessions as JSON / CSV / bridge format
serve    ->  OpenAI-compatible bridge on 127.0.0.1, round-robins the pool
connect  ->  register the pool in 9Router and verify a reply end to end
run      ->  signup -> export -> background serve -> connect (one shot)
```

Every account gets a 60-credit signup bonus (12 replies at 5 credits each), granted
**asynchronously** — often ~40 minutes after signup, not present in the first `/users/me` read.
Do not panic at `credits: 0`.

## Fixed vs upstream

1. **mail.tm collection shape** — `GET /domains` / `GET /messages` return a bare JSON array with
   `Accept: application/json` and a hydra JSON-LD object otherwise. Both are now accepted.
2. **Signup payload** — `POST /auth/web/login` now requires `firebaseToken` (the old
   `email`/`name`/`password` shape answers `property email should not exist`). Firebase
   Email/Password users are created or signed in via Identity Toolkit, then exchanged.
3. **Captcha** — the same endpoint answers `403 captcha_failed` without a Cloudflare Turnstile
   token (sitekey `0x4AAAAAAFAxvhRMZ0pu3lBE`, action `web-login`). Solved via YesCaptcha
   `TurnstileTaskProxyless`; the proxyless solve only succeeds when `pageAction` is passed.
   Expect ~10-30% solver failures per batch — just re-run `signup` for the remainder.
4. **Refresh cookie rotation** — `__Secure-jango_rt` is single-use: every refresh issues a new
   cookie and revokes the old one. Upstream never wrote the rotated value back, so accounts died
   on the next restart. Rotations now persist to the store and the session file automatically.

Pool hardening on top:

- `creator` polls `/credits/balance` for up to 60s after signup so the stored balance is real.
- `bridge.Pick` only routes to accounts with `credits >= 5`; with nothing usable it returns
  `503 no pooled account has at least 5 credits` instead of handing requests to dead accounts.

## Build

```
make build          # -> bin/jango-bulk
make test           # full offline suite (in-process fakes, no network)
make vet && make fmt
```

Requires Go 1.23+.

## Requirements

- A **YesCaptcha** API key in the environment (`YESCAPTCHA_API_KEY`) — nothing else is external.
- mail.tm, api.jangoai.chat, app.jangoai.chat, jango-ai.firebaseapp.com reachable.

## Usage

```
# 5 accounts, random per-account password (do NOT use the weak upstream default)
jango-bulk signup -n 5 --password "$(openssl rand -hex 12)"

# inspect the pool
jango-bulk list
jango-bulk check

# serve the pool as an OpenAI-compatible endpoint
jango-bulk serve --addr 127.0.0.1:20581 --api-key "$SOME_KEY"

# register in 9Router and verify end to end
jango-bulk connect
jango-bulk verify-models
```

Models surface as `jango/<account-id>/<model>`, e.g. `jango/deepseek-v4-1-flash-uncensored`.

## Operational notes (read before running)

- **One owner per refresh cookie.** `__Secure-jango_rt` is single-use. Two clients refreshing the
  same account (the bridge plus a manual probe, say) revoke each other and the account looks dead.
  Run either the bridge or a probe — never both — against the same store.
- **The bridge is unauthenticated unless `--api-key` is set.** It binds loopback by default; keep
  it that way unless you know what you are doing.
- **Never point `--router` at a host you do not control.** The CLI token derivation sends
  `x-9r-cli-token` to whatever `--router` names. Default `127.0.0.1:20128` is safe.
- **Per-account passwords.** `--password` with a random value per run; the upstream default is a
  public constant.
- **Model id format** is `jango/<account-id>/<model>` — the upstream README's `acc-` form is wrong.

## Layout

```
cmd/jango-bulk/     CLI entry
internal/jango/     Jango API client (login, refresh, credits, chat)
internal/firebase/  Identity Toolkit Email/Password auth
internal/captcha/   YesCaptcha Turnstile solver
internal/mailtm/    mail.tm disposable inboxes
internal/creator/   signup pipeline
internal/bridge/    OpenAI-compatible pool bridge
internal/router/    9Router registration client
internal/store/     JSON account vault (0600)
internal/e2e/       offline end-to-end suite
internal/mock/      in-process fakes
```

## Hygiene

`bin/`, `data/` (the account vault — credentials, JWTs, refresh cookies) and session files are
git-ignored. Nothing sensitive belongs in this tree; if you find any, it is a bug — file an issue.

