package framework

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	virtv1 "kubevirt.io/api/core/v1"
	kubevirtclient "kubevirt.io/client-go/kubecli"

	mpv1alpha1 "maroonedpods.io/maroonedpods/staging/src/maroonedpods.io/api/pkg/apis/core/v1alpha1"
	"maroonedpods.io/maroonedpods/tests/flags"
)

// Framework provides access to Kubernetes and KubeVirt clients for tests
type Framework struct {
	K8sClient       kubernetes.Interface
	KubevirtClient  kubevirtclient.KubevirtClient
	RestConfig      *rest.Config
	Namespace       *v1.Namespace
	NamespaceName   string
	mpNamespace     string
}

var (
	// DefaultFramework is the global test framework instance
	DefaultFramework *Framework
)

// InitFramework initializes the global test framework
func InitFramework() {
	var err error
	DefaultFramework, err = NewFramework()
	if err != nil {
		panic(fmt.Sprintf("Failed to initialize test framework: %v", err))
	}
	flags.PrintFlags()
}

// NewFramework creates a new test framework instance
func NewFramework() (*Framework, error) {
	f := &Framework{
		mpNamespace: *flags.MaroonedPodsNamespace,
	}

	// Build Kubernetes config
	var err error
	if *flags.KubeConfig != "" {
		f.RestConfig, err = clientcmd.BuildConfigFromFlags(*flags.KubeURL, *flags.KubeConfig)
	} else {
		f.RestConfig, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("failed to build kubeconfig: %v", err)
	}

	// Create Kubernetes client
	f.K8sClient, err = kubernetes.NewForConfig(f.RestConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %v", err)
	}

	// Create KubeVirt client
	f.KubevirtClient, err = kubevirtclient.GetKubevirtClientFromRESTConfig(f.RestConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubevirt client: %v", err)
	}

	return f, nil
}

// CreateNamespace creates a new test namespace
func (f *Framework) CreateNamespace(name string) (*v1.Namespace, error) {
	ns := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"test": "maroonedpods-e2e",
			},
		},
	}

	createdNs, err := f.K8sClient.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}

	f.Namespace = createdNs
	f.NamespaceName = createdNs.Name
	return createdNs, nil
}

// DeleteNamespace deletes a test namespace
func (f *Framework) DeleteNamespace(name string) error {
	return f.K8sClient.CoreV1().Namespaces().Delete(context.Background(), name, metav1.DeleteOptions{})
}

// CreatePod creates a pod in the test namespace
func (f *Framework) CreatePod(pod *v1.Pod) (*v1.Pod, error) {
	return f.K8sClient.CoreV1().Pods(f.NamespaceName).Create(context.Background(), pod, metav1.CreateOptions{})
}

// GetPod gets a pod from the test namespace
func (f *Framework) GetPod(name string) (*v1.Pod, error) {
	return f.K8sClient.CoreV1().Pods(f.NamespaceName).Get(context.Background(), name, metav1.GetOptions{})
}

// DeletePod deletes a pod from the test namespace
func (f *Framework) DeletePod(name string) error {
	return f.K8sClient.CoreV1().Pods(f.NamespaceName).Delete(context.Background(), name, metav1.DeleteOptions{})
}

// GetVMI gets a VirtualMachineInstance
func (f *Framework) GetVMI(namespace, name string) (*virtv1.VirtualMachineInstance, error) {
	return f.KubevirtClient.VirtualMachineInstance(namespace).Get(context.Background(), name, &metav1.GetOptions{})
}

// ListVMIs lists VirtualMachineInstances in a namespace
func (f *Framework) ListVMIs(namespace string) (*virtv1.VirtualMachineInstanceList, error) {
	return f.KubevirtClient.VirtualMachineInstance(namespace).List(context.Background(), &metav1.ListOptions{})
}

// GetNode gets a node by name
func (f *Framework) GetNode(name string) (*v1.Node, error) {
	return f.K8sClient.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
}

// WaitForPodPhase waits for a pod to reach a specific phase
func (f *Framework) WaitForPodPhase(podName string, phase v1.PodPhase, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pod, err := f.GetPod(podName)
		if err != nil {
			return err
		}
		if pod.Status.Phase == phase {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timeout waiting for pod %s to reach phase %s", podName, phase)
}

// WaitForVMI waits for a VMI to exist
func (f *Framework) WaitForVMI(namespace, name string, timeout time.Duration) (*virtv1.VirtualMachineInstance, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		vmi, err := f.GetVMI(namespace, name)
		if err == nil {
			return vmi, nil
		}
		time.Sleep(2 * time.Second)
	}
	return nil, fmt.Errorf("timeout waiting for VMI %s/%s to exist", namespace, name)
}

// WaitForVMIPhase waits for a VMI to reach a specific phase
func (f *Framework) WaitForVMIPhase(namespace, name string, phase virtv1.VirtualMachineInstancePhase, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		vmi, err := f.GetVMI(namespace, name)
		if err == nil && vmi.Status.Phase == phase {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timeout waiting for VMI %s/%s to reach phase %s", namespace, name, phase)
}

// WaitForNodeReady waits for a node to be ready
func (f *Framework) WaitForNodeReady(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		node, err := f.GetNode(name)
		if err == nil {
			for _, condition := range node.Status.Conditions {
				if condition.Type == v1.NodeReady && condition.Status == v1.ConditionTrue {
					return nil
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timeout waiting for node %s to be ready", name)
}

// WaitForPodDeleted waits for a pod to be deleted
func (f *Framework) WaitForPodDeleted(podName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := f.GetPod(podName)
		if err != nil {
			// Pod not found, it's deleted
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timeout waiting for pod %s to be deleted", podName)
}

// WaitForVMIDeleted waits for a VMI to be deleted
func (f *Framework) WaitForVMIDeleted(namespace, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := f.GetVMI(namespace, name)
		if err != nil {
			// VMI not found, it's deleted
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timeout waiting for VMI %s/%s to be deleted", namespace, name)
}
