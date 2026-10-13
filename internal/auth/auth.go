// Package auth signs ntctl in to a NeuralTrust console with the OAuth 2.0
// authorization code flow, PKCE and a loopback redirect (RFC 8252), and keeps
// the tokens between runs. Tokens are never logged.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ClientID is ntctl's public OAuth client; it has no secret.
const ClientID = "ntctl"

// Scope is what ntctl asks for.
const Scope = "openid profile email cli"

// ErrExpired means the console no longer accepts the refresh token: the login
// expired, was revoked, or the user lost access.
var ErrExpired = errors.New("your login has expired or was revoked; run `ntctl login`")

// Metadata are the console's OAuth endpoints (RFC 8414).
type Metadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RevocationEndpoint    string `json:"revocation_endpoint"`
}

// Discover reads the console's OAuth endpoints.
func Discover(ctx context.Context, hc *http.Client, appURL string) (*Metadata, error) {
	if err := checkURL(appURL); err != nil {
		return nil, err
	}
	u := strings.TrimSuffix(appURL, "/") + "/.well-known/oauth-authorization-server"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reaching %s: %w", appURL, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s does not offer CLI sign-in (HTTP %d from %s)", appURL, resp.StatusCode, u)
	}
	var md Metadata
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&md); err != nil {
		return nil, fmt.Errorf("reading %s: %w", u, err)
	}
	for _, e := range []string{md.AuthorizationEndpoint, md.TokenEndpoint} {
		if err := checkURL(e); err != nil {
			return nil, fmt.Errorf("%s: %w", u, err)
		}
	}
	if md.RevocationEndpoint != "" {
		if err := checkURL(md.RevocationEndpoint); err != nil {
			return nil, fmt.Errorf("%s: %w", u, err)
		}
	}
	return &md, nil
}

// checkURL accepts https, and http only to this machine, for a local console.
func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid URL %q", raw)
	}
	if u.Scheme == "https" || (u.Scheme == "http" && isLoopback(u.Hostname())) {
		return nil
	}
	return fmt.Errorf("refusing %q: the console must be reached over https", raw)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Token is an access token and the refresh token that renews it.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry"`
}

// Valid reports whether the access token can still be used for a request.
func (t *Token) Valid() bool {
	return t.AccessToken != "" && time.Until(t.Expiry) > 30*time.Second
}

// LoginOptions configure Login.
type LoginOptions struct {
	HTTP *http.Client
	// Open opens a URL in the browser. The URL is printed either way, so a
	// failure to open is not an error.
	Open func(url string) error
	// Out receives the instructions for the user.
	Out io.Writer
	// Timeout bounds the wait for the browser to come back.
	Timeout time.Duration
}

// Login runs the browser sign-in and returns the tokens.
func Login(ctx context.Context, md *Metadata, o LoginOptions) (*Token, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listening for the browser's redirect: %w", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close() // not a TCP listener; nothing was served on it
		return nil, fmt.Errorf("unexpected listener address %v", ln.Addr())
	}
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", addr.Port)
	verifier, err := random(32)
	if err != nil {
		return nil, err
	}
	state, err := random(16)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {ClientID},
		"redirect_uri":          {redirect},
		"scope":                 {Scope},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	authURL := md.AuthorizationEndpoint + sep(md.AuthorizationEndpoint) + q.Encode()

	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query()
		// A request without our state is not the console's redirect: ignore
		// it rather than let it end the sign-in.
		if p.Get("state") != state {
			http.Error(w, "Unexpected request.", http.StatusBadRequest)
			return
		}
		res := result{code: p.Get("code")}
		switch {
		case p.Get("error") != "":
			res.err = fmt.Errorf("sign-in was not completed: %s", describe(p.Get("error"), p.Get("error_description")))
		case res.code == "":
			res.err = errors.New("sign-in was not completed: the console sent no authorization code")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if res.err != nil {
			fmt.Fprint(w, page("Sign-in was not completed", "Return to the terminal for details."))
		} else {
			fmt.Fprint(w, page("Signed in to ntctl", "You can close this window and return to the terminal."))
		}
		select {
		case done <- res:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln) //nolint:errcheck // returns ErrServerClosed on Shutdown
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx) //nolint:contextcheck // best effort, and must run after ctx is cancelled too
	}()

	fmt.Fprintf(o.Out, "Opening the browser to sign in. If it does not open, visit:\n\n  %s\n\n", authURL)
	if o.Open != nil {
		_ = o.Open(authURL) // the URL is printed above for when this fails
	}
	timeout := o.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var res result
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("no sign-in after %s; run `ntctl login` again", timeout)
	case res = <-done:
	}
	if res.err != nil {
		return nil, res.err
	}
	return tokenRequest(ctx, o.HTTP, md.TokenEndpoint, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {res.code},
		"redirect_uri":  {redirect},
		"client_id":     {ClientID},
		"code_verifier": {verifier},
	})
}

// Refresh exchanges a refresh token for new tokens. The console rotates the
// refresh token; if it sends none, the old one stays in use.
func Refresh(ctx context.Context, hc *http.Client, md *Metadata, refreshToken string) (*Token, error) {
	if refreshToken == "" {
		return nil, ErrExpired
	}
	t, err := tokenRequest(ctx, hc, md.TokenEndpoint, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {ClientID},
	})
	if err != nil {
		return nil, err
	}
	if t.RefreshToken == "" {
		t.RefreshToken = refreshToken
	}
	return t, nil
}

// Revoke ends the session a refresh token belongs to (RFC 7009).
func Revoke(ctx context.Context, hc *http.Client, md *Metadata, refreshToken string) error {
	if md.RevocationEndpoint == "" {
		return errors.New("the console offers no way to revoke the login")
	}
	form := url.Values{"token": {refreshToken}, "token_type_hint": {"refresh_token"}, "client_id": {ClientID}}
	resp, err := postForm(ctx, hc, md.RevocationEndpoint, form)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("revoking the login: HTTP %d", resp.StatusCode)
	}
	return nil
}

func tokenRequest(ctx context.Context, hc *http.Client, endpoint string, form url.Values) (*Token, error) {
	resp, err := postForm(ctx, hc, endpoint, form)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	var body struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int64  `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("reading the console's token response (HTTP %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || body.AccessToken == "" {
		if body.Error == "invalid_grant" {
			return nil, ErrExpired
		}
		return nil, fmt.Errorf("the console refused the sign-in: %s", describe(body.Error, body.ErrorDescription))
	}
	return &Token{
		AccessToken:  body.AccessToken,
		RefreshToken: body.RefreshToken,
		Expiry:       time.Now().Add(time.Duration(body.ExpiresIn) * time.Second),
	}, nil
}

func postForm(ctx context.Context, hc *http.Client, endpoint string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reaching the console: %w", err)
	}
	return resp, nil
}

func describe(code, desc string) string {
	switch {
	case code == "access_denied":
		return "access was denied in the browser"
	case desc != "":
		return desc
	case code != "":
		return code
	}
	return "unknown error"
}

func random(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating a random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func sep(u string) string {
	if strings.Contains(u, "?") {
		return "&"
	}
	return "?"
}

func page(title, body string) string {
	return `<!doctype html><html><head><meta charset="utf-8"><title>` + title +
		`</title></head><body style="font-family:system-ui,sans-serif;margin:4rem auto;max-width:32rem;text-align:center">` +
		`<h1 style="font-size:1.4rem">` + title + `</h1><p>` + body + `</p></body></html>`
}
