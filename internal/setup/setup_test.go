package setup

import (
	"encoding/json"
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
	if r := byName[RedisSecret]; r == nil || string(r.Data["REDIS_HOST"]) != "cache" || string(r.Data["REDIS_TLS"]) != "true" {
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
