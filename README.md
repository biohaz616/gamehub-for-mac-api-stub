# gamehub-for-mac-api-stub

By **BioHaz616**. Released under the [MIT License](./LICENSE).

[简体中文 README](./README.zh-CN.md)

> **Not affiliated with the vendor.** This is an independent third-party
> compatibility tool. It does not modify the GameHub application or any
> file shipped with it. See the Disclaimer and legal notices section
> below for the full statement.

## Purpose

The macOS application **GameHub** (a Wine-based Windows game launcher
distributed by Guangzhou Chicken Run Network Technology Co., Ltd.) gates
its UI behind an email-code login against
`api-international-gamehub.xiaoji.com`. The vendor's real backend rejects
emails that aren't on its allow-list with the message *"No internal test
qualification yet"*, so users without a pre-approved account can't get
past the login screen at all.

This project is a local TLS server that lets you use GameHub on macOS
**without providing valid login credentials**. It pretends to be the
xiaoji backend for the auth flow, accepts any email and any verification
code, and hands GameHub a synthetic user object so the app reaches a
"logged in" state. Public content (game catalog, languages, store
listings) still comes from the real upstream so the UI stays functional;
crash reports and telemetry containing identifying data are answered
locally so nothing leaks.

Steam and Epic authentication are unaffected. Those run through the
embedded clients and talk to their respective vendors directly.

## Why this exists

GameHub's UI sits in a Tauri 2 webview. The Vue frontend hardcodes the API
base URL into its bundled JavaScript (`VITE_API_BASE_URL`) at build time, so
the runtime environment variable `GAMEHUB_API_BASE_URL` only redirects
Rust-side calls. The webview's `fetch()` calls keep going to the original
hostname. Redirecting that traffic therefore needs:

1. A `/etc/hosts` entry pointing the hostname at `127.0.0.1`, and
2. A TLS server presenting a cert that the system trust store accepts for that
   hostname.

This program does both, plus serves the API responses.

## Operating modes

Two modes, chosen at startup:

- **Passthrough (default).** The stub intercepts only the auth-flow endpoints
  (login, verify-code, profile, logout) and a handful of telemetry endpoints
  that send identifying data. Everything else is forwarded to the real
  upstream so dictionaries, languages, banners, store listings, and any other
  public content come back as live data. This makes the application's UI
  fully functional.
- **Stub-only (`-stub-only`).** The stub never forwards. Anything not
  explicitly handled returns a generic
  `{"code":200,"message":"success","data":{}}`. Use this when you want zero
  network egress to the vendor. Trade-off: screens depending on real content
  (homepage banners, search, store) will be empty.

Identifying request headers (`X-Device-Id`, forwarded-for variants) are
stripped on every forwarded request unless you pass `-strip-pii=false`.

## Requirements

- macOS (tested on Apple Silicon)
- Go 1.22 or newer
- [mitmproxy](https://mitmproxy.org/) installed once, only to obtain its CA.
  Any CA you control works; mitmproxy is just the easiest source.
- The mitmproxy CA trusted by the macOS System keychain (one-time setup,
  instructions below).
- `sudo` access (required to bind port 443 and write to `/etc/hosts`).

## One-time setup

### 1. Generate and trust a CA

The simplest path is to use mitmproxy's CA, since it self-generates one and
the stub reads it directly:

```sh
brew install mitmproxy
mitmproxy   # press q, y to quit. This creates ~/.mitmproxy/*
sudo security add-trusted-cert -d -r trustRoot \
  -k /Library/Keychains/System.keychain \
  ~/.mitmproxy/mitmproxy-ca-cert.pem
```

Verify with:

```sh
security find-certificate -c mitmproxy /Library/Keychains/System.keychain >/dev/null && echo OK
```

To use your own CA instead, pass `-ca=/path/to/ca.pem` when running the stub.
The PEM file must contain both the certificate and its private key.

### 2. Build

```sh
git clone <this-repo>
cd gamehub-for-mac-api-stub
go build -o gamehub-for-mac-api-stub .
```

## Running

```sh
sudo ./gamehub-for-mac-api-stub
```

What this does:

1. Loads the CA (default: `~/.mitmproxy/mitmproxy-ca.pem`).
2. Adds two entries to `/etc/hosts`:
   ```
   127.0.0.1 api-international-gamehub.xiaoji.com
   127.0.0.1 api-cn-gamehub.xiaoji.com
   ```
   The entries are wrapped in a clearly-marked block and removed
   automatically on shutdown.
3. Listens on `:443` with TLS, dynamically issuing server certs for any SNI
   hostname (signed by the loaded CA).
4. Launches `/Applications/GameHub.app` as the invoking user (via
   `sudo -u $SUDO_USER`) so it writes to the correct home directory.
5. Forwards anything that isn't an auth or telemetry endpoint to the real
   backend.

When the GameHub login screen appears:

1. Enter any email address.
2. Click **Send Code**.
3. Enter any six-digit value.
4. Click **Login**.

Press **Ctrl+C** in the terminal to shut down. The `/etc/hosts` entries are
restored before exit.

## Flags

| Flag | Default | Meaning |
|------|---------|---------|
| `-listen` | `:443` | Bind address. Change requires keeping the redirect target in sync. |
| `-ca` | `~/.mitmproxy/mitmproxy-ca.pem` | CA cert + key (PEM) used to sign per-SNI certs. |
| `-app` | `/Applications/GameHub.app/Contents/MacOS/GameHub` | GameHub binary path. |
| `-launch` | `true` | Auto-launch GameHub on startup. |
| `-no-hosts` | `false` | Skip `/etc/hosts` editing (you must redirect another way). |
| `-stub-only` | `false` | Never forward to the upstream; return generic success for unhandled paths. |
| `-upstream` | `https://api-international-gamehub.xiaoji.com` | Passthrough target. |
| `-strip-pii` | `true` | Strip identifying headers (X-Device-Id and friends) on forwarded requests. |
| `-http` | `false` | Plain HTTP on `:8443` (no TLS, no `/etc/hosts`, no root). For testing only; will not work as a redirect target since the real URLs are HTTPS. |
| `-v` | `true` | Verbose request/response logging. |

## What's stubbed

These endpoints are answered locally regardless of mode:

| Method | Path | Why intercepted |
|--------|------|-----------------|
| `POST` | `/oauth/v1/public/verify-code/send` | Real backend rejects accounts not on its allow-list. |
| `POST` | `/oauth/v1/public/login` | Same. Returns 400 "verification code invalid" against real upstream. |
| `POST` | `/oauth/v1/public/logout`, `/oauth/v1/user/logout` | Tokens are synthetic; remote logout would 401. |
| `GET`  | `/oauth/v1/user/{profile,info,me,current,account}`, `/game/v1/user/{profile,info,me}` | Profile fetch with a synthetic bearer would 401. Listed explicitly so wildcards on `/user/` (search, library, etc.) still passthrough. |
| `POST` | `/game/v1/public/report/sync`, `/game/v1/public/report/...` | Telemetry, kept local. |
| `POST` | `/game/v1/user/crash-feedbacks/check`, `/game/v1/user/crash-feedbacks` | Sends host MAC address as `crash_id`, kept local. |
| `GET`  | `/oauth/v1/user/third-party/bindings`, `/oauth/v1/user/bindings` | Frontend calls `.filter()` on this list; must be an array. Returns `[]` since no providers are linked. |
| `GET`  | `/game/v1/user/game_lib/list` | Empty pagination shape. The user's library can't be fetched with a synthetic token. See Limitations. |
| `GET`  | `/game/v1/user/simulator/component` | Empty pagination shape. Steam-launch-required components are gated behind a real account. See Limitations. |
| `OPTIONS *` | `*` | `204` with permissive CORS headers (matches what the real backend returns). |

Everything else, in default mode, is forwarded to the upstream with PII
headers stripped.

The response envelope used by every stub is:

```json
{ "code": 200, "message": "...", "data": ... }
```

`code: 200` indicates success; the live backend does not use `code: 0`.

## Iteration workflow

If something specific breaks (an in-app screen errors, a feature is unusable),
the log line that matters is the upstream status code printed by the
passthrough proxy:

```
[passthrough] 401 api-international-gamehub.xiaoji.com/some/path  (auth-rejected, candidate to stub)
```

A `401` or `403` means the real backend rejected the synthetic token for
that path. That's the next handler to write. Add it in `main.go`:

```go
mux.HandleFunc("GET /game/v1/some/new/path", func(w http.ResponseWriter, r *http.Request) {
    ok(w, r, map[string]any{ /* shape that satisfies the UI */ })
})
```

To see what the real backend returns for a path (so the stub can mimic the
shape), capture it once with mitmproxy or a similar tool, or temporarily
un-stub the path and let it fall through to passthrough while logged in
with a real account.

## Architecture

```
+-----------------+     +------------------+     +---------------------+
|  GameHub.app    |     |  /etc/hosts      |     |  gamehub-for-mac-api-stub       |
|  (Tauri + Vue)  |fetch| routes xiaoji    |TLS  |  TLS terminator     |
|                 |---->| to 127.0.0.1     |---->|  + handlers         |
+-----------------+     +------------------+     +----------+----------+
                                                            |
                                       +--------------------+--------------------+
                                       |                    |                    |
                                auth + telemetry      catch-all (default)    -stub-only
                                (always stubbed)            |                    |
                                                strip PII headers          generic success
                                                            |
                                                  +---------v---------+
                                                  |  real upstream    |
                                                  |  (xiaoji.com)     |
                                                  +-------------------+
```

The TLS terminator uses `crypto/tls`'s `GetCertificate` callback, which fires
on every ClientHello. It generates an ECDSA P-256 server cert signed by the
loaded CA, names the requested SNI in both Subject CN and SubjectAltName, and
caches it. So the same server handles `*.xiaoji.com` and any other host you
add to `/etc/hosts` without configuration changes.

## Limitations

- **Steam game launching needs proprietary components that gate behind a real
  account.** The first time you try to launch a Steam game, GameHub fetches
  metadata from `GET /game/v1/user/simulator/component` for two components:
  `steamAgent` (type 7) and `steamClient` (type 8). That endpoint requires a
  real authenticated session; the synthetic token is rejected, and there is
  no public alternative path. The component download URLs are not embedded
  in the application binary or available from any unauthenticated endpoint.

  Practical workaround: log in once with a real account, let GameHub
  download both components into
  `~/Library/Application Support/com.gamemac.www/wine-engine/`, then switch
  to running with this stub. The frontend's `[steam-deps]` check consults
  local storage before the API; once the binaries are on disk you never
  need the components endpoint again.

  Alternative: avoid GameHub for Steam games and run Steam through Whisky,
  CrossOver, or the Apple Game Porting Toolkit directly. Those have no
  vendor-side metadata gate.

- **Steam and Epic account auth is out of scope of this stub.** GameHub
  talks to Valve and Epic directly (Steam via the bundled steamkit-core,
  Epic via OAuth). That traffic does not touch the xiaoji API and is
  unaffected by this stub. To play Steam games offline you still need to
  sign into Steam once inside GameHub, then use Steam's own offline mode.

- **Default mode still talks to the vendor.** Public content endpoints
  (dictionaries, banners, store listings) are forwarded so the UI works.
  The stub strips identifying headers but the upstream still sees your IP,
  the User-Agent, and the request shape. Use `-stub-only` for full network
  isolation, accepting that some screens will be empty.

- **Token-binding fields exist but appear client-only.** The application
  contains references to `userTokenHash`, `deviceFingerprint`,
  `localBindingKeyHash`, `localBindingSignature`. Static analysis suggests
  these are derived locally rather than verified against a server-issued
  signature. If something breaks with a binding-related error, that
  assumption was wrong.

- **The Tauri updater hardcodes the China endpoint**
  (`api-cn-gamehub.xiaoji.com`), so it always hits the redirect set up
  here. The stub returns `204` (the Tauri updater contract for "no
  update").

- **SIP not affected.** The stub does not require disabling System
  Integrity Protection. The runtime modifications it makes (`/etc/hosts`,
  port 443) are ordinary administrative privileges, not SIP-protected.

## Cleanup

If the stub is killed (kill -9, crash, etc.) without running its shutdown
handler, the `/etc/hosts` entries remain. Restore manually with:

```sh
sudo sed -i '' '/# gamehub-for-mac-api-stub managed entries/,/^$/d' /etc/hosts
```

Or remove the block by hand. It is wrapped in a clearly-marked comment.

Or remove the block by hand. It is wrapped in a clearly-marked comment.

## Disclaimer and legal notices

### No affiliation

This project is **not affiliated with, endorsed by, sponsored by, or
otherwise associated with** Guangzhou Chicken Run Network Technology Co.,
Ltd., GameHub, xiaoji.com, or any of their subsidiaries, employees, or
representatives. *GameHub* and any related marks are the property of their
respective owners and are referenced here solely to identify the
application that this tool interoperates with.

### What this tool does and does not do

This tool **does not**:

- modify, patch, recompile, or otherwise alter the GameHub application
  binary or any file shipped with it
- decrypt, extract, distribute, or rehost any proprietary code, assets, or
  content belonging to the vendor
- bypass digital rights management protecting paid content
- grant access to games or content the user does not already own and have
  installed on their own machine
- circumvent payment systems or licensing controls

This tool **does**:

- run as a separate process on the user's own machine
- intercept network traffic addressed to the vendor's servers by editing
  the user's own `/etc/hosts` file (a configuration file the user has
  full rights to modify on their own system)
- present an alternative TLS endpoint signed by a certificate authority
  the user controls and has explicitly chosen to trust
- respond to requests with synthesized data so the application behaves as
  though a remote backend were available

Game content, Steam authentication, and Epic authentication are out of
scope and pass through unchanged to their respective vendors.

### Interoperability framing

This tool is intended for **interoperability and personal compatibility
purposes**, allowing a user who already possesses a legitimate copy of
the GameHub application to use it on their own hardware without being
gated behind a third-party authentication system that may not accept
their account.

In jurisdictions with relevant statutes (for example 17 U.S.C. § 1201(f)
in the United States, or analogous interoperability provisions in EU
directives), compatibility shims of this kind are generally not
considered circumvention of technological protection measures. **This is
not legal advice. Consult a lawyer in your own jurisdiction if in
doubt.**

### Terms of service and EULA

Using this tool may violate GameHub's terms of service, end-user licence
agreement, or acceptable-use policy. **You are solely responsible for
reviewing and complying with those terms.** A breach of contract is a
separate matter from any computer-misuse statute and is between you and
the vendor.

### No warranty

This software is provided "as is", without warranty of any kind, express
or implied, including but not limited to the warranties of
merchantability, fitness for a particular purpose, and noninfringement.
The authors and contributors disclaim all liability for any damages
arising from use of this software, including but not limited to data
loss, account suspension, or any incidental or consequential damages.

### Use responsibly

This project is published for **educational, research, and personal-use
compatibility** purposes only. Do not use it to facilitate piracy,
account sharing, abuse of vendor infrastructure, or any unlawful
activity. If the vendor's infrastructure would be measurably impacted by
sustained passthrough traffic, switch to `-stub-only` mode.
