// Package apptest is an in-memory NeuralTrust console for tests. It implements
// the contract ntctl relies on: OAuth discovery, authorization code + PKCE with
// a loopback redirect, rotating refresh tokens and revocation, and the CLI API
// under /api/cli/v1. Every secret it issues (codes, access, refresh,
// config-sync and enrolment tokens) contains "CANARY", so tests can check none
// leaks.
package apptest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/NeuralTrust/ntctl/internal/api"
)

// Server is the fake console.
type Server struct {
	*httptest.Server

	mu sync.Mutex
	// Email is the signed-in user; Orgs are the organizations they belong to;
	// Chosen is the one picked on the consent page.
	Email  string
	Orgs   []api.Org
	Chosen string
	// Deny makes the user refuse consent.
	Deny bool
	// AccessTTL is how long access tokens live.
	AccessTTL time.Duration
	// ReadOnly makes every write answer 403, as for a member without ADMIN.
	ReadOnly bool

	n         int
	codes     map[string]grant
	access    map[string]access
	refresh   map[string]string // live refresh token → session
	used      map[string]string // rotated-out refresh token → session
	revoked   map[string]bool   // session → revoked
	instances map[string][]api.Instance
	idem      map[string]api.Instance
	issued    []Issued
	finalized map[string]string
}

type grant struct{ challenge, redirect string }

type access struct {
	session string
	expiry  time.Time
}

// Issued is one install config the console handed out.
type Issued struct {
	Product, Org, ID string
	Rotate           bool
	Config           api.InstallConfig
}

// New starts a fake console with one user in two organizations.
func New() *Server {
	s := &Server{
		Email:     "ops@example.com",
		Orgs:      []api.Org{{ID: "org-1", Name: "Acme", Region: "eu", Role: "ADMIN"}, {ID: "org-2", Name: "Acme Labs", Region: "eu", Role: "ADMIN"}},
		Chosen:    "org-1",
		AccessTTL: time.Hour,
		codes:     map[string]grant{},
		access:    map[string]access{},
		refresh:   map[string]string{},
		used:      map[string]string{},
		revoked:   map[string]bool{},
		instances: map[string][]api.Instance{},
		idem:      map[string]api.Instance{},
		finalized: map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.discovery)
	mux.HandleFunc("GET /api/cli/oauth/authorize", s.authorize)
	mux.HandleFunc("POST /api/cli/oauth/token", s.token)
	mux.HandleFunc("POST /api/cli/oauth/revoke", s.revoke)
	mux.HandleFunc("GET /api/cli/v1/me", s.authed(s.me))
	mux.HandleFunc("GET /api/cli/v1/orgs", s.authed(s.orgs))
	mux.HandleFunc("GET /api/cli/v1/orgs/{org}/{kind}", s.authed(s.list))
	mux.HandleFunc("POST /api/cli/v1/orgs/{org}/{kind}", s.authed(s.create))
	mux.HandleFunc("POST /api/cli/v1/orgs/{org}/{kind}/{id}/install-config", s.authed(s.installConfig))
	mux.HandleFunc("POST /api/cli/v1/orgs/{org}/{kind}/{id}/finalize", s.authed(s.finalize))
	s.Server = httptest.NewServer(mux)
	return s
}

// Browser stands in for the user's browser: it opens the authorization URL
// and follows the console's redirect back to ntctl.
func (s *Server) Browser(authURL string) error {
	resp, err := http.Get(authURL) // #nosec G107 -- test helper following the URL ntctl printed
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Update changes the console's settings (Deny, ReadOnly, Chosen…) safely
// while it is serving.
func (s *Server) Update(f func(*Server)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

// Issued returns every install config handed out so far.
func (s *Server) Issued() []Issued {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Issued(nil), s.issued...)
}

// Finalized returns the endpoint recorded for an instance.
func (s *Server) Finalized(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finalized[id]
}

// ExpireAccessTokens makes every access token issued so far stale, as if an
// hour had passed.
func (s *Server) ExpireAccessTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, a := range s.access {
		a.expiry = time.Now().Add(-time.Minute)
		s.access[k] = a
	}
}

// AddInstance adds an existing private instance.
func (s *Server) AddInstance(product api.Product, org string, in api.Instance) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := kind(product) + "/" + org
	s.instances[k] = append(s.instances[k], in)
}

// Instances returns an organization's instances of a product.
func (s *Server) Instances(product api.Product, org string) []api.Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]api.Instance(nil), s.instances[kind(product)+"/"+org]...)
}

// Sessions returns how many sign-in sessions are still live.
func (s *Server) Sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	live := 0
	for _, sess := range s.refresh {
		if !s.revoked[sess] {
			live++
		}
	}
	return live
}

func kind(p api.Product) string {
	if p == api.TrustGuard {
		return "guards"
	}
	return "gateways"
}

// next returns a new secret value; id a new identifier, which may be shown.
func (s *Server) next(prefix string) string {
	s.n++
	return fmt.Sprintf("%s-CANARY-%d", prefix, s.n)
}

func (s *Server) id(prefix string) string {
	s.n++
	return fmt.Sprintf("%s-%d", prefix, s.n)
}

func (s *Server) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"issuer":                 s.URL,
		"authorization_endpoint": s.URL + "/api/cli/oauth/authorize",
		"token_endpoint":         s.URL + "/api/cli/oauth/token",
		"revocation_endpoint":    s.URL + "/api/cli/oauth/revoke",
	})
}

// authorize checks the request as the console must, then plays the user
// signing in and choosing an organization on the consent page.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || redirect.Scheme != "http" || redirect.Hostname() != "127.0.0.1" || redirect.Path != "/callback" {
		http.Error(w, "redirect_uri must be a loopback /callback", http.StatusBadRequest)
		return
	}
	if q.Get("client_id") != "ntctl" || q.Get("response_type") != "code" ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		http.Error(w, "invalid authorization request", http.StatusBadRequest)
		return
	}
	back := redirect.Query()
	back.Set("state", q.Get("state"))
	s.mu.Lock()
	if s.Deny {
		back.Set("error", "access_denied")
	} else {
		code := s.next("code")
		s.codes[code] = grant{challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri")}
		back.Set("code", code)
	}
	s.mu.Unlock()
	redirect.RawQuery = back.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound) // #nosec G710 -- checked above to be a loopback /callback
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || r.PostForm.Get("client_id") != "ntctl" {
		oauthError(w, "invalid_request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var session string
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		g, ok := s.codes[r.PostForm.Get("code")]
		delete(s.codes, r.PostForm.Get("code"))
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || g.redirect != r.PostForm.Get("redirect_uri") || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
			oauthError(w, "invalid_grant")
			return
		}
		session = s.id("session")
	case "refresh_token":
		rt := r.PostForm.Get("refresh_token")
		if sess, replay := s.used[rt]; replay {
			// A rotated-out token coming back means it leaked: end the session.
			s.revoked[sess] = true
			oauthError(w, "invalid_grant")
			return
		}
		sess, ok := s.refresh[rt]
		if !ok || s.revoked[sess] {
			oauthError(w, "invalid_grant")
			return
		}
		delete(s.refresh, rt)
		s.used[rt] = sess
		session = sess
	default:
		oauthError(w, "unsupported_grant_type")
		return
	}
	at, rt := s.next("access"), s.next("refresh")
	s.access[at] = access{session: session, expiry: time.Now().Add(s.AccessTTL)}
	s.refresh[rt] = session
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": at, "refresh_token": rt, "token_type": "Bearer", "expires_in": int(s.AccessTTL.Seconds()),
	})
}

func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.refresh[r.PostForm.Get("token")]; ok {
		s.revoked[sess] = true
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.mu.Lock()
		a, ok := s.access[token]
		valid := ok && time.Now().Before(a.expiry) && !s.revoked[a.session]
		s.mu.Unlock()
		if !valid {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "unauthorized", "message": "invalid or expired token"})
			return
		}
		h(w, r)
	}
}

func (s *Server) me(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	me := api.Me{ID: "user-1", Email: s.Email, Region: "eu"}
	for _, o := range s.Orgs {
		if o.ID == s.Chosen {
			me.Org = &o
		}
	}
	writeJSON(w, http.StatusOK, me)
}

func (s *Server) orgs(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusOK, s.Orgs)
}

// member checks the path's organization and kind, writing the error answer
// when they do not pass.
func (s *Server) member(w http.ResponseWriter, r *http.Request, write bool) (string, bool) {
	org, k := r.PathValue("org"), r.PathValue("kind")
	if k != "gateways" && k != "guards" {
		writeJSON(w, http.StatusNotFound, map[string]string{"code": "not_found"})
		return "", false
	}
	for _, o := range s.Orgs {
		if o.ID == org {
			if write && s.ReadOnly {
				writeJSON(w, http.StatusForbidden, map[string]string{"code": "forbidden", "message": "ADMIN on this product is required"})
				return "", false
			}
			return k + "/" + org, true
		}
	}
	writeJSON(w, http.StatusForbidden, map[string]string{"code": "forbidden", "message": "you are not a member of this organization"})
	return "", false
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.member(w, r, false)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, append([]api.Instance{}, s.instances[k]...))
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid", "message": "name is required"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.member(w, r, true)
	if !ok {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if in, seen := s.idem[key]; seen && key != "" {
		writeJSON(w, http.StatusOK, in)
		return
	}
	in := api.Instance{ID: s.id("inst"), Name: body.Name, Region: "eu", CreatedAt: time.Now().UTC()}
	s.instances[k] = append(s.instances[k], in)
	if key != "" {
		s.idem[key] = in
	}
	writeJSON(w, http.StatusCreated, in)
}

func (s *Server) installConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Rotate bool `json:"rotate"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.member(w, r, true)
	if !ok {
		return
	}
	if !s.owns(k, r.PathValue("id")) {
		writeJSON(w, http.StatusNotFound, map[string]string{"code": "not_found", "message": "no such instance in this organization"})
		return
	}
	cfg := api.InstallConfig{TenantID: r.PathValue("org"), ConfigSyncToken: s.next("configsync"), EnrolmentToken: s.next("enrolment")}
	product := api.TrustGate
	if r.PathValue("kind") == "guards" {
		product = api.TrustGuard
	}
	s.issued = append(s.issued, Issued{Product: string(product), Org: r.PathValue("org"), ID: r.PathValue("id"), Rotate: body.Rotate, Config: cfg})
	writeJSON(w, http.StatusOK, map[string]string{
		"tenantId": cfg.TenantID, "configSyncToken": cfg.ConfigSyncToken, "enrolmentToken": cfg.EnrolmentToken,
		"configSyncGrpcEndpoint": "agentgateway-configsync.example:443", "databridgeAddr": "databridge.example:443",
	})
}

func (s *Server) finalize(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Endpoint == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid", "message": "endpoint is required"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.member(w, r, true)
	if !ok {
		return
	}
	if !s.owns(k, r.PathValue("id")) {
		writeJSON(w, http.StatusNotFound, map[string]string{"code": "not_found"})
		return
	}
	s.finalized[r.PathValue("id")] = body.Endpoint
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) owns(k, id string) bool {
	for _, in := range s.instances[k] {
		if in.ID == id {
			return true
		}
	}
	return false
}

func oauthError(w http.ResponseWriter, code string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": code})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the test sees a failed write as a failed request
}
