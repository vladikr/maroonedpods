package builders

import (
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"maroonedpods.io/maroonedpods/pkg/util"
)

// PodBuilder helps build test pods
type PodBuilder struct {
	pod *v1.Pod
}

// NewPod creates a new PodBuilder
func NewPod(name, namespace string) *PodBuilder {
	return &PodBuilder{
		pod: &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels:    make(map[string]string),
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{},
			},
		},
	}
}

// WithMaroonedLabel adds the maroonedpods.io/maroon label
func (b *PodBuilder) WithMaroonedLabel() *PodBuilder {
	b.pod.Labels[util.MaroonedPodLabel] = "true"
	return b
}

// WithLabel adds a custom label
func (b *PodBuilder) WithLabel(key, value string) *PodBuilder {
	b.pod.Labels[key] = value
	return b
}

// WithContainer adds a container to the pod
func (b *PodBuilder) WithContainer(name, image string) *PodBuilder {
	container := v1.Container{
		Name:  name,
		Image: image,
	}
	b.pod.Spec.Containers = append(b.pod.Spec.Containers, container)
	return b
}

// WithContainerResources adds resource requests/limits to the last container
func (b *PodBuilder) WithContainerResources(cpu, memory string) *PodBuilder {
	if len(b.pod.Spec.Containers) == 0 {
		return b
	}

	lastIdx := len(b.pod.Spec.Containers) - 1
	b.pod.Spec.Containers[lastIdx].Resources = v1.ResourceRequirements{
		Requests: v1.ResourceList{
			v1.ResourceCPU:    resource.MustParse(cpu),
			v1.ResourceMemory: resource.MustParse(memory),
		},
		Limits: v1.ResourceList{
			v1.ResourceCPU:    resource.MustParse(cpu),
			v1.ResourceMemory: resource.MustParse(memory),
		},
	}
	return b
}

// WithRestartPolicy sets the restart policy
func (b *PodBuilder) WithRestartPolicy(policy v1.RestartPolicy) *PodBuilder {
	b.pod.Spec.RestartPolicy = policy
	return b
}

// Build returns the built pod
func (b *PodBuilder) Build() *v1.Pod {
	return b.pod
}

// NewMaroonedPod creates a simple marooned pod with nginx
func NewMaroonedPod(name, namespace string) *v1.Pod {
	return NewPod(name, namespace).
		WithMaroonedLabel().
		WithContainer("nginx", "nginx:latest").
		WithRestartPolicy(v1.RestartPolicyAlways).
		Build()
}

// NewMaroonedPodWithResources creates a marooned pod with specific resource requests
func NewMaroonedPodWithResources(name, namespace, cpu, memory string) *v1.Pod {
	return NewPod(name, namespace).
		WithMaroonedLabel().
		WithContainer("nginx", "nginx:latest").
		WithContainerResources(cpu, memory).
		WithRestartPolicy(v1.RestartPolicyAlways).
		Build()
}
