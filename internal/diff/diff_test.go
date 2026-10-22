package diff

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/NeuralTrust/ntctl/internal/manifest"
)

const base = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: proxy
  labels:
    helm.sh/chart: neuraltrust-platform-3.0.2
    app.kubernetes.io/version: "3.0.2"
spec:
  replicas: 2
  selector:
    matchLabels: {app: proxy}
  template:
    metadata:
      annotations:
        checksum/config: aaa
    spec:
      containers:
      - name: proxy
        image: registry.example.com/proxy:1.0.0
---
apiVersion: v1
kind: Secret
metadata:
  name: tokens
stringData:
  TOKEN: CANARY-old
  OTHER: same
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: db
spec:
  serviceName: db
  selector:
    matchLabels: {app: db}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
spec:
  storageClassName: standard
  accessModes: [ReadWriteOnce]
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: settings
data:
  level: info
`

func parse(t *testing.T, s string) []manifest.Object {
	t.Helper()
	objs, err := manifest.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return objs
}

func compare(t *testing.T, to string) Result {
	t.Helper()
	return Compare(parse(t, base), parse(t, to))
}

func TestNothingMeaningfulChanged(t *testing.T) {
	// A new chart version and new checksums alone are not changes.
	to := strings.NewReplacer("neuraltrust-platform-3.0.2", "neuraltrust-platform-3.0.3", `"3.0.2"`, `"3.0.3"`, "checksum/config: aaa", "checksum/config: bbb").Replace(base)
	if r := compare(t, to); !r.Empty() {
		t.Fatalf("volatile labels and checksums should not count: %+v", r.Changes)
	}
}

func TestSafeChanges(t *testing.T) {
	to := strings.NewReplacer("replicas: 2", "replicas: 3", "proxy:1.0.0", "proxy:1.1.0", "level: info", "level: debug").Replace(base) +
		"---\napiVersion: v1\nkind: Service\nmetadata:\n  name: new\n"
	r := compare(t, to)
	if len(r.Blocking()) != 0 {
		t.Fatalf("replicas, image and ConfigMap changes are safe: %+v", r.Blocking())
	}
	ops := map[string]Op{}
	for _, c := range r.Changes {
		ops[c.Kind+"/"+c.Name] = c.Op
	}
	if ops["Deployment/proxy"] != Changed || ops["ConfigMap/settings"] != Changed || ops["Service/new"] != Added {
		t.Fatalf("ops = %v", ops)
	}
}

func TestBlockingChanges(t *testing.T) {
	cases := map[string]struct{ from, to, want string }{
		"deployment selector": {"matchLabels: {app: proxy}", "matchLabels: {app: gateway}", "spec.selector is immutable"},
		"statefulset service": {"serviceName: db", "serviceName: db-headless", "spec.serviceName is immutable"},
		"pvc class":           {"storageClassName: standard", "storageClassName: fast", "spec.storageClassName is immutable"},
		"pvc access":          {"accessModes: [ReadWriteOnce]", "accessModes: [ReadWriteMany]", "spec.accessModes is immutable"},
		"secret key removed":  {"  OTHER: same\n", "", "removes Secret keys OTHER"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := compare(t, strings.Replace(base, c.from, c.to, 1))
			b := r.Blocking()
			if len(b) != 1 || !strings.Contains(strings.Join(b[0].Blocking, ";"), c.want) {
				t.Fatalf("blocking = %+v, want %q", b, c.want)
			}
		})
	}
}

func TestRemovingDataKindsBlocks(t *testing.T) {
	without := func(kind string) string {
		var keep []string
		for _, doc := range strings.Split(base, "---\n") {
			if !strings.Contains(doc, "kind: "+kind+"\n") {
				keep = append(keep, doc)
			}
		}
		return strings.Join(keep, "---\n")
	}
	for _, kind := range []string{"Secret", "StatefulSet", "PersistentVolumeClaim"} {
		if b := compare(t, without(kind)).Blocking(); len(b) != 1 || b[0].Op != Removed {
			t.Errorf("removing a %s should block: %+v", kind, b)
		}
	}
	if b := compare(t, without("ConfigMap")).Blocking(); len(b) != 0 {
		t.Errorf("removing a ConfigMap is safe: %+v", b)
	}
}

func TestSecretValuesNeverShown(t *testing.T) {
	r := compare(t, strings.Replace(base, "TOKEN: CANARY-old", "TOKEN: CANARY-new", 1))
	if len(r.Changes) != 1 || r.Changes[0].SecretKeys == nil || r.Changes[0].SecretKeys.Changed[0] != "TOKEN" {
		t.Fatalf("a new Secret value should be reported by key: %+v", r.Changes)
	}
	var out bytes.Buffer
	r.Print(&out, true)
	if strings.Contains(out.String(), "CANARY") {
		t.Fatalf("a Secret value was printed:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "new values: TOKEN") {
		t.Fatalf("output should name the key:\n%s", out.String())
	}
}

func TestSecretDataAndStringDataAgree(t *testing.T) {
	// The same value as base64 data and as stringData is not a change.
	// The base64 is computed so secret scanners do not mistake it for a key.
	enc := base64.StdEncoding.EncodeToString
	to := strings.Replace(base, "stringData:\n  TOKEN: CANARY-old\n  OTHER: same",
		"data:\n  TOKEN: "+enc([]byte("CANARY-old"))+"\n  OTHER: "+enc([]byte("same")), 1)
	r := compare(t, to)
	for _, c := range r.Changes {
		if c.SecretKeys != nil {
			t.Fatalf("equal values in data and stringData reported as changed: %+v", c.SecretKeys)
		}
	}
}

func TestPrintShowsDiff(t *testing.T) {
	var out bytes.Buffer
	compare(t, strings.Replace(base, "replicas: 2", "replicas: 3", 1)).Print(&out, true)
	for _, want := range []string{"0 added, 1 changed, 0 removed", "~ Deployment/proxy", "-  replicas: 2", "+  replicas: 3"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestImageChanges(t *testing.T) {
	to := strings.Replace(base, "registry.example.com/proxy:1.0.0", "registry.example.com/proxy:1.1.0", 1) + `---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: worker
spec:
  template:
    spec:
      containers:
      - name: worker
        image: registry.example.com/worker:2.0.0
`
	from := base + `---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: worker
spec:
  template:
    spec:
      initContainers:
      - name: migrate
        image: registry.example.com/proxy:1.0.0
      containers:
      - name: worker
        image: other.example.com/worker:2.0.0
`
	res := Compare(parse(t, from), parse(t, to))
	var proxy, worker Change
	for _, c := range res.Changes {
		switch c.Name {
		case "proxy":
			proxy = c
		case "worker":
			worker = c
		}
	}
	if len(proxy.Images) != 1 || proxy.Images[0].String() != "proxy 1.0.0 → 1.1.0" {
		t.Fatalf("proxy images = %+v", proxy.Images)
	}
	// A new registry shows both references; the removed init container is
	// not an image change.
	if len(worker.Images) != 1 || worker.Images[0].String() != "other.example.com/worker:2.0.0 → registry.example.com/worker:2.0.0" {
		t.Fatalf("worker images = %+v", worker.Images)
	}
	if got := len(res.Components()); got != 2 {
		t.Fatalf("components = %+v, want 2", res.Components())
	}
	var out bytes.Buffer
	res.Print(&out, false)
	for _, want := range []string{"Image versions:\n  other.example.com", "  proxy 1.0.0 → 1.1.0\n", "~ Deployment/proxy  proxy 1.0.0 → 1.1.0"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestComponentsCountsAnImageOnce(t *testing.T) {
	two := func(tag string) string {
		var b strings.Builder
		for _, n := range []string{"a", "b"} {
			b.WriteString("---\napiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: " + n +
				"\nspec:\n  template:\n    spec:\n      containers:\n      - name: w\n        image: example.com/fw@" + tag + "\n")
		}
		return b.String()
	}
	res := Compare(parse(t, two("sha256:aa")), parse(t, two("sha256:bb")))
	if c := res.Components(); len(c) != 1 || c[0].String() != "fw sha256:aa → sha256:bb" {
		t.Fatalf("components = %+v", c)
	}
}
