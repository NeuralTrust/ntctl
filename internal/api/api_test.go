package api_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NeuralTrust/ntctl/internal/api"
	"github.com/NeuralTrust/ntctl/internal/apptest"
	"github.com/NeuralTrust/ntctl/internal/auth"
)

// tokens is a signed-in token source over the fake console.
type tokens struct {
	md        *auth.Metadata
	tok       *auth.Token
	refreshes int
}

func (t *tokens) Token(ctx context.Context) (string, error) { return t.tok.AccessToken, nil }

func (t *tokens) Refresh(ctx context.Context) (string, error) {
	t.refreshes++
	next, err := auth.Refresh(ctx, http.DefaultClient, t.md, t.tok.RefreshToken)
	if err != nil {
		return "", err
	}
	t.tok = next
	return next.AccessToken, nil
}

func signIn(t *testing.T, srv *apptest.Server) *tokens {
	t.Helper()
	ctx := context.Background()
	md, err := auth.Discover(ctx, http.DefaultClient, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	tok, err := auth.Login(ctx, md, auth.LoginOptions{HTTP: http.DefaultClient, Open: srv.Browser, Out: &out, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return &tokens{md: md, tok: tok}
}

func TestInstanceFlow(t *testing.T) {
	srv := apptest.New()
	defer srv.Close()
	c := api.New(srv.URL, http.DefaultClient, signIn(t, srv))
	ctx := context.Background()

	me, err := c.Me(ctx)
	if err != nil || me.Org == nil || me.Org.ID != "org-1" {
		t.Fatalf("me = %+v, %v; want the org chosen at sign-in", me, err)
	}
	orgs, err := c.Orgs(ctx)
	if err != nil || len(orgs) != 2 {
		t.Fatalf("orgs = %v, %v", orgs, err)
	}
	in, err := c.Create(ctx, api.TrustGuard, "org-1", "edge-1")
	if err != nil {
		t.Fatal(err)
	}
	list, err := c.Instances(ctx, api.TrustGuard, "org-1")
	if err != nil || len(list) != 1 || list[0].ID != in.ID {
		t.Fatalf("instances = %v, %v", list, err)
	}
	cfg, err := c.InstallConfig(ctx, api.TrustGuard, "org-1", in.ID, true)
	if err != nil || cfg.ConfigSyncToken == "" || cfg.EnrolmentToken == "" {
		t.Fatalf("install config = %v, %v", cfg, err)
	}
	if got := srv.Issued(); len(got) != 1 || !got[0].Rotate || got[0].Product != "trustguard" {
		t.Fatalf("issued = %+v, want one rotated trustguard config", got)
	}
	for _, s := range []string{fmt.Sprint(cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg), fmt.Sprint(*cfg)} {
		if strings.Contains(s, "CANARY") {
			t.Errorf("formatting an install config printed a token: %s", s)
		}
	}
	if err := c.Finalize(ctx, api.TrustGuard, "org-1", in.ID, "https://trustguard.example.com"); err != nil {
		t.Fatal(err)
	}
	if got := srv.Finalized(in.ID); got != "https://trustguard.example.com" {
		t.Fatalf("finalized endpoint = %q", got)
	}
}

func TestRefreshesOnceOn401(t *testing.T) {
	srv := apptest.New()
	defer srv.Close()
	ts := signIn(t, srv)
	c := api.New(srv.URL, http.DefaultClient, ts)
	srv.ExpireAccessTokens()
	if _, err := c.Orgs(context.Background()); err != nil {
		t.Fatalf("orgs after expiry: %v", err)
	}
	if ts.refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", ts.refreshes)
	}
}

func TestErrorsArePlain(t *testing.T) {
	srv := apptest.New()
	defer srv.Close()
	c := api.New(srv.URL, http.DefaultClient, signIn(t, srv))
	ctx := context.Background()

	_, err := c.Instances(ctx, api.TrustGate, "someone-elses-org")
	var e *api.Error
	if !errors.As(err, &e) || e.Status != http.StatusForbidden || !strings.Contains(err.Error(), "not a member") {
		t.Fatalf("err = %v, want 403 naming the reason", err)
	}
	_, err = c.InstallConfig(ctx, api.TrustGate, "org-1", "not-ours", false)
	if !errors.As(err, &e) || e.Status != http.StatusNotFound {
		t.Fatalf("err = %v, want 404 for an instance outside the org", err)
	}
	srv.Update(func(s *apptest.Server) { s.ReadOnly = true })
	if _, err = c.Create(ctx, api.TrustGate, "org-1", "x"); err == nil || !strings.Contains(err.Error(), "ADMIN") {
		t.Fatalf("err = %v, want the missing permission", err)
	}
}

// lossy drops the answer to the first create, after the console handled it.
type lossy struct{ dropped atomic.Bool }

func (l *lossy) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err == nil && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/gateways") && l.dropped.CompareAndSwap(false, true) {
		resp.Body.Close()
		return nil, errors.New("connection reset")
	}
	return resp, err
}

func TestCreateRetryIsIdempotent(t *testing.T) {
	srv := apptest.New()
	defer srv.Close()
	c := api.New(srv.URL, &http.Client{Transport: &lossy{}}, signIn(t, srv))
	in, err := c.Create(context.Background(), api.TrustGate, "org-1", "edge-1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if list := srv.Instances(api.TrustGate, "org-1"); len(list) != 1 || list[0].ID != in.ID {
		t.Fatalf("instances = %v, want exactly the one created", list)
	}
}
