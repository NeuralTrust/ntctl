package manifest

import "testing"

const rendered = `---
# Source: c/templates/secret.yaml
apiVersion: v1
kind: Secret
metadata:
  name: made-by-chart
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
spec:
  resources: {requests: {storage: 10Gi}}
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: web
spec:
  ingressClassName: nginx
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  replicas: 2
  template:
    spec:
      imagePullSecrets: [{name: gcr-secret}]
      volumes:
        - name: tls
          secret: {secretName: tls-cert}
      containers:
        - name: api
          resources: {requests: {cpu: 250m, memory: 512Mi}}
          env:
            - name: TOKEN
              valueFrom: {secretKeyRef: {name: tokens, key: CONFIG_SYNC_TOKEN}}
            - name: OPTIONAL
              valueFrom: {secretKeyRef: {name: maybe, key: X, optional: true}}
            - name: OWN
              valueFrom: {secretKeyRef: {name: made-by-chart, key: Y}}
          envFrom:
            - secretRef: {name: bulk}
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: db
spec:
  volumeClaimTemplates:
    - metadata: {name: data}
      spec: {storageClassName: fast}
  template:
    spec:
      containers:
        - name: db
          resources: {requests: {cpu: "1", memory: 1Gi}}
`

func TestAnalyse(t *testing.T) {
	objs, err := Parse(rendered)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 5 {
		t.Fatalf("parsed %d objects, want 5", len(objs))
	}
	n := Analyse(objs)

	var got []string
	for _, s := range n.Secrets {
		got = append(got, s.Name+"/"+s.Key)
	}
	want := []string{"bulk/", "tls-cert/", "tokens/CONFIG_SYNC_TOKEN"}
	if len(got) != len(want) {
		t.Fatalf("secrets = %v, want %v (optional and chart-made Secrets excluded)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("secrets = %v, want %v", got, want)
		}
	}
	if len(n.PullSecrets) != 1 || n.PullSecrets[0] != "gcr-secret" {
		t.Errorf("pull secrets = %v", n.PullSecrets)
	}
	if !n.DefaultStorage || len(n.StorageClasses) != 1 || n.StorageClasses[0] != "fast" {
		t.Errorf("storage: default=%v classes=%v", n.DefaultStorage, n.StorageClasses)
	}
	if n.DefaultIngress || len(n.IngressClasses) != 1 || n.IngressClasses[0] != "nginx" {
		t.Errorf("ingress: default=%v classes=%v", n.DefaultIngress, n.IngressClasses)
	}
	// 2 x 250m + 1 CPU; 2 x 512Mi + 1Gi.
	if n.CPUMilli != 1500 {
		t.Errorf("cpu = %dm, want 1500m", n.CPUMilli)
	}
	if n.Memory != 2<<30 {
		t.Errorf("memory = %d, want 2Gi", n.Memory)
	}
}
