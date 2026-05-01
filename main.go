// gamehub-for-mac-api-stub: local TLS server that intercepts api.xiaoji.com so
// GameHub.app believes it is logged in without ever talking to the vendor.
//
// The Vue frontend hardcodes the API URL at build time (VITE_API_BASE_URL), so
// env vars don't redirect it. Approach:
//   1. Edit /etc/hosts so api-international-gamehub.xiaoji.com -> 127.0.0.1
//   2. Listen on :443 with TLS using a cert signed by the mitmproxy CA the user
//      already trusts (~/.mitmproxy/mitmproxy-ca.pem)
//   3. Stub auth endpoints, return generic success for others.
//
// Requires root (port 443 + /etc/hosts editing). Run with sudo.

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const hostsMarker = "# gamehub-for-mac-api-stub managed entries (do not edit by hand)"

// Domains we redirect to ourselves
var stubbedHosts = []string{
	"api-international-gamehub.xiaoji.com",
	"api-cn-gamehub.xiaoji.com",
}

var (
	listenAddr  = flag.String("listen", ":443", "where to listen (TLS)")
	upstream    = flag.String("upstream", "https://api-international-gamehub.xiaoji.com", "real backend used as the passthrough target")
	stubOnly    = flag.Bool("stub-only", false, "never forward to upstream; return generic success for unhandled paths")
	stripPII    = flag.Bool("strip-pii", true, "strip device-identifying headers and crash bodies on forwarded requests")
	autoLaunch  = flag.Bool("launch", true, "launch GameHub.app on startup")
	appBinary   = flag.String("app", "/Applications/GameHub.app/Contents/MacOS/GameHub", "GameHub binary")
	verbose     = flag.Bool("v", true, "verbose request logging")
	caPath      = flag.String("ca", "", "CA cert+key pem path (default: ~/.mitmproxy/mitmproxy-ca.pem)")
	noHosts     = flag.Bool("no-hosts", false, "do not edit /etc/hosts (you must redirect another way)")
	httpOnly    = flag.Bool("http", false, "listen on plain HTTP (8443); skips TLS, hosts editing, root requirement")
)

// ---------------------------------------------------------------------------
// Fake user state

type fakeUser struct {
	ID       int64  `json:"id"`
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
	Nickname string `json:"nickname"`
	Email    string `json:"email"`
	Token    string `json:"token"`
	IssuedAt int64  `json:"issued_at_ms"`
	ExpireAt int64  `json:"expires_at_ms"`
}

var (
	stateMu sync.RWMutex
	state   *fakeUser
)

func mintUser(email string) *fakeUser {
	if email == "" {
		email = "offline@local"
	}
	now := time.Now().UnixMilli()
	tok := fmt.Sprintf("OFFLINE-TOKEN-%d", now)
	return &fakeUser{
		ID: 1, UserID: 1,
		Username: strings.SplitN(email, "@", 2)[0],
		Nickname: "Offline",
		Email:    email,
		Token:    tok,
		IssuedAt: now,
		ExpireAt: now + 365*24*60*60*1000,
	}
}

// ---------------------------------------------------------------------------
// Response helpers. Matches observed envelope: {"code":200,"message":"...","data":...}

type envelope struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data"`
}

func corsHeaders(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = "tauri://localhost"
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Credentials", "true")
	h.Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PATCH, PUT, DELETE")
	h.Set("Access-Control-Allow-Headers", "Content-Type, Origin, X-CSRF-Token, Authorization, AccessToken, Token, Range, X-User-Id, X-Lang-Id")
	h.Set("Access-Control-Expose-Headers", "Content-Length, Access-Control-Allow-Origin, Access-Control-Allow-Headers")
	h.Set("Access-Control-Max-Age", "86400")
	h.Set("Vary", "Origin")
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, v interface{}) {
	corsHeaders(w, r)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func ok(w http.ResponseWriter, r *http.Request, data interface{}) {
	writeJSON(w, r, 200, envelope{Code: 200, Message: "success", Data: data})
}

func okMsg(w http.ResponseWriter, r *http.Request, msg string, data interface{}) {
	writeJSON(w, r, 200, envelope{Code: 200, Message: msg, Data: data})
}

// ---------------------------------------------------------------------------
// Logging middleware

type capturingWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (c *capturingWriter) WriteHeader(s int) { c.status = s; c.ResponseWriter.WriteHeader(s) }
func (c *capturingWriter) Write(b []byte) (int, error) {
	if c.body.Len() < 16384 {
		c.body.Write(b)
	}
	return c.ResponseWriter.Write(b)
}

// summariseBody returns a one-line representation of a response body for
// logging. Textual bodies are truncated and shown; binary bodies (gzip etc.)
// are reported as size + magic prefix so the log file stays plain text.
func summariseBody(b []byte) string {
	const max = 600
	if len(b) == 0 {
		return "(empty)"
	}
	probe := b
	if len(probe) > 1024 {
		probe = probe[:1024]
	}
	nonPrintable := 0
	for _, c := range probe {
		if c == '\n' || c == '\r' || c == '\t' {
			continue
		}
		if c < 0x20 || c >= 0x7f {
			nonPrintable++
		}
	}
	if nonPrintable*4 > len(probe) {
		head := b
		if len(head) > 8 {
			head = head[:8]
		}
		return fmt.Sprintf("(%d bytes binary, magic=%x)", len(b), head)
	}
	s := string(b)
	if len(s) > max {
		s = s[:max] + "...(truncated)"
	}
	return strings.TrimSpace(s)
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))

		cw := &capturingWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(cw, r)

		if !*verbose {
			log.Printf("%s %s -> %d", r.Method, r.URL.Path, cw.status)
			return
		}
		log.Printf("\n---- %s %s%s ----", r.Method, r.Host, r.URL.RequestURI())
		for _, k := range []string{"Origin", "Authorization", "Token", "AccessToken", "X-User-Id", "X-Lang-Id"} {
			if v := r.Header.Get(k); v != "" {
				log.Printf("  > %s: %s", k, v)
			}
		}
		if len(body) > 0 && len(body) < 4096 {
			log.Printf("  > body: %s", string(body))
		}
		respBody := cw.body.Bytes()
		respText := summariseBody(respBody)
		log.Printf("  < %d  %s", cw.status, respText)
	})
}

// ---------------------------------------------------------------------------
// Stub handlers

func handleSendVerifyCode(w http.ResponseWriter, r *http.Request) {
	okMsg(w, r, "Verification code sent", map[string]any{
		"sent": true, "resendIn": 60, "resend_in": 60, "expire_in": 600, "valid_period": 600,
	})
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct{ Type, Username, Code string }
	_ = json.Unmarshal(body, &req)

	u := mintUser(req.Username)
	stateMu.Lock()
	state = u
	stateMu.Unlock()

	okMsg(w, r, "Login successful", map[string]any{
		// token: snake_case and camelCase variants for safety
		"token":         u.Token,
		"access_token":  u.Token,
		"accessToken":   u.Token,
		"user_token":    u.Token,
		"userToken":     u.Token,
		"refresh_token": u.Token + "-R",
		"refreshToken":  u.Token + "-R",
		"token_type":    "Bearer",
		"tokenType":     "Bearer",
		"expires_in":    int64((365 * 24 * time.Hour).Seconds()),
		"expiresIn":     int64((365 * 24 * time.Hour).Seconds()),
		"expires_at_ms": u.ExpireAt,
		"expiresAtMs":   u.ExpireAt,
		"issued_at_ms":  u.IssuedAt,
		"issuedAtMs":    u.IssuedAt,
		// user identity
		"user":      userPayload(u),
		"userInfo":  userPayload(u),
		"user_info": userPayload(u),
		"profile":   userPayload(u),
		"id":        u.ID,
		"user_id":   u.UserID,
		"userId":    u.UserID,
	})
}

func userPayload(u *fakeUser) map[string]any {
	return map[string]any{
		"id":             u.ID,
		"user_id":        u.UserID,
		"userId":         u.UserID,
		"uid":            u.UserID,
		"username":       u.Username,
		"user_name":      u.Username,
		"userName":       u.Username,
		"nickname":       u.Nickname,
		"nick_name":      u.Nickname,
		"display_name":   u.Nickname,
		"displayName":    u.Nickname,
		"email":          u.Email,
		"phone":          "",
		"avatar":         "",
		"avatar_url":     "",
		"avatarUrl":      "",
		"country":        "US",
		"country_code":   "US",
		"countryCode":    "US",
		"language_id":    1,
		"languageId":     1,
		"registered":     true,
		"activated":      true,
		"is_activated":   true,
		"banned":         false,
		"is_banned":      false,
		"verified":       true,
		"email_verified": true,
		"phone_verified": false,
		"created_at":     u.IssuedAt / 1000,
		"createdAt":      u.IssuedAt / 1000,
		"vip":            0,
		"vip_level":      0,
	}
}

func handleProfile(w http.ResponseWriter, r *http.Request) {
	stateMu.RLock()
	u := state
	stateMu.RUnlock()
	if u == nil {
		u = mintUser("offline@local")
		stateMu.Lock()
		state = u
		stateMu.Unlock()
	}
	ok(w, r, userPayload(u))
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	stateMu.Lock()
	state = nil
	stateMu.Unlock()
	ok(w, r, nil)
}

func handleSetLanguage(w http.ResponseWriter, r *http.Request) {
	okMsg(w, r, "User information updated successfully", nil)
}

func handleEmptyArray(w http.ResponseWriter, r *http.Request)  { ok(w, r, []any{}) }
func handleEmptyObject(w http.ResponseWriter, r *http.Request) { ok(w, r, map[string]any{}) }

func handleProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, 200, envelope{Code: 200, Message: "", Data: map[string]any{"providers": nil}})
}

func handleLanguages(w http.ResponseWriter, r *http.Request) {
	ok(w, r, []map[string]any{
		{"id": 1, "name": "English", "aliases": "en,en-us", "lang_code": "en", "status": 2, "weight": 10,
			"created_time": "2022-11-25 08:21:25", "updated_time": "2024-12-20 02:17:49"},
		{"id": 2, "name": "中文", "aliases": "zh,zh-cn", "lang_code": "zh-cn", "status": 2, "weight": 20,
			"created_time": "2022-11-25 08:21:50", "updated_time": "2024-12-20 02:17:49"},
	})
}

func handleUpgradeLatest(w http.ResponseWriter, r *http.Request) { ok(w, r, []any{}) }
func handleUpgradeTauri(w http.ResponseWriter, r *http.Request) {
	corsHeaders(w, r)
	w.WriteHeader(204)
}
func handleMonitorPolicy(w http.ResponseWriter, r *http.Request) {
	ok(w, r, map[string]any{"mode": "off", "enabled": false})
}

// ---------------------------------------------------------------------------
// Routing

func newRouter(upstreamIPs []string) http.Handler {
	mux := http.NewServeMux()

	// Stubs always intercepted, even in passthrough mode.
	//
	// Login flow: the real backend rejects accounts not on its allow-list.
	mux.HandleFunc("POST /oauth/v1/public/verify-code/send", handleSendVerifyCode)
	mux.HandleFunc("POST /oauth/v1/public/login", handleLogin)
	mux.HandleFunc("POST /oauth/v1/public/logout", handleLogout)
	mux.HandleFunc("POST /oauth/v1/user/logout", handleLogout)

	// Profile/identity. Listed as specific paths only: a wildcard like
	// "/game/v1/user/" would also match search, library, container-bindings,
	// and the steam-component fetch, returning the user profile object to
	// all of them and breaking the UI.
	for _, p := range []string{
		"/oauth/v1/user/profile", "/oauth/v1/user/info", "/oauth/v1/user/me",
		"/oauth/v1/user/current", "/oauth/v1/user/account",
		"/game/v1/user/profile", "/game/v1/user/info", "/game/v1/user/me",
	} {
		mux.HandleFunc("GET "+p, handleProfile)
	}

	// Third-party provider bindings (Steam/Epic external accounts linked to
	// this account). Frontend calls .filter() on the response, so it must
	// be an array.
	mux.HandleFunc("GET /oauth/v1/user/third-party/bindings", handleEmptyArray)
	mux.HandleFunc("GET /oauth/v1/user/third-party", handleEmptyArray)
	mux.HandleFunc("GET /oauth/v1/user/bindings", handleEmptyArray)

	// Telemetry. Kept local so identifying data (MAC address as crash_id,
	// hardware model, etc.) never leaves the machine.
	mux.HandleFunc("POST /game/v1/public/report/sync", handleEmptyObject)
	mux.HandleFunc("POST /game/v1/public/report/", handleEmptyObject)
	mux.HandleFunc("POST /game/v1/user/crash-feedbacks/check", handleEmptyObject)
	mux.HandleFunc("POST /game/v1/user/crash-feedbacks", handleEmptyObject)

	// Authenticated endpoints that the upstream rejects with our synthetic
	// token. Stubbed as empty paginated responses so the UI shows
	// "no library / no components" rather than looping 401s. See the
	// README's Limitations section: Steam-game launching still needs the
	// proprietary steamAgent and steamClient binaries on disk, downloaded
	// via a real account once.
	mux.HandleFunc("GET /game/v1/user/game_lib/list", func(w http.ResponseWriter, r *http.Request) {
		ok(w, r, map[string]any{
			"list": []any{}, "items": []any{}, "data": []any{},
			"total": 0, "page": 1, "page_size": 0,
		})
	})
	mux.HandleFunc("GET /game/v1/user/simulator/component", func(w http.ResponseWriter, r *http.Request) {
		ok(w, r, map[string]any{
			"list": []any{}, "items": []any{}, "data": []any{},
			"total": 0, "page": 1, "page_size": 0,
		})
	})

	// Catch-all: passthrough by default, generic success in -stub-only mode.
	var fallback http.Handler
	if *stubOnly {
		fallback = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.Printf("!!! unhandled %s %s; returning generic success", r.Method, r.URL.Path)
			ok(w, r, map[string]any{})
		})
	} else {
		fallback = passthroughProxy(upstreamIPs)
	}
	mux.Handle("/", fallback)

	return withLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			corsHeaders(w, r)
			w.WriteHeader(204)
			return
		}
		mux.ServeHTTP(w, r)
	}))
}

// Headers stripped on forwarded requests. Public endpoints respond without
// them, and the upstream has no business needing them to identify the client.
var piiHeaders = []string{
	"X-Device-Id", "X-Device-ID", "X-Real-IP",
	"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto",
	"Cf-Connecting-Ip", "True-Client-Ip",
}

// resolveUpstream looks up the upstream hostname's real IPs before /etc/hosts
// is modified, so the passthrough proxy can dial them directly without looping
// back into our own listener (which is what /etc/hosts now points at).
func resolveUpstream(host string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, errors.New("no addresses returned")
	}
	return addrs, nil
}

func passthroughProxy(upstreamIPs []string) http.Handler {
	target, err := url.Parse(*upstream)
	if err != nil {
		log.Fatalf("bad upstream %q: %v", *upstream, err)
	}
	upstreamHost := target.Hostname()

	rp := httputil.NewSingleHostReverseProxy(target)
	origDir := rp.Director
	rp.Director = func(r *http.Request) {
		origDir(r)
		r.Host = target.Host
		if *stripPII {
			for _, h := range piiHeaders {
				r.Header.Del(h)
			}
		}
	}

	// Dial the cached IPs directly. /etc/hosts now points the hostname at us,
	// so the resolver must not run for upstream calls (it would loop back).
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			port = "443"
		}
		var lastErr error
		for _, ip := range upstreamIPs {
			d := &net.Dialer{Timeout: 8 * time.Second}
			c, err := d.DialContext(ctx, network, net.JoinHostPort(ip, port))
			if err == nil {
				return c, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}

	rp.Transport = &http.Transport{
		DialContext:           dial,
		TLSClientConfig:       &tls.Config{ServerName: upstreamHost},
		ForceAttemptHTTP2:     true,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	rp.ModifyResponse = func(resp *http.Response) error {
		marker := ""
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			marker = "  (auth-rejected, candidate to stub)"
		}
		log.Printf("    [passthrough] %d %s%s%s", resp.StatusCode, target.Host, resp.Request.URL.Path, marker)
		return nil
	}
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("    [passthrough error] %s: %v", r.URL.Path, err)
		http.Error(w, "upstream error", 502)
	}
	return rp
}

// ---------------------------------------------------------------------------
// CA loading and dynamic per-SNI cert generation

type certIssuer struct {
	caCert *x509.Certificate
	caKey  any // RSA or ECDSA
	mu     sync.Mutex
	cache  map[string]*tls.Certificate
}

func loadMitmCA(path string) (*certIssuer, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}

	var (
		cert *x509.Certificate
		key  any
	)
	for {
		block, rest := pem.Decode(pemBytes)
		if block == nil {
			break
		}
		switch block.Type {
		case "CERTIFICATE":
			c, err := x509.ParseCertificate(block.Bytes)
			if err == nil && cert == nil {
				cert = c
			}
		case "RSA PRIVATE KEY":
			k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
			if err == nil {
				key = k
			}
		case "EC PRIVATE KEY":
			k, err := x509.ParseECPrivateKey(block.Bytes)
			if err == nil {
				key = k
			}
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err == nil {
				key = k
			}
		}
		pemBytes = rest
	}
	if cert == nil {
		return nil, errors.New("no CERTIFICATE found in CA pem")
	}
	if key == nil {
		return nil, errors.New("no PRIVATE KEY found in CA pem")
	}
	return &certIssuer{caCert: cert, caKey: key, cache: map[string]*tls.Certificate{}}, nil
}

func (ci *certIssuer) issue(host string) (*tls.Certificate, error) {
	ci.mu.Lock()
	defer ci.mu.Unlock()
	if c, ok := ci.cache[host]; ok {
		return c, nil
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ci.caCert, &priv.PublicKey, ci.caKey)
	if err != nil {
		return nil, err
	}
	tlsCert := &tls.Certificate{
		Certificate: [][]byte{der, ci.caCert.Raw},
		PrivateKey:  priv,
	}
	ci.cache[host] = tlsCert
	return tlsCert, nil
}

func (ci *certIssuer) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := hello.ServerName
	if host == "" {
		host = "localhost"
	}
	return ci.issue(host)
}

// ---------------------------------------------------------------------------
// /etc/hosts management

func editHosts(add bool) error {
	const path = "/etc/hosts"
	contents, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(contents), "\n")

	out := make([]string, 0, len(lines))
	skipping := false
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == hostsMarker {
			skipping = true
			continue
		}
		if skipping {
			if t == "" || strings.HasPrefix(t, "#") {
				skipping = false
			} else {
				continue
			}
		}
		out = append(out, l)
	}

	if add {
		for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
			out = out[:len(out)-1]
		}
		out = append(out, "", hostsMarker)
		for _, h := range stubbedHosts {
			out = append(out, "127.0.0.1 "+h)
		}
		out = append(out, "")
	}

	return os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o644)
}

// ---------------------------------------------------------------------------
// Launch GameHub as the original (non-root) user

func launchGameHub() *exec.Cmd {
	if _, err := os.Stat(*appBinary); err != nil {
		log.Fatalf("GameHub binary not found at %s: %v", *appBinary, err)
	}
	log.Printf("Launching GameHub: %s", *appBinary)

	// When running under sudo, drop back to the invoking user so GameHub writes
	// to the right home directory.
	cmd := exec.Command(*appBinary)
	if os.Geteuid() == 0 {
		if u := os.Getenv("SUDO_USER"); u != "" {
			cmd = exec.Command("sudo", "-u", u, *appBinary)
		}
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Fatalf("launch failed: %v", err)
	}
	log.Printf("GameHub PID: %d", cmd.Process.Pid)
	return cmd
}

// ---------------------------------------------------------------------------

func defaultCAPath() string {
	home := os.Getenv("HOME")
	if u := os.Getenv("SUDO_USER"); u != "" && os.Geteuid() == 0 {
		// Under sudo, HOME points at /var/root; recover the invoking user's home.
		out, err := exec.Command("sh", "-c", "echo ~"+u).Output()
		if err == nil {
			home = strings.TrimSpace(string(out))
		}
	}
	return filepath.Join(home, ".mitmproxy", "mitmproxy-ca.pem")
}

// resolveUpstreamForProxy resolves the upstream hostname before /etc/hosts is
// modified, so the passthrough proxy can reach the real backend.
func resolveUpstreamForProxy() []string {
	target, err := url.Parse(*upstream)
	if err != nil {
		log.Fatalf("bad upstream %q: %v", *upstream, err)
	}
	host := target.Hostname()
	ips, err := resolveUpstream(host)
	if err != nil {
		log.Fatalf("could not resolve upstream %q: %v", host, err)
	}
	log.Printf("upstream %s resolved to %v", host, ips)
	return ips
}

func runHTTP() {
	addr := ":8443"
	if *listenAddr != ":443" {
		addr = *listenAddr
	}
	log.Printf("HTTP-only mode: listening on %s (no TLS, no /etc/hosts changes)", addr)
	upstreamIPs := resolveUpstreamForProxy()
	srv := &http.Server{Addr: addr, Handler: newRouter(upstreamIPs), ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
	startAndWait(srv, false)
}

func runTLS() {
	if os.Geteuid() != 0 {
		log.Fatal("must run as root for TLS on :443 and /etc/hosts editing. Try: sudo ./gamehub-for-mac-api-stub")
	}

	caFile := *caPath
	if caFile == "" {
		caFile = defaultCAPath()
	}
	log.Printf("loading CA from %s", caFile)
	issuer, err := loadMitmCA(caFile)
	if err != nil {
		log.Fatalf("CA load failed: %v\n(install/trust mitmproxy CA first, or pass -ca=PATH)", err)
	}
	log.Printf("CA loaded (CN=%q)", issuer.caCert.Subject.CommonName)

	// Clean stale managed entries from a prior crashed run before resolving.
	// Otherwise the resolver picks up the loop-back redirect left behind.
	if !*noHosts {
		if err := editHosts(false); err != nil {
			log.Printf("warning: failed to pre-clean /etc/hosts: %v (continuing)", err)
		}
	}

	// Resolve upstream before /etc/hosts redirects the hostname at us.
	upstreamIPs := resolveUpstreamForProxy()

	for _, ip := range upstreamIPs {
		if strings.HasPrefix(ip, "127.") || ip == "::1" {
			log.Fatalf("refusing to start: upstream resolved to %s. /etc/hosts may have stale entries from a prior crashed run. Run: sudo sed -i '' '/# gamehub-for-mac-api-stub managed entries/,/^$/d' /etc/hosts", ip)
		}
	}

	if !*noHosts {
		log.Println("editing /etc/hosts")
		if err := editHosts(true); err != nil {
			log.Fatalf("failed to edit /etc/hosts: %v", err)
		}
		defer func() {
			log.Println("restoring /etc/hosts")
			if err := editHosts(false); err != nil {
				log.Printf("failed to restore /etc/hosts: %v (fix manually)", err)
			}
		}()
	}

	srv := &http.Server{
		Addr:    *listenAddr,
		Handler: newRouter(upstreamIPs),
		TLSConfig: &tls.Config{
			GetCertificate: issuer.GetCertificate,
			MinVersion:     tls.VersionTLS12,
		},
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	startAndWait(srv, true)
}

func startAndWait(srv *http.Server, useTLS bool) {
	go func() {
		if useTLS {
			log.Printf("listening on https://%s%s  (stub-only=%v, strip-pii=%v)", "127.0.0.1", srv.Addr, *stubOnly, *stripPII)
			if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
				log.Fatalf("server: %v", err)
			}
		} else {
			log.Printf("listening on http://127.0.0.1%s  (stub-only=%v)", srv.Addr, *stubOnly)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("server: %v", err)
			}
		}
	}()

	var cmd *exec.Cmd
	if *autoLaunch {
		time.Sleep(400 * time.Millisecond)
		cmd = launchGameHub()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	if cmd != nil {
		go func() {
			_ = cmd.Wait()
			log.Println("GameHub exited")
			sigCh <- syscall.SIGTERM
		}()
	}
	<-sigCh

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(os.Interrupt)
	}
}

func main() {
	flag.Parse()
	if *httpOnly {
		runHTTP()
		return
	}
	runTLS()
	log.Println("bye")
}
