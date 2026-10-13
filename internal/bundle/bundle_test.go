package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	rcommon "helm.sh/helm/v4/pkg/release/common"
	release "helm.sh/helm/v4/pkg/release/v1"

	"github.com/NeuralTrust/ntctl/internal/preflight"
)

const ns = "neuraltrust"

// TestNoSecretLeaks seeds an install with fake secrets everywhere one can
// hide and checks none reaches the bundle, while what support needs stays.
func TestNoSecretLeaks(t *testing.T) {
	cs := fake.NewClientset(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ntctl-postgresql", Namespace: ns}, Data: map[string][]byte{
			"POSTGRES_PASSWORD": []byte("CANARY-pg-password"),
			"POSTGRES_USER":     []byte("neuraltrust"),
			"ODDLY_NAMED":       []byte("CANARYx9f3k2m8q7w1z5r4t6"),
		}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "agentgateway-config-sync", Namespace: ns}, Data: map[string][]byte{
			"CONFIG_SYNC_TOKEN": []byte("CANARY-config-sync"),
		}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "proxy-config", Namespace: ns}, Data: map[string]string{
			"config.yaml":    "upstream: https://api.example.com\napi_key: CANARY-in-config\n",
			"DATABASE_URL":   "postgres://nt:CANARY-in-url@db:5432/nt",
			"COPIED_FROM_PG": "CANARYx9f3k2m8q7w1z5r4t6",
		}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "proxy", Namespace: ns}, Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Env: []corev1.EnvVar{
				{Name: "REDIS_PASSWORD", Value: "CANARY-env"},
				{Name: "LOG_LEVEL", Value: "info"},
			}}}}},
		}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "proxy-1", Namespace: ns}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app", RestartCount: 2}}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
	)
	rel := &release.Release{
		Name: "neuraltrust-platform", Version: 2, Namespace: ns,
		Chart: &chartv2.Chart{Metadata: &chartv2.Metadata{Version: "3.0.2"}},
		Info:  &release.Info{Status: rcommon.StatusDeployed},
		Config: map[string]any{
			"global":       map[string]any{"domain": "platform.example.com", "postgresql": map[string]any{"host": "db", "password": "CANARY-values"}},
			"agentgateway": map[string]any{"configSync": map[string]any{"token": "CANARY-values-token"}},
		},
		Manifest: "---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: platform-secrets\nstringData:\n  SIGNING_KEY: CANARY-manifest\n" +
			"---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: c\ndata:\n  JWT: eyJhbGciOiJIUzI1NiJ9.eyJDQU5BUlkiOjF9.Q0FOQVJZc2ln\n",
	}
	findings := []preflight.Result{{Code: "NT-DR-012", Check: "crashing", Severity: preflight.Fail, Message: "proxy keeps crashing",
		Detail: []string{"connecting with token=CANARY-detail"}}}

	var buf bytes.Buffer
	sum, err := Write(context.Background(), &buf, Input{
		Clientset: cs, Namespace: ns, Release: "neuraltrust-platform", Current: rel, History: []*release.Release{rel},
		Findings: findings, Versions: map[string]string{"ntctl": "test"}, Now: time.Unix(0, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	files := untar(t, &buf)
	if sum.Files != len(files) {
		t.Errorf("summary says %d files, archive has %d", sum.Files, len(files))
	}
	for name, body := range files {
		if strings.Contains(body, "CANARY") {
			t.Errorf("%s leaks a secret:\n%s", name, body)
		}
	}
	// What support needs is still there.
	for name, want := range map[string]string{
		"secrets.yaml":                  "CONFIG_SYNC_TOKEN",
		"release/values.yaml":           "platform.example.com",
		"release/manifest.yaml":         "SIGNING_KEY",
		"resources/configmaps.yaml":     "upstream: https://api.example.com",
		"resources/deployments.yaml":    "LOG_LEVEL",
		"logs/proxy-1/app.log":          "fake logs",
		"logs/proxy-1/app.previous.log": "fake logs",
		"nodes.yaml":                    "node-1",
		"README.md":                     "What was removed",
		"doctor.txt":                    "proxy keeps crashing",
		"versions.yaml":                 "ntctl: test",
	} {
		if !strings.Contains(files[name], want) {
			t.Errorf("%s should contain %q:\n%s", name, want, files[name])
		}
	}
	// A plain user name stored in a Secret must not be masked everywhere.
	if !strings.Contains(files["resources/deployments.yaml"], "namespace: neuraltrust") {
		t.Error("the namespace name was masked because it equals a non-secret Secret value")
	}
}

func untar(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(r)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = string(b)
	}
}
