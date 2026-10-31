package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/NeuralTrust/ntctl/internal/generator"
)

func TestCreatePlanNeverChangesAValue(t *testing.T) {
	ctx := context.Background()
	managed := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "platform-secrets", Namespace: "nt",
		Annotations: map[string]string{generator.Marker: "managed"}}, Data: map[string][]byte{"KEPT": []byte("live")}}
	cs := fake.NewClientset(managed)
	plan := &generator.Plan{Secrets: []generator.Secret{
		{Name: "platform-secrets", Type: corev1.SecretTypeOpaque, Owner: generator.Managed,
			Data: map[string]string{"KEPT": "would-differ", "NEW": "n"}},
		{Name: "trustguard-configsync-tls", Type: corev1.SecretTypeTLS, Owner: generator.Absent,
			Data: map[string]string{"tls.crt": "c", "tls.key": "k", "ca.crt": "a"}, Annotations: map[string]string{"neuraltrust.ai/tls-sans": "abc"}},
	}}
	var out bytes.Buffer
	if err := createPlan(ctx, &out, cs, "nt", plan); err != nil {
		t.Fatal(err)
	}
	got, _ := cs.CoreV1().Secrets("nt").Get(ctx, "platform-secrets", metav1.GetOptions{})
	if string(got.Data["KEPT"]) != "live" || string(got.Data["NEW"]) != "n" {
		t.Errorf("data = %v; a value already there must stay, a missing key is added", got.Data)
	}
	tls, err := cs.CoreV1().Secrets("nt").Get(ctx, "trustguard-configsync-tls", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if tls.Type != corev1.SecretTypeTLS || tls.Labels["app.kubernetes.io/managed-by"] != generator.ManagedBy ||
		tls.Annotations[generator.Marker] != "managed" || tls.Annotations["neuraltrust.ai/tls-sans"] != "abc" {
		t.Errorf("a created Secret should carry the generator's own metadata: %+v", tls.ObjectMeta)
	}
	for _, want := range []string{"Secret platform-secrets: added NEW", "Secret trustguard-configsync-tls: created with 3 keys"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestCreatePlanLeavesTheOperatorsSecret(t *testing.T) {
	ctx := context.Background()
	theirs := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "platform-secrets", Namespace: "nt"}, Data: map[string][]byte{"A": []byte("x")}}
	cs := fake.NewClientset(theirs)
	plan := &generator.Plan{Secrets: []generator.Secret{{Name: "platform-secrets", Owner: generator.External,
		Data: map[string]string{"A": "x"}, Missing: []string{"B"}}}}
	err := createPlan(ctx, &bytes.Buffer{}, cs, "nt", plan)
	if err == nil || !strings.Contains(err.Error(), "platform-secrets is yours and lacks B") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := cs.CoreV1().Secrets("nt").Get(ctx, "platform-secrets", metav1.GetOptions{}); len(got.Data) != 1 {
		t.Errorf("an operator's Secret was written: %v", got.Data)
	}
}

func TestPrintPlan(t *testing.T) {
	plan := &generator.Plan{Secrets: []generator.Secret{
		{Name: "platform-secrets", Type: corev1.SecretTypeOpaque, Owner: generator.Absent, Data: map[string]string{"K": "v"}},
		{Name: "trustguard-configsync-tls", Type: corev1.SecretTypeTLS, Owner: generator.External, Missing: []string{"ca.crt"}},
	}}
	var out bytes.Buffer
	err := printPlan(&out, plan, "nt", "json")
	if err == nil || !strings.Contains(err.Error(), "trustguard-configsync-tls (yours) lacks ca.crt") {
		t.Errorf("err = %v", err)
	}
	var flat map[string]map[string]string
	if err := json.Unmarshal(out.Bytes(), &flat); err != nil || len(flat) != 1 || flat["platform-secrets"]["K"] != "v" {
		t.Errorf("json = %s (%v); an operator's Secret is not printed", out.String(), err)
	}
	out.Reset()
	if err := printPlan(&out, &generator.Plan{Secrets: plan.Secrets[:1]}, "nt", "yaml"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"kind: Secret", "namespace: nt", "type: Opaque", "K: v", "Real credentials"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("yaml lacks %q:\n%s", want, out.String())
		}
	}
}
