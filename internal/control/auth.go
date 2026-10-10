// Package control isolates administrator authentication from player authentication.
package control

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"fuseronline/internal/config"
	"fuseronline/internal/logx"
	"fuseronline/internal/store"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

type user struct {
	store.ControlAdmin
	cred webauthn.Credential
}

func (u user) WebAuthnID() []byte          { return []byte(u.ID) }
func (u user) WebAuthnName() string        { return u.Name }
func (u user) WebAuthnDisplayName() string { return u.Name }
func (u user) WebAuthnCredentials() []webauthn.Credential {
	if len(u.cred.ID) == 0 {
		return nil
	}
	return []webauthn.Credential{u.cred}
}

type ceremony struct {
	data    webauthn.SessionData
	user    user
	hash    string
	token   store.ControlPendingToken
	client  string // clientKey of the request that began it
	seq     uint64 // creation order; clock readings can tie
	enroll  bool
	expires time.Time
}
type rate struct {
	at time.Time
	n  int
}
type Auth struct {
	st      *store.Store
	wa      *webauthn.WebAuthn
	logs    *logx.Logs
	log     *slog.Logger
	origin  string
	secure  bool
	allowed []netip.Prefix // empty: every address
	zone    *time.Location // control.timezone: for sessions whose browser reported no usable zone
	zoneMu  sync.Mutex
	zones   map[string]*time.Location // loaded session zones by name
	// Cookie names: __Host- when the origin is HTTPS, so the browser accepts the cookies only from this exact host
	// over HTTPS (Secure, Path=/, no Domain): a sibling subdomain or a plain-HTTP response cannot plant them.
	sessionCookie, ceremonyCookie string
	mu                            sync.Mutex
	pending                       map[string]ceremony
	seq                           uint64
	limits                        map[string]rate
}

const (
	authAttempts = 20 // per client per minute
	// The rate-limit and ceremony tables are bounded. When one is full the oldest entry is dropped, never the new
	// request: a flood from many addresses must delay administrators, not lock them out.
	maxClients       = 4096
	maxPending       = 1024
	pendingPerClient = 3
	// tokenMaxMinutes bounds enrollment tokens issued from the panel; the command line allows up to 24 hours.
	tokenMaxMinutes = 60
	auditShown      = 200
	hstsHeader      = "max-age=31536000"
)

type ctxKey struct{}
type zoneKey struct{}

// Location returns the timezone of the session Protect authenticated: the zone the browser reported at sign-in, or
// control.timezone. Panel times are shown and entered in it. Without a session it is UTC.
func Location(ctx context.Context) *time.Location {
	if loc, ok := ctx.Value(zoneKey{}).(*time.Location); ok {
		return loc
	}
	return time.UTC
}

// AdminFrom returns the administrator Protect authenticated for the request.
func AdminFrom(ctx context.Context) (store.ControlAdmin, bool) {
	a, ok := ctx.Value(ctxKey{}).(store.ControlAdmin)
	return a, ok
}

// remoteHost is the connection's peer address. fuserd's panel is served directly, never behind a proxy, so
// X-Forwarded-For is never read: a client could claim any address with it.
func remoteHost(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if host == "" {
		host = r.RemoteAddr
	}
	return host
}
func peerAddr(r *http.Request) (netip.Addr, bool) {
	a, err := netip.ParseAddr(remoteHost(r))
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap().WithZone(""), true
}

// clientKey groups requests for rate limiting: an IPv4 address, or the /64 of an IPv6 address, since one host or
// subscriber normally holds a whole /64 and could otherwise rotate through its addresses.
func clientKey(r *http.Request) string {
	a, ok := peerAddr(r)
	if !ok {
		return remoteHost(r)
	}
	if a.Is4() {
		return a.String()
	}
	p, _ := a.Prefix(64)
	return p.String()
}

func ValidateOrigin(origin string) error {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return errors.New("control.origin must be an exact origin without a path")
	}
	if net.ParseIP(u.Hostname()) != nil {
		return errors.New("control.origin must use a hostname; use localhost for local development")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) {
		return errors.New("control.origin requires HTTPS (HTTP localhost is allowed for development)")
	}
	return nil
}
func New(cfg config.ControlConfig, st *store.Store, logs *logx.Logs) (*Auth, error) {
	if err := ValidateOrigin(cfg.Origin); err != nil {
		return nil, err
	}
	u, _ := url.Parse(cfg.Origin)
	yes := true
	wa, err := webauthn.New(&webauthn.Config{RPID: u.Hostname(), RPDisplayName: "FUSER control", RPOrigins: []string{cfg.Origin}, AttestationPreference: protocol.PreferNoAttestation, AuthenticatorSelection: protocol.AuthenticatorSelection{ResidentKey: protocol.ResidentKeyRequirementRequired, RequireResidentKey: &yes, UserVerification: protocol.VerificationRequired}})
	if err != nil {
		return nil, err
	}
	allowed, err := cfg.Networks()
	if err != nil {
		return nil, err
	}
	zone, err := cfg.Location()
	if err != nil {
		return nil, err
	}
	a := &Auth{st: st, wa: wa, logs: logs, log: logs.For(logx.Control), origin: cfg.Origin, secure: u.Scheme == "https", allowed: allowed, zone: zone, zones: map[string]*time.Location{},
		sessionCookie: "fuser_control", ceremonyCookie: "fuser_ceremony", pending: map[string]ceremony{}, limits: map[string]rate{}}
	if a.secure {
		a.sessionCookie, a.ceremonyCookie = "__Host-fuser_control", "__Host-fuser_ceremony"
	}
	return a, nil
}

// Restrict refuses every control route to addresses outside control.allowed_networks, before any other handling,
// except published artwork, which FUSER downloads from anywhere.
func (a *Auth) Restrict(next http.Handler) http.Handler {
	if len(a.allowed) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method == "GET" || r.Method == "HEAD") && strings.HasPrefix(path.Clean(r.URL.Path), "/control/media/") {
			next.ServeHTTP(w, r)
			return
		}
		if addr, ok := peerAddr(r); ok {
			for _, p := range a.allowed {
				if p.Contains(addr) {
					next.ServeHTTP(w, r)
					return
				}
			}
		}
		Fail(w, 403, "the control panel is not available from this address")
	})
}

// Session returns the administrator whose session cookie r carries.
// Session returns the administrator whose session cookie r carries, and the session's timezone.
func (a *Auth) Session(r *http.Request) (store.ControlAdmin, *time.Location, error) {
	c, err := r.Cookie(a.sessionCookie)
	if err != nil {
		return store.ControlAdmin{}, nil, err
	}
	admin, zone, err := a.st.ControlSessionLookup(r.Context(), store.ControlHash(c.Value))
	if err != nil {
		return store.ControlAdmin{}, nil, err
	}
	return admin, a.location(zone), nil
}

// location loads a session's zone, falling back to control.timezone when it is empty or no longer loads.
func (a *Auth) location(name string) *time.Location {
	if name == "" {
		return a.zone
	}
	a.zoneMu.Lock()
	defer a.zoneMu.Unlock()
	if loc, ok := a.zones[name]; ok {
		return loc
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return a.zone
	}
	a.zones[name] = loc
	return loc
}

// browserZone is the IANA zone the sign-in page reported in X-Timezone, or "" (use control.timezone) when it is
// missing or not a zone this server can load. "Local" would be the server host's zone, not the browser's.
func browserZone(r *http.Request) string {
	name := strings.TrimSpace(r.Header.Get("X-Timezone"))
	if name == "" || len(name) > 64 || name == "Local" {
		return ""
	}
	if _, err := time.LoadLocation(name); err != nil {
		return ""
	}
	return name
}
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func Fail(w http.ResponseWriter, status int, msg string) {
	JSON(w, status, map[string]string{"error": msg})
}

// identifying reports whether an action's target identifies a player (a player ID or an Epic key). The CONTROL log
// carries its tag; the audit table, which only administrators read, keeps the value. Details never hold such values.
func identifying(action string) bool {
	return strings.HasPrefix(action, "users.") || strings.HasPrefix(action, "exemptions.")
}

// Audit records an action by the administrator Protect authenticated for r, in the audit table and the CONTROL log.
func (a *Auth) Audit(r *http.Request, action, target, detail string) {
	admin, _ := AdminFrom(r.Context())
	a.audit(r, admin, action, target, detail)
}
func (a *Auth) audit(r *http.Request, admin store.ControlAdmin, action, target, detail string) {
	ip := remoteHost(r)
	e := store.ControlAuditEntry{At: time.Now(), AdminID: admin.ID, AdminName: admin.Name, IP: ip, Action: action, Target: target, Detail: detail}
	if err := a.st.ControlAuditAdd(context.WithoutCancel(r.Context()), e); err != nil {
		a.log.Error("audit entry not stored", "action", action, "admin_id", admin.ID, "err", err)
	}
	if identifying(action) {
		target = a.logs.Tag(target)
	}
	a.log.Info("admin action", "action", action, "admin", admin.Name, "admin_id", admin.ID, "client", a.logs.Tag(ip), "target", target, "detail", detail)
}

// failed logs a rejected authentication attempt. It stays out of the audit table, which anyone on the internet could
// otherwise grow.
func (a *Auth) failed(r *http.Request, kind, reason string, err error) {
	args := []any{"ceremony", kind, "client", a.logs.Tag(remoteHost(r)), "reason", reason}
	if err != nil {
		args = append(args, "err", err)
	}
	a.log.Warn("authentication failed", args...)
}
func (a *Auth) cookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode, MaxAge: int(ttl.Seconds()), Expires: time.Now().Add(ttl)})
}
func (a *Auth) session(w http.ResponseWriter, r *http.Request, id string) error {
	v, err := store.ControlRandom(32)
	if err != nil {
		return err
	}
	ttl := 8 * time.Hour
	if err = a.st.ControlSession(r.Context(), store.ControlHash(v), id, time.Now().Add(ttl), browserZone(r)); err != nil {
		return err
	}
	a.cookie(w, a.sessionCookie, v, ttl)
	return nil
}
func (a *Auth) Origin(r *http.Request) bool { return r.Header.Get("Origin") == a.origin }
func (a *Auth) Allow(r *http.Request) bool {
	key := clientKey(r)
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, v := range a.limits {
		if now.Sub(v.at) > time.Minute {
			delete(a.limits, k)
		}
	}
	v, ok := a.limits[key]
	if !ok {
		if len(a.limits) >= maxClients {
			oldest := ""
			for k, x := range a.limits {
				if oldest == "" || x.at.Before(a.limits[oldest].at) {
					oldest = k
				}
			}
			delete(a.limits, oldest)
		}
		v.at = now
	}
	v.n++
	a.limits[key] = v
	if v.n == authAttempts+1 {
		a.log.Warn("authentication rate limited", "client", a.logs.Tag(key))
	}
	return v.n <= authAttempts
}
func (a *Auth) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /control/auth/enroll/begin", a.beginEnrollment)
	mux.HandleFunc("POST /control/auth/enroll/finish", func(w http.ResponseWriter, r *http.Request) { a.finish(w, r, true) })
	mux.HandleFunc("POST /control/auth/login/begin", a.beginLogin)
	mux.HandleFunc("POST /control/auth/login/finish", func(w http.ResponseWriter, r *http.Request) { a.finish(w, r, false) })
	mux.Handle("POST /control/auth/logout", a.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := r.Cookie(a.sessionCookie)
		if err := a.st.ControlLogout(r.Context(), store.ControlHash(c.Value)); err != nil {
			Fail(w, 500, "logout failed")
			return
		}
		a.Audit(r, "auth.logout", "", "")
		a.cookie(w, a.sessionCookie, "", -time.Second)
		JSON(w, 200, map[string]bool{"ok": true})
	})))
	mux.Handle("GET /control/auth/session", a.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := AdminFrom(r.Context())
		JSON(w, 200, map[string]string{"name": u.Name, "id": u.ID, "timezone": Location(r.Context()).String()})
	})))
	mux.Handle("GET /control/api/admins", a.Protect(http.HandlerFunc(a.listAdmins)))
	mux.Handle("POST /control/api/admins/tokens", a.Protect(http.HandlerFunc(a.createToken)))
	mux.Handle("DELETE /control/api/admins/tokens/{id}", a.Protect(http.HandlerFunc(a.cancelToken)))
	mux.Handle("DELETE /control/api/admins/{id}", a.Protect(http.HandlerFunc(a.revokeAdmin)))
}
func (a *Auth) Protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" && !a.Origin(r) {
			Fail(w, 403, "origin rejected")
			return
		}
		admin, zone, err := a.Session(r)
		if err != nil {
			Fail(w, 401, "sign in required")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(context.WithValue(r.Context(), ctxKey{}, admin), zoneKey{}, zone)))
	})
}
func (a *Auth) check(w http.ResponseWriter, r *http.Request) bool {
	if !a.Origin(r) {
		Fail(w, 403, "origin rejected")
		return false
	}
	if !a.Allow(r) {
		Fail(w, 429, "too many authentication attempts; try again later")
		return false
	}
	return true
}

// put stores a ceremony for r's client. A client keeps at most pendingPerClient ceremonies and the table at most
// maxPending; beyond either, the oldest is dropped (its finish then asks the user to start again).
func (a *Auth) put(w http.ResponseWriter, r *http.Request, c ceremony) error {
	v, err := store.ControlRandom(32)
	if err != nil {
		return err
	}
	c.client = clientKey(r)
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	mine := 0
	for k, x := range a.pending {
		if now.After(x.expires) {
			delete(a.pending, k)
		} else if x.client == c.client {
			mine++
		}
	}
	for ; mine >= pendingPerClient; mine-- {
		a.dropOldest(c.client)
	}
	if len(a.pending) >= maxPending {
		a.dropOldest("")
	}
	a.seq++
	c.seq, c.expires = a.seq, now.Add(3*time.Minute)
	a.pending[store.ControlHash(v)] = c
	a.cookie(w, a.ceremonyCookie, v, 3*time.Minute)
	return nil
}

// dropOldest removes the oldest pending ceremony, or the oldest of client when it is not empty. a.mu must be held.
func (a *Auth) dropOldest(client string) {
	oldest := ""
	for k, x := range a.pending {
		if (client == "" || x.client == client) && (oldest == "" || x.seq < a.pending[oldest].seq) {
			oldest = k
		}
	}
	delete(a.pending, oldest)
}
func (a *Auth) beginEnrollment(w http.ResponseWriter, r *http.Request) {
	if !a.check(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2048)
	var req struct {
		Token string `json:"token"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		Fail(w, 400, "invalid enrollment token")
		return
	}
	req.Token = strings.ToLower(strings.TrimSpace(req.Token))
	b, err := hex.DecodeString(req.Token)
	if err != nil || len(b) != 8 {
		a.failed(r, "enroll", "malformed token", nil)
		Fail(w, 400, "invalid or expired enrollment token")
		return
	}
	hash := store.ControlHash(req.Token)
	token, err := a.st.ControlTokenLookup(r.Context(), hash)
	if err != nil {
		a.failed(r, "enroll", "unknown or expired token", nil)
		Fail(w, 400, "invalid or expired enrollment token")
		return
	}
	id, err := store.ControlRandom(32)
	if err != nil {
		Fail(w, 500, "could not start enrollment")
		return
	}
	u := user{ControlAdmin: store.ControlAdmin{ID: id, Name: token.Name}}
	opts, data, err := a.wa.BeginRegistration(u)
	if err == nil {
		err = a.put(w, r, ceremony{data: *data, user: u, hash: hash, token: token, enroll: true})
	}
	if err != nil {
		a.log.Error("enrollment not started", "client", a.logs.Tag(remoteHost(r)), "err", err)
		Fail(w, 500, "could not start enrollment")
		return
	}
	JSON(w, 200, opts)
}
func (a *Auth) beginLogin(w http.ResponseWriter, r *http.Request) {
	if !a.check(w, r) {
		return
	}
	opts, data, err := a.wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err == nil {
		err = a.put(w, r, ceremony{data: *data})
	}
	if err != nil {
		a.log.Error("sign-in not started", "client", a.logs.Tag(remoteHost(r)), "err", err)
		Fail(w, 500, "could not start sign-in")
		return
	}
	JSON(w, 200, opts)
}
func (a *Auth) finish(w http.ResponseWriter, r *http.Request, enroll bool) {
	if !a.check(w, r) {
		return
	}
	kind := "login"
	if enroll {
		kind = "enroll"
	}
	cookie, err := r.Cookie(a.ceremonyCookie)
	if err != nil {
		a.failed(r, kind, "no ceremony", nil)
		Fail(w, 400, "ceremony expired; start again")
		return
	}
	a.mu.Lock()
	c, ok := a.pending[store.ControlHash(cookie.Value)]
	delete(a.pending, store.ControlHash(cookie.Value))
	a.mu.Unlock()
	a.cookie(w, a.ceremonyCookie, "", -time.Second)
	if !ok || c.enroll != enroll || time.Now().After(c.expires) {
		a.failed(r, kind, "unknown or expired ceremony", nil)
		Fail(w, 400, "ceremony expired; start again")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var cred *webauthn.Credential
	u := c.user
	if enroll {
		cred, err = a.wa.FinishRegistration(u, c.data, r)
	} else {
		cred, err = a.wa.FinishDiscoverableLogin(func(rawID, handle []byte) (webauthn.User, error) {
			admin, e := a.st.ControlAdmin(r.Context(), string(handle))
			if e != nil {
				return nil, e
			}
			var old webauthn.Credential
			if e = json.Unmarshal(admin.Credential, &old); e != nil {
				return nil, e
			}
			if subtle.ConstantTimeCompare(rawID, old.ID) != 1 {
				return nil, errors.New("unknown credential")
			}
			u = user{ControlAdmin: admin, cred: old}
			return u, nil
		}, c.data, r)
	}
	if err == nil && cred != nil && cred.Authenticator.CloneWarning {
		// A signature counter that went backwards: the credential may have been copied.
		a.log.Warn("passkey signature counter went backwards; possible cloned authenticator", "admin", u.Name, "admin_id", u.ID, "client", a.logs.Tag(remoteHost(r)))
		a.audit(r, u.ControlAdmin, "auth.clone_warning", u.ID, "sign-in refused: signature counter went backwards")
		Fail(w, 401, "WebAuthN verification failed")
		return
	}
	if err != nil || cred == nil {
		a.failed(r, kind, "verification failed", err)
		Fail(w, 401, "WebAuthN verification failed")
		return
	}
	data, err := json.Marshal(cred)
	if err == nil {
		if enroll {
			u.CredentialID = base64.RawURLEncoding.EncodeToString(cred.ID)
			u.Credential = data
			err = a.st.ControlEnroll(r.Context(), c.hash, u.ControlAdmin)
		} else {
			err = a.st.ControlCredential(r.Context(), u.ID, u.Credential, data)
		}
	}
	if err == nil {
		err = a.session(w, r, u.ID)
	}
	if err != nil {
		a.failed(r, kind, "credential not stored", err)
		Fail(w, 401, "credential changed, or enrollment token expired or already used")
		return
	}
	if enroll {
		a.audit(r, u.ControlAdmin, "auth.enroll", u.ID, "enrollment token "+c.token.ID)
	} else {
		a.audit(r, u.ControlAdmin, "auth.login", "", "")
	}
	JSON(w, 200, map[string]bool{"ok": true})
}

type adminView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Created   string `json:"created"`
	CreatedBy string `json:"created_by"`
	LastLogin string `json:"last_login"`
}
type tokenView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Created   string `json:"created"`
	CreatedBy string `json:"created_by"`
	Expires   string `json:"expires"`
}
type auditView struct {
	At      string `json:"at"`
	Admin   string `json:"admin"`
	AdminID string `json:"admin_id"`
	IP      string `json:"ip"`
	Action  string `json:"action"`
	Target  string `json:"target"`
	Detail  string `json:"detail"`
}

func local(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return ""
	}
	return store.ControlWall(t, loc)
}

// listAdmins serves the administrators page: administrators, unused enrollment tokens and the recent audit trail.
func (a *Auth) listAdmins(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	self, _ := AdminFrom(ctx)
	loc := Location(ctx)
	admins, err := a.st.ControlAdmins(ctx)
	if err != nil {
		Fail(w, 500, "could not load administrators")
		return
	}
	tokens, err := a.st.ControlTokens(ctx)
	if err != nil {
		Fail(w, 500, "could not load enrollment tokens")
		return
	}
	audit, err := a.st.ControlAudit(ctx, auditShown)
	if err != nil {
		Fail(w, 500, "could not load the audit trail")
		return
	}
	names := map[string]string{store.ControlCLI: "Command line"}
	for _, ad := range admins {
		names[ad.ID] = ad.Name
	}
	creator := func(id string) string {
		if id == "" {
			return ""
		}
		if n, ok := names[id]; ok {
			return n
		}
		return "Removed administrator"
	}
	out := struct {
		Self       string      `json:"self"`
		Admins     []adminView `json:"admins"`
		Tokens     []tokenView `json:"tokens"`
		Audit      []auditView `json:"audit"`
		MaxMinutes int         `json:"token_max_minutes"`
	}{Self: self.ID, Admins: []adminView{}, Tokens: []tokenView{}, Audit: []auditView{}, MaxMinutes: tokenMaxMinutes}
	for _, ad := range admins {
		out.Admins = append(out.Admins, adminView{ad.ID, ad.Name, local(ad.Created, loc), creator(ad.CreatedBy), local(ad.LastLogin, loc)})
	}
	for _, t := range tokens {
		out.Tokens = append(out.Tokens, tokenView{t.ID, t.Name, local(t.Created, loc), creator(t.CreatedBy), local(t.Expires, loc)})
	}
	for _, e := range audit {
		out.Audit = append(out.Audit, auditView{local(e.At, loc), e.AdminName, e.AdminID, e.IP, e.Action, e.Target, e.Detail})
	}
	JSON(w, 200, out)
}

// createToken issues an enrollment token. The response is the only time the token is shown.
func (a *Auth) createToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 2048)
	var req struct {
		Name    string `json:"name"`
		Minutes int    `json:"minutes"`
	}
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(&req) != nil {
		Fail(w, 400, "invalid request")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 80 {
		Fail(w, 400, "name required (80 characters maximum)")
		return
	}
	if req.Minutes < 1 || req.Minutes > tokenMaxMinutes {
		Fail(w, 400, fmt.Sprintf("expiry must be between 1 and %d minutes", tokenMaxMinutes))
		return
	}
	self, _ := AdminFrom(r.Context())
	ttl := time.Duration(req.Minutes) * time.Minute
	token, id, err := a.st.ControlToken(r.Context(), req.Name, ttl, self.ID)
	if err != nil {
		Fail(w, 500, "could not create the enrollment token")
		return
	}
	a.Audit(r, "admins.token_create", id, fmt.Sprintf("for %q, expires in %d min", req.Name, req.Minutes))
	JSON(w, 201, map[string]string{"id": id, "token": token, "expires": local(time.Now().Add(ttl), Location(r.Context()))})
}
func (a *Auth) cancelToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.st.ControlTokenRevoke(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			Fail(w, 404, "enrollment token not found; it may have been used or expired")
		} else {
			Fail(w, 500, "could not cancel the enrollment token")
		}
		return
	}
	a.Audit(r, "admins.token_cancel", id, "")
	JSON(w, 200, map[string]bool{"ok": true})
}

// revokeAdmin removes another administrator. Removing yourself is refused, so the panel can never remove the last
// administrator; "fuserd control revoke" can.
func (a *Auth) revokeAdmin(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	self, _ := AdminFrom(r.Context())
	if id == self.ID {
		Fail(w, 400, "you cannot remove yourself; ask another administrator, or use fuserd control revoke")
		return
	}
	target, err := a.st.ControlAdmin(r.Context(), id)
	if err == nil {
		err = a.st.ControlRevoke(r.Context(), id)
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			Fail(w, 404, "administrator not found")
		} else {
			Fail(w, 500, "could not remove the administrator")
		}
		return
	}
	a.Audit(r, "admins.revoke", id, target.Name)
	JSON(w, 200, map[string]bool{"ok": true})
}

// Headers applies to every control route, including unauthenticated sign-in pages. fuserd terminates TLS itself, so
// it sends HSTS when the request arrived over TLS and the origin is HTTPS (browsers ignore HSTS over plain HTTP).
// It covers this hostname only, without includeSubDomains or preload.
func (a *Auth) Headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.secure && r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", hstsHeader)
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "publickey-credentials-get=(self), publickey-credentials-create=(self)")
		next.ServeHTTP(w, r)
	})
}
