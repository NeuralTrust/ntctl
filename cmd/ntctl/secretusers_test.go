package main

import (
	"context"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/NeuralTrust/ntctl/internal/gatewayapi/gatewayapitest"
	"github.com/NeuralTrust/ntctl/internal/manifest"
)

func TestSecretUsers(t *testing.T) {
	https := gatewayapitest.Listener("https", 443, map[string]any{"from": "All"})
	https["tls"] = map[string]any{"certificateRefs": []any{
		map[string]any{"name": "served-tls", "namespace": "nt"},
		map[string]any{"name": "gateway-own-tls"},
	}}
	dyn := gatewayapitest.Client(gatewayapitest.Gateway("kgateway-system", "internal", "10.0.6.10", https))
	ingress := func(name, secret string) *networkingv1.Ingress {
		return &networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "nt"},
			Spec:       networkingv1.IngressSpec{TLS: []networkingv1.IngressTLS{{SecretName: secret}}},
		}
	}
	cs := fake.NewSimpleClientset(ingress("operators-own", "operator-tls"), ingress("release-app", "release-tls"))
	release, err := manifest.Parse("apiVersion: networking.k8s.io/v1\nkind: Ingress\nmetadata:\n  name: release-app\n")
	if err != nil {
		t.Fatal(err)
	}
	users := secretUsers(context.Background(), cs, dyn, "nt", release)
	for name, want := range map[string]string{
		"served-tls":      "Gateway kgateway-system/internal listener https",
		"operator-tls":    "Ingress nt/operators-own",
		"release-tls":     "", // the release's own Ingress is the manifest check's job
		"gateway-own-tls": "", // a Secret in the Gateway's namespace, not the install's
		"unused":          "",
	} {
		if got := users(name); got != want {
			t.Errorf("users(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestSecretUsersWhenTheClusterCannotBeRead(t *testing.T) {
	users := unknownUsers(context.DeadlineExceeded)
	if got := users("anything"); !strings.Contains(got, "could not check") {
		t.Fatalf("an unreadable cluster must keep every removal blocking: %q", got)
	}
}
