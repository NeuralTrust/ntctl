package setup

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/NeuralTrust/ntctl/internal/secrets"
	"github.com/NeuralTrust/ntctl/internal/values"
)

func TestParsePostgres(t *testing.T) {
	pg, err := ParsePostgres("postgres://nt:s3cr%40t@db.example.com:6543/platform?sslmode=verify-full")
	if err != nil {
		t.Fatal(err)
	}
	want := Postgres{Host: "db.example.com", Port: 6543, User: "nt", Password: "s3cr@t", Database: "platform", SSLMode: "verify-full"}
	if *pg != want {
		t.Fatalf("got %+v, want %+v", *pg, want)
	}
	pg, err = ParsePostgres("postgresql://nt:pw@db.example.com")
	if err != nil || pg.Port != 5432 || pg.Database != "neuraltrust" || pg.SSLMode != "require" {
		t.Fatalf("defaults: %+v %v", pg, err)
	}
	for _, bad := range []string{
		"mysql://u:p@h/db",
		"postgres://h/db",
		"postgres://u@h/db",
		"postgres://u:p@h:abc/db",
		"postgres://u:p@h/db?sslmode=maybe",
	} {
		if _, err := ParsePostgres(bad); err == nil {
			t.Errorf("accepted %q", bad)
		} else if strings.Contains(err.Error(), ":p@") {
			t.Errorf("error echoes the password: %v", err)
		}
	}
}

func TestParsePostgresAddress(t *testing.T) {
	pg, err := ParsePostgresAddress("postgres://nt@db.example.com/platform")
	if err != nil || pg.User != "nt" || pg.Password != "" || pg.Database != "platform" {
		t.Fatalf("got %+v %v", pg, err)
	}
	if _, err := ParsePostgresAddress("postgres://db.example.com/platform"); err == nil {
		t.Error("accepted a connection string with no user")
	}
}

func TestParseRedis(t *testing.T) {
	r, err := ParseRedis("rediss://default:pw@cache.example.com:6380")
	if err != nil {
		t.Fatal(err)
	}
	if *r != (Redis{Host: "cache.example.com", Port: 6380, Username: "default", Password: "pw", TLS: true}) {
		t.Fatalf("got %+v", *r)
	}
	r, err = ParseRedis("redis://cache.example.com")
	if err != nil || r.Port != 6379 || r.TLS || r.Password != "" {
		t.Fatalf("defaults: %+v %v", r, err)
	}
	if _, err := ParseRedis("http://cache"); err == nil {
		t.Error("accepted a non-redis URL")
	}
}

func TestRegistryServer(t *testing.T) {
	for in, want := range map[string]string{
		"registry.example.com/neuraltrust":       "registry.example.com",
		"https://registry.example.com:5000/a/b":  "registry.example.com:5000",
		"123.dkr.ecr.eu-west-1.amazonaws.com/nt": "123.dkr.ecr.eu-west-1.amazonaws.com",
	} {
		if got := RegistryServer(in); got != want {
			t.Errorf("RegistryServer(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDefaultsChangeNothing(t *testing.T) {
	var a Answers
	if v := a.Values(); len(v) != 0 {
		t.Errorf("default answers produced values %v", v)
	}
	if s, err := a.Secrets("nt"); err != nil || len(s) != 0 {
		t.Errorf("default answers produced Secrets %v %v", s, err)
	}
}

func TestExternalEverything(t *testing.T) {
	a := Answers{
		Images:   Images{Registry: "registry.example.com/nt", Username: "robot", Password: "tok"},
		Postgres: &Postgres{Host: "db", Port: 5432, User: "nt", Password: "pgpw", Database: "nt", SSLMode: "require"},
		Redis:    &Redis{Host: "cache", Port: 6379, Password: "rpw", TLS: true},
	}
	v := a.Values()
	checks := map[string]any{
		"global.imageRegistry":                  "registry.example.com/nt",
		"global.imageRegistryScope":             "all",
		"global.postgresql.deploy":              false,
		"global.postgresql.host":                "db",
		"global.postgresql.passwordSecret.name": PostgresSecret,
		"global.redis.deploy":                   false,
		"global.redis.existingSecret.name":      RedisSecret,
	}
	for path, want := range checks {
		if got := values.Get(v, strings.Split(path, ".")...); got != want {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}
	b, _ := json.Marshal(v)
	for _, secret := range []string{"tok", "pgpw", "rpw"} {
		if strings.Contains(string(b), `"`+secret+`"`) {
			t.Errorf("credential %q leaked into values: %s", secret, b)
		}
	}

	list, err := a.Secrets("nt")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*corev1.Secret{}
	for _, s := range list {
		byName[s.Name] = s
	}
	if pull := byName[secrets.PullSecret]; pull == nil || !strings.Contains(string(pull.Data[corev1.DockerConfigJsonKey]), `"registry.example.com"`) {
		t.Error("mirror pull Secret missing or not keyed by the registry host")
	}
	if pg := byName[PostgresSecret]; pg == nil || string(pg.Data[PostgresKey]) != "pgpw" {
		t.Error("PostgreSQL password Secret missing")
	}
	if got := values.Get(v, "data-plane-api", "dataPlane", "components", "api", "redis", "existingSecret", "key"); got != RedisURLKey {
		t.Errorf("data-plane-api should read its REDIS_URL from the Secret: %v", got)
	}
	if u := string(byName[RedisSecret].Data[RedisURLKey]); u != "rediss://:rpw@cache:6379/0" {
		t.Errorf("REDIS_URL = %q", u)
	}
	// The hybrid workloads load it whole and read the endpoint from it too.
	if r := byName[RedisSecret]; r == nil || string(r.Data["REDIS_HOST"]) != "cache" || string(r.Data["REDIS_TLS"]) != "true" || string(r.Data["REDIS_PASSWORD"]) != "rpw" {
		t.Error("Redis Secret missing or wrong")
	}
}

func TestNodeIdentityHasNoPullSecret(t *testing.T) {
	a := Answers{Images: Images{Registry: "123.dkr.ecr.eu-west-1.amazonaws.com/nt", NodeIdentity: true}}
	if got := values.Get(a.Values(), "global", "imagePullSecrets"); got == nil {
		t.Fatal("node identity must set imagePullSecrets: [none]")
	}
	if s, _ := a.Secrets("nt"); len(s) != 0 {
		t.Errorf("node identity created Secrets: %v", s)
	}
}

func TestSummaryDescribesDatastoresFromValues(t *testing.T) {
	global := func(g map[string]any) map[string]any { return map[string]any{"global": g} }
	cases := []struct {
		name      string
		vals      map[string]any
		pg, redis string
	}{
		{"chart runs both", map[string]any{}, "PostgreSQL: in the cluster", "Redis:      in the cluster"},
		{"own, with passwords", global(map[string]any{
			"postgresql": map[string]any{"deploy": false, "host": "db.example.com"},
			"redis":      map[string]any{"deploy": false, "host": "cache.example.com"},
		}), "PostgreSQL: db.example.com, from your values", "Redis:      cache.example.com, from your values"},
		{"own, with IAM", global(map[string]any{
			"postgresql": map[string]any{"deploy": false, "host": "10.0.0.2", "authMode": "iam"},
			"redis":      map[string]any{"deploy": false, "host": "10.0.0.3", "authMode": "iam", "cluster": true},
		}), "PostgreSQL: 10.0.0.2 (IAM), from your values", "Redis:      10.0.0.3 (IAM, cluster), from your values"},
	}
	for _, c := range cases {
		got := strings.Join(Answers{}.Summary(c.vals), "\n")
		if !strings.Contains(got, c.pg) || !strings.Contains(got, c.redis) {
			t.Errorf("%s:\n%s", c.name, got)
		}
	}
}

func TestEmailCredentialOnlyForTheChosenProvider(t *testing.T) {
	cases := map[string]struct {
		email  Email
		keys   []string
		values map[string]any
	}{
		"resend": {Email{Provider: "resend", From: "a@example.com", APIKey: "re_x"}, []string{ResendKey},
			map[string]any{"global.email.provider": "resend", "global.email.from": "a@example.com", "global.email.existingSecret.name": EmailSecret}},
		"smtp with login": {Email{Provider: "smtp", From: "a@example.com", SMTPHost: "mail.example.com", SMTPPort: 587, SMTPSecure: true, SMTPUser: "u", Password: "p"},
			[]string{SMTPPasswordKey},
			map[string]any{"global.email.smtp.host": "mail.example.com", "global.email.smtp.port": 587, "global.email.smtp.secure": true, "global.email.smtp.user": "u"}},
		"smtp relay": {Email{Provider: "smtp", From: "a@example.com", SMTPHost: "relay", SMTPPort: 25}, nil,
			map[string]any{"global.email.existingSecret": nil, "global.email.smtp.user": nil}},
		"ses with keys": {Email{Provider: "ses", From: "a@example.com", SESRegion: "eu-west-1", AccessKeyID: "AKIA", SecretAccessKey: "s"},
			[]string{SESKeyIDKey, SESSecretKey}, map[string]any{"global.email.ses.region": "eu-west-1"}},
		"ses with the IAM role": {Email{Provider: "ses", From: "a@example.com", SESRegion: "eu-west-1"}, nil,
			map[string]any{"global.email.existingSecret": nil}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			a := Answers{Email: &c.email}
			v := a.Values()
			for path, want := range c.values {
				if got := values.Get(v, strings.Split(path, ".")...); got != want {
					t.Errorf("%s = %v, want %v", path, got, want)
				}
			}
			b, _ := json.Marshal(v)
			for _, secret := range []string{"re_x", `"p"`, "AKIA", `"s"`} {
				if strings.Contains(string(b), secret) {
					t.Errorf("credential %s leaked into values: %s", secret, b)
				}
			}
			list, err := a.Secrets("nt")
			if err != nil {
				t.Fatal(err)
			}
			var keys []string
			for _, s := range list {
				for k := range s.Data {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			if strings.Join(keys, ",") != strings.Join(c.keys, ",") {
				t.Errorf("Secret keys = %v, want %v", keys, c.keys)
			}
		})
	}
}

func TestRedisURLEscapesTheCredentials(t *testing.T) {
	r := Redis{Host: "cache.example.com", Port: 6380, Username: "app", Password: "a/b+c=@:", TLS: true}
	got := r.URL()
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if pw, _ := u.User.Password(); pw != r.Password || u.User.Username() != "app" || u.Host != "cache.example.com:6380" || u.Path != "/0" || u.Scheme != "rediss" {
		t.Errorf("%s does not parse back: %+v", got, u)
	}
	if got := (Redis{Host: "cache", Port: 6379}).URL(); got != "redis://cache:6379/0" {
		t.Errorf("no password: %s", got)
	}
}
