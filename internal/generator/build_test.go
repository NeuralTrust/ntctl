package generator

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NeuralTrust/ntctl/internal/manifest"
)

// hooks are a chart's generator hooks for an external install, as rendered:
// its spec ConfigMaps and the Job's environment (its script left out).
func hooks(t *testing.T) []manifest.Object {
	t.Helper()
	b, err := os.ReadFile("testdata/external-hooks.yaml")
	if err != nil {
		t.Fatal(err)
	}
	objs, err := manifest.Parse(string(b))
	if err != nil {
		t.Fatal(err)
	}
	return objs
}

func cluster(secrets ...*corev1.Secret) Lookup {
	m := map[string]*corev1.Secret{}
	for _, s := range secrets {
		m[s.Name] = s
	}
	return func(_ context.Context, name string) (*corev1.Secret, error) { return m[name], nil }
}

func secret(name string, ann map[string]string, kv ...string) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: ann}, Data: map[string][]byte{}}
	for i := 0; i+1 < len(kv); i += 2 {
		s.Data[kv[i]] = []byte(kv[i+1])
	}
	return s
}

func byName(t *testing.T, p *Plan, name string) Secret {
	t.Helper()
	for _, s := range p.Secrets {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("plan has no %s", name)
	return Secret{}
}

func TestFreshInstall(t *testing.T) {
	p, err := Build(context.Background(), hooks(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range p.Secrets {
		names = append(names, s.Name)
	}
	if want := []string{"platform-secrets", "agentgateway-configsync-tls", "neuraltrust-ingress-tls", "trustguard-configsync-tls"}; !sameSet(names, want) {
		t.Fatalf("secrets = %v, want %v", names, want)
	}
	store := byName(t, p, "platform-secrets")
	if store.Owner != Absent || len(store.Live) != 0 {
		t.Errorf("owner = %s, live = %v", store.Owner, store.Live)
	}
	// The keys the keygen hooks used to mint, generated as one pair.
	if pub, err := publicHalf(store.Data["AGENTGATEWAY_M2M_PRIVATE_KEY"]); err != nil || pub != store.Data["ADMIN_M2M_PUBLIC_KEYS"] {
		t.Errorf("the M2M halves are not one pair: %v", err)
	}
	entries, err := Spec(hooks(t))
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, e := range entries {
		if e.Type != "tls" && e.Type != "adopt" {
			want = append(want, e.Key)
		}
	}
	if got := sortedKeys(store.Data); len(want) < 20 || !sameSet(got, want) {
		t.Errorf("platform-secrets keys = %v, want %v", got, want)
	}
	if v := store.Data["AUTH_SECRET_KEY"]; len(v) != 64 {
		t.Errorf("AUTH_SECRET_KEY (install) should be generated on a fresh install: %q", v)
	}
	if store.Data["OTEL_EXPORTER_OTLP_HEADERS"] != "authorization="+store.Data["OTLP_AUTH_TOKEN"] {
		t.Error("the derived header does not follow its source")
	}
	if len(store.Data["POSTGRES_PASSWORD"]) != 32 || strings.Trim(store.Data["POSTGRES_PASSWORD"], alnumChars) != "" {
		t.Errorf("POSTGRES_PASSWORD = %q, want 32 alphanumerics", store.Data["POSTGRES_PASSWORD"])
	}
	if b, _ := pem.Decode([]byte(store.Data["STS_SIGNING_KEY"])); b == nil || b.Type != "RSA PRIVATE KEY" {
		t.Error("STS_SIGNING_KEY should be a PKCS#1 PEM key")
	}

	// A CA-signed certificate verifies against its ca.crt for every SAN.
	tg := byName(t, p, "trustguard-configsync-tls")
	if tg.Type != corev1.SecretTypeTLS || tg.Annotations["neuraltrust.ai/tls-sans"] == "" {
		t.Errorf("type = %s, annotations = %v", tg.Type, tg.Annotations)
	}
	verify(t, tg.Data, "trustguard-control-plane.neuraltrust.svc.cluster.local")
	verify(t, byName(t, p, "neuraltrust-ingress-tls").Data, "neuraltrust.local")
}

func verify(t *testing.T, d map[string]string, host string) {
	t.Helper()
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(d["ca.crt"])) {
		t.Fatal("ca.crt does not parse")
	}
	b, _ := pem.Decode([]byte(d["tls.crt"]))
	crt, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crt.Verify(x509.VerifyOptions{Roots: roots, DNSName: host}); err != nil {
		t.Errorf("%s: %v", host, err)
	}
	if k, _ := pem.Decode([]byte(d["tls.key"])); k == nil || k.Type != "PRIVATE KEY" {
		t.Error("tls.key should be a PKCS#8 PEM key")
	}
}

func TestUpgradeFromTheEarlierGenerator(t *testing.T) {
	// An install made on charts 3.7 to 3.9: its values are in
	// neuraltrust-generated, which the services read until now.
	live := cluster(
		secret("neuraltrust-generated", nil, "POSTGRES_PASSWORD", "pg-from-3.7", "AUTH_SECRET_KEY", "key-from-3.7"),
		secret("neuraltrust-generated-trustguard-configsync-tls", nil, "tls.crt", "C", "tls.key", "K", "ca.crt", "A"),
		secret("trustguard-secrets", nil, "DB_PASSWORD", "older-copy"),
	)
	p, err := Build(context.Background(), hooks(t), live)
	if err != nil {
		t.Fatal(err)
	}
	store := byName(t, p, "platform-secrets")
	if store.Data["POSTGRES_PASSWORD"] != "pg-from-3.7" || store.Origin["POSTGRES_PASSWORD"] != "adopted from neuraltrust-generated" {
		t.Errorf("POSTGRES_PASSWORD = %q (%s)", store.Data["POSTGRES_PASSWORD"], store.Origin["POSTGRES_PASSWORD"])
	}
	if store.Data["TRUSTGUARD_DB_PASSWORD"] != "older-copy" || store.Origin["TRUSTGUARD_DB_PASSWORD"] != "adopted from trustguard-secrets" {
		t.Errorf("TRUSTGUARD_DB_PASSWORD = %q (%s)", store.Data["TRUSTGUARD_DB_PASSWORD"], store.Origin["TRUSTGUARD_DB_PASSWORD"])
	}
	tg := byName(t, p, "trustguard-configsync-tls")
	if !reflect.DeepEqual(tg.Data, map[string]string{"tls.crt": "C", "tls.key": "K", "ca.crt": "A"}) || tg.Origin["tls.crt"] != "adopted from neuraltrust-generated-trustguard-configsync-tls" {
		t.Errorf("certificate = %v (%v)", tg.Data, tg.Origin)
	}
}

func TestInstallKeyNotGeneratedForAnOlderInstall(t *testing.T) {
	// The install predates AUT_SECRET_KEY's generation: the app may already
	// hold data under its default, so no value is invented.
	p, err := Build(context.Background(), hooks(t), cluster(secret("neuraltrust-generated", nil, "POSTGRES_PASSWORD", "x")))
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := byName(t, p, "platform-secrets").Data["AUTH_SECRET_KEY"]; ok {
		t.Errorf("AUTH_SECRET_KEY = %q, want none", v)
	}
}

func TestManagedSecretIsLeftAsItIs(t *testing.T) {
	first, err := Build(context.Background(), hooks(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	var live []*corev1.Secret
	managed := map[string]string{Marker: "managed"}
	for _, s := range first.Secrets {
		ls := secret(s.Name, managed)
		for k, v := range s.Data {
			ls.Data[k] = []byte(v)
		}
		live = append(live, ls)
	}
	// An earlier generator's leftover must not override a managed Secret.
	live = append(live, secret("neuraltrust-generated", nil, "POSTGRES_PASSWORD", "stale"))
	again, err := Build(context.Background(), hooks(t), cluster(live...))
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range again.Secrets {
		if s.Owner != Managed || len(s.Origin) != 0 || !reflect.DeepEqual(s.Data, first.Secrets[i].Data) {
			t.Errorf("%s changed on a second run: owner %s, origin %v", s.Name, s.Owner, s.Origin)
		}
	}
}

func TestOperatorSecretIsOnlyChecked(t *testing.T) {
	ext := secret("platform-secrets", map[string]string{Marker: "external"}, "POSTGRES_PASSWORD", "theirs")
	esoTLS := secret("trustguard-configsync-tls", nil, "tls.crt", "C")
	esoTLS.OwnerReferences = []metav1.OwnerReference{{Kind: "ExternalSecret", Name: "tg"}}
	p, err := Build(context.Background(), hooks(t), cluster(ext, esoTLS))
	if err != nil {
		t.Fatal(err)
	}
	store := byName(t, p, "platform-secrets")
	if store.Owner != External || store.Data["POSTGRES_PASSWORD"] != "theirs" || len(store.Data) != 1 {
		t.Errorf("an operator's Secret must not be filled in: %+v", store)
	}
	// The RSA keys the chart mints count like any other; a public half is
	// needed only with its private key.
	if !slices.Contains(store.Missing, "AUTH_SECRET") || slices.Contains(store.Missing, "POSTGRES_PASSWORD") ||
		!slices.Contains(store.Missing, "MCP_OAUTH_SIGNING_KEY") || slices.Contains(store.Missing, "ADMIN_M2M_PUBLIC_KEYS") {
		t.Errorf("missing = %v", store.Missing)
	}
	if tg := byName(t, p, "trustguard-configsync-tls"); tg.Owner != External || !reflect.DeepEqual(tg.Missing, []string{"tls.key", "ca.crt"}) {
		t.Errorf("certificate: owner %s, missing %v", tg.Owner, tg.Missing)
	}
}

func TestPinsAndUnknownTypes(t *testing.T) {
	objs := hooks(t)
	pins, err := manifest.Parse(`apiVersion: v1
kind: Secret
metadata:
  name: neuraltrust-generator-pins-platform
  labels: {neuraltrust.ai/generator-spec: "2"}
data:
  AUTH_SECRET: ` + "cGlubmVk" + "\n")
	if err != nil {
		t.Fatal(err)
	}
	p, err := Build(context.Background(), append(objs, pins...), cluster(secret("platform-secrets", map[string]string{Marker: "managed"}, "AUTH_SECRET", "old")))
	if err != nil {
		t.Fatal(err)
	}
	if s := byName(t, p, "platform-secrets"); s.Data["AUTH_SECRET"] != "pinned" || s.Origin["AUTH_SECRET"] != "from values" {
		t.Errorf("a value pinned in the chart values wins: %q (%s)", s.Data["AUTH_SECRET"], s.Origin["AUTH_SECRET"])
	}

	if _, err := generate(Entry{Key: "K", Type: "ed25519"}); err == nil || !strings.Contains(err.Error(), "update ntctl") {
		t.Errorf("an unknown type must fail, got %v", err)
	}
	if _, err := Build(context.Background(), nil, nil); err != ErrNoGenerator {
		t.Errorf("no generator: %v", err)
	}
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	sort.Strings(a)
	sort.Strings(b)
	return reflect.DeepEqual(a, b)
}

func TestReissueFollowsTheNames(t *testing.T) {
	// A published listener: the certificate counts only while it covers the
	// names the spec asks for.
	e := Entry{Type: "tls", Secret: "databridge-southbound-tls", CN: "databridge.cp.example.com", CACN: "ca",
		SANs: []string{"databridge.cp.example.com", "databridge-southbound"}, Reissue: true,
		Annotations: map[string]string{"neuraltrust.ai/southbound-host": "databridge.cp.example.com"}}
	job := manifest.Object{"kind": "Job", "spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{
		map[string]any{"env": []any{map[string]any{"name": "GENERATED_SECRET", "value": "platform-secrets"}}}}}}}}
	raw, _ := json.Marshal([]Entry{e})
	cm := manifest.Object{"kind": "ConfigMap", "metadata": map[string]any{"name": "neuraltrust-generator-spec-databridge"}, "data": map[string]any{"spec.json": string(raw)}}
	issued := func(hash string) *corev1.Secret {
		return secret("databridge-southbound-tls", map[string]string{Marker: "managed", sansAnnotation: hash}, "tls.crt", "C", "tls.key", "K", "ca.crt", "A")
	}
	for name, c := range map[string]struct {
		hash string
		kept bool
	}{"same names": {sansHash(e.SANs), true}, "domain retargeted": {"0000000000000000", false}} {
		t.Run(name, func(t *testing.T) {
			p, err := Build(context.Background(), []manifest.Object{job, cm}, cluster(issued(c.hash)))
			if err != nil {
				t.Fatal(err)
			}
			s := byName(t, p, "databridge-southbound-tls")
			if kept := s.Data["tls.crt"] == "C"; kept != c.kept {
				t.Fatalf("kept = %v, want %v (origin %v)", kept, c.kept, s.Origin)
			}
			if s.Annotations["neuraltrust.ai/southbound-host"] != "databridge.cp.example.com" || s.Annotations[sansAnnotation] != sansHash(e.SANs) {
				t.Errorf("annotations = %v", s.Annotations)
			}
		})
	}
}

func TestPublicKeyFollowsThePrivate(t *testing.T) {
	// The keygen hook's format: base64 of the SPKI PEM. TrustGate verifies
	// with it what the app signs with the private half, so the two must be one
	// pair whether the private key was adopted or generated.
	priv, err := generate(Entry{Key: "K", Type: "rsa-pkcs8", Bits: 2048})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := publicHalf(priv)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(pub)
	if err != nil {
		t.Fatalf("not base64: %v", err)
	}
	b, _ := pem.Decode(raw)
	if b == nil || b.Type != "PUBLIC KEY" {
		t.Fatalf("not an SPKI PEM: %q", raw)
	}
	got, err := x509.ParsePKIXPublicKey(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pb, _ := pem.Decode([]byte(priv))
	key, err := x509.ParsePKCS8PrivateKey(pb.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	rk := key.(*rsa.PrivateKey)
	if !rk.PublicKey.Equal(got) || rk.N.BitLen() != 2048 {
		t.Errorf("the public half is not the private key's (%d bits)", rk.N.BitLen())
	}

	// Built from a spec: an adopted private key gets its own public half.
	job := manifest.Object{"kind": "Job", "spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{
		map[string]any{"env": []any{map[string]any{"name": "GENERATED_SECRET", "value": "platform-secrets"}}}}}}}}
	raw2, _ := json.Marshal([]Entry{
		{Key: "AGENTGATEWAY_M2M_PRIVATE_KEY", Type: "rsa-pkcs8", Bits: 2048, Adopt: [][]string{{"agentgateway-m2m-keys", "AGENTGATEWAY_M2M_PRIVATE_KEY"}}},
		{Key: "ADMIN_M2M_PUBLIC_KEYS", Type: "public-key", Source: "AGENTGATEWAY_M2M_PRIVATE_KEY"},
	})
	cm := manifest.Object{"kind": "ConfigMap", "metadata": map[string]any{"name": "neuraltrust-generator-spec-platform"}, "data": map[string]any{"spec.json": string(raw2)}}
	p, err := Build(context.Background(), []manifest.Object{job, cm}, cluster(secret("agentgateway-m2m-keys", nil, "AGENTGATEWAY_M2M_PRIVATE_KEY", priv)))
	if err != nil {
		t.Fatal(err)
	}
	s := byName(t, p, "platform-secrets")
	if s.Data["AGENTGATEWAY_M2M_PRIVATE_KEY"] != priv || s.Data["ADMIN_M2M_PUBLIC_KEYS"] != pub {
		t.Errorf("adopted pair: origin %v", s.Origin)
	}
}
