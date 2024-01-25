package cluster

import (
	"context"
	"github.com/go-logr/logr"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	mpserver "maroonedpods.io/pkg/maroonedpods-server"
	"maroonedpods.io/pkg/util"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	mpServerResourceName               = "maroonedpods-server"
	MutatingWebhookConfigurationName   = "maroonedpods-mutator"
	validatingWebhookConfigurationName = "maroonedpods-validator"
	MaroonedPodsServerServiceName      = mpServerResourceName
)

func createStaticMaroonedPodsLockResources(args *FactoryArgs) []client.Object {
	return []client.Object{
		createAPIServerClusterRole(),
		createAPIServerClusterRoleBinding(args.Namespace),
	}
}
func createDynamicMutatingGatingServerResources(args *FactoryArgs) []client.Object {
	var objectsToAdd []client.Object
	gatingMutatingWebhook := createGatingMutatingWebhook(args.Namespace, args.Client, args.Logger)
	if gatingMutatingWebhook != nil {
		objectsToAdd = append(objectsToAdd, gatingMutatingWebhook)
	}
	objectsToAdd = append(objectsToAdd, createGatingValidatingWebhook(args.Namespace, args.Client, args.Logger))
	return objectsToAdd
}
func getMaroonedPodsServerClusterPolicyRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{
			APIGroups: []string{
				"kubevirt.io",
			},
			Resources: []string{
				"kubevirts",
			},
			Verbs: []string{
				"list",
				"watch",
			},
		},
		{
			APIGroups: []string{
				"",
			},
			Resources: []string{
				"resourcequotas",
			},
			Verbs: []string{
				"list",
				"watch",
				"update",
				"create",
			},
		},
	}
}

func createAPIServerClusterRoleBinding(namespace string) *rbacv1.ClusterRoleBinding {
	return util.ResourceBuilder.CreateClusterRoleBinding(mpServerResourceName, mpServerResourceName, mpServerResourceName, namespace)
}

func createAPIServerClusterRole() *rbacv1.ClusterRole {
	return util.ResourceBuilder.CreateClusterRole(mpServerResourceName, getMaroonedPodsServerClusterPolicyRules())
}
func createGatingMutatingWebhook(namespace string, c client.Client, l logr.Logger) *admissionregistrationv1.MutatingWebhookConfiguration {
	cr, _ := util.GetActiveMaroonedPods(c)
	if cr == nil {
		return nil
	}
	includeHooks := true
	serverDeployment, err := util.GetDeployment(c, mpServerResourceName, namespace)
	if err != nil || serverDeployment == nil || serverDeployment.Status.ReadyReplicas < 1 {
		includeHooks = false
	}
	controllerDeployment, err := util.GetDeployment(c, util.ControllerResourceName, namespace)
	if err != nil || controllerDeployment == nil || controllerDeployment.Status.ReadyReplicas < 1 {
		includeHooks = false
	}

	path := mpserver.ServePath
	defaultServicePort := int32(443)
	namespacedScope := admissionregistrationv1.NamespacedScope
	exactPolicy := admissionregistrationv1.Equivalent
	failurePolicy := admissionregistrationv1.Fail
	sideEffect := admissionregistrationv1.SideEffectClassNone

	hooks := []admissionregistrationv1.MutatingWebhook{}
	if includeHooks {
		hooks = []admissionregistrationv1.MutatingWebhook{
			{
				Name:                    "gater.maroonedpods.io",
				AdmissionReviewVersions: []string{"v1", "v1beta1"},
				FailurePolicy:           &failurePolicy,
				SideEffects:             &sideEffect,
				MatchPolicy:             &exactPolicy,
				NamespaceSelector:       cr.Spec.NamespaceSelector,
				Rules: []admissionregistrationv1.RuleWithOperations{{
					Operations: []admissionregistrationv1.OperationType{
						admissionregistrationv1.Create,
					},
					Rule: admissionregistrationv1.Rule{
						APIGroups:   []string{"*"},
						APIVersions: []string{"*"},
						Scope:       &namespacedScope,
						Resources:   []string{"pods"},
					},
				}},
				ClientConfig: admissionregistrationv1.WebhookClientConfig{
					Service: &admissionregistrationv1.ServiceReference{
						Namespace: namespace,
						Name:      MaroonedPodsServerServiceName,
						Path:      &path,
						Port:      &defaultServicePort,
					},
				},
			},
		}
	}

	mhc := &admissionregistrationv1.MutatingWebhookConfiguration{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admissionregistration.k8s.io/v1",
			Kind:       "MutatingWebhookConfiguration",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: MutatingWebhookConfigurationName,
			Labels: map[string]string{
				util.MaroonedPodsLabel: MaroonedPodsServerServiceName,
			},
		},
		Webhooks: hooks,
	}

	if c == nil {
		return mhc
	}
	bundle := getAPIServerCABundle(namespace, c, l)
	if bundle != nil {
		for i := range mhc.Webhooks {
			mhc.Webhooks[i].ClientConfig.CABundle = bundle
		}
	}

	return mhc
}

func createGatingValidatingWebhook(namespace string, c client.Client, l logr.Logger) *admissionregistrationv1.ValidatingWebhookConfiguration {
	cr, _ := util.GetActiveMaroonedPods(c)
	if cr == nil {
		return nil
	}
	includeHooks := true
	serverDeployment, err := util.GetDeployment(c, mpServerResourceName, namespace)
	if err != nil || serverDeployment == nil || serverDeployment.Status.ReadyReplicas < 1 {
		includeHooks = false
	}
	controllerDeployment, err := util.GetDeployment(c, util.ControllerResourceName, namespace)
	if err != nil || controllerDeployment == nil || controllerDeployment.Status.ReadyReplicas < 1 {
		includeHooks = false
	}
	path := mpserver.ServePath
	defaultServicePort := int32(443)
	namespacedScope := admissionregistrationv1.NamespacedScope
	exactPolicy := admissionregistrationv1.Equivalent
	failurePolicy := admissionregistrationv1.Fail
	sideEffect := admissionregistrationv1.SideEffectClassNone
	hooks := []admissionregistrationv1.ValidatingWebhook{}
	if includeHooks {
		hooks = []admissionregistrationv1.ValidatingWebhook{
			{
				Name:                    "maroonedpods.validator",
				AdmissionReviewVersions: []string{"v1", "v1beta1"},
				FailurePolicy:           &failurePolicy,
				SideEffects:             &sideEffect,
				MatchPolicy:             &exactPolicy,
				Rules: []admissionregistrationv1.RuleWithOperations{
					{
						Operations: []admissionregistrationv1.OperationType{
							admissionregistrationv1.Create,
							admissionregistrationv1.Update,
						},
						Rule: admissionregistrationv1.Rule{
							APIGroups:   []string{"*"},
							APIVersions: []string{"*"},
							Scope:       &namespacedScope,
							Resources:   []string{"maroonedpods"},
						},
					},
				},

				ClientConfig: admissionregistrationv1.WebhookClientConfig{
					Service: &admissionregistrationv1.ServiceReference{
						Namespace: namespace,
						Name:      MaroonedPodsServerServiceName,
						Path:      &path,
						Port:      &defaultServicePort,
					},
				},
			},
			{
				Name:                    "remove.pod.gate.validator",
				AdmissionReviewVersions: []string{"v1", "v1beta1"},
				FailurePolicy:           &failurePolicy,
				SideEffects:             &sideEffect,
				MatchPolicy:             &exactPolicy,
				NamespaceSelector:       cr.Spec.NamespaceSelector,
				Rules: []admissionregistrationv1.RuleWithOperations{
					{
						Operations: []admissionregistrationv1.OperationType{
							admissionregistrationv1.Update,
						},
						Rule: admissionregistrationv1.Rule{
							APIGroups:   []string{"*"},
							APIVersions: []string{"*"},
							Scope:       &namespacedScope,
							Resources:   []string{"pods"},
						},
					},
				},

				ClientConfig: admissionregistrationv1.WebhookClientConfig{
					Service: &admissionregistrationv1.ServiceReference{
						Namespace: namespace,
						Name:      MaroonedPodsServerServiceName,
						Path:      &path,
						Port:      &defaultServicePort,
					},
				},
			},
		}
	}

	mhc := &admissionregistrationv1.ValidatingWebhookConfiguration{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admissionregistration.k8s.io/v1",
			Kind:       "ValidatingWebhookConfiguration",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: validatingWebhookConfigurationName,
			Labels: map[string]string{
				util.MaroonedPodsLabel: MaroonedPodsServerServiceName,
			},
		},
		Webhooks: hooks,
	}

	if c == nil {
		return mhc
	}
	bundle := getAPIServerCABundle(namespace, c, l)
	if bundle != nil {
		for i := range mhc.Webhooks {
			mhc.Webhooks[i].ClientConfig.CABundle = bundle
		}
	}
	return mhc
}

func getAPIServerCABundle(namespace string, c client.Client, l logr.Logger) []byte {
	cm := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: namespace, Name: "maroonedpods-server-signer-bundle"}
	if err := c.Get(context.TODO(), key, cm); err != nil {
		l.Error(err, "error getting gater ca bundle")
		return nil
	}
	if cert, ok := cm.Data["ca-bundle.crt"]; ok {
		return []byte(cert)
	}
	l.V(2).Info("CA bundle missing")
	return nil
}
