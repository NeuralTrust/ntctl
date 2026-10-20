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

	// Non-interactive, nothing named: a new instance named after the domain.
	plan, err := chooseInstances(ctx, prompt.New(context.Background(), strings.NewReader(""), &out, false), &out, s, "org-1", gateSpecs(), nil, "platform.example.com")
	if err != nil || len(plan) != 1 || plan[0].inst != nil || plan[0].newName != "platform.example.com" {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	// Named by flag.
	plan, err = chooseInstances(ctx, prompt.New(context.Background(), strings.NewReader(""), &out, false), &out, s, "org-1", gateSpecs(), map[api.Product]string{api.TrustGate: "prod"}, "x")
	if err != nil || plan[0].inst == nil || plan[0].inst.ID != "gw-1" {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	if _, err = chooseInstances(ctx, prompt.New(context.Background(), strings.NewReader(""), &out, false), &out, s, "org-1", gateSpecs(), map[api.Product]string{api.TrustGate: "nope"}, "x"); err == nil {
		t.Fatal("an unknown instance name should fail")
	}
	// Interactive: the existing one is offered; Enter creates a new one.
	plan, err = chooseInstances(ctx, prompt.Scripted(strings.NewReader("1\n"), &out), &out, s, "org-1", gateSpecs(), nil, "x")
	if err != nil || plan[0].inst == nil || plan[0].inst.ID != "gw-1" {
		t.Fatalf("choosing the existing one: %+v, %v", plan, err)
	}
	plan, err = chooseInstances(ctx, prompt.Scripted(strings.NewReader("\nedge\n"), &out), &out, s, "org-1", gateSpecs(), nil, "x")
	if err != nil || plan[0].inst != nil || plan[0].newName != "edge" {
		t.Fatalf("creating a new one: %+v, %v", plan, err)
	}
	if len(srv.Instances(api.TrustGate, "org-1")) != 1 {
		t.Fatal("choosing must not create anything")
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
