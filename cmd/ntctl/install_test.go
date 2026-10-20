package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/NeuralTrust/ntctl/internal/detect"
	"github.com/NeuralTrust/ntctl/internal/gatewayapi"
	"github.com/NeuralTrust/ntctl/internal/prompt"
	"github.com/NeuralTrust/ntctl/internal/values"
)

func TestAskInteractive(t *testing.T) {
	// domain, platform (Enter = detected), products (Enter = all),
	// operational telemetry (Enter = no),
	// images: own registry, address, username/password,
	// PostgreSQL: own + connection string, Redis: in the cluster.
	script := strings.Join([]string{
		"platform.example.com",
		"",
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
	if o.telemetry.v == nil || *o.telemetry.v {
		t.Errorf("operational telemetry = %v, want an explicit off from the default answer", o.telemetry.String())
	}
	if strings.Contains(out.String(), "pw@") || strings.Contains(out.String(), "token\n") {
		t.Errorf("a credential was echoed:\n%s", out.String())
	}
}

func TestAskSkipsWhatFilesSet(t *testing.T) {
	files := map[string]any{"global": map[string]any{
		"domain": "f.example.com", "platform": "gcp", "products": map[string]any{"trustguard": true},
		"imageRegistry": "mirror.example.com", "postgresql": map[string]any{"deploy": true}, "redis": map[string]any{"deploy": true},
		"observability": map[string]any{"operationalTelemetry": false},
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
	in := prompt.New(context.Background(), strings.NewReader(""), &out, false)
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

func stgCluster() *detect.Cluster {
	return &detect.Cluster{Platform: "azure", GatewayAPI: true, GatewaysRead: true, Gateways: []gatewayapi.Gateway{{
		Namespace: "kgateway-system", Name: "internal", Class: "kgateway",
		Addresses: []gatewayapi.Address{{Type: "IPAddress", Value: "10.0.6.10"}},
	}}}
}

func TestAskOffersGateway(t *testing.T) {
	// No IngressClass: Enter takes the Gateway.
	files := map[string]any{"global": map[string]any{
		"domain": "h1.example.com", "platform": "azure", "products": map[string]any{"trustguard": true},
		"imageRegistry": "mirror.example.com", "postgresql": map[string]any{"deploy": true}, "redis": map[string]any{"deploy": true},
		"observability": map[string]any{"operationalTelemetry": false},
	}}
	var out bytes.Buffer
	o := opts{}
	if _, err := ask(context.Background(), prompt.Scripted(strings.NewReader("\n"), &out), &out, fake.NewSimpleClientset(), stgCluster(), &o, installFlags{}, files); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if o.gateway != "kgateway-system/internal" || !strings.Contains(out.String(), "HTTPRoutes on Gateway kgateway-system/internal (kgateway, 10.0.6.10)") {
		t.Errorf("gateway = %q\n%s", o.gateway, out.String())
	}

	// With an IngressClass, Ingress stays the default.
	cluster := stgCluster()
	cluster.IngressClasses = []string{"nginx"}
	o = opts{}
	if _, err := ask(context.Background(), prompt.Scripted(strings.NewReader("\n"), &out), &out, fake.NewSimpleClientset(), cluster, &o, installFlags{}, files); err != nil {
		t.Fatal(err)
	}
	if o.gateway != "" {
		t.Errorf("chose Gateway %q over an existing IngressClass by default", o.gateway)
	}

	// A provider in the files is not asked about.
	withProvider := values.Merge(map[string]any{}, files)
	withProvider = values.Merge(withProvider, map[string]any{"global": map[string]any{"ingress": map[string]any{"provider": "azure"}}})
	o = opts{}
	if _, err := ask(context.Background(), prompt.Scripted(strings.NewReader(""), &out), &out, fake.NewSimpleClientset(), stgCluster(), &o, installFlags{}, withProvider); err != nil {
		t.Fatalf("asked about exposure the files answer: %v", err)
	}
}

func TestGatewayFlag(t *testing.T) {
	o := opts{gateway: "kgateway-system/internal:https"}
	vals, err := o.effectiveValues(values.Inputs{Platform: "azure"})
	if err != nil {
		t.Fatal(err)
	}
	if got := exposure(vals); got != "HTTPRoutes on Gateway kgateway-system/internal:https" {
		t.Errorf("exposure = %q (values %v)", got, values.Get(vals, "global", "ingress"))
	}
	if _, err := (&opts{gateway: "internal"}).effectiveValues(values.Inputs{}); err == nil {
		t.Error("accepted --gateway without a namespace")
	}
	if exposure(values.Generate(values.Inputs{Platform: "aws"})) != "" {
		t.Error("an Ingress install described as HTTPRoutes")
	}
}

// telemetryFiles answer every install question except operational telemetry.
func telemetryFiles(mode string) map[string]any {
	global := map[string]any{
		"domain": "t.example.com", "platform": "aws", "products": map[string]any{"trustgate": true},
		"imageRegistry": "mirror.example.com", "postgresql": map[string]any{"deploy": true}, "redis": map[string]any{"deploy": true},
	}
	if mode != "" {
		global["deploymentMode"] = mode
	}
	return map[string]any{"global": global}
}

func TestAskOperationalTelemetry(t *testing.T) {
	cases := []struct {
		name, mode, input string
		want              bool
		where             string
	}{
		{"hybrid by default, yes", "", "y\n", true, "to NeuralTrust, to help support diagnose your gateway"},
		{"hybrid, Enter keeps no", "hybrid", "\n", false, "to NeuralTrust, to help support diagnose your gateway"},
		{"external, yes", "external", "yes\n", true, "to your own in-cluster collector"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			o := opts{}
			if _, err := ask(context.Background(), prompt.Scripted(strings.NewReader(c.input), &out), &out,
				fake.NewSimpleClientset(), &detect.Cluster{Platform: "aws"}, &o, installFlags{}, telemetryFiles(c.mode)); err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			if o.telemetry.v == nil || *o.telemetry.v != c.want {
				t.Errorf("operational telemetry = %q, want %v", o.telemetry.String(), c.want)
			}
			for _, s := range []string{c.where, "ntctl upgrade --operational-telemetry=true", "[y/N]"} {
				if !strings.Contains(out.String(), s) {
					t.Errorf("question does not say %q:\n%s", s, out.String())
				}
			}
			if c.mode == "external" && strings.Contains(out.String(), "to NeuralTrust") {
				t.Errorf("an external install was told its telemetry goes to NeuralTrust:\n%s", out.String())
			}
		})
	}
}

func TestAskTelemetryNotAskedWhenSettled(t *testing.T) {
	on := true
	for name, o := range map[string]opts{
		"flag": {telemetry: optionalBool{v: &on}},
		"none": {},
	} {
		files := telemetryFiles("")
		var out bytes.Buffer
		in := prompt.Scripted(strings.NewReader(""), &out)
		if name == "none" {
			// Without a terminal nothing is asked and nothing is chosen, so
			// the install default (off) applies.
			in = prompt.New(context.Background(), strings.NewReader(""), &out, false)
		}
		if _, err := ask(context.Background(), in, &out, fake.NewSimpleClientset(), &detect.Cluster{Platform: "aws"}, &o, installFlags{}, files); err != nil {
			t.Fatalf("%s: asked anyway: %v\n%s", name, err, out.String())
		}
		if strings.Contains(out.String(), "Operational telemetry") {
			t.Errorf("%s: asked about operational telemetry:\n%s", name, out.String())
		}
	}
}
