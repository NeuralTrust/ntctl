package redact

import (
	"strings"
	"testing"
)

func TestText(t *testing.T) {
	cases := []struct{ in, keep string }{
		{"connecting to postgres://nt:CANARY@db:5432/nt?sslmode=require", "postgres://nt:" + Mask + "@db:5432/nt"},
		{"rediss://:CANARY@cache:6380", "rediss://:" + Mask + "@cache"},
		{"auth eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJDQU5BUlkifQ.c2lnbmF0dXJlQ0FOQVJZ ok", "auth " + Mask + " ok"},
		{"Authorization: Bearer CANARYCANARY123", "Bearer " + Mask},
		{`level=info password=CANARY user=nt`, "password=" + Mask + " user=nt"},
		{`{"api_key": "CANARY", "region": "eu"}`, `"api_key": ` + Mask + `, "region": "eu"`},
		{"token: CANARY\nhost: db", "token: " + Mask + "\nhost: db"},
		{`"private_key": "-----BEGIN PRIVATE KEY-----\nCANARY\n-----END PRIVATE KEY-----\n"`, Mask},
		{"-----BEGIN RSA PRIVATE KEY-----\nCANARY\n-----END RSA PRIVATE KEY-----", Mask},
		{"secretName=my-secret secret_name: other", "secretName=my-secret secret_name: other"},
	}
	for _, c := range cases {
		got := Text(c.in)
		if strings.Contains(got, "CANARY") {
			t.Errorf("Text(%q) = %q still holds the secret", c.in, got)
		}
		if !strings.Contains(got, c.keep) {
			t.Errorf("Text(%q) = %q, want it to keep %q", c.in, got, c.keep)
		}
	}
}

func TestValue(t *testing.T) {
	doc := map[string]any{
		"global": map[string]any{
			"postgresql": map[string]any{"host": "db", "password": "CANARY", "passwordSecret": map[string]any{"name": "pg", "key": "password"}},
			"redis":      map[string]any{"existingSecret": map[string]any{"name": "cache"}},
		},
		"env": []any{
			map[string]any{"name": "DB_PASSWORD", "value": "CANARY"},
			map[string]any{"name": "DB_HOST", "value": "db"},
			map[string]any{"name": "API_TOKEN", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "t", "key": "TOKEN"}}},
		},
		"configSync": map[string]any{"token": "CANARY"},
		"url":        "https://user:CANARY@example.com/x",
	}
	Value(doc)
	s := toString(doc)
	if strings.Contains(s, "CANARY") {
		t.Fatalf("a secret survived: %s", s)
	}
	for _, keep := range []string{"db", "pg", "cache", "TOKEN", "DB_HOST"} {
		if !strings.Contains(s, keep) {
			t.Errorf("%q should be kept: %s", keep, s)
		}
	}
}

func toString(v any) string {
	var b strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, c := range t {
				b.WriteString(k + "=")
				walk(c)
				b.WriteString(" ")
			}
		case []any:
			for _, c := range t {
				walk(c)
			}
		case string:
			b.WriteString(t)
		}
	}
	walk(v)
	return b.String()
}

func TestTextKeepsYAMLStructure(t *testing.T) {
	in := "env:\n- name: TOKEN\n  valueFrom:\n    secretKeyRef:\n      name: agentgateway-config-sync\n      key: CONFIG_SYNC_TOKEN\n"
	if got := Text(in); got != in {
		t.Fatalf("Text changed YAML that holds no secret:\n%s", got)
	}
}
