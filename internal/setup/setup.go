// Package setup turns install choices (where images come from, which database
// and Redis to use) into chart values and the Secrets that carry their
// credentials. Credentials never go into values.
package setup

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/NeuralTrust/ntctl/internal/secrets"
)

// Secret names and keys ntctl uses for external datastores. They are names,
// not credentials.
const (
	PostgresSecret = "ntctl-postgresql" // #nosec G101 -- a Secret name, not a credential
	PostgresKey    = "POSTGRES_PASSWORD"
	RedisSecret    = "ntctl-redis"
)

// Images says where the cluster pulls images from. An empty Registry means
// the NeuralTrust registry, authenticated with RegistryKey.
type Images struct {
	Registry    string
	RegistryKey []byte
	Username    string
	Password    string
	// NodeIdentity means the nodes pull with their own cloud identity (ECR,
	// Artifact Registry, ACR) and no pull Secret is used.
	NodeIdentity bool
}

// Postgres is an external PostgreSQL.
type Postgres struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string
	SSLMode  string
}

// Redis is an external Redis.
type Redis struct {
	Host     string
	Port     int
	Username string
	Password string
	TLS      bool
}

// Answers are the install choices. Nil datastores stay in the cluster.
type Answers struct {
	Images   Images
	Postgres *Postgres
	Redis    *Redis
}

// Values returns the chart values the answers stand for.
func (a Answers) Values() map[string]any {
	global := map[string]any{}
	if a.Images.Registry != "" {
		global["imageRegistry"] = a.Images.Registry
		global["imageRegistryScope"] = "all"
		if a.Images.NodeIdentity {
			global["imagePullSecrets"] = []any{"none"}
		}
	}
	if pg := a.Postgres; pg != nil {
		global["postgresql"] = map[string]any{
			"deploy":         false,
			"host":           pg.Host,
			"port":           pg.Port,
			"user":           pg.User,
			"database":       pg.Database,
			"sslMode":        pg.SSLMode,
			"passwordSecret": map[string]any{"name": PostgresSecret, "key": PostgresKey},
		}
	}
	if a.Redis != nil {
		global["redis"] = map[string]any{
			"deploy":         false,
			"existingSecret": map[string]any{"name": RedisSecret},
		}
	}
	if len(global) == 0 {
		return map[string]any{}
	}
	return map[string]any{"global": global}
}

// Secrets returns the Secrets the answers need. A pull Secret is included
// only when credentials were given.
func (a Answers) Secrets(ns string) ([]*corev1.Secret, error) {
	var out []*corev1.Secret
	switch {
	case a.Images.Registry == "" && len(a.Images.RegistryKey) > 0:
		s, err := secrets.Pull(ns, a.Images.RegistryKey)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	case a.Images.Registry != "" && a.Images.Username != "":
		s, err := secrets.DockerRegistry(ns, secrets.PullSecret, RegistryServer(a.Images.Registry), a.Images.Username, a.Images.Password)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if pg := a.Postgres; pg != nil {
		out = append(out, secrets.Opaque(ns, PostgresSecret, map[string]string{PostgresKey: pg.Password}))
	}
	if r := a.Redis; r != nil {
		out = append(out, secrets.Opaque(ns, RedisSecret, map[string]string{
			"REDIS_HOST":     r.Host,
			"REDIS_PORT":     strconv.Itoa(r.Port),
			"REDIS_USERNAME": r.Username,
			"REDIS_PASSWORD": r.Password,
			"REDIS_TLS":      strconv.FormatBool(r.TLS),
		}))
	}
	return out, nil
}

// Summary describes the answers without any credential.
func (a Answers) Summary() []string {
	var out []string
	switch {
	case a.Images.Registry == "":
		out = append(out, "Images:     NeuralTrust registry")
	case a.Images.NodeIdentity:
		out = append(out, "Images:     "+a.Images.Registry+" (nodes authenticate with their own identity)")
	case a.Images.Username != "":
		out = append(out, "Images:     "+a.Images.Registry+" (user "+a.Images.Username+")")
	default:
		out = append(out, "Images:     "+a.Images.Registry+" (existing pull Secret)")
	}
	if pg := a.Postgres; pg != nil {
		out = append(out, fmt.Sprintf("PostgreSQL: %s@%s:%d/%s (sslmode=%s)", pg.User, pg.Host, pg.Port, pg.Database, pg.SSLMode))
	} else {
		out = append(out, "PostgreSQL: in the cluster")
	}
	if r := a.Redis; r != nil {
		tls := ""
		if r.TLS {
			tls = " (TLS)"
		}
		out = append(out, fmt.Sprintf("Redis:      %s:%d%s", r.Host, r.Port, tls))
	} else {
		out = append(out, "Redis:      in the cluster")
	}
	return out
}

var sslModes = map[string]bool{"disable": true, "allow": true, "prefer": true, "require": true, "verify-ca": true, "verify-full": true}

// ParsePostgres reads postgres://user:password@host:port/database?sslmode=...
// The port defaults to 5432, the database to "neuraltrust" and sslmode to
// require, since an external database is reached over the network.
func ParsePostgres(raw string) (*Postgres, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return nil, errors.New("expected postgres://user:password@host:5432/database?sslmode=require")
	}
	pg := &Postgres{Host: u.Hostname(), Port: 5432, Database: strings.TrimPrefix(u.Path, "/"), SSLMode: "require"}
	if pg.Host == "" {
		return nil, errors.New("the connection string has no host")
	}
	if u.User == nil || u.User.Username() == "" {
		return nil, errors.New("the connection string has no user")
	}
	pg.User = u.User.Username()
	pg.Password, _ = u.User.Password()
	if pg.Password == "" {
		return nil, errors.New("the connection string has no password; IAM authentication is configured with -f values instead")
	}
	if p := u.Port(); p != "" {
		if pg.Port, err = strconv.Atoi(p); err != nil {
			return nil, fmt.Errorf("invalid port %q", p)
		}
	}
	if pg.Database == "" {
		pg.Database = "neuraltrust"
	}
	if m := u.Query().Get("sslmode"); m != "" {
		if !sslModes[m] {
			return nil, fmt.Errorf("unknown sslmode %q", m)
		}
		pg.SSLMode = m
	}
	return pg, nil
}

// ParseRedis reads redis://[user:password@]host:port, or rediss:// for TLS.
func ParseRedis(raw string) (*Redis, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "redis" && u.Scheme != "rediss") {
		return nil, errors.New("expected redis://:password@host:6379, or rediss:// for TLS")
	}
	r := &Redis{Host: u.Hostname(), Port: 6379, TLS: u.Scheme == "rediss"}
	if r.Host == "" {
		return nil, errors.New("the Redis URL has no host")
	}
	if u.User != nil {
		r.Username = u.User.Username()
		r.Password, _ = u.User.Password()
	}
	if p := u.Port(); p != "" {
		if r.Port, err = strconv.Atoi(p); err != nil {
			return nil, fmt.Errorf("invalid port %q", p)
		}
	}
	return r, nil
}

// RegistryServer is the host part of a registry address, which is what pull
// credentials are keyed by: registry.example.com/team/neuraltrust becomes
// registry.example.com.
func RegistryServer(addr string) string {
	addr = strings.TrimPrefix(strings.TrimPrefix(addr, "https://"), "http://")
	host, _, _ := strings.Cut(addr, "/")
	return host
}
