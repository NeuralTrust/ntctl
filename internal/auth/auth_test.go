package auth_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/NeuralTrust/ntctl/internal/apptest"
	"github.com/NeuralTrust/ntctl/internal/auth"
)

func login(t *testing.T, srv *apptest.Server, open func(string) error) (*auth.Metadata, *auth.Token, error) {
	t.Helper()
	ctx := context.Background()
	md, err := auth.Discover(ctx, http.DefaultClient, srv.URL)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	var out bytes.Buffer
	tok, err := auth.Login(ctx, md, auth.LoginOptions{HTTP: http.DefaultClient, Open: open, Out: &out, Timeout: 5 * time.Second})
	if strings.Contains(out.String(), "CANARY") {
		t.Errorf("login printed a token: %s", out.String())
	}
	return md, tok, err
}

func TestLoginWithPKCE(t *testing.T) {
	srv := apptest.New()
	defer srv.Close()
	_, tok, err := login(t, srv, srv.Browser)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if tok.AccessToken == "" || tok.RefreshToken == "" || !tok.Valid() {
		t.Fatalf("token = %+v, want access and refresh tokens", tok)
	}
}

func TestLoginIgnoresARequestWithoutTheState(t *testing.T) {
	srv := apptest.New()
	defer srv.Close()
	// Something else on the machine hits the callback first, with a code but
	// not our state. The sign-in must carry on and complete.
	open := func(authURL string) error {
		u, _ := url.Parse(authURL)
		redirect := u.Query().Get("redirect_uri")
		resp, err := http.Get(redirect + "?code=forged&state=wrong")
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("forged callback answered %d, want 400", resp.StatusCode)
		}
		return srv.Browser(authURL)
	}
	if _, _, err := login(t, srv, open); err != nil {
		t.Fatalf("login: %v", err)
	}
}

func TestLoginDenied(t *testing.T) {
	srv := apptest.New()
	defer srv.Close()
	srv.Update(func(s *apptest.Server) { s.Deny = true })
	_, _, err := login(t, srv, srv.Browser)
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("err = %v, want access denied", err)
	}
}

func TestLoginTimesOut(t *testing.T) {
	srv := apptest.New()
	defer srv.Close()
	md, err := auth.Discover(context.Background(), http.DefaultClient, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_, err = auth.Login(context.Background(), md, auth.LoginOptions{HTTP: http.DefaultClient, Out: &out, Timeout: 50 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "no sign-in") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if !strings.Contains(out.String(), "code_challenge_method=S256") {
		t.Errorf("the printed URL should carry the PKCE challenge: %s", out.String())
	}
}

func TestRefreshRotatesAndDetectsReplay(t *testing.T) {
	srv := apptest.New()
	defer srv.Close()
	md, tok, err := login(t, srv, srv.Browser)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	next, err := auth.Refresh(ctx, http.DefaultClient, md, tok.RefreshToken)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if next.RefreshToken == tok.RefreshToken {
		t.Fatal("the refresh token was not rotated")
	}
	// Reusing the rotated-out token is a replay: it fails and ends the session.
	if _, err := auth.Refresh(ctx, http.DefaultClient, md, tok.RefreshToken); !errors.Is(err, auth.ErrExpired) {
		t.Fatalf("replay err = %v, want ErrExpired", err)
	}
	if _, err := auth.Refresh(ctx, http.DefaultClient, md, next.RefreshToken); !errors.Is(err, auth.ErrExpired) {
		t.Fatalf("after a replay the session should be over, got %v", err)
	}
}

func TestRevoke(t *testing.T) {
	srv := apptest.New()
	defer srv.Close()
	md, tok, err := login(t, srv, srv.Browser)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := auth.Revoke(ctx, http.DefaultClient, md, tok.RefreshToken); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := auth.Refresh(ctx, http.DefaultClient, md, tok.RefreshToken); !errors.Is(err, auth.ErrExpired) {
		t.Fatalf("refresh after revoke: %v, want ErrExpired", err)
	}
}

func TestDiscoverRefusesPlainHTTP(t *testing.T) {
	if _, err := auth.Discover(context.Background(), http.DefaultClient, "http://app.example.com"); err == nil {
		t.Fatal("a console over plain http on another host must be refused")
	}
}

func TestStore(t *testing.T) {
	t.Setenv("NTCTL_CONFIG_DIR", t.TempDir())
	const app = "https://app.example.com"
	if _, err := auth.Load(app); !errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatalf("load before save: %v, want ErrNotLoggedIn", err)
	}
	c := &auth.Credentials{Token: auth.Token{AccessToken: "a", RefreshToken: "r", Expiry: time.Now().Add(time.Hour)}, Email: "ops@example.com"}
	if err := auth.Save(app, c); err != nil {
		t.Fatal(err)
	}
	got, err := auth.Load(app)
	if err != nil || got.RefreshToken != "r" || got.Email != "ops@example.com" {
		t.Fatalf("load = %+v, %v", got, err)
	}
	if err := auth.Delete(app); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Load(app); !errors.Is(err, auth.ErrNotLoggedIn) {
		t.Fatalf("load after delete: %v, want ErrNotLoggedIn", err)
	}
}
