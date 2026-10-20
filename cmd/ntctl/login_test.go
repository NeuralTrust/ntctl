package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/NeuralTrust/ntctl/internal/api"
	"github.com/NeuralTrust/ntctl/internal/apptest"
	"github.com/NeuralTrust/ntctl/internal/prompt"
	"github.com/NeuralTrust/ntctl/internal/secrets"
	"github.com/NeuralTrust/ntctl/internal/values"
)

// console starts a fake console and points ntctl's config and browser at it.
func console(t *testing.T) *apptest.Server {
	t.Helper()
	srv := apptest.New()
	t.Cleanup(srv.Close)
	t.Setenv("NTCTL_CONFIG_DIR", t.TempDir())
	prev := openBrowser
	openBrowser = srv.Browser
	t.Cleanup(func() { openBrowser = prev })
	return srv
}

// ntctl runs a command and fails the test unless it exits with want.
func ntctl(t *testing.T, want int, args ...string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := run(context.Background(), args, strings.NewReader(""), &out, &errOut); code != want {
		t.Fatalf("ntctl %s: exit %d, want %d\n%s%s", strings.Join(args, " "), code, want, out.String(), errOut.String())
	}
	s := out.String() + errOut.String()
	if strings.Contains(s, "CANARY") {
		t.Fatalf("ntctl %s printed a token:\n%s", strings.Join(args, " "), s)
	}
	return s
}

func TestConsoleCommands(t *testing.T) {
	srv := console(t)

	out := ntctl(t, 0, "login", "--app-url", srv.URL)
	if !strings.Contains(out, "Signed in to "+srv.URL+" as ops@example.com") || !strings.Contains(out, "Organization: Acme (org-1)") {
		t.Fatalf("login output:\n%s", out)
	}
	if out := ntctl(t, 0, "orgs", "list"); !strings.Contains(out, "*  org-1") || !strings.Contains(out, "org-2") {
		t.Fatalf("orgs list:\n%s", out)
	}
	ntctl(t, 0, "config", "set", "org", "Acme Labs")
	if out := ntctl(t, 0, "auth", "status"); !strings.Contains(out, "Acme Labs (org-2)") {
		t.Fatalf("auth status after switching org:\n%s", out)
	}
	ntctl(t, 1, "config", "set", "org", "not-mine")

	out = ntctl(t, 0, "trustgate", "create", "edge-1")
	if !strings.Contains(out, `Created private TrustGate "edge-1"`) {
		t.Fatalf("create:\n%s", out)
	}
	if got := srv.Instances(api.TrustGate, "org-2"); len(got) != 1 {
		t.Fatalf("the instance should be in the selected org, got %v", got)
	}
	if out = ntctl(t, 1, "trustgate", "create", "EDGE-1"); !strings.Contains(out, "already exists") || !strings.Contains(out, "--trustgate-instance") {
		t.Fatalf("a taken name should be refused:\n%s", out)
	}
	if got := srv.Instances(api.TrustGate, "org-2"); len(got) != 1 {
		t.Fatalf("a refused create made an instance: %v", got)
	}
	if out := ntctl(t, 0, "trustgate", "list", "--org", "org-2"); !strings.Contains(out, "edge-1") {
		t.Fatalf("list:\n%s", out)
	}
	if out := ntctl(t, 0, "trustguard", "list"); !strings.Contains(out, "No private TrustGuard") {
		t.Fatalf("empty list:\n%s", out)
	}

	// An expired access token is renewed without the user noticing.
	srv.ExpireAccessTokens()
	ntctl(t, 0, "orgs", "list")

	// The saved files are the user's alone and hold no plaintext secrets
	// beyond the session tokens.
	dir := os.Getenv("NTCTL_CONFIG_DIR")
	for _, f := range []string{"config.json", "credentials.json"} {
		st, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", f, st.Mode().Perm())
		}
	}

	if out := ntctl(t, 0, "logout"); !strings.Contains(out, "Signed out") {
		t.Fatalf("logout:\n%s", out)
	}
	if srv.Sessions() != 0 {
		t.Fatal("logout should end the session in the console")
	}
	if out := ntctl(t, 1, "orgs", "list"); !strings.Contains(out, "not signed in") {
		t.Fatalf("after logout:\n%s", out)
	}
}

func TestLoginRefusesUnknownRegion(t *testing.T) {
	console(t)
	if out := ntctl(t, 1, "login", "--region", "mars"); !strings.Contains(out, "unknown region") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestLoginValidatesTheRegionWithAnAppURL(t *testing.T) {
	srv := console(t)
	if out := ntctl(t, 1, "login", "--app-url", srv.URL, "--region", "mars"); !strings.Contains(out, "unknown region") {
		t.Fatalf("output:\n%s", out)
	}
}

// TestRegionFollowsTheConsole: signed in, the region is the console's, so
// changing it alone would pair one region's data plane with another's tokens.
func TestRegionFollowsTheConsole(t *testing.T) {
	srv := console(t)
	ntctl(t, 0, "login", "--app-url", srv.URL)
	if out := ntctl(t, 1, "config", "set", "region", "us"); !strings.Contains(out, "ntctl login --region us") {
		t.Fatalf("output:\n%s", out)
	}
	ntctl(t, 0, "logout")
	if out := ntctl(t, 0, "config", "set", "region", "us"); !strings.Contains(out, "Region set to us") {
		t.Fatalf("signed out, the region is the next login's:\n%s", out)
	}
}

func TestFindOrg(t *testing.T) {
	orgs := []api.Org{{ID: "o1", Name: "Acme"}, {ID: "o2", Name: "acme"}, {ID: "o3", Name: "Labs"}}
	if o, err := findOrg(orgs, "labs"); err != nil || o.ID != "o3" {
		t.Errorf("by name: %v, %v", o, err)
	}
	if o, err := findOrg(orgs, "o2"); err != nil || o.ID != "o2" {
		t.Errorf("by ID: %v, %v", o, err)
	}
	if _, err := findOrg(orgs, "ACME"); err == nil || !strings.Contains(err.Error(), "o1, o2") {
		t.Errorf("a shared name: %v", err)
	}
	if _, err := findOrg(orgs, "nope"); err == nil {
		t.Error("an unknown organization")
	}
}

func signedIn(t *testing.T) (*apptest.Server, *session) {
	t.Helper()
	srv := console(t)
	ntctl(t, 0, "login", "--app-url", srv.URL)
	s, err := openSession()
	if err != nil {
		t.Fatal(err)
	}
	return srv, s
}

func gateSpecs() map[api.Product][]secrets.Spec {
	return map[api.Product][]secrets.Spec{api.TrustGate: secrets.TokenSpecs([]string{"trustgate"})}
}

func TestChooseInstances(t *testing.T) {
	srv, s := signedIn(t)
	srv.AddInstance(api.TrustGate, "org-1", api.Instance{ID: "gw-1", Name: "prod"})
	ctx := context.Background()
	var out bytes.Buffer
	noTerminal := func() *prompt.Prompter { return prompt.New(context.Background(), strings.NewReader(""), &out, false) }
	choose := func(in *prompt.Prompter, pick map[api.Product]string, def string) ([]*consoleInstance, error) {
		return chooseInstances(ctx, in, &out, s, "org-1", "Acme", gateSpecs(), pick, def)
	}

	// Non-interactive, nothing named: a new instance named after the domain.
	plan, err := choose(noTerminal(), nil, "platform.example.com")
	if err != nil || len(plan) != 1 || plan[0].inst != nil || plan[0].newName != "platform.example.com" {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	// Non-interactive, an instance already has the default name: it is
	// reused, so running an install again never adds a second one.
	plan, err = choose(noTerminal(), nil, "PROD")
	if err != nil || plan[0].inst == nil || plan[0].inst.ID != "gw-1" || plan[0].newName != "" {
		t.Fatalf("default name taken: %+v, %v", plan, err)
	}
	// Named by flag, by name or by ID.
	for _, key := range []string{"prod", "gw-1"} {
		plan, err = choose(noTerminal(), map[api.Product]string{api.TrustGate: key}, "x")
		if err != nil || plan[0].inst == nil || plan[0].inst.ID != "gw-1" {
			t.Fatalf("--trustgate-instance %s: %+v, %v", key, plan, err)
		}
	}
	if _, err = choose(noTerminal(), map[api.Product]string{api.TrustGate: "nope"}, "x"); err == nil || !strings.Contains(err.Error(), "in organization Acme") {
		t.Fatalf("an unknown instance name: err = %v, want it to name the organization", err)
	}
	// Interactive: the existing one is offered; Enter creates a new one.
	plan, err = choose(prompt.Scripted(strings.NewReader("1\n"), &out), nil, "x")
	if err != nil || plan[0].inst == nil || plan[0].inst.ID != "gw-1" {
		t.Fatalf("choosing the existing one: %+v, %v", plan, err)
	}
	plan, err = choose(prompt.Scripted(strings.NewReader("\nedge\n"), &out), nil, "x")
	if err != nil || plan[0].inst != nil || plan[0].newName != "edge" {
		t.Fatalf("creating a new one: %+v, %v", plan, err)
	}
	// Interactive: Enter takes the instance named like the default.
	plan, err = choose(prompt.Scripted(strings.NewReader("\n"), &out), nil, "prod")
	if err != nil || plan[0].inst == nil || plan[0].inst.ID != "gw-1" {
		t.Fatalf("the default should be the instance named like the install: %+v, %v", plan, err)
	}
	// Interactive: typing a taken name means that instance.
	plan, err = choose(prompt.Scripted(strings.NewReader("2\nprod\n"), &out), nil, "x")
	if err != nil || plan[0].inst == nil || plan[0].inst.ID != "gw-1" {
		t.Fatalf("a taken name should reuse the instance: %+v, %v", plan, err)
	}
	if len(srv.Instances(api.TrustGate, "org-1")) != 1 {
		t.Fatal("choosing must not create anything")
	}

	// Two instances share a name: refused, listing both IDs.
	srv.AddInstance(api.TrustGate, "org-1", api.Instance{ID: "gw-2", Name: "prod"})
	for _, pick := range []map[api.Product]string{{api.TrustGate: "prod"}, nil} {
		if _, err = choose(noTerminal(), pick, "prod"); err == nil || !strings.Contains(err.Error(), "gw-1, gw-2") {
			t.Fatalf("an ambiguous name: err = %v, want both IDs", err)
		}
	}
	// An ID always wins.
	if plan, err = choose(noTerminal(), map[api.Product]string{api.TrustGate: "gw-2"}, "x"); err != nil || plan[0].inst.ID != "gw-2" {
		t.Fatalf("by ID: %+v, %v", plan, err)
	}
}

func TestPointAtConsole(t *testing.T) {
	gate := gateOnly()
	us := values.Merge(gateOnly(), map[string]any{"global": map[string]any{"saasRegion": "us"}})
	pinned := values.Merge(gateOnly(), values.ControlPlaneValues("cp.example.com"))
	external := values.Merge(gateOnly(), map[string]any{"global": map[string]any{"deploymentMode": "external"}})
	cases := []struct {
		name   string
		vals   map[string]any
		domain string
		fresh  bool
		want   string // the domain set, "" for none
		err    string
	}{
		{name: "region console", vals: gate, domain: "neuraltrust.ai", fresh: true},
		{name: "us console", vals: us, domain: "us.neuraltrust.ai", fresh: true},
		{name: "unknown console", vals: gate, domain: "", fresh: true},
		{name: "dev console, new install", vals: gate, domain: "cp.example.com", fresh: true, want: "cp.example.com"},
		{name: "dev console, existing release", vals: gate, domain: "cp.example.com", err: "release's data plane dials the control plane at neuraltrust.ai"},
		{name: "values name another one", vals: pinned, domain: "other.example.com", fresh: true, err: "global.controlPlane.domain"},
		{name: "values name the same one", vals: pinned, domain: "cp.example.com"},
		{name: "external dials nothing", vals: external, domain: "cp.example.com", fresh: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := pointAtConsole(c.vals, c.domain, c.fresh)
			switch {
			case c.err != "":
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want %q", err, c.err)
				}
				return
			case err != nil:
				t.Fatal(err)
			}
			d, _ := values.Get(got, "global", "controlPlane", "domain").(string)
			if d != c.want {
				t.Fatalf("set %q, want %q", d, c.want)
			}
		})
	}
}

func TestIssuedControlPlane(t *testing.T) {
	srv, s := signedIn(t)
	srv.ControlPlane = "dev.example.com"
	ctx := context.Background()
	var out bytes.Buffer
	plan := &consolePlan{org: "org-1", instances: []*consoleInstance{
		{product: api.TrustGate, specs: secrets.TokenSpecs([]string{"trustgate"}), newName: "a"},
		{product: api.TrustGuard, specs: secrets.TokenSpecs([]string{"trustguard"}), newName: "b"},
	}}
	if d, err := plan.issuedControlPlane(); err != nil || d != "" {
		t.Fatalf("before issue: %q, %v", d, err)
	}
	if _, err := plan.issue(ctx, &out, s, "neuraltrust"); err != nil {
		t.Fatal(err)
	}
	if d, err := plan.issuedControlPlane(); err != nil || d != "dev.example.com" {
		t.Fatalf("issued for %q, %v; want dev.example.com", d, err)
	}
	plan.instances[1].cfg.ConfigSyncEndpoint = "agentgateway-configsync.other.example.com:443"
	if _, err := plan.issuedControlPlane(); err == nil {
		t.Fatal("two products issued for two control planes must be refused")
	}
}

func TestIssueCreatesAndFillsSecrets(t *testing.T) {
	srv, s := signedIn(t)
	ctx := context.Background()
	var out bytes.Buffer
	c := &consoleInstance{product: api.TrustGate, specs: secrets.TokenSpecs([]string{"trustgate"}), newName: "edge-1"}
	list, err := c.issue(ctx, &out, s, "org-1", "neuraltrust")
	if err != nil {
		t.Fatal(err)
	}
	issued := srv.Issued()
	if len(issued) != 1 || issued[0].Rotate || issued[0].ID != c.inst.ID {
		t.Fatalf("issued = %+v; want one non-rotating config for the new instance", issued)
	}
	got := map[string]string{}
	for _, sec := range list {
		for k, v := range sec.StringData {
			got[sec.Name+"/"+k] = v
		}
		for k, v := range sec.Data {
			got[sec.Name+"/"+k] = string(v)
		}
	}
	if got[values.TrustGateConfigSync+"/"+values.ConfigSyncKey] != issued[0].Config.ConfigSyncToken ||
		got[values.TrustGateEnrolment+"/"+values.EnrolmentKey] != issued[0].Config.EnrolmentToken {
		t.Fatalf("Secrets do not carry the issued tokens: %v", got)
	}
	if strings.Contains(out.String(), "CANARY") {
		t.Fatalf("issue printed a token: %s", out.String())
	}
	// The Secrets apply like any other ntctl creates.
	if _, err := secrets.Apply(ctx, fake.NewClientset(), list, false); err != nil {
		t.Fatal(err)
	}

	vals := values.Generate(values.Inputs{Domain: "platform.example.com", Products: []string{"trustgate"}})
	c.finalize(ctx, &out, s, "org-1", vals)
	if ep := srv.Finalized(c.inst.ID); ep != "https://gateway.platform.example.com" {
		t.Fatalf("finalized endpoint = %q", ep)
	}
}

func TestEndpoint(t *testing.T) {
	vals := map[string]any{
		"global":     map[string]any{"domain": "example.com"},
		"trustguard": map[string]any{"ingress": map[string]any{"dataPlane": map[string]any{"hostPrefix": "guard"}}},
	}
	if got := endpoint(vals, api.TrustGate); got != "https://gateway.example.com" {
		t.Errorf("TrustGate = %q", got)
	}
	if got := endpoint(vals, api.TrustGuard); got != "https://guard.example.com" {
		t.Errorf("TrustGuard = %q", got)
	}
	vals["agentgateway"] = map[string]any{"ingress": map[string]any{"dataPlane": map[string]any{"host": "llm.corp.internal"}}}
	if got := endpoint(vals, api.TrustGate); got != "https://llm.corp.internal" {
		t.Errorf("explicit host = %q", got)
	}
	if got := endpoint(map[string]any{}, api.TrustGate); got != "" {
		t.Errorf("no domain = %q, want empty", got)
	}
}
