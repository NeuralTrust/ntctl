// Package firewall checks the FIREWALL_JWT a TrustGuard uses to reach the
// NeuralTrust-hosted firewall.
package firewall

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// The claims the console and NeuralTrust put in every FIREWALL_JWT.
const (
	wantType   = "firewall-client"
	wantIssuer = "neuraltrust-console"
)

// ErrInvalid is wrapped by every error Validate returns.
var ErrInvalid = errors.New("FIREWALL_JWT is not valid")

// Validate checks that token is a FIREWALL_JWT TrustGuard can use: an HS256
// JWT signed with its own client_secret, of type firewall-client, issued by
// the NeuralTrust console, naming an OAuth client and the firewall's https
// URL. It catches a truncated or wrong paste before anything changes in the
// cluster; whether the firewall still accepts the client is only known when
// TrustGuard exchanges it. Errors never include the token or its secret.
func Validate(token string) error {
	token = strings.TrimSpace(token)
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return invalid("it has %d dot-separated parts, not the 3 of a JWT (was it pasted whole?)", len(parts))
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if err := decode(parts[0], &header); err != nil {
		return invalid("its header is not base64url JSON")
	}
	if header.Alg != "HS256" {
		return invalid("it is signed with %q, not HS256", header.Alg)
	}
	var claims struct {
		Type         string `json:"typ"`
		Issuer       string `json:"iss"`
		URL          string `json:"url"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := decode(parts[1], &claims); err != nil {
		return invalid("its payload is not base64url JSON")
	}
	switch {
	case claims.Type != wantType:
		return invalid("typ is %q, not %q: this is not a hosted-firewall credential", claims.Type, wantType)
	case claims.Issuer != wantIssuer:
		return invalid("iss is %q, not %q", claims.Issuer, wantIssuer)
	case claims.ClientID == "":
		return invalid("it names no client_id")
	case claims.ClientSecret == "":
		return invalid("it carries no client_secret")
	}
	if u, err := url.Parse(claims.URL); err != nil || u.Scheme != "https" || u.Host == "" {
		return invalid("url %q is not an absolute https URL", claims.URL)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return invalid("its signature is not base64url")
	}
	mac := hmac.New(sha256.New, []byte(claims.ClientSecret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return invalid("its signature does not match its client_secret (edited or truncated)")
	}
	return nil
}

// ClientID returns the client_id a token names, or "" when it has none. It is
// not secret and identifies the credential in messages and on revocation.
func ClientID(token string) string {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return ""
	}
	var claims struct {
		ClientID string `json:"client_id"`
	}
	if decode(parts[1], &claims) != nil {
		return ""
	}
	return claims.ClientID
}

func decode(seg string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(seg, "="))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}
