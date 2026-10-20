package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

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
