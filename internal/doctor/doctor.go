// Package doctor diagnoses a running install. It runs the preflight checks
// against what is deployed, then looks at what goes wrong at runtime: images
// that do not pull, missing Secrets, crashing or OOM-killed containers, pods
// that cannot be scheduled, volumes and hook Jobs that never finish, and
// endpoints without an address. Every finding carries a code and a fix. It
// changes nothing in the cluster.
package doctor

import (
	"bufio"
	"context"
	"fmt"
	"sort"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	rcommon "helm.sh/helm/v4/pkg/release/common"
	release "helm.sh/helm/v4/pkg/release/v1"

	"github.com/NeuralTrust/ntctl/internal/detect"
	"github.com/NeuralTrust/ntctl/internal/gatewayapi"
	"github.com/NeuralTrust/ntctl/internal/manifest"
	"github.com/NeuralTrust/ntctl/internal/preflight"
	"github.com/NeuralTrust/ntctl/internal/redact"
	"github.com/NeuralTrust/ntctl/internal/secrets"
)

// Input is what the diagnosis looks at.
type Input struct {
	Clientset kubernetes.Interface
	// Dynamic reads Gateway API objects; nil skips those checks.
	Dynamic   dynamic.Interface
	Namespace string
	Release   string
	// Current is the deployed release, nil when there is none.
	Current *release.Release
	Cluster *detect.Cluster
	// LogLines is how many log lines a crashing container's finding carries.
	LogLines int64
}

// Run returns every finding, most important first within each area.
func Run(ctx context.Context, in Input) ([]preflight.Result, error) {
	if in.LogLines == 0 {
		in.LogLines = 10
	}
	out := []preflight.Result{releaseCheck(in)}
	if in.Current != nil {
		pre, err := deployedChecks(ctx, in)
		if err != nil {
			return nil, err
		}
		out = append(out, pre...)
	}
	pods, err := in.Clientset.CoreV1().Pods(in.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}
	out = append(out, podFindings(ctx, in, pods.Items)...)
	more, err := resourceFindings(ctx, in)
	if err != nil {
		return nil, err
	}
	out = append(out, more...)
	// Log lines and events can carry credentials; they are printed, so they
	// go through the same redaction as a support bundle.
	for i := range out {
		for j, d := range out[i].Detail {
			out[i].Detail[j] = redact.Text(d)
		}
	}
	return out, nil
}

// releaseCheck reports the Helm release's own state.
func releaseCheck(in Input) preflight.Result {
	r := preflight.Result{Code: "NT-DR-001", Check: "release", Severity: preflight.OK}
	rel := in.Current
	if rel == nil {
		r.Severity = preflight.Warn
		r.Message = fmt.Sprintf("no release %q in namespace %q; only the namespace's workloads are checked", in.Release, in.Namespace)
		r.Fix = "Pass --namespace and --release for the install to check."
		return r
	}
	r.Message = fmt.Sprintf("release %s revision %d, chart %s, %s", rel.Name, rel.Version, rel.Chart.Metadata.Version, rel.Info.Status)
	switch rel.Info.Status {
	case rcommon.StatusDeployed:
	case rcommon.StatusFailed:
		r.Severity = preflight.Fail
		r.Detail = lines(rel.Info.Description, 8)
		r.Fix = "Fix the cause shown by the other findings, then run `ntctl upgrade` again."
	case rcommon.StatusPendingInstall, rcommon.StatusPendingUpgrade, rcommon.StatusPendingRollback:
		r.Severity = preflight.Fail
		r.Message += " (an operation was interrupted, or is still running)"
		r.Fix = "If no install or upgrade is running, roll back to the last working revision with `helm rollback " + rel.Name + " -n " + in.Namespace + "`, then upgrade again."
	default:
		r.Severity = preflight.Warn
	}
	return r
}

// deployedChecks runs the preflight checks against the deployed manifest, so
// a Secret or storage class that went missing after the install shows up.
func deployedChecks(ctx context.Context, in Input) ([]preflight.Result, error) {
	objs, err := manifest.Parse(in.Current.Manifest)
	if err != nil {
		return nil, fmt.Errorf("reading the deployed manifest: %w", err)
	}
	platform := ""
	if g, ok := in.Current.Config["global"].(map[string]any); ok {
		platform, _ = g["platform"].(string)
	}
	nsLabels, err := detect.NamespaceLabels(ctx, in.Clientset, in.Namespace)
	if err != nil {
		return nil, err
	}
	rs, err := preflight.Run(ctx, preflight.Input{
		Mode: preflight.ForUpgrade, Release: in.Release, Namespace: in.Namespace, ReleaseExists: true,
		Cluster: in.Cluster, Needs: manifest.Analyse(objs), Platform: platform, NamespaceLabels: nsLabels,
		Secrets: func(ctx context.Context, name string) ([]string, bool, error) {
			return secrets.Keys(ctx, in.Clientset, in.Namespace, name)
		},
	})
	if err != nil {
		return nil, err
	}
	out := rs[:0]
	for _, r := range rs {
		// The release check above already covers this, and capacity compares
		// requests with free room as if nothing were running yet.
		if r.Check == "release" || r.Check == "capacity" {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// group collects pods that share a workload and a problem into one finding.
type group struct {
	r    preflight.Result
	pods []string
}

func podFindings(ctx context.Context, in Input, pods []corev1.Pod) []preflight.Result {
	pulls := &pullFailures{}
	groups := map[string]*group{}
	var order []string
	add := func(workload string, r preflight.Result, pod string) *group {
		k := r.Code + "/" + workload
		g, ok := groups[k]
		if !ok {
			g = &group{r: r}
			groups[k] = g
			order = append(order, k)
		}
		g.pods = append(g.pods, pod)
		return g
	}
	for _, p := range pods {
		w := workloadOf(p)
		statuses := append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
		for _, c := range statuses {
			if c.State.Waiting != nil {
				switch reason := c.State.Waiting.Reason; reason {
				case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
					pulls.add(w, c.Image, reason, c.State.Waiting.Message, p)
				case "CreateContainerConfigError", "CreateContainerError":
					add(w, preflight.Result{
						Code: "NT-DR-011", Check: "config", Severity: preflight.Fail,
						Message: fmt.Sprintf("%s cannot start container %s: %s", w, c.Name, c.State.Waiting.Message),
						Fix:     "Create the Secret or ConfigMap named above in namespace " + in.Namespace + ", or fix the name in your values; the pod starts on its own once it exists.",
					}, p.Name)
				case "CrashLoopBackOff":
					g := add(w, preflight.Result{
						Code: "NT-DR-012", Check: "crashing", Severity: preflight.Fail,
						Message: fmt.Sprintf("%s container %s keeps crashing (restarts: %d)", w, c.Name, c.RestartCount),
						Fix:     "The last log lines are above. `ntctl support-bundle` collects the full logs for NeuralTrust support.",
					}, p.Name)
					if len(g.pods) == 1 {
						g.r.Detail = lastLogs(ctx, in, p.Name, c.Name)
					}
				}
			}
			if t := c.LastTerminationState.Terminated; t != nil && t.Reason == "OOMKilled" {
				add(w, preflight.Result{
					Code: "NT-DR-014", Check: "memory", Severity: preflight.Warn,
					Message: fmt.Sprintf("%s container %s was killed for exceeding its memory limit", w, c.Name),
					Fix:     "Raise that component's resources.limits.memory in your values, then `ntctl upgrade`.",
				}, p.Name)
			}
		}
		if p.Status.Phase == corev1.PodPending {
			for _, cond := range p.Status.Conditions {
				if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse {
					add(w, preflight.Result{
						Code: "NT-DR-013", Check: "schedule", Severity: preflight.Fail,
						Message: fmt.Sprintf("%s cannot be scheduled: %s", w, schedulerReason(cond.Message)),
						Fix:     "Add nodes or capacity, or check that the nodeSelector, tolerations and affinity in your values match your nodes.",
					}, p.Name)
				}
			}
		}
	}
	out := pulls.findings()
	for _, k := range order {
		g := groups[k]
		if n := len(g.pods); n > 1 {
			g.r.Message += fmt.Sprintf(" [%d pods]", n)
		}
		out = append(out, g.r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

func resourceFindings(ctx context.Context, in Input) ([]preflight.Result, error) {
	var out []preflight.Result
	pvcs, err := in.Clientset.CoreV1().PersistentVolumeClaims(in.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing volumes: %w", err)
	}
	for _, c := range pvcs.Items {
		if c.Status.Phase != corev1.ClaimPending {
			continue
		}
		class := "the default StorageClass"
		if c.Spec.StorageClassName != nil && *c.Spec.StorageClassName != "" {
			class = "StorageClass " + *c.Spec.StorageClassName
		}
		out = append(out, preflight.Result{
			Code: "NT-DR-020", Check: "volumes", Severity: preflight.Fail,
			Message: fmt.Sprintf("volume %s is not bound (%s)", c.Name, class),
			Detail:  eventsFor(ctx, in, "PersistentVolumeClaim", c.Name),
			Fix:     "Check that the StorageClass exists and its provisioner is running; a WaitForFirstConsumer class binds only once the pod is scheduled.",
		})
	}
	jobs, err := in.Clientset.BatchV1().Jobs(in.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing jobs: %w", err)
	}
	for _, j := range jobs.Items {
		if failed(j) {
			out = append(out, preflight.Result{
				Code: "NT-DR-021", Check: "jobs", Severity: preflight.Fail,
				Message: fmt.Sprintf("job %s failed (%d attempts)", j.Name, j.Status.Failed),
				Detail:  jobLogs(ctx, in, j.Name),
				Fix:     "Fix the cause in the log lines above, then `ntctl upgrade` runs it again.",
			})
		}
	}
	ings, err := in.Clientset.NetworkingV1().Ingresses(in.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing ingresses: %w", err)
	}
	var pending []string
	for _, i := range ings.Items {
		if len(i.Status.LoadBalancer.Ingress) == 0 {
			pending = append(pending, i.Name)
		}
	}
	if len(pending) > 0 {
		out = append(out, preflight.Result{
			Code: "NT-DR-022", Check: "ingress", Severity: preflight.Warn,
			Message: fmt.Sprintf("%d of %d ingresses have no address yet: %s", len(pending), len(ings.Items), strings.Join(pending, ", ")),
			Fix:     "An ingress gets its address from the ingress controller's load balancer. Check the controller is installed and its Service of type LoadBalancer got an address.",
		})
	}
	if in.Dynamic != nil {
		routes, err := gatewayapi.ListRoutes(ctx, in.Dynamic, in.Namespace)
		if err != nil {
			return nil, err
		}
		var detail, names []string
		for _, r := range routes {
			problems := r.Problems()
			for _, p := range problems {
				detail = append(detail, r.Name+": "+p)
			}
			if len(problems) > 0 {
				names = append(names, r.Name)
			}
		}
		if len(names) > 0 {
			out = append(out, preflight.Result{
				Code: "NT-DR-023", Check: "ingress", Severity: preflight.Warn,
				Message: fmt.Sprintf("%d of %d HTTPRoutes are not accepted by their Gateway: %s", len(names), len(routes), strings.Join(names, ", ")),
				Detail:  detail,
				Fix:     "The Gateway controller's reason is shown above. Check the parent Gateway exists, its listener admits this namespace, and the backend Services exist.",
			})
		}
	}
	return out, nil
}

func failed(j batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return false
		}
	}
	return false
}

// pullFailures groups image pulls that fail for the same cause, so one
// registry problem is one finding however many workloads it stops.
type pullFailures struct {
	causes []string
	by     map[string]*pullCause
}

type pullCause struct {
	fix   string
	lines []string
	seen  map[string]bool
	wl    map[string]bool
}

func (f *pullFailures) add(workload, image, reason, msg string, p corev1.Pod) {
	cause, fix := pullCauseOf(reason, msg, p)
	if f.by == nil {
		f.by = map[string]*pullCause{}
	}
	c, ok := f.by[cause]
	if !ok {
		c = &pullCause{fix: fix, seen: map[string]bool{}, wl: map[string]bool{}}
		f.by[cause] = c
		f.causes = append(f.causes, cause)
	}
	c.wl[workload] = true
	if line := workload + ": " + image; !c.seen[line] {
		c.seen[line] = true
		c.lines = append(c.lines, line)
	}
}

func (f *pullFailures) findings() []preflight.Result {
	var out []preflight.Result
	for _, cause := range f.causes {
		c := f.by[cause]
		what := fmt.Sprintf("%d workloads cannot pull their images", len(c.wl))
		if len(c.wl) == 1 {
			what = "1 workload cannot pull its image"
		}
		sort.Strings(c.lines)
		out = append(out, preflight.Result{
			Code: "NT-DR-010", Check: "images", Severity: preflight.Fail,
			Message: what + ": " + cause, Detail: c.lines, Fix: c.fix,
		})
	}
	return out
}

// pullCauseOf reads the kubelet's message for why a pull fails, and the fix.
func pullCauseOf(reason, msg string, p corev1.Pod) (string, string) {
	m := strings.ToLower(msg)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(m, s) {
				return true
			}
		}
		return false
	}
	switch {
	case reason == "InvalidImageName":
		return "the image name is invalid", "Fix the image repository or tag in your values, or global.imageRegistry."
	case has("401", "403", "unauthorized", "denied", "failed to authorize", "authentication required", "forbidden"):
		return "the registry refused the credentials", pullFix(p)
	case has("not found", "manifest unknown", "notfound"):
		return "the image or tag is not in that registry", "If you mirror the images, copy this chart version's images under the same tags; otherwise check the image overrides in your values."
	case has("no such host", "i/o timeout", "connection refused", "dial tcp", "tls handshake", "network is unreachable"):
		return "the nodes cannot reach the registry", "Allow the nodes egress to the registry (firewall, proxy, DNS), or mirror the images to a registry they can reach and set global.imageRegistry."
	}
	return "the pull fails (" + firstLine(msg) + ")", pullFix(p)
}

// pullFix names the pull Secrets the pod uses, which is where an image pull
// usually fails.
func pullFix(p corev1.Pod) string {
	var names []string
	for _, s := range p.Spec.ImagePullSecrets {
		names = append(names, s.Name)
	}
	if len(names) == 0 {
		return "The pod has no pull Secret, so the nodes' own identity must be allowed to pull from this registry. If the images are mirrored, check they are there under the same tag."
	}
	return "Check that pull Secret " + strings.Join(names, ", ") + " holds a valid key for this registry (`ntctl install --registry-key` creates it), or that the image is mirrored under the same tag."
}

// lastLogs returns the last lines the crashed container wrote.
func lastLogs(ctx context.Context, in Input, pod, container string) []string {
	n := in.LogLines
	req := in.Clientset.CoreV1().Pods(in.Namespace).GetLogs(pod, &corev1.PodLogOptions{Container: container, Previous: true, TailLines: &n})
	rc, err := req.Stream(ctx)
	if err != nil {
		return []string{"(no logs: " + err.Error() + ")"}
	}
	defer rc.Close() //nolint:errcheck // read-only stream
	var lines []string
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		lines = append(lines, clip(sc.Text(), 300))
	}
	return lines
}

func jobLogs(ctx context.Context, in Input, job string) []string {
	pods, err := in.Clientset.CoreV1().Pods(in.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + job})
	if err != nil || len(pods.Items) == 0 {
		return nil
	}
	p := pods.Items[len(pods.Items)-1]
	if len(p.Spec.Containers) == 0 {
		return nil
	}
	n := in.LogLines
	rc, err := in.Clientset.CoreV1().Pods(in.Namespace).GetLogs(p.Name, &corev1.PodLogOptions{Container: p.Spec.Containers[0].Name, TailLines: &n}).Stream(ctx)
	if err != nil {
		return nil
	}
	defer rc.Close() //nolint:errcheck // read-only stream
	var lines []string
	sc := bufio.NewScanner(rc)
	for sc.Scan() {
		lines = append(lines, clip(sc.Text(), 300))
	}
	return lines
}

func eventsFor(ctx context.Context, in Input, kind, name string) []string {
	evs, err := in.Clientset.CoreV1().Events(in.Namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.kind=" + kind + ",involvedObject.name=" + name,
	})
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range evs.Items {
		if e.Type == corev1.EventTypeWarning {
			out = append(out, clip(e.Reason+": "+e.Message, 300))
		}
	}
	if len(out) > 3 {
		out = out[len(out)-3:]
	}
	return out
}

// workloadOf names the Deployment, StatefulSet, DaemonSet or Job a pod
// belongs to.
func workloadOf(p corev1.Pod) string {
	if len(p.OwnerReferences) == 0 {
		return p.Name
	}
	o := p.OwnerReferences[0]
	if i := strings.LastIndex(o.Name, "-"); o.Kind == "ReplicaSet" && i > 0 {
		return o.Name[:i]
	}
	return o.Name
}

// schedulerReason keeps the cause from a scheduler message such as
// "0/3 nodes are available: 3 Insufficient memory. preemption: ...".
func schedulerReason(msg string) string {
	msg = strings.TrimSpace(msg)
	if _, rest, ok := strings.Cut(msg, "nodes are available: "); ok {
		msg = rest
	}
	if cause, _, ok := strings.Cut(msg, ". "); ok {
		msg = cause
	}
	return strings.TrimSuffix(msg, ".")
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return clip(s, 300)
}

// clip redacts a line, then shortens it. Redacting first means a secret cut
// in half by the limit is still recognised.
func clip(s string, n int) string {
	s = redact.Text(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// lines splits text into at most n non-empty lines.
func lines(s string, n int) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, clip(l, 300))
		}
	}
	if len(out) > n {
		out = append(out[:n], fmt.Sprintf("(%d more lines)", len(out)-n))
	}
	return out
}
