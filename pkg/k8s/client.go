package k8s

import (
	"fmt"
	"os"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// NewClient creates a Kubernetes clientset. If kubeconfig is empty,
// it uses in-cluster config. Returns the clientset, rest config, and resolved namespace.
func NewClient(kubeconfig, namespace string) (kubernetes.Interface, *rest.Config, string, error) {
	var cfg *rest.Config
	var err error

	if kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, nil, "", fmt.Errorf("building k8s config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, "", fmt.Errorf("creating k8s clientset: %w", err)
	}

	if namespace == "" {
		if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
			namespace = string(data)
		} else {
			namespace = "default"
		}
	}

	return clientset, cfg, namespace, nil
}
