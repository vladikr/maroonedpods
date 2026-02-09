/*
 * This file is part of the MaroonedPods project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2023 Red Hat, Inc.
 *
 */

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sdkapi "kubevirt.io/controller-lifecycle-operator-sdk/api"
)

// this has to be here otherwise informer-gen doesn't recognize it
// see https://github.com/kubernetes/code-generator/issues/59
// +genclient:nonNamespaced

// MaroonPods is the MaroonPods Operator CRD
// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=mp;mps,scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
type MaroonedPods struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec MaroonedPodsSpec `json:"spec"`
	// +optional
	Status MaroonedPodsStatus `json:"status"`
}

// CertConfig contains the tunables for TLS certificates
type CertConfig struct {
	// The requested 'duration' (i.e. lifetime) of the Certificate.
	Duration *metav1.Duration `json:"duration,omitempty"`

	// The amount of time before the currently issued certificate's `notAfter`
	// time that we will begin to attempt to renew the certificate.
	RenewBefore *metav1.Duration `json:"renewBefore,omitempty"`
}

// MaroonedPodsCertConfig has the CertConfigs for MaroonedPods
type MaroonedPodsCertConfig struct {
	// CA configuration
	// CA certs are kept in the CA bundle as long as they are valid
	CA *CertConfig `json:"ca,omitempty"`

	// Server configuration
	// Certs are rotated and discarded
	Server *CertConfig `json:"server,omitempty"`
}

// MaroonedPodsSpec defines our specification for the MaroonedPods installation
type MaroonedPodsSpec struct {
	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	// PullPolicy describes a policy for if/when to pull a container image
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty" valid:"required"`
	// Rules on which nodes MaroonedPods infrastructure pods will be scheduled
	Infra sdkapi.NodePlacement `json:"infra,omitempty"`
	// Restrict on which nodes MaroonedPods workload pods will be scheduled
	Workloads sdkapi.NodePlacement `json:"workload,omitempty"`
	// certificate configuration
	CertConfig *MaroonedPodsCertConfig `json:"certConfig,omitempty"`
	// PriorityClass of the MaroonedPods control plane
	PriorityClass *MaroonedPodsPriorityClass `json:"priorityClass,omitempty"`
	// namespaces where pods should be gated before scheduling
	// Default to the empty LabelSelector, which matches everything.
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`
}

// MaroonedPodsPriorityClass defines the priority class of the MaroonedPods control plane.
type MaroonedPodsPriorityClass string

// MaroonedPodsPhase is the current phase of the MaroonedPods deployment
type MaroonedPodsPhase string

// MaroonedPodsStatus defines the status of the installation
type MaroonedPodsStatus struct {
	sdkapi.Status `json:",inline"`
}

// MaroonedPodsList provides the needed parameters to do request a list of MaroonedPods from the system
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type MaroonedPodsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	// Items provides a list of MaroonedPods
	Items []MaroonedPods `json:"items"`
}

// MaroonedPodsConfig is the configuration for MaroonedPods virtual nodes
// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=mpconfig;mpconfigs,scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Node Image",type="string",JSONPath=".spec.nodeImage"
// +kubebuilder:printcolumn:name="Warm Pool Size",type="integer",JSONPath=".spec.warmPoolSize"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type MaroonedPodsConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec MaroonedPodsConfigSpec `json:"spec"`
	// +optional
	Status MaroonedPodsConfigStatus `json:"status,omitempty"`
}

// VMResources defines CPU and memory resources for virtual machines
type VMResources struct {
	// CPU cores for the VM (default: 2)
	// +kubebuilder:default=2
	// +optional
	CPU uint32 `json:"cpu,omitempty"`

	// Memory for the VM in Mi (default: 3072 = 3Gi)
	// +kubebuilder:default=3072
	// +optional
	MemoryMi uint64 `json:"memoryMi,omitempty"`
}

// MaroonedPodsConfigSpec defines the configuration for MaroonedPods behavior
type MaroonedPodsConfigSpec struct {
	// Container disk image to use for virtual node VMs
	// Default: quay.io/capk/ubuntu-2004-container-disk:v1.26.0
	// +kubebuilder:default="quay.io/capk/ubuntu-2004-container-disk:v1.26.0"
	// +optional
	NodeImage string `json:"nodeImage,omitempty"`

	// Number of pre-booted VM nodes to keep in warm pool
	// Default: 0 (disabled)
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +optional
	WarmPoolSize int32 `json:"warmPoolSize,omitempty"`

	// Base VM resources (CPU/memory) for virtual nodes
	// These are the resources allocated to the VM itself
	// +optional
	BaseVMResources VMResources `json:"baseVMResources,omitempty"`

	// Resource overhead to add on top of pod requests for VM sizing
	// This accounts for kubelet, kube-proxy, and other node components
	// Default: 500m CPU, 512Mi memory
	// +optional
	ResourceOverhead *corev1.ResourceList `json:"resourceOverhead,omitempty"`

	// Taint key prefix for pod-specific node affinity
	// Default: "maroonedpods.io"
	// The full taint key will be: <prefix>/<pod-name>
	// +kubebuilder:default="maroonedpods.io"
	// +optional
	NodeTaintKey string `json:"nodeTaintKey,omitempty"`
}

// MaroonedPodsConfigStatus defines the observed state of MaroonedPodsConfig
type MaroonedPodsConfigStatus struct {
	// Total number of VMs in the warm pool
	// +optional
	WarmPoolTotal int32 `json:"warmPoolTotal,omitempty"`

	// Number of available (unclaimed) VMs in the warm pool
	// +optional
	WarmPoolAvailable int32 `json:"warmPoolAvailable,omitempty"`

	// Number of claimed VMs currently in use
	// +optional
	WarmPoolClaimed int32 `json:"warmPoolClaimed,omitempty"`

	// Conditions represent the latest available observations of the config state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// MaroonedPodsConfigList provides the list of MaroonedPodsConfig
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type MaroonedPodsConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`

	Items []MaroonedPodsConfig `json:"items"`
}

