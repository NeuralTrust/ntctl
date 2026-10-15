// Package kube resolves cluster access from the same kubeconfig and context
// flags kubectl and helm use.
package kube

import (
	"fmt"

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/cli"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Options selects the cluster and namespace.
type Options struct {
	Kubeconfig string
	Context    string
	Namespace  string
}

// Settings returns Helm environment settings bound to these options.
func (o Options) Settings() *cli.EnvSettings {
	s := cli.New()
	if o.Kubeconfig != "" {
		s.KubeConfig = o.Kubeconfig
	}
	if o.Context != "" {
		s.KubeContext = o.Context
	}
	s.SetNamespace(o.Namespace)
	return s
}

// Clientset returns a Kubernetes client for the selected context.
func (o Options) Clientset() (kubernetes.Interface, error) {
	cfg, err := o.Settings().RESTClientGetter().ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating Kubernetes client: %w", err)
	}
	return cs, nil
}

// Dynamic returns a dynamic client for the selected context, for the Gateway
// API objects client-go has no typed client for.
func (o Options) Dynamic() (dynamic.Interface, error) {
	cfg, err := o.Settings().RESTClientGetter().ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating Kubernetes client: %w", err)
	}
	return dyn, nil
}

// Target names the kubeconfig context and API server in use, so prompts can
// say exactly which cluster a change is about to touch.
func (o Options) Target() string {
	raw, err := o.Settings().RESTClientGetter().ToRawKubeConfigLoader().RawConfig()
	if err != nil {
		return "unknown cluster"
	}
	name := raw.CurrentContext
	if o.Context != "" {
		name = o.Context
	}
	server := ""
	if c, ok := raw.Contexts[name]; ok {
		if cl, ok := raw.Clusters[c.Cluster]; ok {
			server = cl.Server
		}
	}
	return fmt.Sprintf("context %q (%s)", name, server)
}

// Helm returns a Helm action configuration for the selected namespace, storing
// releases in Secrets as the helm CLI does.
func (o Options) Helm(opts ...action.ConfigurationOption) (*action.Configuration, error) {
	cfg := action.NewConfiguration(opts...)
	if err := cfg.Init(o.Settings().RESTClientGetter(), o.Namespace, "secret"); err != nil {
		return nil, fmt.Errorf("initialising Helm: %w", err)
	}
	return cfg, nil
}
