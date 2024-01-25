package mp_controller

import (
	"context"
	"fmt"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	v14 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	_ "kubevirt.io/api/core/v1"
	"maroonedpods.io/maroonedpods/pkg/client"
	"maroonedpods.io/maroonedpods/pkg/log"
	"maroonedpods.io/maroonedpods/pkg/util"
	"time"
)

type enqueueState string

const (
	Immediate  enqueueState = "Immediate"
	Forget     enqueueState = "Forget"
	BackOff    enqueueState = "BackOff"
)

type MaroonedPodsGateController struct {
	podInformer                  cache.SharedIndexInformer
	maroonedpodsCli                       client.MaroonedPodsClient
	recorder                     record.EventRecorder
	stop                         <-chan struct{}
	enqueueAllGateControllerChan <-chan struct{}
    queue                     workqueue.RateLimitingInterface
}

func NewMaroonedPodsGateController(maroonedpodsCli client.MaroonedPodsClient,
	podInformer cache.SharedIndexInformer,
	stop <-chan struct{},
	enqueueAllGateControllerChan <-chan struct{},
) *MaroonedPodsGateController {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&v14.EventSinkImpl{Interface: maroonedpodsCli.CoreV1().Events(v1.NamespaceAll)})

	ctrl := MaroonedPodsGateController{
		maroonedpodsCli:                       maroonedpodsCli,
		podInformer:                  podInformer,
        queue:                     workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "maroonedpods-queue"),

		recorder:                     eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: util.ControllerPodName}),
		maroonedpodsEvaluator:                 maroonedpods_evaluator.NewMaroonedPodsEvaluator(podInformer, calcRegistry, clock.RealClock{}),
		stop:                         stop,
		enqueueAllGateControllerChan: enqueueAllGateControllerChan,
	}

	_, err := ctrl.podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ctrl.addPod,
		UpdateFunc: ctrl.updatePod,
	})
	if err != nil {
		panic("something is wrong")

	}

	return &ctrl
}


func (ctrl *MaroonedPodsGateController) addPod(obj interface{}) {
	pod := obj.(*v1.Pod)

	if pod.Spec.SchedulingGates != nil &&
		len(pod.Spec.SchedulingGates) == 1 &&
		pod.Spec.SchedulingGates[0].Name == util.MaroonedPodsGate {
        klog.Info(fmt.Sprintf("Adding pod with gate %s", pod.Name))
	}
}
func (ctrl *MaroonedPodsGateController) updatePod(old, curr interface{}) {
	pod := curr.(*v1.Pod)

	if pod.Spec.SchedulingGates != nil &&
		len(pod.Spec.SchedulingGates) == 1 &&
		pod.Spec.SchedulingGates[0].Name == util.MaroonedPodsGate {
        klog.Info(fmt.Sprintf("Updating pod with gate %s", pod.Name))
	}
}

func (ctrl *MaroonedPodsGateController) runWorker() {
	for ctrl.Execute() {
	}
}

func (ctrl *MaroonedPodsGateController) Execute() bool {
	key, quit := ctrl.queue.Get()
	if quit {
		return false
	}
	defer ctrl.queue.Done(key)

	err, enqueueState := ctrl.execute(key.(string))
	if err != nil {
		klog.Errorf(fmt.Sprintf("MaroonedPodsGateController: Error with key: %v err: %v", key, err))
	}
	switch enqueueState {
	case BackOff:
		ctrl.queue.AddRateLimited(key)
	case Forget:
		ctrl.queue.Forget(key)
	case Immediate:
		ctrl.queue.Add(key)
	}

	return true
}

func (ctrl *MaroonedPodsGateController) execute(key string) (error, enqueueState) {
	return nil, Forget
}

func (ctrl *MaroonedPodsGateController) Run(ctx context.Context, threadiness int) {
	defer utilruntime.HandleCrash()
	klog.Info("Starting maroonedpods controller")
	defer klog.Info("Shutting down maroonedpods controller")
	defer ctrl.queue.ShutDown()

	// Start a goroutine to listen for enqueue signals and call enqueueAll in case the configuration is changed.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ctrl.enqueueAllGateControllerChan:
				log.Log.Infof("MaroonedPodsGateController: Signal processed enqueued All")
				ctrl.enqueueAll()
			}
		}
	}()
	for i := 0; i < threadiness; i++ {
		go wait.Until(ctrl.runWorker, time.Second, ctrl.stop)
	}

	<-ctrl.stop

}
