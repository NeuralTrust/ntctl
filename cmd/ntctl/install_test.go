package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/NeuralTrust/ntctl/internal/detect"
	"github.com/NeuralTrust/ntctl/internal/gatewayapi"
	"github.com/NeuralTrust/ntctl/internal/prompt"
	"github.com/NeuralTrust/ntctl/internal/setup"
	"github.com/NeuralTrust/ntctl/internal/values"
)

func TestAskInteractive(t *testing.T) {
	// mode (Enter = hybrid), domain, platform (Enter = detected), products
	// (Enter = all), operational telemetry (Enter = no),
	// images: own registry, address, username/password,
	// PostgreSQL: own + connection string, Redis: in the cluster.
	script := strings.Join([]string{
		"",
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
	if o.domain != "platform.example.com" || o.platform != "aws" || o.products != "trustgate,trustguard,dataPlane" || o.mode != "" {
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
		"deploymentMode": "hybrid", "domain": "f.example.com", "platform": "gcp", "products": map[string]any{"trustguard": true},
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
		"deploymentMode": "hybrid", "domain": "h1.example.com", "platform": "azure", "products": map[string]any{"trustguard": true},
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
	t.Setenv("ONPREM_SUPERADMIN_EMAIL", "admin@example.com")
	t.Setenv("ONPREM_SUPERADMIN_PASSWORD", "correct-horse-battery")
	cases := []struct {
		name, mode, input string
		want              bool
		where             string
	}{
		{"hybrid by default, yes", "", "\ny\n", true, "to NeuralTrust, to help support diagnose your gateway"},
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
		files := telemetryFiles("hybrid")
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

func TestAskMode(t *testing.T) {
	files := map[string]any{"global": map[string]any{
		"domain": "m.example.com", "platform": "aws", "products": map[string]any{"trustguard": true},
		"imageRegistry": "mirror.example.com", "observability": map[string]any{"operationalTelemetry": false},
	}}
	run := func(o *opts, in *prompt.Prompter, out *bytes.Buffer) error {
		_, err := ask(context.Background(), in, out, fake.NewSimpleClientset(), &detect.Cluster{Platform: "aws"}, o, installFlags{}, files)
		return err
	}

	// Choosing external records it, asks no hybrid datastore question, and
	// asks for the console's first admin.
	var out bytes.Buffer
	o := opts{}
	if err := run(&o, prompt.Scripted(strings.NewReader("2\nadmin@example.com\ncorrect-horse-battery\ncorrect-horse-battery\n"), &out), &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if o.mode != "external" || !strings.Contains(out.String(), "Mode external: PostgreSQL and Redis run in the cluster") {
		t.Errorf("mode = %q\n%s", o.mode, out.String())
	}

	// --mode is not asked again; nor is a mode the files set.
	t.Setenv("ONPREM_SUPERADMIN_EMAIL", "admin@example.com")
	t.Setenv("ONPREM_SUPERADMIN_PASSWORD", "correct-horse-battery")
	for name, o := range map[string]opts{"flag": {mode: "SaaS"}} {
		out.Reset()
		if err := run(&o, prompt.Scripted(strings.NewReader(""), &out), &out); err != nil {
			t.Fatalf("%s: asked anyway: %v\n%s", name, err, out.String())
		}
		if o.mode != "saas" {
			t.Errorf("%s: mode = %q, want saas", name, o.mode)
		}
	}
	if _, err := (&opts{mode: "mars"}).effectiveValues(values.Inputs{}); err == nil || !strings.Contains(err.Error(), "--mode must be one of hybrid, external, saas") {
		t.Errorf("an unknown mode: %v", err)
	}
	vals, err := (&opts{mode: "external", products: "trustgate"}).effectiveValues(installDefaults("aws"))
	if err != nil {
		t.Fatal(err)
	}
	if values.DeploymentMode(vals) != "external" || values.Get(vals, "agentgateway") != nil {
		t.Errorf("--mode external values = %v; want the mode and no token references", vals)
	}
	if len(managedSecrets(vals)) != 1 {
		t.Errorf("an external install manages %v; want only the pull Secret", managedSecrets(vals))
	}
}

func TestAskAdmin(t *testing.T) {
	ctx := context.Background()
	external := func() *opts { return &opts{mode: "external"} }
	adminFiles := func() map[string]any {
		return map[string]any{"global": map[string]any{
			"domain": "a.example.com", "platform": "aws", "products": map[string]any{"trustguard": true},
			"imageRegistry": "mirror.example.com", "observability": map[string]any{"operationalTelemetry": false},
		}}
	}
	files := adminFiles()
	ask1 := func(in *prompt.Prompter, out *bytes.Buffer, cs *fake.Clientset, o *opts, f installFlags, files map[string]any) (setup.Answers, error) {
		return ask(ctx, in, out, cs, &detect.Cluster{Platform: "aws"}, o, f, files)
	}

	// Asked on a terminal: a short password and a typo are asked again.
	var out bytes.Buffer
	a, err := ask1(prompt.Scripted(strings.NewReader("nobody\nadmin@example.com\nshort\nshort\ncorrect-horse-battery\ncorrect-horse-batery\ncorrect-horse-battery\ncorrect-horse-battery\n"), &out),
		&out, fake.NewClientset(), external(), installFlags{}, files)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if a.Admin == nil || a.Admin.Email != "admin@example.com" || a.Admin.Password != "correct-horse-battery" {
		t.Fatalf("admin = %+v", a.Admin)
	}
	for _, want := range []string{"Enter an email address", "at least 12 characters", "The passwords differ"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "correct-horse") {
		t.Errorf("the password was echoed:\n%s", out.String())
	}
	sec, err := a.Secrets("nt")
	if err != nil {
		t.Fatal(err)
	}
	if last := sec[len(sec)-1]; last.Name != setup.AdminSecret || string(last.Data[setup.AdminPassKey]) != "correct-horse-battery" {
		t.Errorf("admin Secret = %+v", last)
	}
	if values.Get(a.Values(), "global", "superadmin", "existingSecret", "name") != setup.AdminSecret {
		t.Errorf("values do not name the admin Secret: %v", a.Values())
	}

	// Without a terminal and without the environment: refused, saying how.
	noTerminal := func() *prompt.Prompter { return prompt.New(ctx, strings.NewReader(""), &out, false) }
	if _, err := ask1(noTerminal(), &out, fake.NewClientset(), external(), installFlags{}, files); err == nil || !strings.Contains(err.Error(), "ONPREM_SUPERADMIN_EMAIL") {
		t.Errorf("no terminal, no env: %v", err)
	}
	// An existing Secret is kept and nothing is asked.
	cs := fake.NewClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: setup.AdminSecret, Namespace: ""}})
	if a, err = ask1(noTerminal(), &out, cs, external(), installFlags{}, files); err != nil || a.Admin == nil || !a.Admin.Existing {
		t.Errorf("existing Secret: %+v, %v", a.Admin, err)
	}
	// Values that name their own admin: nothing asked, nothing created.
	own := values.Merge(adminFiles(), map[string]any{"global": map[string]any{"superadmin": map[string]any{"existingSecret": map[string]any{"name": "mine"}}}})
	if a, err = ask1(noTerminal(), &out, fake.NewClientset(), external(), installFlags{}, own); err != nil || a.Admin != nil {
		t.Errorf("values with their own admin: %+v, %v", a.Admin, err)
	}
	// Print mode: no password asked.
	t.Setenv("ONPREM_SUPERADMIN_EMAIL", "admin@example.com")
	if a, err = ask1(noTerminal(), &out, fake.NewClientset(), external(), installFlags{secretsMode: "print"}, files); err != nil || a.Admin == nil || a.Admin.Password != "" {
		t.Errorf("print mode: %+v, %v", a.Admin, err)
	}
	// Hybrid never asks.
	if a, err = ask1(noTerminal(), &out, fake.NewClientset(), &opts{}, installFlags{}, values.Merge(adminFiles(), map[string]any{"global": map[string]any{"postgresql": map[string]any{"deploy": true}, "redis": map[string]any{"deploy": true}}})); err != nil || a.Admin != nil {
		t.Errorf("hybrid: %+v, %v", a.Admin, err)
	}
}

func TestInstalledMessage(t *testing.T) {
	mode := func(m string) map[string]any {
		return map[string]any{"global": map[string]any{"deploymentMode": m, "domain": "p.example.com"}}
	}
	cases := map[string][]string{
		"hybrid":   {"finish the setup in the NeuralTrust console"},
		"external": {"https://app.p.example.com"},
		"saas":     {"https://app.p.example.com", "export-controlplane-ca.sh", "global.controlPlane.domain: p.example.com"},
	}
	for m, wants := range cases {
		got := installedMessage(mode(m))
		for _, w := range wants {
			if !strings.Contains(got, w) {
				t.Errorf("%s: message lacks %q:\n%s", m, w, got)
			}
		}
	}
}
