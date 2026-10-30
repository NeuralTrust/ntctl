package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	release "helm.sh/helm/v4/pkg/release/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestUninstallFindsWhatItKeeps(t *testing.T) {
	ctx := context.Background()
	obj := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: "nt"} }
	cs := fake.NewClientset(
		&corev1.Secret{ObjectMeta: obj("gcr-secret")},
		&corev1.Secret{ObjectMeta: obj("agentgateway-config-sync")},
		&corev1.Secret{ObjectMeta: obj("onprem-superadmin")},
		&corev1.Secret{ObjectMeta: obj("someone-elses")},
		&corev1.PersistentVolumeClaim{ObjectMeta: obj("data-clickhouse-0")},
	)
	own, err := ownSecrets(ctx, cs, "nt")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(own, ","); got != "gcr-secret,onprem-superadmin,agentgateway-config-sync" {
		t.Errorf("own Secrets = %s; a Secret ntctl does not create must never be listed", got)
	}
	vols, err := volumeClaims(ctx, cs, "nt")
	if err != nil || len(vols) != 1 || vols[0] != "data-clickhouse-0" {
		t.Fatalf("volumes = %v, %v", vols, err)
	}
	var out bytes.Buffer
	if err := deleteEach(ctx, &out, "Secret", own, func(ctx context.Context, name string) error {
		return cs.CoreV1().Secrets("nt").Delete(ctx, name, metav1.DeleteOptions{})
	}); err != nil {
		t.Fatal(err)
	}
	if left, _ := cs.CoreV1().Secrets("nt").List(ctx, metav1.ListOptions{}); len(left.Items) != 1 || left.Items[0].Name != "someone-elses" {
		t.Errorf("left = %v; only the operator's own Secret should stay", left.Items)
	}
}

func TestUninstallFindsTheKeptCredentials(t *testing.T) {
	ctx := context.Background()
	obj := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: "nt"} }
	generated := obj("platform-secrets")
	generated.Labels = map[string]string{"app.kubernetes.io/managed-by": "neuraltrust-secrets-generator"}
	taken := obj("trustguard-configsync-tls")
	taken.Annotations = map[string]string{"neuraltrust.ai/secrets-generator": "managed"}
	external := obj("operator-platform-secrets")
	external.Annotations = map[string]string{"neuraltrust.ai/secrets-generator": "external"}
	cs := fake.NewClientset(
		&corev1.Secret{ObjectMeta: generated}, &corev1.Secret{ObjectMeta: taken}, &corev1.Secret{ObjectMeta: external},
		&corev1.Secret{ObjectMeta: obj("postgresql-secrets")}, &corev1.Secret{ObjectMeta: obj("clickhouse-secrets")},
		&corev1.Secret{ObjectMeta: obj("rendered")}, &corev1.Secret{ObjectMeta: obj("onprem-superadmin")},
	)
	rel := &release.Release{
		Manifest: "apiVersion: v1\nkind: Secret\nmetadata:\n  name: postgresql-secrets\n  annotations: {helm.sh/resource-policy: keep}\n" +
			"---\napiVersion: v1\nkind: Secret\nmetadata: {name: rendered}\n",
		Hooks: []*release.Hook{
			{Manifest: "apiVersion: v1\nkind: Secret\nmetadata: {name: clickhouse-secrets}\n"},
			{Manifest: "apiVersion: v1\nkind: Secret\nmetadata: {name: onprem-superadmin}\n"},
			{Manifest: "apiVersion: v1\nkind: Secret\nmetadata: {name: already-deleted-hook}\n"},
		},
	}
	kept, err := keptSecrets(ctx, cs, "nt", rel, []string{"onprem-superadmin"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(kept, ","); got != "clickhouse-secrets,platform-secrets,postgresql-secrets,trustguard-configsync-tls" {
		t.Errorf("kept = %s; want the generator's, the kept and the hook-made Secrets, never ntctl's own, a rendered one or an operator's", got)
	}
}
