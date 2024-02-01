package informers

import (
	"context"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	k6tv1 "kubevirt.io/api/core/v1"
	"maroonedpods.io/maroonedpods/pkg/client"
	v1alpha13 "maroonedpods.io/maroonedpods/staging/src/maroonedpods.io/api/pkg/apis/core/v1alpha1"
	"time"
)

func GetMaroonedPodsInformer(maroonedpodsCli client.MaroonedPodsClient) cache.SharedIndexInformer {
	listWatcher := NewListWatchFromClient(maroonedpodsCli.RestClient(), "maroonedpods", metav1.NamespaceAll, fields.Everything(), labels.Everything())
	return cache.NewSharedIndexInformer(listWatcher, &v1alpha13.MaroonedPods{}, 1*time.Hour, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
}

func GetPodInformer(maroonedpodsCli client.MaroonedPodsClient) cache.SharedIndexInformer {
	listWatcher := NewListWatchFromClient(maroonedpodsCli.CoreV1().RESTClient(), "pods", metav1.NamespaceAll, fields.Everything(), labels.Everything())
	return cache.NewSharedIndexInformer(listWatcher, &v1.Pod{}, 1*time.Hour, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
}

func GetPodsToMaroonInformer(maroonedpodsCli client.MaroonedPodsClient) cache.SharedIndexInformer {
	labelSelector, err := labels.Parse("maroonedpods.io/maroon=true")
	if err != nil {
		panic(err)
	}
	listWatcher := NewListWatchFromClient(maroonedpodsCli.CoreV1().RESTClient(), "pods", metav1.NamespaceAll, fields.Everything(), labelSelector)
	return cache.NewSharedIndexInformer(listWatcher, &v1.Pod{}, 1*time.Hour, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
}

func GetNodesInformer(maroonedpodsCli client.MaroonedPodsClient) cache.SharedIndexInformer {
	listWatcher := NewListWatchFromClient(maroonedpodsCli.CoreV1().RESTClient(), "nodes", metav1.NamespaceAll, fields.Everything(), labels.Everything())
	return cache.NewSharedIndexInformer(listWatcher, &v1.Node{}, 1*time.Hour, cache.Indexers{})
}

func GetSecretInformer(maroonedpodsCli client.MaroonedPodsClient, ns string) cache.SharedIndexInformer {
	listWatcher := NewListWatchFromClient(maroonedpodsCli.CoreV1().RESTClient(), "secrets", ns, fields.Everything(), labels.Everything())
	return cache.NewSharedIndexInformer(listWatcher, &v1.Secret{}, 1*time.Hour, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
}

func GetVMIInformer(maroonedpodsCli client.MaroonedPodsClient) cache.SharedIndexInformer {
	listWatcher := NewListWatchFromClient(maroonedpodsCli.KubevirtClient().KubevirtV1().RESTClient(), "virtualmachineinstances", metav1.NamespaceAll, fields.Everything(), labels.Everything())
	return cache.NewSharedIndexInformer(listWatcher, &k6tv1.VirtualMachineInstance{}, 1*time.Hour, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
}

// NewListWatchFromClient creates a new ListWatch from the specified client, resource, kubevirtNamespace and field selector.
func NewListWatchFromClient(c cache.Getter, resource string, namespace string, fieldSelector fields.Selector, labelSelector labels.Selector) *cache.ListWatch {
	listFunc := func(options metav1.ListOptions) (runtime.Object, error) {
		options.FieldSelector = fieldSelector.String()
		options.LabelSelector = labelSelector.String()
		return c.Get().
			Namespace(namespace).
			Resource(resource).
			VersionedParams(&options, metav1.ParameterCodec).
			Do(context.Background()).
			Get()
	}
	watchFunc := func(options metav1.ListOptions) (watch.Interface, error) {
		options.FieldSelector = fieldSelector.String()
		options.LabelSelector = labelSelector.String()
		options.Watch = true
		return c.Get().
			Namespace(namespace).
			Resource(resource).
			VersionedParams(&options, metav1.ParameterCodec).
			Watch(context.Background())
	}
	return &cache.ListWatch{ListFunc: listFunc, WatchFunc: watchFunc}
}

type fakePodSharedIndexInformer struct {
	indexer cache.Indexer
}

func NewFakePodSharedIndexInformer(podObjs []metav1.Object) cache.SharedIndexInformer {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, podObj := range podObjs {
		indexer.Add(podObj)
	}
	return fakePodSharedIndexInformer{indexer: indexer}
}

func (i fakePodSharedIndexInformer) RemoveEventHandler(handle cache.ResourceEventHandlerRegistration) error {
	//TODO implement me
	panic("implement me")
}

func (i fakePodSharedIndexInformer) IsStopped() bool {
	panic("implement me")
}

func (i fakePodSharedIndexInformer) AddIndexers(indexers cache.Indexers) error { return nil }
func (i fakePodSharedIndexInformer) GetIndexer() cache.Indexer {
	return i.indexer

}
func (i fakePodSharedIndexInformer) AddEventHandler(handler cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}
func (i fakePodSharedIndexInformer) AddEventHandlerWithResyncPeriod(handler cache.ResourceEventHandler, resyncPeriod time.Duration) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}
func (i fakePodSharedIndexInformer) GetStore() cache.Store           { return nil }
func (i fakePodSharedIndexInformer) GetController() cache.Controller { return nil }
func (i fakePodSharedIndexInformer) Run(stopCh <-chan struct{})      {}
func (i fakePodSharedIndexInformer) HasSynced() bool                 { panic("implement me") }
func (i fakePodSharedIndexInformer) LastSyncResourceVersion() string { return "" }
func (i fakePodSharedIndexInformer) SetWatchErrorHandler(handler cache.WatchErrorHandler) error {
	return nil
}
func (i fakePodSharedIndexInformer) SetTransform(f cache.TransformFunc) error {
	return nil
}
