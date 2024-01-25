package client

//go:generate mockgen -source $GOFILE -package=$GOPACKAGE -destination=generated_mock_$GOFILE

/*
 ATTENTION: Rerun code generators when interface signatures are modified.
*/

import (
	"context"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	generatedclient "maroonedpods.io/maroonedpods/pkg/generated/maroonedpods/clientset/versioned"
	mpv1alpha1 "maroonedpods.io/maroonedpods/pkg/generated/maroonedpods/clientset/versioned/typed/core/v1alpha1"
	kubevirtclient "maroonedpods.io/maroonedpods/pkg/generated/kubevirt/clientset/versioned"
	"maroonepods.io/staging/src/maroonedpods.io/api/pkg/apis/core/v1alpha1"
)

type MaroonedPodsClient interface {
	RestClient() *rest.RESTClient
	kubernetes.Interface
	MaroonedPods() MaroonedPodsInterface
	GeneratedMaroonedPodsClient() generatedclient.Interface
	KubevirtClient() kubevirtclient.Interface
	Config() *rest.Config
}

type maroonedpods struct {
	master             string
	kubeconfig         string
	restClient         *rest.RESTClient
	config             *rest.Config
	generatedMaroonedPodsClient *generatedclient.Clientset
	kubevirtClient     *kubevirtclient.Clientset
	dynamicClient      dynamic.Interface
	*kubernetes.Clientset
}

func (k maroonedpods) KubevirtClient() kubevirtclient.Interface {
	return k.kubevirtClient
}

func (k maroonedpods) Config() *rest.Config {
	return k.config
}

func (k maroonedpods) RestClient() *rest.RESTClient {
	return k.restClient
}

func (k maroonedpods) GeneratedMaroonedPodsClient() generatedclient.Interface {
	return k.generatedMaroonedPodsClient
}

func (k maroonedpods) MaroonedPods() MaroonedPodsInterface {
	return k.generatedMaroonedPodsClient.MaroonedPodsV1alpha1().MaroonedPods()
}

func (k maroonedpods) DynamicClient() dynamic.Interface {
	return k.dynamicClient
}

// MaroonedPodsInterface has methods to work with MaroonedPods resources.
type MaroonedPodsInterface interface {
	Create(ctx context.Context, maroonedPods *v1alpha1.MaroonedPods, opts metav1.CreateOptions) (*v1alpha1.MaroonedPods, error)
	Update(ctx context.Context, maroonedPods *v1alpha1.MaroonedPods, opts metav1.UpdateOptions) (*v1alpha1.MaroonedPods, error)
	UpdateStatus(ctx context.Context, maroonedPods *v1alpha1.MaroonedPods, opts metav1.UpdateOptions) (*v1alpha1.MaroonedPods, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
	DeleteCollection(ctx context.Context, opts metav1.DeleteOptions, listOpts metav1.ListOptions) error
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*v1alpha1.MaroonedPods, error)
	List(ctx context.Context, opts metav1.ListOptions) (*v1alpha1.MaroonedPodsList, error)
	Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error)
	Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (result *v1alpha1.MaroonedPods, err error)
	mpv1alpha1.MaroonedPodsExpansion
}
