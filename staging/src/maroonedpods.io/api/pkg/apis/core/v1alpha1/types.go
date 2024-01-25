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

