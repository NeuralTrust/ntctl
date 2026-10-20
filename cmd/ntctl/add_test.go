package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	release "helm.sh/helm/v4/pkg/release/v1"

	"github.com/NeuralTrust/ntctl/internal/prompt"
	"github.com/NeuralTrust/ntctl/internal/values"
)

func installed(cfg map[string]any) *release.Release {
	return &release.Release{Name: "neuraltrust-platform", Version: 1, Config: cfg,
		Chart: &chartv2.Chart{Metadata: &chartv2.Metadata{Name: "neuraltrust-platform", Version: "3.5.3"}}}
}

// TestAddProductsStopsBeforeAnyToken covers the runs of `ntctl install` on an
// existing release that end before a token is asked for or the cluster is
// touched: nothing to add, or a setting that would change the release.
func TestAddProductsStopsBeforeAnyToken(t *testing.T) {
	gate := gateOnly()
	all := values.Generate(values.Inputs{Domain: "d.example.com", Products: values.Products})
	pinned := values.Merge(gateOnly(), map[string]any{"global": map[string]any{"controlPlane": map[string]any{"configSyncAddr": "lb.example.com:443"}}})
	exposed := values.Merge(gateOnly(), map[string]any{"global": map[string]any{"ingress": map[string]any{
		"provider": "gateway-api", "gatewayApi": map[string]any{"parentRefs": []any{map[string]any{"name": "shared", "namespace": "gw"}}}}}})
	noTerminal := func(w io.Writer) *prompt.Prompter {
		return prompt.New(context.Background(), strings.NewReader(""), w, false)
	}
	terminal := func(input string) func(io.Writer) *prompt.Prompter {
		return func(w io.Writer) *prompt.Prompter { return prompt.Scripted(strings.NewReader(input), w) }
	}

	cases := []struct {
		name string
		o    opts
		f    installFlags
		cfg  map[string]any
		in   func(io.Writer) *prompt.Prompter
		err  string // "" for a clean no-op
		says []string
	}{
		{name: "every product installed", cfg: all, in: terminal(""), says: []string{"products trustgate, trustguard, dataPlane", "nothing to do"}},
		{name: "no terminal and no --products", cfg: gate, in: noTerminal, says: []string{"pass --products to add trustguard, dataPlane"}},
		{name: "only installed products named", o: opts{products: "trustgate"}, cfg: gate, in: noTerminal, says: []string{"trustgate is already installed", "nothing to do"}},
		{name: "same domain and a registry key are fine", o: opts{products: "trustgate", domain: "d.example.com"}, f: installFlags{registryKey: "key.json"}, cfg: gate, in: noTerminal,
			says: []string{"registry key is not used", "nothing to do"}},
		{name: "another domain", o: opts{products: "trustguard", domain: "other.example.com"}, cfg: gate, in: noTerminal, err: "--domain other.example.com differs"},
		{name: "another platform", o: opts{products: "trustguard", platform: "gcp"}, cfg: values.Merge(gateOnly(), map[string]any{"global": map[string]any{"platform": "azure"}}), in: noTerminal, err: "--platform gcp differs"},
		{name: "another gateway", o: opts{products: "trustguard", gateway: "gw/external"}, cfg: exposed, in: noTerminal, err: "--gateway gw/external differs from how the release is exposed (HTTPRoutes on Gateway gw/shared)"},
		{name: "a gateway on an Ingress install", o: opts{products: "trustguard", gateway: "gw/shared"}, cfg: gate, in: noTerminal, err: "(Ingresses)"},
		{name: "another mode", o: opts{products: "trustguard", mode: "external"}, cfg: gate, in: noTerminal, err: "--mode external differs from the release's mode, hybrid"},
		{name: "the same mode", o: opts{products: "trustgate", mode: "Hybrid"}, cfg: gate, in: noTerminal, says: []string{"nothing to do"}},
		{name: "print mode", o: opts{products: "trustguard"}, f: installFlags{secretsMode: "print"}, cfg: gate, in: noTerminal, err: "--secrets-mode print is for a new install"},
		{name: "configSyncAddr serves one product", o: opts{products: "trustguard"}, cfg: pinned, in: noTerminal, err: "configSyncAddr, which serves a single product"},
		{name: "the question offers what is missing", cfg: pinned, in: terminal("\n"), err: "serves a single product",
			says: []string{"Products to add, comma-separated [trustguard,dataPlane]"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			o := c.o
			o.release, o.kube.Namespace = "neuraltrust-platform", "neuraltrust"
			err := o.addProducts(context.Background(), c.in(&out), &out, c.f, nil, installed(c.cfg))
			switch {
			case c.err == "" && err != nil:
				t.Fatalf("err = %v\n%s", err, out.String())
			case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
				t.Fatalf("err = %v, want %q\n%s", err, c.err, out.String())
			}
			for _, s := range c.says {
				if !strings.Contains(out.String(), s) {
					t.Errorf("output lacks %q:\n%s", s, out.String())
				}
			}
			if strings.Contains(out.String(), "token") {
				t.Errorf("asked for a token:\n%s", out.String())
			}
		})
	}
}

func TestConfigSyncFitsDataPlane(t *testing.T) {
	pinned := map[string]any{"global": map[string]any{"controlPlane": map[string]any{"configSyncAddr": "lb.example.com:443"}}}
	if err := configSyncFits(pinned, []string{"trustgate", "dataPlane"}); err != nil {
		t.Errorf("dataPlane has no config-sync listener, so it fits: %v", err)
	}
	if err := configSyncFits(map[string]any{}, []string{"trustgate", "trustguard"}); err != nil {
		t.Errorf("without configSyncAddr both fit: %v", err)
	}
}

func TestUpdateSavedValuesOnlyWhenKept(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	gone := filepath.Join(dir, "gone.yaml")
	if err := updateSavedValues(&out, gone, gateOnly()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Fatal("wrote a values file the install did not keep")
	}

	kept := filepath.Join(dir, "neuraltrust-values.yaml")
	if err := os.WriteFile(kept, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	vals := values.Merge(gateOnly(), map[string]any{"global": map[string]any{"postgresql": map[string]any{"password": "CANARY-pg"}}})
	if err := updateSavedValues(&out, kept, vals); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(kept)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "CANARY") || !strings.Contains(string(b), "trustgate: true") {
		t.Fatalf("saved values:\n%s", b)
	}
}

func TestFilesMatchRelease(t *testing.T) {
	current := values.Merge(gateOnly(), map[string]any{"global": map[string]any{"platform": "azure"}})
	cases := []struct {
		name  string
		files map[string]any
		err   string
	}{
		{name: "no files", files: map[string]any{}},
		{name: "the first install's own values", files: gateOnly()},
		{name: "another domain", files: map[string]any{"global": map[string]any{"domain": "other.example.com"}}, err: `global.domain to "other.example.com", but the release has "d.example.com"`},
		{name: "another platform", files: map[string]any{"global": map[string]any{"platform": "aws"}}, err: "global.platform"},
		{name: "hybrid spelled out", files: map[string]any{"global": map[string]any{"deploymentMode": "hybrid"}}},
		{name: "another mode", files: map[string]any{"global": map[string]any{"deploymentMode": "external"}}, err: "cannot change where its control plane runs"},
	}
	for _, c := range cases {
		err := filesMatchRelease(c.files, current)
		switch {
		case c.err == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s: err = %v, want %q", c.name, err, c.err)
		}
	}
}
