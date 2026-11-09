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

var guardVals = map[string]any{"global": map[string]any{"products": map[string]any{"trustguard": true}}}

func TestFirewallChoice(t *testing.T) {
	quiet := func() *prompt.Prompter {
		return prompt.New(context.Background(), strings.NewReader(""), &bytes.Buffer{}, false)
	}
	cases := []struct {
		name       string
		in         *prompt.Prompter
		flag, env  string
		version    string
		vals       map[string]any
		existing   bool
		wantVals   map[string]any
		wantSecret string
	}{
		{name: "3.x needs no choice", in: quiet(), version: "3.15.1", vals: guardVals},
		{name: "no TrustGuard needs no choice", in: quiet(), version: "4.0.1", vals: map[string]any{"global": map[string]any{"products": map[string]any{"trustgate": true}}}},
		{name: "values that settle it are left alone", in: quiet(), version: "4.0.1", vals: values.Merge(values.Merge(map[string]any{}, values.InClusterFirewall()), map[string]any{"global": map[string]any{"products": map[string]any{"trustguard": true}}})},
		{name: "--yes without a JWT runs it in the cluster", in: quiet(), version: "4.0.1", vals: guardVals, wantVals: values.InClusterFirewall()},
		{name: "NT_FIREWALL_JWT picks the hosted firewall", in: quiet(), env: "jwt-from-env", version: "4.0.1", vals: guardVals, wantVals: values.HostedFirewall(), wantSecret: "jwt-from-env"},
		{name: "--firewall in-cluster wins over the JWT", in: quiet(), flag: firewallInCluster, env: "jwt-from-env", version: "4.0.1", vals: guardVals, wantVals: values.InClusterFirewall()},
		{name: "asked: hosted, then the JWT", in: prompt.Scripted(strings.NewReader("2\njwt-typed\n"), &bytes.Buffer{}), version: "4.0.1", vals: guardVals, wantVals: values.HostedFirewall(), wantSecret: "jwt-typed"},
		{name: "asked: Enter keeps it in the cluster", in: prompt.Scripted(strings.NewReader("\n"), &bytes.Buffer{}), version: "4.0.1", vals: guardVals, wantVals: values.InClusterFirewall()},
		{name: "hosted reuses the Secret already there", in: quiet(), flag: firewallHosted, version: "4.0.1", vals: guardVals, existing: true, wantVals: values.HostedFirewall()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("NT_FIREWALL_JWT", c.env)
			cs := fake.NewSimpleClientset()
			if c.existing {
				if _, err := cs.CoreV1().Secrets("nt").Create(context.Background(), &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: values.FirewallJWTSecret, Namespace: "nt"},
					Data:       map[string][]byte{values.FirewallJWTKey: []byte("kept")},
				}, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			got, secret, err := firewallChoice(context.Background(), c.in, cs, "nt", installFlags{firewall: c.flag}, c.version, c.vals)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.wantVals) {
				t.Errorf("values = %v, want %v", got, c.wantVals)
			}
			switch {
			case c.wantSecret == "" && secret != nil:
				t.Errorf("created Secret %s, want none", secret.Name)
			case c.wantSecret != "" && secret == nil:
				t.Error("no Secret created")
			case secret != nil && (secret.Name != values.FirewallJWTSecret || string(secret.Data[values.FirewallJWTKey]) != c.wantSecret):
				t.Errorf("Secret %s holds %q, want %s/%s = %q", secret.Name, secret.Data[values.FirewallJWTKey], values.FirewallJWTSecret, values.FirewallJWTKey, c.wantSecret)
			}
		})
	}
}

func TestFirewallChoiceHostedWithoutJWT(t *testing.T) {
	t.Setenv("NT_FIREWALL_JWT", "")
	in := prompt.New(context.Background(), strings.NewReader(""), &bytes.Buffer{}, false)
	_, _, err := firewallChoice(context.Background(), in, fake.NewSimpleClientset(), "nt", installFlags{firewall: firewallHosted}, "4.0.1", guardVals)
	if err == nil || !strings.Contains(err.Error(), "NT_FIREWALL_JWT") {
		t.Fatalf("hosted with no JWT and no terminal: err = %v, want one naming NT_FIREWALL_JWT", err)
	}
}

func TestDescribeFirewall(t *testing.T) {
	t.Parallel()
	with := func(extra map[string]any) map[string]any {
		return values.Merge(values.Merge(map[string]any{}, map[string]any{"global": map[string]any{"products": map[string]any{"trustguard": true}}}), extra)
	}
	for _, c := range []struct {
		version string
		vals    map[string]any
		want    string
	}{
		{"4.0.1", map[string]any{"global": map[string]any{"products": map[string]any{"trustgate": true}}}, ""},
		{"3.15.1", with(nil), "in this cluster"},
		{"4.0.1", with(values.InClusterFirewall()), "in this cluster"},
		{"4.0.1", with(values.HostedFirewall()), "NeuralTrust-hosted (FIREWALL_JWT in Secret trustguard-firewall)"},
		{"4.0.1", with(map[string]any{"global": map[string]any{"firewall": map[string]any{"deploy": false, "baseURL": "http://fw.example.com"}}}), "yours, at http://fw.example.com"},
	} {
		if got := describeFirewall(c.version, c.vals); got != c.want {
			t.Errorf("describeFirewall(%s, %v) = %q, want %q", c.version, c.vals, got, c.want)
		}
	}
}
