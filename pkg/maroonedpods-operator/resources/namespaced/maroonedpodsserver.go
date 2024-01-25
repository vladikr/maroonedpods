package namespaced

import (
	"fmt"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utils2 "maroonedpods.io/maroonedpods/pkg/util"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sdkapi "kubevirt.io/controller-lifecycle-operator-sdk/api"
)

func createMaroonedPodsServerResources(args *FactoryArgs) []client.Object {
	return []client.Object{
		createMaroonedPodsServerRole(),
		createMaroonedPodsServerRoleBinding(),
		createMaroonedPodsServerServiceAccount(),
		createMaroonedPodsServerService(),
		createMaroonedPodsServerDeployment(args.MaroonedPodsServerImage, args.PullPolicy, args.ImagePullSecrets, args.PriorityClassName, args.Verbosity, args.InfraNodePlacement),
	}
}

func createMaroonedPodsServerServiceAccount() *corev1.ServiceAccount {
	return utils2.ResourceBuilder.CreateServiceAccount(utils2.MaroonedPodsServerResourceName)
}

func createMaroonedPodsServerService() *corev1.Service {
	service := utils2.ResourceBuilder.CreateService("maroonedpods-server", utils2.MaroonedPodsLabel, utils2.MaroonedPodsServerResourceName, nil)
	service.Spec.Type = corev1.ServiceTypeNodePort
	service.Spec.Ports = []corev1.ServicePort{
		{
			Port: 443,
			TargetPort: intstr.IntOrString{
				Type:   intstr.Int,
				IntVal: 8443,
			},
			Protocol: corev1.ProtocolTCP,
		},
	}
	return service
}

func createMaroonedPodsServerDeployment(image, pullPolicy string, imagePullSecrets []corev1.LocalObjectReference, priorityClassName string, verbosity string, infraNodePlacement *sdkapi.NodePlacement) *appsv1.Deployment {
	defaultMode := corev1.ConfigMapVolumeSourceDefaultMode
	deployment := utils2.CreateDeployment(utils2.MaroonedPodsServerResourceName, utils2.MaroonedPodsLabel, utils2.MaroonedPodsServerResourceName, utils2.MaroonedPodsServerResourceName, imagePullSecrets, 2, infraNodePlacement)
	if priorityClassName != "" {
		deployment.Spec.Template.Spec.PriorityClassName = priorityClassName
	}
	desiredMaxUnavailable := intstr.FromInt(1)
	deployment.Spec.Strategy = appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxUnavailable: &desiredMaxUnavailable,
		},
	}
	container := utils2.CreateContainer(utils2.MaroonedPodsServerResourceName, image, verbosity, pullPolicy)
	container.Ports = createMaroonedPodsServerPorts()

	container.Env = []corev1.EnvVar{
		{
			Name: utils2.InstallerPartOfLabel,
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  fmt.Sprintf("metadata.labels['%s']", utils2.AppKubernetesPartOfLabel),
				},
			},
		},
		{
			Name: utils2.InstallerVersionLabel,
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  fmt.Sprintf("metadata.labels['%s']", utils2.AppKubernetesVersionLabel),
				},
			},
		},
		{
			Name:  utils2.TlsLabel,
			Value: "true",
		},
	}
	container.ReadinessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/healthz",
				Port: intstr.IntOrString{
					Type:   intstr.Int,
					IntVal: 8443,
				},
				Scheme: corev1.URISchemeHTTPS,
			},
		},
		InitialDelaySeconds: 2,
		PeriodSeconds:       5,
		FailureThreshold:    3,
		SuccessThreshold:    1,
		TimeoutSeconds:      1,
	}
	container.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("50Mi"),
		},
	}
	container.VolumeMounts = []corev1.VolumeMount{
		{
			Name:      "tls",
			MountPath: "/etc/admission-webhook/tls",
			ReadOnly:  true,
		},
	}
	deployment.Spec.Template.Spec.Containers = []corev1.Container{container}

	deployment.Spec.Template.Spec.Volumes = []corev1.Volume{
		{
			Name: "tls",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  utils2.SecretResourceName,
					DefaultMode: &defaultMode,
				},
			},
		},
	}
	if infraNodePlacement == nil {
		deployment.Spec.Template.Spec.Affinity = &corev1.Affinity{
			PodAntiAffinity: &corev1.PodAntiAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
					{
						PodAffinityTerm: corev1.PodAffinityTerm{
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{utils2.MaroonedPodsLabel: utils2.MaroonedPodsServerResourceName},
							},
							TopologyKey: "kubernetes.io/hostname",
						},
						Weight: 100,
					},
				},
			},
		}
	}
	return deployment
}

func createMaroonedPodsServerPorts() []corev1.ContainerPort {
	return []corev1.ContainerPort{
		{
			ContainerPort: 8443,
			Protocol:      "TCP",
		},
	}
}

func createMaroonedPodsServerRoleBinding() *rbacv1.RoleBinding {
	return utils2.ResourceBuilder.CreateRoleBinding(utils2.MaroonedPodsServerResourceName, utils2.MaroonedPodsServerResourceName, utils2.MaroonedPodsServerResourceName, "")
}
func createMaroonedPodsServerRole() *rbacv1.Role {
	rules := []rbacv1.PolicyRule{
		{
			APIGroups: []string{
				"",
			},
			Resources: []string{
				"secrets",
			},
			Verbs: []string{
				"get",
				"list",
				"watch",
			},
		},
	}
	return utils2.ResourceBuilder.CreateRole(utils2.MaroonedPodsServerResourceName, rules)
}
