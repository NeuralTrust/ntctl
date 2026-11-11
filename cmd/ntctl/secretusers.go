package main

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/NeuralTrust/ntctl/internal/diff"
	"github.com/NeuralTrust/ntctl/internal/gatewayapi"
	"github.com/NeuralTrust/ntctl/internal/manifest"
)

// secretUsers names what outside the release still serves a Secret of
// namespace ns as a certificate: a Gateway listener's tls.certificateRefs,
// from any namespace, or an Ingress's tls.secretName in ns. Objects the
// release renders itself are left to the manifest check. When the cluster
// cannot be read, every Secret is reported as possibly in use, so a removal
// keeps blocking as it did before this check existed.
func secretUsers(ctx context.Context, cs kubernetes.Interface, dyn dynamic.Interface, ns string, release []manifest.Object) diff.Referrer {
	own := map[string]bool{}
	for _, o := range release {
		on, _ := o["metadata"].(map[string]any)
		ons, _ := on["namespace"].(string)
		if ons == "" {
			ons = ns
		}
		own[o.Kind()+"/"+ons+"/"+o.Name()] = true
	}
	users := map[string]string{}
	add := func(secret, by string) {
		if _, ok := users[secret]; !ok {
			users[secret] = by
		}
	}
	gws, err := gatewayapi.ListGateways(ctx, dyn)
	if err != nil {
		return unknownUsers(err)
	}
	for _, g := range gws {
		if own["Gateway/"+g.Namespace+"/"+g.Name] {
			continue
		}
		for _, l := range g.Listeners {
			for _, ref := range l.Certificates {
				add(ref, fmt.Sprintf("Gateway %s listener %s", g.Ref(), l.Name))
			}
		}
	}
	ings, err := cs.NetworkingV1().Ingresses(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return unknownUsers(err)
	}
	for _, ing := range ings.Items {
		if own["Ingress/"+ns+"/"+ing.Name] {
			continue
		}
		for _, t := range ing.Spec.TLS {
			if t.SecretName != "" {
				add(ns+"/"+t.SecretName, "Ingress "+ns+"/"+ing.Name)
			}
		}
	}
	return func(name string) string { return users[ns+"/"+name] }
}

// unknownUsers reports every Secret as possibly in use, naming why.
func unknownUsers(err error) diff.Referrer {
	return func(string) string {
		return fmt.Sprintf("possibly a Gateway or Ingress ntctl could not check (%v)", err)
	}
}
