package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/NeuralTrust/ntctl/internal/detect"
	"github.com/NeuralTrust/ntctl/internal/prompt"
	"github.com/NeuralTrust/ntctl/internal/values"
)

func TestAskInteractive(t *testing.T) {
	// domain, platform (Enter = detected), products (Enter = all),
	// images: own registry, address, username/password,
	// PostgreSQL: own + connection string, Redis: in the cluster.
	script := strings.Join([]string{
		"platform.example.com",
		"",
		"",
		"2",
		"registry.example.com/neuraltrust",
		"1",
		"robot",
		"token",
		"2",
		"postgres://nt:pw@db.example.com:5432/nt",
		"1",
	}, "\n") + "\n"
	var out bytes.Buffer
	in := prompt.Scripted(strings.NewReader(script), &out)
	o := opts{}
	o.kube.Namespace = "neuraltrust"
	cluster := &detect.Cluster{Platform: "aws"}

	a, err := ask(context.Background(), in, &out, fake.NewSimpleClientset(), cluster, &o, installFlags{}, map[string]any{})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if o.domain != "platform.example.com" || o.platform != "aws" || o.products != "trustgate,trustguard,dataPlane" {
		t.Errorf("domain=%q platform=%q products=%q", o.domain, o.platform, o.products)
	}
	if a.Images.Registry != "registry.example.com/neuraltrust" || a.Images.Username != "robot" || a.Images.Password != "token" {
		t.Errorf("images = %+v", a.Images)
	}
	if a.Postgres == nil || a.Postgres.Host != "db.example.com" {
		t.Errorf("postgres = %+v", a.Postgres)
	}
	if a.Redis != nil {
		t.Errorf("redis should stay in the cluster: %+v", a.Redis)
	}
	if strings.Contains(out.String(), "pw@") || strings.Contains(out.String(), "token\n") {
		t.Errorf("a credential was echoed:\n%s", out.String())
	}
}

func TestAskSkipsWhatFilesSet(t *testing.T) {
	files := map[string]any{"global": map[string]any{
		"domain": "f.example.com", "platform": "gcp", "products": map[string]any{"trustguard": true},
		"imageRegistry": "mirror.example.com", "postgresql": map[string]any{"deploy": true}, "redis": map[string]any{"deploy": true},
	}}
	var out bytes.Buffer
	// No input at all: any question would fail on EOF.
	in := prompt.Scripted(strings.NewReader(""), &out)
	o := opts{}
	a, err := ask(context.Background(), in, &out, fake.NewSimpleClientset(), &detect.Cluster{Platform: "gcp"}, &o, installFlags{}, files)
	if err != nil {
		t.Fatalf("asked a question the files answer: %v\n%s", err, out.String())
	}
	if len(a.Values()) != 0 {
		t.Errorf("answers added values on top of the files: %v", a.Values())
	}
}

func TestAskFromEnvironment(t *testing.T) {
	t.Setenv("NT_POSTGRES_URL", "postgres://nt:pw@db.example.com/nt")
	t.Setenv("NT_REDIS_URL", "rediss://:pw@cache.example.com:6380")
	var out bytes.Buffer
	in := prompt.New(strings.NewReader(""), &out, false)
	o := opts{domain: "d.example.com"}
	a, err := ask(context.Background(), in, &out, fake.NewSimpleClientset(), &detect.Cluster{Platform: "aws"}, &o,
		installFlags{imageRegistry: "123.dkr.ecr.eu-west-1.amazonaws.com/nt", nodeIdentity: true}, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Postgres == nil || a.Redis == nil || !a.Redis.TLS || !a.Images.NodeIdentity {
		t.Fatalf("answers from env = %+v", a)
	}
	if values.Get(a.Values(), "global", "imagePullSecrets") == nil {
		t.Error("node identity should set imagePullSecrets: [none]")
	}
}
