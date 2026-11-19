package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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

// testJWT is a well-formed FIREWALL_JWT for client id, as the console issues
// it: HS256 keyed with its own client_secret.
func testJWT(t *testing.T, id string) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	const secret = "client-secret"
	in := enc(map[string]any{"alg": "HS256", "typ": "JWT"}) + "." + enc(map[string]any{
		"typ": "firewall-client", "iss": "neuraltrust-console", "url": "https://firewall.example.com",
		"client_id": id, "client_secret": secret,
	})
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(in))
	return in + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestFirewallChoice(t *testing.T) {
	quiet := func() *prompt.Prompter {
		return prompt.New(context.Background(), strings.NewReader(""), &bytes.Buffer{}, false)
	}
	envJWT, typedJWT, keptJWT := testJWT(t, "fwc_env"), testJWT(t, "fwc_typed"), testJWT(t, "fwc_kept")
	cases := []struct {
		name       string
		in         *prompt.Prompter
		flag, env  string
		version    string
		vals       map[string]any
		existing   string // FIREWALL_JWT of a trustguard-firewall Secret already there
		wantVals   map[string]any
		wantSecret string
		wantWarn   bool
		wantErr    string
	}{
		{name: "3.x needs no choice", in: quiet(), version: "3.15.1", vals: guardVals},
		{name: "no TrustGuard needs no choice", in: quiet(), version: "4.0.1", vals: map[string]any{"global": map[string]any{"products": map[string]any{"trustgate": true}}}},
		{name: "values that settle it are left alone", in: quiet(), version: "4.0.1", vals: values.Merge(values.Merge(map[string]any{}, values.InClusterFirewall()), map[string]any{"global": map[string]any{"products": map[string]any{"trustguard": true}}})},
		{name: "--yes without a JWT runs it in the cluster, with a warning", in: quiet(), version: "4.0.1", vals: guardVals, wantVals: values.InClusterFirewall(), wantWarn: true},
		{name: "--firewall in-cluster without a JWT says nothing", in: quiet(), flag: firewallInCluster, version: "4.0.1", vals: guardVals, wantVals: values.InClusterFirewall()},
		{name: "NT_FIREWALL_JWT picks the hosted firewall", in: quiet(), env: envJWT, version: "4.0.1", vals: guardVals, wantVals: values.HostedFirewall(), wantSecret: envJWT},
		{name: "a malformed NT_FIREWALL_JWT is refused", in: quiet(), env: "not-a-jwt", version: "4.0.1", vals: guardVals, wantErr: "NT_FIREWALL_JWT"},
		{name: "--firewall in-cluster wins over the JWT", in: quiet(), flag: firewallInCluster, env: envJWT, version: "4.0.1", vals: guardVals, wantVals: values.InClusterFirewall()},
		{name: "asked: hosted, then the JWT", in: prompt.Scripted(strings.NewReader("2\n"+typedJWT+"\n"), &bytes.Buffer{}), version: "4.0.1", vals: guardVals, wantVals: values.HostedFirewall(), wantSecret: typedJWT},
		{name: "asked: a malformed typed JWT is refused", in: prompt.Scripted(strings.NewReader("2\ngarbage\n"), &bytes.Buffer{}), version: "4.0.1", vals: guardVals, wantErr: "not valid"},
		{name: "asked: Enter keeps it in the cluster", in: prompt.Scripted(strings.NewReader("\n"), &bytes.Buffer{}), version: "4.0.1", vals: guardVals, wantVals: values.InClusterFirewall()},
		{name: "hosted reuses the Secret already there", in: quiet(), flag: firewallHosted, version: "4.0.1", vals: guardVals, existing: keptJWT, wantVals: values.HostedFirewall()},
		{name: "the Secret already there picks the hosted firewall", in: quiet(), version: "4.0.1", vals: guardVals, existing: keptJWT, wantVals: values.HostedFirewall()},
		{name: "a malformed Secret already there is refused", in: quiet(), version: "4.0.1", vals: guardVals, existing: "kept", wantErr: values.FirewallJWTSecret},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("NT_FIREWALL_JWT", c.env)
			cs := fake.NewSimpleClientset()
			if c.existing != "" {
				if _, err := cs.CoreV1().Secrets("nt").Create(context.Background(), &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: values.FirewallJWTSecret, Namespace: "nt"},
					Data:       map[string][]byte{values.FirewallJWTKey: []byte(c.existing)},
				}, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			var warn bytes.Buffer
			got, secret, err := firewallChoice(context.Background(), c.in, &warn, cs, "nt", installFlags{firewall: c.flag}, c.version, c.vals)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want one mentioning %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.wantVals) {
				t.Errorf("values = %v, want %v", got, c.wantVals)
			}
			if gotWarn := strings.Contains(warn.String(), "NT_FIREWALL_JWT"); gotWarn != c.wantWarn {
				t.Errorf("warning = %q, want one: %v", warn.String(), c.wantWarn)
			}
			switch {
			case c.wantSecret == "" && secret != nil:
				t.Errorf("created Secret %s, want none", secret.Name)
			case c.wantSecret != "" && secret == nil:
				t.Error("no Secret created")
			case secret != nil && (secret.Name != values.FirewallJWTSecret || string(secret.Data[values.FirewallJWTKey]) != c.wantSecret):
				t.Errorf("Secret %s holds the wrong FIREWALL_JWT, want %s/%s", secret.Name, values.FirewallJWTSecret, values.FirewallJWTKey)
			}
		})
	}
}

func TestFirewallChoiceHostedWithoutJWT(t *testing.T) {
	t.Setenv("NT_FIREWALL_JWT", "")
	in := prompt.New(context.Background(), strings.NewReader(""), &bytes.Buffer{}, false)
	_, _, err := firewallChoice(context.Background(), in, &bytes.Buffer{}, fake.NewSimpleClientset(), "nt", installFlags{firewall: firewallHosted}, "4.0.1", guardVals)
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

// settleFirewall is what preflight uses: it never asks, so what it cannot
// decide stays open for prepare to explain.
func TestSettleFirewall(t *testing.T) {
	jwt := testJWT(t, "fwc_env")
	withSecret := func() *fake.Clientset {
		return fake.NewSimpleClientset(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: values.FirewallJWTSecret, Namespace: "nt"},
			Data:       map[string][]byte{values.FirewallJWTKey: []byte(testJWT(t, "fwc_kept"))},
		})
	}
	cases := []struct {
		name        string
		cs          *fake.Clientset
		choice, env string
		settled     bool
		secret      bool
		wantErr     string
	}{
		{name: "nothing decides", cs: fake.NewSimpleClientset()},
		{name: "hosted with no JWT anywhere stays open", cs: fake.NewSimpleClientset(), choice: firewallHosted},
		{name: "NT_FIREWALL_JWT", cs: fake.NewSimpleClientset(), env: jwt, settled: true, secret: true},
		{name: "the Secret already there", cs: withSecret(), settled: true},
		{name: "in-cluster", cs: fake.NewSimpleClientset(), choice: firewallInCluster, settled: true},
		{name: "a malformed NT_FIREWALL_JWT", cs: fake.NewSimpleClientset(), env: jwt[:20], wantErr: "NT_FIREWALL_JWT"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := settleFirewall(context.Background(), c.cs, "nt", c.choice, c.env, false)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) || strings.Contains(err.Error(), jwt[:20]) {
					t.Fatalf("err = %v, want one naming %q without the token", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.settled != c.settled || (got.secret != nil) != c.secret {
				t.Errorf("settled = %v, secret = %v; want %v, %v", got.settled, got.secret != nil, c.settled, c.secret)
			}
		})
	}
}
