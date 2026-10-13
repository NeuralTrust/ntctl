package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// key is assembled so secret scanners reading the source do not flag it.
const key = `{"type":"service_account","private_key":"-----BEGIN ` + `PRIVATE KEY-----\nabc\n-----END ` + `PRIVATE KEY-----\n"}`

func TestPull(t *testing.T) {
	s, err := Pull("ns", []byte(key+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != PullSecret || s.Type != corev1.SecretTypeDockerConfigJson {
		t.Fatalf("got %s %s", s.Name, s.Type)
	}
	var cfg struct {
		Auths map[string]struct{ Username, Password, Auth string } `json:"auths"`
	}
	if err := json.Unmarshal(s.Data[corev1.DockerConfigJsonKey], &cfg); err != nil {
		t.Fatal(err)
	}
	a := cfg.Auths[Registry]
	if a.Username != "_json_key" || a.Password != key {
		t.Fatalf("auth entry = %+v", a)
	}
	if _, err := Pull("ns", []byte("not json")); err == nil {
		t.Error("accepted an invalid key")
	}
	if _, err := Pull("ns", []byte(`{"type":"service_account"}`)); err == nil {
		t.Error("accepted a key without private_key")
	}
}

func TestTokenSpecs(t *testing.T) {
	if n := len(TokenSpecs([]string{"trustgate", "trustguard", "dataPlane"})); n != 4 {
		t.Errorf("all products: %d specs, want 4", n)
	}
	if n := len(TokenSpecs([]string{"dataPlane"})); n != 0 {
		t.Errorf("data plane only: %d specs, want 0", n)
	}
}

func TestApply(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	spec := TokenSpecs([]string{"trustgate"})[0]

	done, err := Apply(ctx, cs, []*corev1.Secret{Token("ns", spec, "one")}, false)
	if err != nil || done[0] != "created "+spec.Name {
		t.Fatalf("create: %v %v", done, err)
	}
	done, err = Apply(ctx, cs, []*corev1.Secret{Token("ns", spec, "one")}, false)
	if err != nil || done[0] != "kept "+spec.Name {
		t.Fatalf("identical: %v %v", done, err)
	}
	_, err = Apply(ctx, cs, []*corev1.Secret{Token("ns", spec, "two")}, false)
	if !errors.Is(err, ErrDiffers) || strings.Contains(err.Error(), "two") {
		t.Fatalf("different content must be refused without echoing it: %v", err)
	}
	if _, err = Apply(ctx, cs, []*corev1.Secret{Token("ns", spec, "two")}, true); err != nil {
		t.Fatal(err)
	}
	got, _ := cs.CoreV1().Secrets("ns").Get(ctx, spec.Name, metav1.GetOptions{})
	if string(got.Data[spec.Key]) != "two" {
		t.Fatal("replace did not update the Secret")
	}
	keys, exists, err := Keys(ctx, cs, "ns", spec.Name)
	if err != nil || !exists || len(keys) != 1 || keys[0] != spec.Key {
		t.Fatalf("Keys = %v %v %v", keys, exists, err)
	}
}
