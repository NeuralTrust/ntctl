// Package api is a client for the NeuralTrust console's CLI API, /api/cli/v1:
// organizations, private TrustGate and TrustGuard instances, and the tokens an
// install needs. Responses carrying tokens are never logged.
package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Product is a product with private instances.
type Product string

// The products the API manages instances of.
const (
	TrustGate  Product = "trustgate"
	TrustGuard Product = "trustguard"
)

// Name is the product's display name.
func (p Product) Name() string {
	if p == TrustGuard {
		return "TrustGuard"
	}
	return "TrustGate"
}

func (p Product) path() string {
	if p == TrustGuard {
		return "guards"
	}
	return "gateways"
}

// Tokens supplies the bearer token. Refresh is called once when the console
// answers 401.
type Tokens interface {
	Token(ctx context.Context) (string, error)
	Refresh(ctx context.Context) (string, error)
}

// Client calls the CLI API of one console.
type Client struct {
	base   string
	hc     *http.Client
	tokens Tokens
}

// New returns a client for the console at appURL.
func New(appURL string, hc *http.Client, t Tokens) *Client {
	return &Client{base: strings.TrimSuffix(appURL, "/") + "/api/cli/v1", hc: hc, tokens: t}
}

// Org is an organization the user belongs to.
type Org struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Region string `json:"region"`
	Role   string `json:"role"`
}

// Me is the signed-in user, and the organization chosen at sign-in.
type Me struct {
	ID     string `json:"id"`
	Email  string `json:"email"`
	Region string `json:"region"`
	Org    *Org   `json:"org,omitempty"`
}

// Instance is a private TrustGate or TrustGuard.
type Instance struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Region    string    `json:"region,omitempty"`
	Endpoint  string    `json:"endpoint,omitempty"`
	CreatedAt time.Time `json:"createdAt,omitzero"`
}

// InstallConfig are the credentials an install of one instance needs.
type InstallConfig struct {
	TenantID        string `json:"tenantId"`
	ConfigSyncToken string `json:"configSyncToken"`
	EnrolmentToken  string `json:"enrolmentToken"`
	// ConfigSyncEndpoint and DataBridgeAddr are where the console's control
	// plane listens, host:port: the tokens are valid only there.
	ConfigSyncEndpoint string `json:"configSyncGrpcEndpoint,omitempty"`
	DataBridgeAddr     string `json:"databridgeAddr,omitempty"`
}

// String keeps the tokens out of any formatted output.
func (c InstallConfig) String() string {
	return fmt.Sprintf("InstallConfig{tenant %s, tokens redacted}", c.TenantID)
}

// GoString keeps the tokens out of %#v.
func (c InstallConfig) GoString() string { return c.String() }

// Error is an error answer from the console.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	msg := e.Message
	if msg == "" {
		msg = e.Code
	}
	switch e.Status {
	case http.StatusUnauthorized:
		return "your login has expired or was revoked; run `ntctl login`"
	case http.StatusForbidden:
		if msg == "" {
			msg = "your role in this organization does not allow it"
		}
		return "not allowed: " + msg
	case http.StatusNotFound:
		if msg == "" {
			msg = "it does not exist, or belongs to another organization"
		}
		return "not found: " + msg
	}
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("the console answered %d: %s", e.Status, msg)
}

// Me returns the signed-in user.
func (c *Client) Me(ctx context.Context) (*Me, error) {
	var out Me
	return &out, c.do(ctx, http.MethodGet, "/me", nil, "", &out)
}

// Orgs lists the user's organizations.
func (c *Client) Orgs(ctx context.Context) ([]Org, error) {
	var out []Org
	return out, c.do(ctx, http.MethodGet, "/orgs", nil, "", &out)
}

// Instances lists an organization's private instances of a product.
func (c *Client) Instances(ctx context.Context, p Product, org string) ([]Instance, error) {
	var out []Instance
	return out, c.do(ctx, http.MethodGet, orgPath(org, p.path())+"?deployment=hybrid", nil, "", &out)
}

// Create creates a private instance. A retry after a lost answer reuses the
// idempotency key, so the console returns the same instance.
func (c *Client) Create(ctx context.Context, p Product, org, name string) (*Instance, error) {
	key, err := idempotencyKey()
	if err != nil {
		return nil, err
	}
	var out Instance
	return &out, c.do(ctx, http.MethodPost, orgPath(org, p.path()), map[string]string{"name": name}, key, &out)
}

// InstallConfig issues credentials for an instance. With rotate, the
// credentials issued before stop working.
func (c *Client) InstallConfig(ctx context.Context, p Product, org, id string, rotate bool) (*InstallConfig, error) {
	var out InstallConfig
	err := c.do(ctx, http.MethodPost, orgPath(org, p.path(), id, "install-config"), map[string]bool{"rotate": rotate}, "", &out)
	if err != nil {
		return nil, err
	}
	if out.ConfigSyncToken == "" || out.EnrolmentToken == "" {
		return nil, errors.New("the console returned an incomplete install config")
	}
	return &out, nil
}

// Finalize records where an installed instance is reachable.
func (c *Client) Finalize(ctx context.Context, p Product, org, id, endpoint string) error {
	return c.do(ctx, http.MethodPost, orgPath(org, p.path(), id, "finalize"), map[string]string{"endpoint": endpoint}, "", nil)
}

func orgPath(org string, parts ...string) string {
	var b strings.Builder
	b.WriteString("/orgs/")
	b.WriteString(url.PathEscape(org))
	for _, p := range parts {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(p))
	}
	return b.String()
}

// do sends one request. It refreshes the token once on 401 and, for a create
// carrying an idempotency key, retries once when the answer was lost.
func (c *Client) do(ctx context.Context, method, path string, body any, idemKey string, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	refreshed, retried := false, false
	for {
		token, err := c.tokens.Token(ctx)
		if err != nil {
			return err
		}
		resp, err := c.send(ctx, method, path, payload, idemKey, token)
		if err != nil {
			if idemKey != "" && !retried && ctx.Err() == nil {
				retried = true
				continue
			}
			return err
		}
		switch {
		case resp.StatusCode == http.StatusUnauthorized && !refreshed:
			drain(resp)
			refreshed = true
			if _, err := c.tokens.Refresh(ctx); err != nil {
				return err
			}
			continue
		case resp.StatusCode >= 502 && resp.StatusCode <= 504 && idemKey != "" && !retried:
			drain(resp)
			retried = true
			continue
		}
		return decode(resp, out)
	}
}

func (c *Client) send(ctx context.Context, method, path string, payload []byte, idemKey, token string) (*http.Response, error) {
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reaching the console: %w", err)
	}
	return resp, nil
}

func decode(resp *http.Response, out any) error {
	defer resp.Body.Close() //nolint:errcheck // read-only body
	body := io.LimitReader(resp.Body, 1<<20)
	if resp.StatusCode >= 300 {
		e := &Error{Status: resp.StatusCode}
		_ = json.NewDecoder(body).Decode(e) // the status alone still makes a useful error
		return e
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(body).Decode(out); err != nil {
		return fmt.Errorf("reading the console's answer: %w", err)
	}
	return nil
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)) // only to reuse the connection
	_ = resp.Body.Close()                                        // read-only body
}

func idempotencyKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating an idempotency key: %w", err)
	}
	return hex.EncodeToString(b), nil
}
