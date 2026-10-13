// Package redact removes secrets from what ntctl shows or hands to support:
// values, Kubernetes objects and log lines. It errs on the side of removing
// too much. The rules are listed in Rules, which is published with every
// support bundle.
package redact

import (
	"regexp"
	"strings"
)

// Mask replaces a removed value.
const Mask = "[REDACTED]"

// Rules describes what is removed, for the bundle's README and the docs.
var Rules = []string{
	"Kubernetes Secrets: only their names, types and key names are collected, never a value.",
	"Any value whose key or variable name contains password, passwd, pwd, secret, token, apikey, api_key, api-key, private key, credential, dsn, connection string, jwt, cookie or signing (key names ending in \"name\", which name a Secret rather than hold one, are kept).",
	"Passwords in URLs: scheme://user:[REDACTED]@host.",
	"JSON Web Tokens (eyJ….….…).",
	"PEM private keys (-----BEGIN … PRIVATE KEY-----).",
	"Bearer and Basic authorization values.",
	"key=value and key: value pairs in text and logs whose key matches the names above.",
}

var (
	sensitiveKey = regexp.MustCompile(`(?i)(password|passwd|pwd|secret|token|api[_-]?key|private[_-]?key|credential|dsn|connection[_-]?string|jwt|cookie|signing)`)

	urlCreds  = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^:/@\s]*:)[^@\s/]+@`)
	jwt       = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]+`)
	pem       = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)
	pemEscape = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----(\\n|[^"'])*?-----END [A-Z ]*PRIVATE KEY-----`)
	authz     = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)
	pair      = regexp.MustCompile(`(?i)(["']?[\w.-]*(?:password|passwd|pwd|secret|token|api[_-]?key|private[_-]?key|credential|dsn|jwt|cookie|signing)[\w.-]*["']?[ \t]*[:=][ \t]*)("[^"\n]*"|'[^'\n]*'|[^\s,;}&]+)`)
)

// SensitiveKey reports whether a key or variable name suggests its value is a
// secret.
func SensitiveKey(k string) bool {
	if strings.HasSuffix(strings.ToLower(k), "name") {
		return false
	}
	return sensitiveKey.MatchString(k)
}

// Text removes secrets from free text such as a log line or a config file.
func Text(s string) string {
	s = pem.ReplaceAllString(s, Mask)
	s = pemEscape.ReplaceAllString(s, Mask)
	s = urlCreds.ReplaceAllString(s, "${1}"+Mask+"@")
	s = jwt.ReplaceAllString(s, Mask)
	s = authz.ReplaceAllString(s, "${1} "+Mask)
	s = pair.ReplaceAllStringFunc(s, func(m string) string {
		sub := pair.FindStringSubmatch(m)
		key := strings.Trim(strings.TrimRight(sub[1], ":= \t"), `"'`)
		if !SensitiveKey(key) {
			return m
		}
		return sub[1] + Mask
	})
	return s
}

// Value removes secrets from a decoded YAML or JSON document in place and
// returns it:
//   - a string under a sensitive key is masked;
//   - an {name: X, value: Y} pair, as in a container's env, masks Y when X is
//     sensitive;
//   - every other string goes through Text.
func Value(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if name, ok := t["name"].(string); ok && SensitiveKey(name) {
			if _, ok := t["value"].(string); ok {
				t["value"] = Mask
			}
		}
		for k, child := range t {
			if s, ok := child.(string); ok && SensitiveKey(k) && s != "" {
				t[k] = Mask
				continue
			}
			t[k] = Value(child)
		}
		return t
	case []any:
		for i := range t {
			t[i] = Value(t[i])
		}
		return t
	case string:
		return Text(t)
	}
	return v
}
