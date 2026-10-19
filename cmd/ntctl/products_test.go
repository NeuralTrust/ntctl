package main

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/NeuralTrust/ntctl/internal/prompt"
	"github.com/NeuralTrust/ntctl/internal/values"
)

// gateOnly is a release installed by ntctl with TrustGate alone.
func gateOnly() map[string]any {
	return values.Generate(values.Inputs{Domain: "d.example.com", Products: []string{"trustgate"}})
}

func TestChangeProducts(t *testing.T) {
	cases := []struct {
		name, products, remove string
		add, rm, running       []string
		err                    string
	}{
		{name: "add keeps the others", products: "trustguard", add: []string{"trustguard"}},
		{name: "already running is a no-op", products: "trustgate", running: []string{"trustgate"}},
		{name: "remove", products: "trustguard", remove: "trustgate", add: []string{"trustguard"}, rm: []string{"trustgate"}},
		{name: "removing what does not run is a no-op", remove: "dataPlane"},
		{name: "both flags", products: "trustguard", remove: "trustguard", err: "both"},
		{name: "nothing left", remove: "trustgate", err: "no product"},
		{name: "unknown", remove: "agentgateway", err: "unknown product"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := opts{products: c.products}
			got, err := o.changeProducts(gateOnly(), c.remove)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := productChange{add: c.add, remove: c.rm, running: c.running}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("change = %+v, want %+v", got, want)
			}
		})
	}
}

// TestUpgradeAddingAProductKeepsTheOthers is the upgrade's values for
// --products trustguard over a TrustGate release: before, --products named the
// whole set and turned TrustGate off.
func TestUpgradeAddingAProductKeepsTheOthers(t *testing.T) {
	current := gateOnly()
	o := opts{products: "trustguard"}
	change, err := o.changeProducts(current, "")
	if err != nil {
		t.Fatal(err)
	}
	o.products = ""
	o.extra = values.ProductDelta(current, change.add, change.remove)
	vals, err := o.effectiveValues(values.Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	got := reuse(current, vals)
	if want := []string{"trustgate", "trustguard"}; !reflect.DeepEqual(values.Installed(got), want) {
		t.Fatalf("installed after the upgrade = %v, want %v", values.Installed(got), want)
	}
	if !referenced(got, values.TrustGuardConfigSync) || !referenced(got, values.TrustGateConfigSync) {
		t.Errorf("token refs lost: %v", got)
	}
}

func TestTokenSecretsOnlyForMissingOnes(t *testing.T) {
	ctx := context.Background()
	ns := "neuraltrust"
	cs := fake.NewClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: values.TrustGuardEnrolment, Namespace: ns}, Data: map[string][]byte{values.EnrolmentKey: []byte("x")}})
	vals := reuse(gateOnly(), values.ProductDelta(gateOnly(), []string{"trustguard"}, nil))
	t.Setenv("NT_TRUSTGUARD_CONFIG_SYNC_TOKEN", "")
	var out bytes.Buffer

	create, need, err := tokenSecrets(ctx, prompt.Scripted(strings.NewReader("sync-token\n"), &out), cs, ns, nil, vals, []string{"trustguard"}, false, false)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(need) != 0 || len(create) != 1 || create[0].Name != values.TrustGuardConfigSync {
		t.Fatalf("create = %v, need = %v: want only the missing config-sync Secret", names(create), need)
	}
	if !strings.Contains(out.String(), "TrustGuard config-sync token") || strings.Contains(out.String(), "enrolment") {
		t.Errorf("asked for the wrong tokens:\n%s", out.String())
	}
	if got := plannedKeys(create, need); !reflect.DeepEqual(got, map[string][]string{values.TrustGuardConfigSync: {values.ConfigSyncKey}}) {
		t.Errorf("planned = %v", got)
	}

	// A dry run asks nothing.
	out.Reset()
	create, _, err = tokenSecrets(ctx, prompt.Scripted(strings.NewReader(""), &out), cs, ns, nil, vals, []string{"trustguard"}, false, true)
	if err != nil || len(create) != 1 || out.Len() != 0 {
		t.Fatalf("placeholders: create = %v, err = %v, asked:\n%s", names(create), err, out.String())
	}
}

func names(list []*corev1.Secret) []string {
	var out []string
	for _, s := range list {
		out = append(out, s.Name)
	}
	return out
}
