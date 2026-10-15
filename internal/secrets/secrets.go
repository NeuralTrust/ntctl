// Package secrets builds and applies the Secrets ntctl manages: the registry
// pull secret and the per-product console tokens. Values are never logged.
package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/NeuralTrust/ntctl/internal/redact"
	"github.com/NeuralTrust/ntctl/internal/values"
)

// PullSecret is the name every chart component pulls images with by default.
const PullSecret = "gcr-secret"

// Registry hosts the NeuralTrust images.
const Registry = "europe-west1-docker.pkg.dev"

// Tokens are the console-issued credentials for each product.
type Tokens struct {
	TrustGateConfigSync  string
	TrustGateEnrolment   string
	TrustGuardConfigSync string
	TrustGuardEnrolment  string
}

// Spec is one Secret ntctl can create: its name, key, and where the value
// comes from.
type Spec struct {
	Name string
	Key  string
	// Env is the environment variable the value is read from; Prompt is the
	// question asked when it is unset.
	Env    string
	Prompt string
	// Product is the product the token belongs to: trustgate or trustguard.
	Product string
}

// TokenSpecs lists the token Secrets the enabled products need.
func TokenSpecs(products []string) []Spec {
	var out []Spec
	if has(products, "trustgate") {
		out = append(out,
			Spec{values.TrustGateConfigSync, values.ConfigSyncKey, "NT_TRUSTGATE_CONFIG_SYNC_TOKEN", "TrustGate config-sync token", "trustgate"},
			Spec{values.TrustGateEnrolment, values.EnrolmentKey, "NT_TRUSTGATE_ENROLMENT_TOKEN", "TrustGate DataAgent enrolment token", "trustgate"})
	}
	if has(products, "trustguard") {
		out = append(out,
			Spec{values.TrustGuardConfigSync, values.ConfigSyncKey, "NT_TRUSTGUARD_CONFIG_SYNC_TOKEN", "TrustGuard config-sync token", "trustguard"},
			Spec{values.TrustGuardEnrolment, values.EnrolmentKey, "NT_TRUSTGUARD_ENROLMENT_TOKEN", "TrustGuard DataAgent enrolment token", "trustguard"})
	}
	return out
}

// Token builds an Opaque Secret holding one token.
func Token(ns string, s Spec, value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: meta(ns, s.Name),
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{s.Key: []byte(value)},
	}
}

// Pull builds the NeuralTrust registry pull Secret from a JSON service-account
// key, the same Secret create-image-pull-secret.sh creates.
func Pull(ns string, key []byte) (*corev1.Secret, error) {
	var parsed map[string]any
	if err := json.Unmarshal(key, &parsed); err != nil {
		return nil, errors.New("the registry key is not valid JSON; pass the key file NeuralTrust sent you")
	}
	if parsed["private_key"] == nil {
		return nil, errors.New("the registry key has no private_key; pass the service-account key file NeuralTrust sent you")
	}
	return DockerRegistry(ns, PullSecret, Registry, "_json_key", string(bytes.TrimSpace(key)))
}

// DockerRegistry builds an image pull Secret for one registry server.
func DockerRegistry(ns, name, server, username, password string) (*corev1.Secret, error) {
	if server == "" || username == "" {
		return nil, errors.New("a registry pull Secret needs a server and a username")
	}
	cfg := map[string]any{"auths": map[string]any{server: map[string]string{
		"username": username,
		"password": password,
		"auth":     base64.StdEncoding.EncodeToString([]byte(username + ":" + password)),
	}}}
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("encoding registry credentials: %w", err)
	}
	return &corev1.Secret{
		ObjectMeta: meta(ns, name),
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: b},
	}, nil
}

// Opaque builds a plain Secret from string values.
func Opaque(ns, name string, data map[string]string) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: meta(ns, name), Type: corev1.SecretTypeOpaque, Data: map[string][]byte{}}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

// Keys returns the data keys of a Secret in the namespace, and whether it
// exists.
func Keys(ctx context.Context, cs kubernetes.Interface, ns, name string) ([]string, bool, error) {
	s, err := cs.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	keys := make([]string, 0, len(s.Data)+len(s.StringData))
	for k := range s.Data {
		keys = append(keys, k)
	}
	for k := range s.StringData {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, true, nil
}

// ErrDiffers is returned when a Secret exists with different content.
var ErrDiffers = errors.New("exists with different content")

// Apply creates each Secret, leaves identical ones alone, and refuses to
// overwrite a different one unless replace is set. It returns what it did.
func Apply(ctx context.Context, cs kubernetes.Interface, list []*corev1.Secret, replace bool) ([]string, error) {
	var done []string
	for _, want := range list {
		api := cs.CoreV1().Secrets(want.Namespace)
		have, err := api.Get(ctx, want.Name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			if _, err := api.Create(ctx, want, metav1.CreateOptions{}); err != nil {
				return done, fmt.Errorf("creating Secret %s: %w", want.Name, err)
			}
			done = append(done, "created "+want.Name)
		case err != nil:
			return done, fmt.Errorf("reading Secret %s: %w", want.Name, err)
		case have.Type == want.Type && sameData(have.Data, want.Data):
			done = append(done, "kept "+want.Name)
		case !replace:
			return done, fmt.Errorf("secret %s %w; pass --replace-secrets to overwrite it", want.Name, ErrDiffers)
		default:
			have.Type = want.Type
			have.Data = want.Data
			if _, err := api.Update(ctx, have, metav1.UpdateOptions{}); err != nil {
				return done, fmt.Errorf("updating Secret %s: %w", want.Name, err)
			}
			done = append(done, "replaced "+want.Name)
		}
	}
	return done, nil
}

// Commands returns the kubectl commands that create the Secrets, for an
// operator whose policy is that only they write Secrets. Values whose key
// names a credential become <KEY> placeholders; the rest, such as a host or a
// port, are printed as they are. The NeuralTrust registry key is read from
// keyFile with $(cat …).
func Commands(list []*corev1.Secret, keyFile string) string {
	if keyFile == "" {
		keyFile = "registry-key.json"
	}
	var b strings.Builder
	for _, s := range list {
		fmt.Fprintf(&b, "kubectl -n %s create secret ", s.Namespace)
		if s.Type == corev1.SecretTypeDockerConfigJson {
			server, user := registryAuth(s)
			password := quote("<registry password>")
			if user == "_json_key" {
				password = `"$(cat ` + quote(keyFile) + `)"`
			}
			fmt.Fprintf(&b, "docker-registry %s \\\n  --docker-server=%s \\\n  --docker-username=%s \\\n  --docker-password=%s\n",
				s.Name, quote(server), quote(user), password)
			continue
		}
		b.WriteString("generic " + s.Name)
		keys := make([]string, 0, len(s.Data))
		for k := range s.Data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := string(s.Data[k])
			if redact.SensitiveKey(k) {
				v = "<" + k + ">"
			}
			fmt.Fprintf(&b, " \\\n  --from-literal=%s=%s", k, quote(v))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// registryAuth returns the server and username of a docker-registry Secret.
func registryAuth(s *corev1.Secret) (server, user string) {
	var cfg struct {
		Auths map[string]struct{ Username string } `json:"auths"`
	}
	if json.Unmarshal(s.Data[corev1.DockerConfigJsonKey], &cfg) == nil {
		for k, v := range cfg.Auths {
			return k, v.Username
		}
	}
	return "<registry server>", "<registry username>"
}

// quote single-quotes a value for a POSIX shell.
func quote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// HasNamespace reports whether the namespace exists.
func HasNamespace(ctx context.Context, cs kubernetes.Interface, ns string) (bool, error) {
	_, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading namespace %s: %w", ns, err)
	}
	return true, nil
}

// Namespace creates the namespace when it does not exist.
func Namespace(ctx context.Context, cs kubernetes.Interface, ns string) (bool, error) {
	_, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err == nil {
		return false, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("reading namespace %s: %w", ns, err)
	}
	_, err = cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return false, fmt.Errorf("creating namespace %s: %w", ns, err)
	}
	return true, nil
}

func meta(ns, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		Namespace: ns,
		Labels:    map[string]string{"app.kubernetes.io/managed-by": "ntctl"},
	}
}

func sameData(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !bytes.Equal(v, b[k]) {
			return false
		}
	}
	return true
}

func has(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
