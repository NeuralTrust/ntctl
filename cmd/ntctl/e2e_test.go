//go:build e2e

// End-to-end tests against a real cluster, signed in to the fake console.
// Run with `make test-e2e` against a local kind cluster:
//
//	kind get kubeconfig --name kind > /tmp/kind.kubeconfig
//	NTCTL_E2E_KUBECONFIG=/tmp/kind.kubeconfig make test-e2e
package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/action"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/NeuralTrust/ntctl/internal/api"
	"github.com/NeuralTrust/ntctl/internal/chart"
	"github.com/NeuralTrust/ntctl/internal/helm"
	"github.com/NeuralTrust/ntctl/internal/kube"
	"github.com/NeuralTrust/ntctl/internal/values"
)

// cluster points ntctl at the e2e cluster and removes the namespace, and the
// release in it, when the test ends.
func cluster(t *testing.T, ns string) kubernetes.Interface {
	t.Helper()
	kc := os.Getenv("NTCTL_E2E_KUBECONFIG")
	if kc == "" {
		t.Skip("NTCTL_E2E_KUBECONFIG is not set")
	}
	t.Setenv("KUBECONFIG", kc)
	k := kube.Options{Kubeconfig: kc, Namespace: ns}
	cs, err := k.Clientset()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cfg, err := k.Helm(); err == nil {
			_, _ = action.NewUninstall(cfg).Run("neuraltrust-platform") // absent when the install never started
		}
		ctx := context.Background()
		if err := cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("deleting namespace %s: %v", ns, err)
		}
	})
	return cs
}

// secretValue reads one key of a Secret.
func secretValue(t *testing.T, cs kubernetes.Interface, ns, name, key string) string {
	t.Helper()
	s, err := cs.CoreV1().Secrets(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Secret %s: %v", name, err)
	}
	return string(s.Data[key])
}

// noCanary fails when a file holds a token.
func noCanary(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "CANARY") {
		t.Fatalf("%s holds a token", path)
	}
}

// TestE2EInstallWithTheChart installs the published chart signed in: ntctl
// creates both private instances and fills the Secrets the chart reads with
// their tokens. Images cannot be pulled without a real key, so the install is
// expected not to become ready, and nothing is recorded in the console.
func TestE2EInstallWithTheChart(t *testing.T) {
	const ns = "ntctl-e2e-chart"
	cs := cluster(t, ns)
	srv := console(t)
	// A console whose control plane is not a region's, as a dev or
	// self-hosted one: its tokens are valid only there.
	srv.ControlPlane = "cp.example.com"
	ntctl(t, 0, "login", "--app-url", srv.URL)

	dir := t.TempDir()
	key := filepath.Join(dir, "registry-key.json")
	if err := os.WriteFile(key, []byte(`{"type":"service_account","project_id":"e2e","client_email":"e2e@e2e.iam.gserviceaccount.com","private_key":"not-a-real-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	saved := filepath.Join(dir, "values.yaml")
	out := ntctl(t, 1, "install", "--yes", "--ignore-capacity", "--namespace", ns, "--domain", "e2e.example.com",
		"--products", "trustgate,trustguard", "--registry-key", key, "--save-values", saved, "--timeout", "45s")
	for _, want := range []string{"tokens from the console", `Created private TrustGate "e2e.example.com"`, `Created private TrustGuard "e2e.example.com"`,
		"tokens for the control plane at cp.example.com", "install did not complete"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	noCanary(t, saved)
	if b, err := os.ReadFile(saved); err != nil || !strings.Contains(string(b), "domain: cp.example.com") {
		t.Errorf("the saved values do not point the data plane at the console's control plane (%v):\n%s", err, b)
	}

	issued := srv.Issued()
	if len(issued) != 2 {
		t.Fatalf("issued = %d install configs, want 2", len(issued))
	}
	names := map[string][2]string{
		"trustgate":  {values.TrustGateConfigSync, values.TrustGateEnrolment},
		"trustguard": {values.TrustGuardConfigSync, values.TrustGuardEnrolment},
	}
	for _, is := range issued {
		n := names[is.Product]
		if got := secretValue(t, cs, ns, n[0], values.ConfigSyncKey); got != is.Config.ConfigSyncToken {
			t.Errorf("%s does not hold the issued config-sync token", n[0])
		}
		if got := secretValue(t, cs, ns, n[1], values.EnrolmentKey); got != is.Config.EnrolmentToken {
			t.Errorf("%s does not hold the issued enrolment token", n[1])
		}
		if is.Rotate {
			t.Errorf("install must not rotate %s's credentials", is.ID)
		}
		if ep := srv.Finalized(is.ID); ep != "" {
			t.Errorf("an install that is not ready recorded %s for %s", ep, is.ID)
		}
	}

	// The tokens reach the Secrets only, never the Helm release.
	cfg, err := kube.Options{Kubeconfig: os.Getenv("NTCTL_E2E_KUBECONFIG"), Namespace: ns}.Helm()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := helm.Current(cfg, "neuraltrust-platform")
	if err != nil || rel == nil {
		t.Fatalf("reading the release: %v", err)
	}
	if strings.Contains(rel.Manifest, "CANARY") || strings.Contains(fmt.Sprint(rel.Config), "CANARY") {
		t.Fatal("the Helm release holds a token")
	}

	// The same install again finds the release failed: it says how to get
	// out, changes nothing and asks the console for nothing.
	out = ntctl(t, 1, "install", "--yes", "--ignore-capacity", "--namespace", ns, "--domain", "e2e.example.com",
		"--products", "trustgate,trustguard", "--registry-key", key, "--save-values", "")
	if !strings.Contains(out, "revision 1 failed") || !strings.Contains(out, "ntctl doctor") {
		t.Errorf("a second install on a failed release did not say how to recover:\n%s", out)
	}
	if got := len(srv.Issued()); got != 2 {
		t.Fatalf("a second install on a failed release issued tokens (%d configs)", got)
	}

	// Upgrade on the failed release applies it again instead of reporting
	// that it is up to date. The images still cannot be pulled, so it fails
	// again, and with no working revision it stays in place.
	out = ntctl(t, 1, "upgrade", "--yes", "--ignore-capacity", "--namespace", ns, "--version", rel.Chart.Metadata.Version, "--timeout", "30s")
	for _, want := range []string{"Revision 1 failed; this applies it again", "No revision has worked yet", "left in place"} {
		if !strings.Contains(out, want) {
			t.Errorf("upgrade of a failed release: output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Already up to date") {
		t.Errorf("upgrade of a failed release reported it up to date:\n%s", out)
	}

	// Uninstall without typing the namespace removes nothing; with --yes it
	// removes the release and, asked to, ntctl's Secrets.
	ntctl(t, 1, "uninstall", "--namespace", ns)
	if cur, err := helm.Current(cfg, "neuraltrust-platform"); err != nil || cur == nil {
		t.Fatalf("an unconfirmed uninstall removed the release (%v)", err)
	}
	out = ntctl(t, 0, "uninstall", "--yes", "--delete-secrets", "--namespace", ns)
	for _, want := range []string{"Keeps:    namespace " + ns, "Removed release", "Deleted Secret " + values.TrustGateConfigSync} {
		if !strings.Contains(out, want) {
			t.Errorf("uninstall: output lacks %q:\n%s", want, out)
		}
	}
	if cur, err := helm.Current(cfg, "neuraltrust-platform"); err != nil || cur != nil {
		t.Fatalf("after uninstall: release %v, %v", cur, err)
	}
	if _, err := cs.CoreV1().Secrets(ns).Get(context.Background(), values.TrustGateConfigSync, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("--delete-secrets kept %s (%v)", values.TrustGateConfigSync, err)
	}
}

// TestE2EInstallPrintsTheSecrets runs an install with --secrets-mode print,
// signed in: it saves the values and prints the commands, and the cluster and
// the console are left exactly as they were.
func TestE2EInstallPrintsTheSecrets(t *testing.T) {
	const ns = "ntctl-e2e-print"
	cs := cluster(t, ns)
	srv := console(t)
	ntctl(t, 0, "login", "--app-url", srv.URL)
	t.Setenv("NT_POSTGRES_URL", "postgres://nt@db.example.com:5432/platform")
	t.Setenv("NT_REDIS_URL", "rediss://default:CANARY-redis@cache.example.com:6380")

	dir := t.TempDir()
	key := filepath.Join(dir, "registry-key.json")
	if err := os.WriteFile(key, []byte(`{"type":"service_account","project_id":"e2e","private_key":"CANARY-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	saved := filepath.Join(dir, "values.yaml")
	out := ntctl(t, 0, "install", "--yes", "--ignore-capacity", "--secrets-mode", "print", "--namespace", ns,
		"--domain", "print.example.com", "--products", "trustgate,trustguard", "--registry-key", key, "--save-values", saved)
	for _, want := range []string{
		"Nothing was changed in the cluster",
		"kubectl create namespace " + ns,
		"create secret docker-registry gcr-secret",
		`--docker-password="$(cat '` + key + `')"`,
		"create secret generic external-postgresql \\\n  --from-literal=POSTGRES_PASSWORD='<POSTGRES_PASSWORD>'",
		"--from-literal=REDIS_HOST='cache.example.com'",
		"--from-literal=REDIS_PASSWORD='<REDIS_PASSWORD>'",
		"create secret generic " + values.TrustGateConfigSync,
		"create secret generic " + values.TrustGuardEnrolment,
		"ntctl install -n " + ns + " -f " + saved,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	noCanary(t, saved)
	b, err := os.ReadFile(saved)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "external-postgresql") || !strings.Contains(string(b), "db.example.com") {
		t.Errorf("the saved values do not point at the printed Secrets:\n%s", b)
	}
	if _, err := cs.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("print mode created the namespace (err %v)", err)
	}
	if n := len(srv.Issued()); n != 0 {
		t.Errorf("print mode asked the console for %d install configs", n)
	}
}

// TestE2EInstallRecordsTheEndpoint installs a chart that is ready at once,
// into an existing instance named on the command line, and checks the
// endpoint is recorded in the console.
func TestE2EInstallRecordsTheEndpoint(t *testing.T) {
	const ns = "ntctl-e2e-ready"
	cs := cluster(t, ns)
	srv := console(t)
	srv.AddInstance(api.TrustGate, "org-1", api.Instance{ID: "gw-e2e", Name: "edge-1"})
	ntctl(t, 0, "login", "--app-url", srv.URL)

	chart := t.TempDir()
	files := map[string]string{
		"Chart.yaml":            "apiVersion: v2\nname: neuraltrust-platform\nversion: 3.0.0\n",
		"templates/config.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: e2e\ndata:\n  domain: {{ .Values.global.domain | quote }}\n",
	}
	for name, body := range files {
		p := filepath.Join(chart, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out := ntctl(t, 0, "install", "--yes", "--namespace", ns, "--chart", chart, "--domain", "ready.example.com",
		"--products", "trustgate", "--image-registry", "registry.example.com/nt", "--image-registry-node-auth",
		"--trustgate-instance", "edge-1", "--save-values", "", "--timeout", "1m")
	if !strings.Contains(out, "Recorded edge-1 endpoint https://gateway.ready.example.com in the console") {
		t.Fatalf("output:\n%s", out)
	}
	if ep := srv.Finalized("gw-e2e"); ep != "https://gateway.ready.example.com" {
		t.Fatalf("finalized endpoint = %q", ep)
	}
	if got := srv.Instances(api.TrustGate, "org-1"); len(got) != 1 {
		t.Fatalf("naming an existing instance must not create one: %v", got)
	}
	if got := secretValue(t, cs, ns, values.TrustGateConfigSync, values.ConfigSyncKey); got != srv.Issued()[0].Config.ConfigSyncToken {
		t.Fatal("the config-sync Secret does not hold the issued token")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, "e2e", metav1.GetOptions{}); err != nil {
		t.Fatalf("the chart was not installed: %v", err)
	}

	// Installing again with another product adds it: one new instance and
	// its token Secrets, and an upgrade in place.
	out = ntctl(t, 0, "install", "--yes", "--namespace", ns, "--chart", chart, "--products", "trustguard", "--save-values", "", "--timeout", "1m")
	for _, want := range []string{"is already installed", "Adding trustguard", "Upgraded.", "Added trustguard"} {
		if !strings.Contains(out, want) {
			t.Errorf("adding a product: output lacks %q:\n%s", want, out)
		}
	}
	if got := srv.Instances(api.TrustGuard, "org-1"); len(got) != 1 {
		t.Fatalf("adding TrustGuard created %d instances, want 1", len(got))
	}
	if got := secretValue(t, cs, ns, values.TrustGuardConfigSync, values.ConfigSyncKey); got == "" {
		t.Fatal("the added product has no config-sync Secret")
	}
	// The same again changes nothing and asks the console for nothing.
	issued := len(srv.Issued())
	out = ntctl(t, 0, "install", "--yes", "--namespace", ns, "--chart", chart, "--products", "trustguard", "--save-values", "")
	if !strings.Contains(out, "nothing to do") || len(srv.Issued()) != issued {
		t.Errorf("a repeated add was not a no-op (%d -> %d configs):\n%s", issued, len(srv.Issued()), out)
	}
}

// TestE2EDoctorAndSupportBundle installs the chart with tokens that cannot
// work and a registry key that cannot pull, then checks doctor says why, and
// the support bundle holds the evidence but none of the tokens.
func TestE2EDoctorAndSupportBundle(t *testing.T) {
	const ns = "ntctl-e2e-doctor"
	cluster(t, ns)
	t.Setenv("NTCTL_CONFIG_DIR", t.TempDir())
	for _, env := range []string{"NT_TRUSTGATE_CONFIG_SYNC_TOKEN", "NT_TRUSTGATE_ENROLMENT_TOKEN"} {
		t.Setenv(env, "CANARY-"+strings.ToLower(env)+"-x9f3k2m8q7w1z5r4t6")
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "registry-key.json")
	if err := os.WriteFile(key, []byte(`{"type":"service_account","project_id":"e2e","client_email":"e2e@e2e.iam.gserviceaccount.com","private_key":"CANARY-registry-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ntctl(t, 1, "install", "--yes", "--ignore-capacity", "--namespace", ns, "--domain", "doctor.example.com",
		"--products", "trustgate", "--registry-key", key, "--save-values", "", "--timeout", "30s")

	out := ntctl(t, 3, "doctor", "--namespace", ns)
	for _, want := range []string{"NT-DR-001", "release neuraltrust-platform revision 1", "workloads cannot pull their images: the registry refused the credentials", "gcr-secret", "fix (NT-DR-010)"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output lacks %q:\n%s", want, out)
		}
	}
	if js := ntctl(t, 3, "doctor", "--namespace", ns, "--output", "json"); !strings.HasPrefix(strings.TrimSpace(js), "[") {
		t.Errorf("doctor --output json is not JSON:\n%s", js)
	}

	file := filepath.Join(dir, "bundle.tar.gz")
	out = ntctl(t, 0, "support-bundle", "--namespace", ns, "--output-file", file)
	if !strings.Contains(out, "Wrote "+file) {
		t.Fatalf("support-bundle output:\n%s", out)
	}
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	names := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		names[h.Name] = true
		if strings.Contains(string(b), "CANARY") {
			t.Errorf("%s in the bundle holds a secret", h.Name)
		}
	}
	for _, want := range []string{"README.md", "doctor.txt", "secrets.yaml", "release/values.yaml", "release/manifest.yaml", "resources/pods.yaml", "resources/events.yaml", "nodes.yaml"} {
		if !names[want] {
			t.Errorf("the bundle lacks %s", want)
		}
	}
}

// tinyChart writes a chart that becomes ready at once: a pause Deployment, a
// ConfigMap and a Secret. selector and level vary between versions.
func tinyChart(t *testing.T, version, selector, level string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"Chart.yaml": "apiVersion: v2\nname: neuraltrust-platform\nversion: " + version + "\n",
		"templates/app.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: tiny
spec:
  replicas: 1
  selector:
    matchLabels: {app: ` + selector + `}
  template:
    metadata:
      labels: {app: ` + selector + `}
    spec:
      containers:
      - name: pause
        image: registry.k8s.io/pause:3.10
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: tiny
data:
  level: ` + level + `
  domain: {{ .Values.global.domain | quote }}
  sub: {{ .Subcharts.sub.Chart.Version | quote }}
---
apiVersion: v1
kind: Secret
metadata:
  name: tiny
stringData:
  password: {{ .Values.extra.password | quote }}
`,
		// A subchart: Helm does not keep it in the stored release, which
		// is what adopt once tripped on.
		"charts/sub/Chart.yaml":        "apiVersion: v2\nname: sub\nversion: 0.1.0\n",
		"charts/sub/templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: tiny-sub\ndata:\n  level: " + level + "\n",
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestE2EAdoptUpgradeRollback takes a release installed with plain Helm through
// adopt, a safe upgrade, a refused unsafe upgrade, and a rollback.
func TestE2EAdoptUpgradeRollback(t *testing.T) {
	const ns = "ntctl-e2e-upgrade"
	cs := cluster(t, ns)
	t.Setenv("NTCTL_CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	k := kube.Options{Kubeconfig: os.Getenv("NTCTL_E2E_KUBECONFIG"), Namespace: ns}
	cfg, err := k.Helm()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	v1Dir := tinyChart(t, "3.0.0", "tiny", "info")
	v1, err := chart.Load(k.Settings(), chart.Source{Path: v1Dir})
	if err != nil {
		t.Fatal(err)
	}
	vals := map[string]any{"global": map[string]any{"domain": "upgrade.example.com"}, "extra": map[string]any{"password": "CANARY-pw"}}
	if _, err := helm.Install(ctx, cfg, v1, vals, "neuraltrust-platform", ns, 2*time.Minute); err != nil {
		t.Fatalf("plain install: %v", err)
	}

	// adopt changes nothing and writes the values without the password.
	before := versions(t, cs, ns)
	file := filepath.Join(t.TempDir(), "values.yaml")
	out := ntctl(t, 0, "adopt", "--namespace", ns, "--chart", v1Dir, "--values-file", file)
	if !strings.Contains(out, "re-renders exactly as deployed") || !strings.Contains(out, "extra.password") {
		t.Fatalf("adopt output:\n%s", out)
	}
	noCanary(t, file)
	if after := versions(t, cs, ns); !reflect.DeepEqual(before, after) {
		t.Fatalf("adopt changed the cluster: %v -> %v", before, after)
	}
	noNtctlSecrets(t, cs, ns)

	// A safe change is shown, then applied.
	v2 := tinyChart(t, "3.0.1", "tiny", "debug")
	out = ntctl(t, 0, "upgrade", "--namespace", ns, "--chart", v2, "--dry-run")
	if !strings.Contains(out, "~ ConfigMap/tiny") || !strings.Contains(out, "Dry run: nothing was changed") {
		t.Fatalf("dry run:\n%s", out)
	}
	out = ntctl(t, 0, "upgrade", "--namespace", ns, "--chart", v2, "--yes", "--timeout", "2m")
	if !strings.Contains(out, "Upgraded.") {
		t.Fatalf("upgrade:\n%s", out)
	}
	if cm, _ := cs.CoreV1().ConfigMaps(ns).Get(ctx, "tiny", metav1.GetOptions{}); cm.Data["level"] != "debug" {
		t.Fatalf("level = %q after the upgrade", cm.Data["level"])
	}
	if s, _ := cs.CoreV1().Secrets(ns).Get(ctx, "tiny", metav1.GetOptions{}); string(s.Data["password"]) != "CANARY-pw" {
		t.Fatal("the upgrade lost the release's values")
	}
	noNtctlSecrets(t, cs, ns)

	// A new selector is refused, and nothing changes.
	before = versions(t, cs, ns)
	out = ntctl(t, 3, "upgrade", "--namespace", ns, "--chart", tinyChart(t, "3.0.2", "tiny-v2", "debug"), "--yes")
	if !strings.Contains(out, "spec.selector is immutable") || !strings.Contains(out, "Stopped: nothing was changed") {
		t.Fatalf("unsafe upgrade:\n%s", out)
	}
	if after := versions(t, cs, ns); !reflect.DeepEqual(before, after) {
		t.Fatalf("a refused upgrade changed the cluster: %v -> %v", before, after)
	}

	// History, then back to revision 1.
	out = ntctl(t, 0, "history", "--namespace", ns)
	if !strings.Contains(out, "3.0.0") || !strings.Contains(out, "3.0.1") {
		t.Fatalf("history:\n%s", out)
	}
	out = ntctl(t, 0, "rollback", "--namespace", ns, "--to", "1", "--yes", "--timeout", "2m")
	if !strings.Contains(out, "~ ConfigMap/tiny") || !strings.Contains(out, "Rolled back: the new revision 3 runs what revision 1 ran") {
		t.Fatalf("rollback:\n%s", out)
	}
	if cm, _ := cs.CoreV1().ConfigMaps(ns).Get(ctx, "tiny", metav1.GetOptions{}); cm.Data["level"] != "info" {
		t.Fatalf("level = %q after the rollback", cm.Data["level"])
	}
}

// versions returns the resourceVersion of every object the tiny chart makes.
func versions(t *testing.T, cs kubernetes.Interface, ns string) map[string]string {
	t.Helper()
	ctx := context.Background()
	out := map[string]string{}
	d, err := cs.AppsV1().Deployments(ns).Get(ctx, "tiny", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out["deployment"] = d.ResourceVersion
	c, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, "tiny", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out["configmap"] = c.ResourceVersion
	s, err := cs.CoreV1().Secrets(ns).Get(ctx, "tiny", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out["secret"] = s.ResourceVersion
	return out
}

// noNtctlSecrets fails when ntctl created a Secret in a release it did not
// install: adopt and upgrade reuse the release's values, and never add the
// datastore or token Secrets an ntctl install would.
func noNtctlSecrets(t *testing.T, cs kubernetes.Interface, ns string) {
	t.Helper()
	list, err := cs.CoreV1().Secrets(ns).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list.Items {
		if s.Labels["app.kubernetes.io/managed-by"] == "ntctl" || strings.HasPrefix(s.Name, "external-") || strings.HasPrefix(s.Name, "ntctl-") {
			t.Errorf("Secret %s was created in a release ntctl did not install", s.Name)
		}
	}
}
