package flags

import (
	"flag"
	"fmt"
	"os"
)

var (
	KubeConfig               *string
	KubeURL                  *string
	KubectlPath              *string
	MaroonedPodsNamespace    *string
	DockerPrefix             *string
	DockerTag                *string
)

func InitFlags() {
	KubeConfig = flag.String("kubeconfig", "", "Kubeconfig file path (optional)")
	KubeURL = flag.String("kubeurl", "", "Kubernetes API server URL (optional)")
	KubectlPath = flag.String("kubectl-path-maroonedpods", "", "Path to kubectl binary")
	MaroonedPodsNamespace = flag.String("maroonedpods-namespace", "maroonedpods", "MaroonedPods operator namespace")
	DockerPrefix = flag.String("docker-prefix", "", "Docker image prefix")
	DockerTag = flag.String("docker-tag", "", "Docker image tag")
}

func NormalizeFlags() {
	// Initialize flags if not already initialized
	if KubeConfig == nil {
		InitFlags()
	}

	// Set defaults from environment if not provided
	if *KubeConfig == "" {
		if kc := os.Getenv("KUBECONFIG"); kc != "" {
			*KubeConfig = kc
		}
	}

	if *KubectlPath == "" {
		*KubectlPath = "kubectl"
	}
}

func PrintFlags() {
	fmt.Printf("Test Configuration:\n")
	fmt.Printf("  KubeConfig: %s\n", *KubeConfig)
	fmt.Printf("  KubeURL: %s\n", *KubeURL)
	fmt.Printf("  KubectlPath: %s\n", *KubectlPath)
	fmt.Printf("  MaroonedPodsNamespace: %s\n", *MaroonedPodsNamespace)
	fmt.Printf("  DockerPrefix: %s\n", *DockerPrefix)
	fmt.Printf("  DockerTag: %s\n", *DockerTag)
}
