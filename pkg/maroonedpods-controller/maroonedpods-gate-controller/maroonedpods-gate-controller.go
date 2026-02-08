package mp_controller

import (
	"context"
	"encoding/base64"
	"fmt"
	"math/rand"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	k8smetav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	v14 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	virtv1 "kubevirt.io/api/core/v1"
	"maroonedpods.io/maroonedpods/pkg/client"
	"maroonedpods.io/maroonedpods/pkg/log"
	"maroonedpods.io/maroonedpods/pkg/util"
	v1alpha1 "maroonedpods.io/maroonedpods/staging/src/maroonedpods.io/api/pkg/apis/core/v1alpha1"
	"time"
)

var (
	// Move to a controller file
	KeyFunc = cache.DeletionHandlingMetaNamespaceKeyFunc
)

type enqueueState string

const (
	Immediate enqueueState = "Immediate"
	Forget    enqueueState = "Forget"
	BackOff   enqueueState = "BackOff"
)

type MaroonedPodsGateController struct {
	podInformer                  cache.SharedIndexInformer
	vmiInformer                  cache.SharedIndexInformer
	nodeInformer                 cache.SharedIndexInformer
	configInformer               cache.SharedIndexInformer
	maroonedpodsCli              client.MaroonedPodsClient
	recorder                     record.EventRecorder
	stop                         <-chan struct{}
	enqueueAllGateControllerChan <-chan struct{}
	queue                        workqueue.RateLimitingInterface
}

func NewMaroonedPodsGateController(maroonedpodsCli client.MaroonedPodsClient,
	podInformer cache.SharedIndexInformer,
	vmiInformer cache.SharedIndexInformer,
	nodeInformer cache.SharedIndexInformer,
	configInformer cache.SharedIndexInformer,
	stop <-chan struct{},
	enqueueAllGateControllerChan <-chan struct{},
) *MaroonedPodsGateController {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&v14.EventSinkImpl{Interface: maroonedpodsCli.CoreV1().Events(v1.NamespaceAll)})

	ctrl := MaroonedPodsGateController{
		maroonedpodsCli: maroonedpodsCli,
		podInformer:     podInformer,
		vmiInformer:     vmiInformer,
		nodeInformer:    nodeInformer,
		configInformer:  configInformer,
		queue:           workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "maroonedpods-queue"),

		recorder:                     eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: util.ControllerPodName}),
		stop:                         stop,
		enqueueAllGateControllerChan: enqueueAllGateControllerChan,
	}

	_, err := ctrl.podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ctrl.addPod,
		UpdateFunc: ctrl.updatePod,
		DeleteFunc: ctrl.deletePod,
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
		key, err := KeyFunc(pod)
		if err != nil {
			log.Log.Info("Failed to obtain pod key function")
		}

		ctrl.queue.Add(key)
	}
}
func (ctrl *MaroonedPodsGateController) updatePod(old, curr interface{}) {
	pod := curr.(*v1.Pod)

	if pod.Spec.SchedulingGates != nil &&
		len(pod.Spec.SchedulingGates) == 1 &&
		pod.Spec.SchedulingGates[0].Name == util.MaroonedPodsGate {
		klog.Info(fmt.Sprintf("Updating pod with gate %s", pod.Name))
		key, err := KeyFunc(pod)
		if err != nil {
			log.Log.Info("Failed to obtain pod key function")
		}
		ctrl.queue.Add(key)
	}
}

func (ctrl *MaroonedPodsGateController) deletePod(obj interface{}) {
	pod := obj.(*v1.Pod)
	klog.V(3).Infof("Pod %s/%s deleted, checking for warm pool VMI to return", pod.Namespace, pod.Name)

	// Try to find the VMI for this pod
	key, err := KeyFunc(pod)
	if err != nil {
		klog.Errorf("Failed to get key for deleted pod %s/%s: %v", pod.Namespace, pod.Name, err)
		return
	}

	vmiObj, exists, err := ctrl.vmiInformer.GetStore().GetByKey(key)
	if err != nil {
		klog.Errorf("Failed to fetch VMI for deleted pod %s: %v", key, err)
		return
	}

	if !exists {
		klog.V(3).Infof("No VMI found for deleted pod %s", key)
		return
	}

	vmi := vmiObj.(*virtv1.VirtualMachineInstance)

	// Check if this is a pool VMI
	if ctrl.isPoolVMI(vmi) {
		klog.Infof("Returning pool VMI %s to available pool after pod %s/%s deletion", vmi.Name, pod.Namespace, pod.Name)
		err = ctrl.returnVMIToPool(vmi, pod.Name)
		if err != nil {
			klog.Errorf("Failed to return VMI %s to pool: %v", vmi.Name, err)
		}
	} else {
		// Not a pool VMI, delete it (original behavior for on-demand VMs)
		klog.Infof("Deleting on-demand VMI %s for pod %s/%s", vmi.Name, pod.Namespace, pod.Name)
		err = ctrl.maroonedpodsCli.KubevirtClient().KubevirtV1().VirtualMachineInstances(vmi.Namespace).Delete(
			context.Background(), vmi.Name, k8smetav1.DeleteOptions{})
		if err != nil {
			klog.Errorf("Failed to delete VMI %s: %v", vmi.Name, err)
		}
	}
}

// reconcileWarmPool maintains the desired warm pool size
func (ctrl *MaroonedPodsGateController) reconcileWarmPool() {
	config := ctrl.getConfig()
	if config == nil {
		klog.V(4).Info("No config found, skipping warm pool reconciliation")
		return
	}

	desiredPoolSize := config.Spec.WarmPoolSize
	if desiredPoolSize == 0 {
		klog.V(4).Info("Warm pool disabled (size=0), skipping reconciliation")
		return
	}

	// Count pool VMs by state
	creating := 0
	available := 0
	claimed := 0

	vmis := ctrl.vmiInformer.GetStore().List()
	for _, obj := range vmis {
		vmi := obj.(*virtv1.VirtualMachineInstance)
		if vmi.Labels == nil {
			continue
		}

		state, hasLabel := vmi.Labels[util.WarmPoolStateLabel]
		if !hasLabel {
			continue
		}

		switch state {
		case util.PoolStateCreating:
			// Check if VMI is now running and node has joined
			if vmi.Status.Phase == virtv1.Running {
				// Check if node exists
				_, nodeExists, _ := ctrl.nodeInformer.GetStore().GetByKey(vmi.Name)
				if nodeExists {
					// Mark as available
					ctrl.markVMIAvailable(vmi)
					available++
				} else {
					creating++
				}
			} else {
				creating++
			}
		case util.PoolStateAvailable:
			available++
		case util.PoolStateClaimed:
			claimed++
		}
	}

	totalPool := creating + available
	klog.V(3).Infof("Warm pool state: desired=%d, creating=%d, available=%d, claimed=%d, total=%d",
		desiredPoolSize, creating, available, claimed, totalPool)

	// Update config status with pool metrics
	ctrl.updateConfigStatus(int32(totalPool), int32(available), int32(claimed))

	// Create new VMs if below desired size
	if totalPool < int(desiredPoolSize) {
		toCreate := int(desiredPoolSize) - totalPool
		klog.Infof("Warm pool below desired size, creating %d new VMs", toCreate)

		// Use maroonedpods namespace for pool VMs
		// TODO: Make namespace configurable
		namespace := util.DefaultMaroonedPodsNs

		for i := 0; i < toCreate; i++ {
			_, err := ctrl.createPoolVMI(namespace)
			if err != nil {
				klog.Errorf("Failed to create pool VMI: %v", err)
			}
		}
	}

	// Delete excess VMs if above desired size (only available ones)
	if available > int(desiredPoolSize) {
		toDelete := available - int(desiredPoolSize)
		klog.Infof("Warm pool above desired size, deleting %d available VMs", toDelete)

		deleted := 0
		for _, obj := range vmis {
			if deleted >= toDelete {
				break
			}

			vmi := obj.(*virtv1.VirtualMachineInstance)
			if vmi.Labels == nil {
				continue
			}

			if state, ok := vmi.Labels[util.WarmPoolStateLabel]; ok && state == util.PoolStateAvailable {
				err := ctrl.maroonedpodsCli.KubevirtClient().KubevirtV1().VirtualMachineInstances(vmi.Namespace).Delete(
					context.Background(), vmi.Name, k8smetav1.DeleteOptions{})
				if err != nil {
					klog.Errorf("Failed to delete excess pool VMI %s: %v", vmi.Name, err)
				} else {
					klog.Infof("Deleted excess pool VMI %s", vmi.Name)
					deleted++
				}
			}
		}
	}
}

// markVMIAvailable marks a creating VMI as available in the pool
func (ctrl *MaroonedPodsGateController) markVMIAvailable(vmi *virtv1.VirtualMachineInstance) error {
	klog.Infof("Marking VMI %s/%s as available in warm pool", vmi.Namespace, vmi.Name)

	vmiCopy := vmi.DeepCopy()
	if vmiCopy.Labels == nil {
		vmiCopy.Labels = make(map[string]string)
	}
	vmiCopy.Labels[util.WarmPoolStateLabel] = util.PoolStateAvailable

	_, err := ctrl.maroonedpodsCli.KubevirtClient().KubevirtV1().VirtualMachineInstances(vmiCopy.Namespace).Update(
		context.Background(), vmiCopy, k8smetav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to mark VMI as available: %v", err)
	}

	klog.Infof("VMI %s is now available in warm pool", vmi.Name)
	return nil
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
	//log.Log.Infof("Working on pod: %s", key)
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

	// get key from informer
	obj, exists, err := ctrl.podInformer.GetStore().GetByKey(key)
	if err != nil {
		return nil, BackOff
	}
	if !exists {
		// nothing we need to do. It should always be possible to re-create this type of controller
		// c.expectations.DeleteExpectations(key)
		return nil, BackOff
	}
	pod := obj.(*v1.Pod)

	logger := log.Log.Object(pod)

	podKey, err := KeyFunc(pod)
	if err != nil {
		logger.Info("Failed to obtain pod key function")
		return err, BackOff
	}

	// Try to find an existing Virtual Machine Instance
	var vmi *virtv1.VirtualMachineInstance
	vmiObj, exist, err := ctrl.vmiInformer.GetStore().GetByKey(podKey)
	if err != nil {
		logger.Reason(err).Error("Failed to fetch vmi for namespace from cache.")
		return err, BackOff
	}
	if !exist {
		logger.V(4).Infof("VirtualMachineInstance not found in cache %s", key)
		vmi = nil
	} else {
		vmi = vmiObj.(*virtv1.VirtualMachineInstance)
	}
	/*
		// We will need to handle VMI ownerships, but that's later.
		else {
			vmi = vmiObj.(*virtv1.VirtualMachineInstance)

			vmi, err = cm.ClaimVirtualMachineInstanceByName(vmi)
			if err != nil {
				return err
			}
		}*/

	err1 := ctrl.sync(pod, vmi, key)
	if err1 != nil {
		logger.Reason(err1).Error("sync failed")
		return err1, BackOff
	}

	return nil, Immediate
}

// updatePodNodeSelector updates the pod's nodeSelector to point to a specific node
func (ctrl *MaroonedPodsGateController) updatePodNodeSelector(pod *v1.Pod, nodeName string) error {
	podCopy := pod.DeepCopy()
	if podCopy.Spec.NodeSelector == nil {
		podCopy.Spec.NodeSelector = make(map[string]string)
	}
	podCopy.Spec.NodeSelector["kubernetes.io/hostname"] = nodeName

	_, err := ctrl.maroonedpodsCli.CoreV1().Pods(podCopy.Namespace).Update(context.Background(), podCopy, k8smetav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update pod nodeSelector: %v", err)
	}

	klog.Infof("Updated pod %s/%s nodeSelector to node %s", pod.Namespace, pod.Name, nodeName)
	return nil
}

func (ctrl *MaroonedPodsGateController) releasePod(key string) error {
	// check if it's deleted
	log.Log.Infof("Going to release pod: %s", key)
	obj, exists, err := ctrl.podInformer.GetIndexer().GetByKey(key)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	pod := obj.(*v1.Pod).DeepCopy()
	if pod.Spec.SchedulingGates != nil && len(pod.Spec.SchedulingGates) == 1 && pod.Spec.SchedulingGates[0].Name == util.MaroonedPodsGate {
		pod.Spec.SchedulingGates = []v1.PodSchedulingGate{}
		_, err = ctrl.maroonedpodsCli.CoreV1().Pods(pod.Namespace).Update(context.Background(), pod, k8smetav1.UpdateOptions{})
		if err != nil {
			return err
		}
	}
	log.Log.Infof("pod: %s is released", key)
	return nil
}

func (ctrl *MaroonedPodsGateController) sync(pod *v1.Pod, vmi *virtv1.VirtualMachineInstance, key string) error {
	if vmi == nil {
		// Try to claim from warm pool first
		poolVMI := ctrl.getAvailablePoolVMI()
		if poolVMI != nil {
			klog.Infof("Found available pool VMI %s for pod %s/%s", poolVMI.Name, pod.Namespace, pod.Name)
			err := ctrl.claimPoolVMI(poolVMI, pod)
			if err != nil {
				klog.Errorf("Failed to claim pool VMI: %v, falling back to creating new VMI", err)
				// Fall through to create new VMI
			} else {
				// Update pod's nodeSelector to point to the claimed VM's node
				err = ctrl.updatePodNodeSelector(pod, poolVMI.Name)
				if err != nil {
					klog.Errorf("Failed to update pod nodeSelector: %v", err)
					return err
				}
				vmi = poolVMI
				return nil // Pool VMI is already running, no need to wait
			}
		}

		// No available pool VMI, create new one
		klog.Infof("No available pool VMI, creating new VMI for pod %s/%s", pod.Namespace, pod.Name)
		vmi := ctrl.createVMIFromPod(pod)
		vmi, err := ctrl.maroonedpodsCli.KubevirtClient().KubevirtV1().VirtualMachineInstances(pod.Namespace).Create(context.Background(), vmi, k8smetav1.CreateOptions{})
		if err != nil {
			log.Log.Reason(err).Error("failed to create VMI")
			return err
		}
	}

	if vmi.Status.Phase == virtv1.Running {

		vmiObj, exist, err1 := ctrl.vmiInformer.GetStore().GetByKey(key)
		if err1 != nil {
			log.Log.Reason(err1).Error("Failed to fetch vmi for namespace from cache.")
		}
		if !exist {
			log.Log.Errorf("VirtualMachineInstance not found in cache %s", key)
		} else {
			vmi = vmiObj.(*virtv1.VirtualMachineInstance)
			if vmi.Status.Phase != virtv1.Running {
				return fmt.Errorf("wainting for VMI %s to become Running, currently %s", vmi.Name, string(vmi.Status.Phase))
			}
		}

	}

	_, nodeExist, err := ctrl.nodeInformer.GetStore().GetByKey(vmi.Name)
	log.Log.Infof("node %s is already present", vmi.Name)
	if err != nil {
		log.Log.Reason(err).Error("Failed to fetch node from cache.")
		return err
	}
	if !nodeExist {
		log.Log.V(4).Infof("Node not found in cache %s", key)
		return err
	} else {
		err = ctrl.releasePod(key)
		if err != nil {
			return err
		}
		//nodeObj = obj.(*v1.Node)

	}

	/*if nodeObj.Status.Phase == virtv1.Running {
		return fmt.Errorf("wainting for Node %s to become Ready", vmi.Name)
	}*/
	return nil
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
			}
		}
	}()

	// Start warm pool reconciler
	go wait.Until(ctrl.reconcileWarmPool, 30*time.Second, ctrl.stop)

	for i := 0; i < threadiness; i++ {
		go wait.Until(ctrl.runWorker, time.Second, ctrl.stop)
	}

	<-ctrl.stop

}

// getConfig retrieves the MaroonedPodsConfig from the informer cache.
// Returns the first config found, or nil if none exists.
func (ctrl *MaroonedPodsGateController) getConfig() *v1alpha1.MaroonedPodsConfig {
	configs := ctrl.configInformer.GetStore().List()
	if len(configs) == 0 {
		return nil
	}
	// Return the first config (there should typically be only one cluster-scoped config)
	return configs[0].(*v1alpha1.MaroonedPodsConfig)
}

// updateConfigStatus updates the MaroonedPodsConfig status with warm pool metrics
func (ctrl *MaroonedPodsGateController) updateConfigStatus(total, available, claimed int32) {
	config := ctrl.getConfig()
	if config == nil {
		return
	}

	// Check if status actually changed
	if config.Status.WarmPoolTotal == total &&
		config.Status.WarmPoolAvailable == available &&
		config.Status.WarmPoolClaimed == claimed {
		return // No change, skip update
	}

	configCopy := config.DeepCopy()
	configCopy.Status.WarmPoolTotal = total
	configCopy.Status.WarmPoolAvailable = available
	configCopy.Status.WarmPoolClaimed = claimed

	// Use the generated client to update status
	_, err := ctrl.maroonedpodsCli.RestClient().Put().
		Resource("maroonedpodsconfigs").
		Name(configCopy.Name).
		SubResource("status").
		Body(configCopy).
		Do(context.Background()).
		Get()

	if err != nil {
		klog.V(3).Infof("Failed to update MaroonedPodsConfig status: %v", err)
	} else {
		klog.V(4).Infof("Updated MaroonedPodsConfig status: total=%d, available=%d, claimed=%d", total, available, claimed)
	}
}

// getVMResourcesFromConfig returns VM resources from config with defaults.
// Default: 2 CPU, 3072Mi (3Gi) memory
func (ctrl *MaroonedPodsGateController) getVMResourcesFromConfig() (cpuCores uint32, memoryMi uint64, nodeImage string, taintKey string) {
	// Set defaults
	cpuCores = 2
	memoryMi = 3072
	nodeImage = "quay.io/capk/ubuntu-2004-container-disk:v1.26.0"
	taintKey = "maroonedpods.io"

	config := ctrl.getConfig()
	if config == nil {
		klog.V(3).Info("No MaroonedPodsConfig found, using defaults")
		return
	}

	// Apply config values if set
	if config.Spec.BaseVMResources.CPU > 0 {
		cpuCores = config.Spec.BaseVMResources.CPU
	}
	if config.Spec.BaseVMResources.MemoryMi > 0 {
		memoryMi = config.Spec.BaseVMResources.MemoryMi
	}
	if config.Spec.NodeImage != "" {
		nodeImage = config.Spec.NodeImage
	}
	if config.Spec.NodeTaintKey != "" {
		taintKey = config.Spec.NodeTaintKey
	}

	klog.V(3).Infof("Using VM resources: CPU=%d, Memory=%dMi, Image=%s, TaintKey=%s", cpuCores, memoryMi, nodeImage, taintKey)
	return
}

// getAvailablePoolVMI returns an available VMI from the warm pool, or nil if none available
func (ctrl *MaroonedPodsGateController) getAvailablePoolVMI() *virtv1.VirtualMachineInstance {
	vmis := ctrl.vmiInformer.GetStore().List()
	for _, obj := range vmis {
		vmi := obj.(*virtv1.VirtualMachineInstance)

		// Check if this is a pool VM and is available
		if vmi.Labels != nil {
			if state, ok := vmi.Labels[util.WarmPoolStateLabel]; ok && state == util.PoolStateAvailable {
				// Verify VMI is actually running
				if vmi.Status.Phase == virtv1.Running {
					klog.V(3).Infof("Found available pool VMI: %s/%s", vmi.Namespace, vmi.Name)
					return vmi
				}
			}
		}
	}
	klog.V(3).Info("No available pool VMIs found")
	return nil
}

// isPoolVMI checks if a VMI is part of the warm pool
func (ctrl *MaroonedPodsGateController) isPoolVMI(vmi *virtv1.VirtualMachineInstance) bool {
	if vmi.Labels == nil {
		return false
	}
	_, hasLabel := vmi.Labels[util.WarmPoolStateLabel]
	return hasLabel
}

// claimPoolVMI claims an available pool VMI for a specific pod
func (ctrl *MaroonedPodsGateController) claimPoolVMI(vmi *virtv1.VirtualMachineInstance, pod *v1.Pod) error {
	klog.Infof("Claiming pool VMI %s/%s for pod %s/%s", vmi.Namespace, vmi.Name, pod.Namespace, pod.Name)

	// Update VMI labels to mark as claimed
	vmiCopy := vmi.DeepCopy()
	if vmiCopy.Labels == nil {
		vmiCopy.Labels = make(map[string]string)
	}
	vmiCopy.Labels[util.WarmPoolStateLabel] = util.PoolStateClaimed
	vmiCopy.Labels[util.WarmPoolClaimedByLabel] = fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)

	_, err := ctrl.maroonedpodsCli.KubevirtClient().KubevirtV1().VirtualMachineInstances(vmiCopy.Namespace).Update(
		context.Background(), vmiCopy, k8smetav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update VMI labels: %v", err)
	}

	// Update the node with pod-specific taint
	// Get config for taint key
	_, _, _, taintKey := ctrl.getVMResourcesFromConfig()
	nodeName := vmi.Name

	// Fetch the node
	nodeObj, exists, err := ctrl.nodeInformer.GetStore().GetByKey(nodeName)
	if err != nil {
		return fmt.Errorf("failed to fetch node %s: %v", nodeName, err)
	}
	if !exists {
		return fmt.Errorf("node %s not found", nodeName)
	}

	node := nodeObj.(*v1.Node).DeepCopy()

	// Add pod-specific taint
	podTaint := v1.Taint{
		Key:    fmt.Sprintf("%s/%s", pod.Name, taintKey),
		Value:  "claimed",
		Effect: v1.TaintEffectNoSchedule,
	}
	node.Spec.Taints = append(node.Spec.Taints, podTaint)

	_, err = ctrl.maroonedpodsCli.CoreV1().Nodes().Update(context.Background(), node, k8smetav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update node taints: %v", err)
	}

	klog.Infof("Successfully claimed pool VMI %s for pod %s/%s", vmi.Name, pod.Namespace, pod.Name)
	ctrl.recorder.Eventf(pod, v1.EventTypeNormal, "PoolVMIClaimed", "Claimed pre-booted VM %s from warm pool", vmi.Name)

	return nil
}

// returnVMIToPool returns a VMI back to the available pool
func (ctrl *MaroonedPodsGateController) returnVMIToPool(vmi *virtv1.VirtualMachineInstance, podName string) error {
	klog.Infof("Returning VMI %s/%s to warm pool", vmi.Namespace, vmi.Name)

	// Update VMI labels
	vmiCopy := vmi.DeepCopy()
	if vmiCopy.Labels == nil {
		vmiCopy.Labels = make(map[string]string)
	}
	vmiCopy.Labels[util.WarmPoolStateLabel] = util.PoolStateAvailable
	delete(vmiCopy.Labels, util.WarmPoolClaimedByLabel)

	_, err := ctrl.maroonedpodsCli.KubevirtClient().KubevirtV1().VirtualMachineInstances(vmiCopy.Namespace).Update(
		context.Background(), vmiCopy, k8smetav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update VMI labels: %v", err)
	}

	// Remove pod-specific taint from node
	_, _, _, taintKey := ctrl.getVMResourcesFromConfig()
	nodeName := vmi.Name

	nodeObj, exists, err := ctrl.nodeInformer.GetStore().GetByKey(nodeName)
	if err != nil {
		return fmt.Errorf("failed to fetch node %s: %v", nodeName, err)
	}
	if !exists {
		// Node might have been deleted, that's ok
		klog.V(3).Infof("Node %s not found, skipping taint removal", nodeName)
		return nil
	}

	node := nodeObj.(*v1.Node).DeepCopy()

	// Remove pod-specific taint
	podTaintKey := fmt.Sprintf("%s/%s", podName, taintKey)
	newTaints := []v1.Taint{}
	for _, taint := range node.Spec.Taints {
		if taint.Key != podTaintKey {
			newTaints = append(newTaints, taint)
		}
	}
	node.Spec.Taints = newTaints

	_, err = ctrl.maroonedpodsCli.CoreV1().Nodes().Update(context.Background(), node, k8smetav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update node taints: %v", err)
	}

	klog.Infof("Successfully returned VMI %s to warm pool", vmi.Name)
	return nil
}

// generatePoolVMName generates a unique name for a pool VMI
func generatePoolVMName() string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	suffix := make([]byte, 8)
	for i := range suffix {
		suffix[i] = charset[rand.Intn(len(charset))]
	}
	return fmt.Sprintf("%s%s", util.WarmPoolVMNamePrefix, string(suffix))
}

// createPoolVMI creates a generic VMI for the warm pool (no pod-specific configuration)
func (ctrl *MaroonedPodsGateController) createPoolVMI(namespace string) (*virtv1.VirtualMachineInstance, error) {
	// Get VM resources from config
	cpuCores, memoryMi, nodeImage, _ := ctrl.getVMResourcesFromConfig()

	// Generate unique name
	vmiName := generatePoolVMName()

	klog.Infof("Creating pool VMI %s in namespace %s", vmiName, namespace)

	// Create cloud-init without pod-specific taint
	userData := `#!/bin/sh

cat <<EOF >/tmp/kubeadm-join-config.conf
apiVersion: kubeadm.k8s.io/v1beta3
kind: JoinConfiguration
discovery:
  bootstrapToken:
    unsafeSkipCAVerification: true
    apiServerEndpoint: "192.168.66.101:6443"
    token: "abcdef.1234567890123456"
EOF

kubeadm join --config /tmp/kubeadm-join-config.conf --ignore-preflight-errors=all --v=5
useradd -s /bin/bash -d /home/vladik/ -m -G sudo vladik
passwd vladik
`

	encodedData := base64.StdEncoding.EncodeToString([]byte(userData))

	vmi := virtv1.NewVMIReferenceFromNameWithNS(namespace, vmiName)
	vmi.Spec = virtv1.VirtualMachineInstanceSpec{Domain: virtv1.DomainSpec{}}
	vmi.TypeMeta = k8smetav1.TypeMeta{
		APIVersion: virtv1.GroupVersion.String(),
		Kind:       "VirtualMachineInstance",
	}

	// Add pool labels
	vmi.Labels = map[string]string{
		util.WarmPoolStateLabel: util.PoolStateCreating,
	}

	// Network configuration
	bridgeBinding := virtv1.Interface{
		Name: virtv1.DefaultPodNetwork().Name,
		InterfaceBindingMethod: virtv1.InterfaceBindingMethod{
			Masquerade: &virtv1.InterfaceMasquerade{},
		},
	}
	vmi.Spec.Domain.Devices.Interfaces = append(vmi.Spec.Domain.Devices.Interfaces, bridgeBinding)
	vmi.Spec.Networks = append(vmi.Spec.Networks, *virtv1.DefaultPodNetwork())

	// Resources
	guestMemory := resource.MustParse(fmt.Sprintf("%dMi", memoryMi))
	vmi.Spec.Domain.Memory = &virtv1.Memory{Guest: &guestMemory}

	vmi.Spec.Domain.CPU = &virtv1.CPU{
		Threads: 1,
		Sockets: 1,
		Cores:   cpuCores,
	}

	// Disks
	vmi.Spec.Domain.Devices.Disks = append(vmi.Spec.Domain.Devices.Disks,
		virtv1.Disk{
			Name: "containerdisk",
			DiskDevice: virtv1.DiskDevice{
				Disk: &virtv1.DiskTarget{Bus: virtv1.DiskBusVirtio}}})
	vmi.Spec.Volumes = append(vmi.Spec.Volumes,
		virtv1.Volume{
			Name: "containerdisk",
			VolumeSource: virtv1.VolumeSource{
				ContainerDisk: &virtv1.ContainerDiskSource{
					Image: nodeImage},
			}},
	)

	vmi.Spec.Domain.Devices.Disks = append(vmi.Spec.Domain.Devices.Disks,
		virtv1.Disk{
			Name: "cloudinitdisk",
			DiskDevice: virtv1.DiskDevice{
				Disk: &virtv1.DiskTarget{Bus: virtv1.DiskBusVirtio}}})
	vmi.Spec.Volumes = append(vmi.Spec.Volumes,
		virtv1.Volume{
			Name: "cloudinitdisk",
			VolumeSource: virtv1.VolumeSource{
				CloudInitNoCloud: &virtv1.CloudInitNoCloudSource{
					UserData:       "",
					UserDataBase64: encodedData,
				},
			}},
	)

	// Create the VMI
	createdVMI, err := ctrl.maroonedpodsCli.KubevirtClient().KubevirtV1().VirtualMachineInstances(namespace).Create(
		context.Background(), vmi, k8smetav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create pool VMI: %v", err)
	}

	klog.Infof("Created pool VMI %s/%s", createdVMI.Namespace, createdVMI.Name)
	return createdVMI, nil
}

func (ctrl *MaroonedPodsGateController) createVMIFromPod(pod *v1.Pod) *virtv1.VirtualMachineInstance {
	// Get VM resources from config (with defaults)
	cpuCores, memoryMi, nodeImage, taintKey := ctrl.getVMResourcesFromConfig()

	/*	userData := fmt.Sprintf(`#!/bin/sh

		cat <<EOF >/tmp/kubeadm-join-config.conf
		apiVersion: kubeadm.k8s.io/v1beta3
		kind: JoinConfiguration
		nodeRegistration:
		  taints:
		    - key: "%s.maroonedpods.io"
		      value: "created"
		      effect: "NoSchedule"
		discovery:
		  bootstrapToken:
		    unsafeSkipCAVerification: true
		    apiServerEndpoint: "192.168.66.101:6443"
		    token: "abcdef.1234567890123456"
		EOF

		kubeadm join --config /tmp/kubeadm-join-config.conf --ignore-preflight-errors=all --v=5

		users:
		  - name: marn
		    gecos: Marooned User
		    sudo: ALL=(ALL) NOPASSWD:ALL
		    plain_text_passwd: 'marn'
		    lock_passwd: False
		    groups: users, admin

		`, pod.Name)*/
	userData := fmt.Sprintf(`#!/bin/sh

cat <<EOF >/tmp/kubeadm-join-config.conf
apiVersion: kubeadm.k8s.io/v1beta3
kind: JoinConfiguration
nodeRegistration:
  taints:
    - key: "%s.%s"
      value: "created"
      effect: "NoSchedule"
discovery:
  bootstrapToken:
    unsafeSkipCAVerification: true
    apiServerEndpoint: "192.168.66.101:6443"
    token: "abcdef.1234567890123456"
EOF

kubeadm join --config /tmp/kubeadm-join-config.conf --ignore-preflight-errors=all --v=5
useradd -s /bin/bash -d /home/vladik/ -m -G sudo vladik
passwd vladik
`, pod.Name, taintKey)

	encodedData := base64.StdEncoding.EncodeToString([]byte(userData))
	vmi := virtv1.NewVMIReferenceFromNameWithNS(pod.Namespace, pod.Name)
	vmi.Spec = virtv1.VirtualMachineInstanceSpec{Domain: virtv1.DomainSpec{}}
	vmi.TypeMeta = k8smetav1.TypeMeta{
		APIVersion: virtv1.GroupVersion.String(),
		Kind:       "VirtualMachineInstance",
	}
	bridgeBinding := virtv1.Interface{
		Name: virtv1.DefaultPodNetwork().Name,
		/*InterfaceBindingMethod: virtv1.InterfaceBindingMethod{
			Bridge: &virtv1.InterfaceBridge{},
		},*/
		InterfaceBindingMethod: virtv1.InterfaceBindingMethod{
			Masquerade: &virtv1.InterfaceMasquerade{},
		},
	}
	vmi.Spec.Domain.Devices.Interfaces = append(vmi.Spec.Domain.Devices.Interfaces, bridgeBinding)
	vmi.Spec.Networks = append(vmi.Spec.Networks, *virtv1.DefaultPodNetwork())

	// Use config-driven memory (memoryMi is in Mi units)
	guestMemory := resource.MustParse(fmt.Sprintf("%dMi", memoryMi))
	vmi.Spec.Domain.Memory = &virtv1.Memory{Guest: &guestMemory}

	// Use config-driven CPU cores
	vmi.Spec.Domain.CPU = &virtv1.CPU{
		Threads: 1,
		Sockets: 1,
		Cores:   cpuCores,
	}

	vmi.Spec.Domain.Devices.Disks = append(vmi.Spec.Domain.Devices.Disks,
		virtv1.Disk{
			Name: "containerdisk",
			DiskDevice: virtv1.DiskDevice{
				Disk: &virtv1.DiskTarget{Bus: virtv1.DiskBusVirtio}}})
	vmi.Spec.Volumes = append(vmi.Spec.Volumes,
		virtv1.Volume{
			Name: "containerdisk",
			VolumeSource: virtv1.VolumeSource{
				ContainerDisk: &virtv1.ContainerDiskSource{
					Image: nodeImage},
			}},
	)

	vmi.Spec.Domain.Devices.Disks = append(vmi.Spec.Domain.Devices.Disks,
		virtv1.Disk{
			Name: "cloudinitdisk",
			DiskDevice: virtv1.DiskDevice{
				Disk: &virtv1.DiskTarget{Bus: virtv1.DiskBusVirtio}}})
	vmi.Spec.Volumes = append(vmi.Spec.Volumes,
		virtv1.Volume{
			Name: "cloudinitdisk",
			VolumeSource: virtv1.VolumeSource{
				CloudInitNoCloud: &virtv1.CloudInitNoCloudSource{
					UserData:       "",
					UserDataBase64: encodedData,
				},
			}},
	)

	return vmi
}
