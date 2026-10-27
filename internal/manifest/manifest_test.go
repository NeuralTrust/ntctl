package manifest

import (
	"strings"
	"testing"
)

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

func TestAnalyseHTTPRoutes(t *testing.T) {
	objs, err := Parse(`---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: agentgateway-gateway
spec:
  parentRefs:
    - {name: shared, namespace: gateway-system, sectionName: https}
    - {name: local}
    - {name: svc, kind: Service, group: ""}
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: trustguard
spec:
  parentRefs:
    - {name: shared, namespace: gateway-system, sectionName: https}
`)
	if err != nil {
		t.Fatal(err)
	}
	n := Analyse(objs)
	if !n.HTTPRoutes || n.DefaultIngress || len(n.IngressClasses) != 0 {
		t.Errorf("needs = %+v", n)
	}
	if len(n.Gateways) != 2 ||
		n.Gateways[0].String() != "gateway-system/shared:https" ||
		n.Gateways[1].Name != "local" || n.Gateways[1].Namespace != "" {
		t.Errorf("gateways = %+v; want the two Gateway parents once each, the Service parent left out", n.Gateways)
	}
}

func TestAnalyseGroupsByPlacement(t *testing.T) {
	objs, err := Parse(`apiVersion: apps/v1
kind: Deployment
metadata: {name: api}
spec:
  replicas: 2
  template:
    spec:
      containers: [{name: c, resources: {requests: {cpu: 500m, memory: 1Gi}}}]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: worker}
spec:
  template:
    spec:
      nodeSelector: {accelerator: nvidia}
      tolerations: [{key: nvidia.com/gpu, operator: Exists, effect: NoSchedule}]
      containers: [{name: c, resources: {requests: {cpu: "1", memory: 4Gi}}}]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: worker2}
spec:
  template:
    spec:
      nodeSelector: {accelerator: nvidia}
      tolerations: [{key: nvidia.com/gpu, operator: Exists, effect: NoSchedule}]
      containers: [{name: c, resources: {requests: {cpu: "2", memory: 2Gi}}}]
`)
	if err != nil {
		t.Fatal(err)
	}
	n := Analyse(objs)
	if len(n.Groups) != 2 {
		t.Fatalf("groups = %+v, want general and GPU", n.Groups)
	}
	g, w := n.Groups[0], n.Groups[1]
	if g.CPUMilli != 1000 || g.MaxPodCPUMilli != 500 || len(g.Workloads) != 1 {
		t.Errorf("general group = %+v", g)
	}
	if w.CPUMilli != 3000 || w.Memory != 6<<30 || w.MaxPodCPUMilli != 2000 || w.MaxPodMemory != 4<<30 || len(w.Workloads) != 2 {
		t.Errorf("GPU group = %+v", w)
	}
	if w.NodeSelector["accelerator"] != "nvidia" || len(w.Tolerations) != 1 || w.Tolerations[0].Key != "nvidia.com/gpu" {
		t.Errorf("GPU placement = %+v %+v", w.NodeSelector, w.Tolerations)
	}
}

func TestAnalyseLoadBalancers(t *testing.T) {
	objs, err := Parse(`apiVersion: v1
kind: Service
metadata:
  name: databridge-southbound
  annotations:
    service.beta.kubernetes.io/azure-load-balancer-internal: "true"
spec:
  type: LoadBalancer
---
apiVersion: v1
kind: Service
metadata:
  name: api
spec:
  type: ClusterIP
`)
	if err != nil {
		t.Fatal(err)
	}
	n := Analyse(objs)
	if len(n.LoadBalancers) != 1 || n.LoadBalancers[0].Name != "databridge-southbound" ||
		n.LoadBalancers[0].Annotations["service.beta.kubernetes.io/azure-load-balancer-internal"] != "true" {
		t.Fatalf("load balancers = %+v", n.LoadBalancers)
	}
}

func TestAnalyseDials(t *testing.T) {
	objs, err := Parse(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: proxy
spec:
  template:
    spec:
      containers:
      - name: proxy
        env:
        - name: CONFIG_SYNC_GRPC_ENDPOINT
          value: agentgateway-configsync.cp.example.com:443
        - name: OTHER_ADDR
          value: other.example.com:443
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: dataagent-env-vars
data:
  DATABRIDGE_ADDR: databridge.cp.example.com:443
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: guard-env-vars
data:
  CONFIG_SYNC_GRPC_ENDPOINT: trustguard-control.neuraltrust.svc.cluster.local:9090
  DATABRIDGE_ADDR: databridge:443
`)
	if err != nil {
		t.Fatal(err)
	}
	got := Analyse(objs).Dials
	if strings.Join(got, " ") != "agentgateway-configsync.cp.example.com:443 databridge.cp.example.com:443" {
		t.Fatalf("dials = %v, want the two control-plane addresses only", got)
	}
}

func TestAnalyseCountsSecretsAHookWrites(t *testing.T) {
	objs, err := Parse(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: admin
spec:
  template:
    spec:
      containers:
      - name: admin
        env:
        - name: ADMIN_M2M_PUBLIC_KEYS
          valueFrom:
            secretKeyRef: {name: m2m-keys, key: ADMIN_M2M_PUBLIC_KEYS}
        - name: OTHER
          valueFrom:
            secretKeyRef: {name: read-only, key: X}
        - name: LATER
          valueFrom:
            secretKeyRef: {name: post-hook, key: X}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: m2m-keys
  annotations:
    helm.sh/hook: pre-install,pre-upgrade
rules:
- apiGroups: [""]
  resources: ["secrets"]
  resourceNames: ["m2m-keys"]
  verbs: ["get", "patch"]
- apiGroups: [""]
  resources: ["secrets"]
  resourceNames: ["read-only"]
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: later
  annotations:
    helm.sh/hook: post-install
rules:
- apiGroups: [""]
  resources: ["secrets"]
  resourceNames: ["post-hook"]
  verbs: ["patch"]
`)
	if err != nil {
		t.Fatal(err)
	}
	var missing []string
	for _, s := range Analyse(objs).Secrets {
		missing = append(missing, s.Name)
	}
	// A pre-install hook fills m2m-keys before the workload starts; a Role
	// that only reads, or a hook that runs after install, does not.
	if strings.Join(missing, " ") != "post-hook read-only" {
		t.Fatalf("missing = %v, want post-hook and read-only only", missing)
	}
}
