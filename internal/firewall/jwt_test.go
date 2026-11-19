package firewall

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const testSecret = "s3cr3t-value"

func sign(t *testing.T, header, claims map[string]any, key string) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	in := enc(header) + "." + enc(claims)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(in))
	return in + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func claims(edit func(map[string]any)) map[string]any {
	c := map[string]any{
		"typ": "firewall-client", "iss": "neuraltrust-console", "url": "https://firewall.example.com",
		"client_id": "fwc_abc", "client_secret": testSecret, "iat": 1700000000,
	}
	if edit != nil {
		edit(c)
	}
	return c
}

func TestValidate(t *testing.T) {
	t.Parallel()
	hs := map[string]any{"alg": "HS256", "typ": "JWT"}
	good := sign(t, hs, claims(nil), testSecret)
	cases := []struct {
		name  string
		token string
		want  string // "" = valid
	}{
		{"a console-issued token", good, ""},
		{"surrounding whitespace is ignored", "  " + good + "\n", ""},
		{"truncated", good[:len(good)/2], "parts"},
		{"not a JWT", "hello", "parts"},
		{"another algorithm", sign(t, map[string]any{"alg": "HS512"}, claims(nil), testSecret), "HS256"},
		{"wrong type", sign(t, hs, claims(func(c map[string]any) { c["typ"] = "access" }), testSecret), "typ"},
		{"wrong issuer", sign(t, hs, claims(func(c map[string]any) { c["iss"] = "someone" }), testSecret), "iss"},
		{"no client_id", sign(t, hs, claims(func(c map[string]any) { delete(c, "client_id") }), testSecret), "client_id"},
		{"no client_secret", sign(t, hs, claims(func(c map[string]any) { delete(c, "client_secret") }), testSecret), "client_secret"},
		{"plain http url", sign(t, hs, claims(func(c map[string]any) { c["url"] = "http://firewall.example.com" }), testSecret), "https"},
		{"relative url", sign(t, hs, claims(func(c map[string]any) { c["url"] = "/oauth" }), testSecret), "https"},
		{"signed with another key", sign(t, hs, claims(nil), "other"), "signature"},
		{"payload edited after signing", strings.Split(good, ".")[0] + "." + strings.Split(sign(t, hs, claims(func(c map[string]any) { c["client_id"] = "fwc_other" }), testSecret), ".")[1] + "." + strings.Split(good, ".")[2], "signature"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := Validate(c.token)
			if c.want == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Validate = %v, want ErrInvalid mentioning %q", err, c.want)
			}
			if strings.Contains(err.Error(), testSecret) || strings.Contains(err.Error(), c.token) {
				t.Fatalf("error leaks the token or its secret: %v", err)
			}
		})
	}
}

func TestClientID(t *testing.T) {
	t.Parallel()
	tok := sign(t, map[string]any{"alg": "HS256"}, claims(nil), testSecret)
	if got := ClientID(tok); got != "fwc_abc" {
		t.Errorf("ClientID = %q, want fwc_abc", got)
	}
	if got := ClientID("garbage"); got != "" {
		t.Errorf("ClientID(garbage) = %q, want empty", got)
	}
}
