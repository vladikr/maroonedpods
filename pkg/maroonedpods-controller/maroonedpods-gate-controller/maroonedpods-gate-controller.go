package mp_controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	certv1 "k8s.io/api/certificates/v1"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	k8smetav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
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

	// Register node event handlers to re-enqueue pods when nodes become Ready
	_, err = ctrl.nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node := obj.(*v1.Node)
			klog.V(2).Infof("Node %s added, re-enqueueing waiting pods", node.Name)
			ctrl.enqueuePodsForNode(node.Name)
		},
		UpdateFunc: func(old, cur interface{}) {
			oldNode := old.(*v1.Node)
			curNode := cur.(*v1.Node)

			// Check if node transitioned to Ready
			oldReady := false
			curReady := false

			for _, cond := range oldNode.Status.Conditions {
				if cond.Type == v1.NodeReady && cond.Status == v1.ConditionTrue {
					oldReady = true
					break
				}
			}

			for _, cond := range curNode.Status.Conditions {
				if cond.Type == v1.NodeReady && cond.Status == v1.ConditionTrue {
					curReady = true
					break
				}
			}

			// If node transitioned from not-Ready to Ready, re-enqueue waiting pods
			if !oldReady && curReady {
				klog.Infof("Node %s became Ready, re-enqueueing waiting pods", curNode.Name)
				ctrl.enqueuePodsForNode(curNode.Name)
			}
		},
	})
	if err != nil {
		panic("failed to register node event handler")
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
	oldPod := old.(*v1.Pod)
	pod := curr.(*v1.Pod)

	// Check if pod still has scheduling gate
	hasGate := pod.Spec.SchedulingGates != nil &&
		len(pod.Spec.SchedulingGates) == 1 &&
		pod.Spec.SchedulingGates[0].Name == util.MaroonedPodsGate

	if hasGate {
		klog.Info(fmt.Sprintf("Updating pod with gate %s", pod.Name))
		key, err := KeyFunc(pod)
		if err != nil {
			log.Log.Info("Failed to obtain pod key function")
		}
		ctrl.queue.Add(key)
		return
	}

	// Check if resource requests changed (future: trigger VMI resize)
	// Note: Kubernetes doesn't allow changing resource requests on running pods
	// without in-place pod resize (alpha/beta feature). This is for future use.
	if ctrl.podResourcesChanged(oldPod, pod) {
		klog.V(2).Infof("Pod %s/%s resource requests changed, VMI resize not yet implemented",
			pod.Namespace, pod.Name)
		// TODO: Implement VMI resize when KubeVirt supports it or recreate VMI
		// For now, just log the change
	}
}

// podResourcesChanged checks if pod container resource requests have changed
func (ctrl *MaroonedPodsGateController) podResourcesChanged(oldPod, newPod *v1.Pod) bool {
	if len(oldPod.Spec.Containers) != len(newPod.Spec.Containers) {
		return true
	}

	for i := range oldPod.Spec.Containers {
		oldReqs := oldPod.Spec.Containers[i].Resources.Requests
		newReqs := newPod.Spec.Containers[i].Resources.Requests

		oldCPU := oldReqs[v1.ResourceCPU]
		newCPU := newReqs[v1.ResourceCPU]
		if !oldCPU.Equal(newCPU) {
			return true
		}

		oldMem := oldReqs[v1.ResourceMemory]
		newMem := newReqs[v1.ResourceMemory]
		if !oldMem.Equal(newMem) {
			return true
		}
	}

	return false
}

// enqueuePodsForNode re-enqueues all gated pods that are waiting for the given node
func (ctrl *MaroonedPodsGateController) enqueuePodsForNode(nodeName string) {
	pods := ctrl.podInformer.GetStore().List()
	for _, obj := range pods {
		pod := obj.(*v1.Pod)

		// Only process pods that still have the scheduling gate
		if pod.Spec.SchedulingGates == nil ||
			len(pod.Spec.SchedulingGates) != 1 ||
			pod.Spec.SchedulingGates[0].Name != util.MaroonedPodsGate {
			continue
		}

		// Check if this pod is waiting for this node
		shouldEnqueue := false

		// Group mode: check if pod's group label matches node name pattern
		if groupName, ok := pod.Labels[util.GroupLabel]; ok {
			expectedNodeName := fmt.Sprintf("maroonedpods-group-%s", groupName)
			if expectedNodeName == nodeName {
				shouldEnqueue = true
			}
		} else if maroon, ok := pod.Labels[util.MaroonedPodLabel]; ok && maroon == "true" {
			// 1:1 mode: VMI name matches node name
			// For 1:1 mode, we'd need to look up the VMI for this pod to get its name
			// For now, we'll rely on the existing backoff mechanism for 1:1 mode
			// since group mode is the priority
			_ = maroon // avoid unused variable warning
		}

		if shouldEnqueue {
			key, err := KeyFunc(pod)
			if err != nil {
				klog.Errorf("Failed to get key for pod %s/%s: %v", pod.Namespace, pod.Name, err)
				continue
			}
			klog.V(2).Infof("Re-enqueueing pod %s because node %s became available", key, nodeName)
			ctrl.queue.Add(key)
		}
	}
}

func (ctrl *MaroonedPodsGateController) deletePod(obj interface{}) {
	pod := obj.(*v1.Pod)

	// Group pods: cleanup is handled by the finalizer path, not the informer delete handler
	if groupName, ok := pod.Labels[util.GroupLabel]; ok && groupName != "" {
		klog.V(3).Infof("Group pod %s/%s deleted (group: %s), cleanup via finalizer", pod.Namespace, pod.Name, groupName)
		return
	}

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
		return nil, BackOff
	}
	pod := obj.(*v1.Pod)

	logger := log.Log.Object(pod)

	podKey, err := KeyFunc(pod)
	if err != nil {
		logger.Info("Failed to obtain pod key function")
		return err, BackOff
	}

	// Check for group mode
	if groupName, ok := pod.Labels[util.GroupLabel]; ok && groupName != "" {
		if pod.DeletionTimestamp != nil {
			return ctrl.handleGroupPodDeletion(pod, podKey, groupName)
		}
		return ctrl.executeGroup(pod, groupName)
	}

	// Handle pod deletion with finalizer (1:1 mode)
	if pod.DeletionTimestamp != nil {
		return ctrl.handlePodDeletion(pod, podKey)
	}

	// 1:1 mode: find existing Virtual Machine Instance
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

	err1 := ctrl.sync(pod, vmi, key)
	if err1 != nil {
		logger.Reason(err1).Error("sync failed")
		return err1, BackOff
	}

	return nil, Immediate
}

// updatePodNodeSelector updates the pod's nodeSelector to point to a specific node
func (ctrl *MaroonedPodsGateController) updatePodNodeSelector(pod *v1.Pod, nodeName string) error {
	// Use strategic merge patch to avoid triggering OCP SCC re-validation
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"nodeSelector": map[string]string{
				"kubernetes.io/hostname": nodeName,
			},
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal nodeSelector patch: %v", err)
	}
	_, err = ctrl.maroonedpodsCli.CoreV1().Pods(pod.Namespace).Patch(
		context.Background(),
		pod.Name,
		k8stypes.StrategicMergePatchType,
		patchBytes,
		k8smetav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to patch pod nodeSelector: %v", err)
	}

	klog.Infof("Updated pod %s/%s nodeSelector to node %s", pod.Namespace, pod.Name, nodeName)
	return nil
}

// handlePodDeletion handles pod deletion and VMI cleanup when pod has finalizer
func (ctrl *MaroonedPodsGateController) handlePodDeletion(pod *v1.Pod, key string) (error, enqueueState) {
	// Check if pod has our finalizer
	hasFinalizer := false
	for _, f := range pod.Finalizers {
		if f == util.MaroonedPodsFinalizer {
			hasFinalizer = true
			break
		}
	}

	if !hasFinalizer {
		// No finalizer, nothing to do
		klog.V(3).Infof("Pod %s/%s being deleted, no finalizer present", pod.Namespace, pod.Name)
		return nil, Forget
	}

	klog.Infof("Pod %s/%s being deleted, cleaning up VMI", pod.Namespace, pod.Name)

	// Try to find and delete the VMI
	vmiObj, exist, err := ctrl.vmiInformer.GetStore().GetByKey(key)
	if err != nil {
		klog.Errorf("Failed to fetch VMI for pod %s: %v", key, err)
		return err, BackOff
	}

	if exist {
		vmi := vmiObj.(*virtv1.VirtualMachineInstance)
		klog.Infof("Deleting VMI %s/%s for pod %s", vmi.Namespace, vmi.Name, pod.Name)
		err = ctrl.maroonedpodsCli.KubevirtClient().KubevirtV1().VirtualMachineInstances(vmi.Namespace).Delete(
			context.Background(), vmi.Name, k8smetav1.DeleteOptions{})
		if err != nil {
			klog.Errorf("Failed to delete VMI %s/%s: %v", vmi.Namespace, vmi.Name, err)
			return err, BackOff
		}
		ctrl.recorder.Eventf(pod, v1.EventTypeNormal, "VMIDeleted", "Deleted VMI %s for marooned pod", vmi.Name)
	} else {
		klog.V(3).Infof("No VMI found for pod %s, skipping VMI deletion", key)
	}

	// Remove our finalizer using merge patch to avoid SCC re-validation
	newFinalizers := []string{}
	for _, f := range pod.Finalizers {
		if f != util.MaroonedPodsFinalizer {
			newFinalizers = append(newFinalizers, f)
		}
	}
	finalizerPatch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"finalizers": newFinalizers,
		},
	}
	finalizerPatchBytes, err := json.Marshal(finalizerPatch)
	if err != nil {
		return fmt.Errorf("failed to marshal finalizer patch: %v", err), BackOff
	}
	_, err = ctrl.maroonedpodsCli.CoreV1().Pods(pod.Namespace).Patch(
		context.Background(),
		pod.Name,
		k8stypes.MergePatchType,
		finalizerPatchBytes,
		k8smetav1.PatchOptions{},
	)
	if err != nil {
		klog.Errorf("Failed to remove finalizer from pod %s/%s: %v", pod.Namespace, pod.Name, err)
		return err, BackOff
	}

	klog.Infof("Removed finalizer from pod %s/%s, cleanup complete", pod.Namespace, pod.Name)
	return nil, Forget
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
	pod := obj.(*v1.Pod)
	if pod.Spec.SchedulingGates != nil && len(pod.Spec.SchedulingGates) == 1 && pod.Spec.SchedulingGates[0].Name == util.MaroonedPodsGate {
		// Use PATCH instead of UPDATE to avoid triggering OCP SCC re-validation
		patchOps := []map[string]interface{}{
			{
				"op":    "replace",
				"path":  "/spec/schedulingGates",
				"value": []interface{}{},
			},
		}
		patchBytes, err := json.Marshal(patchOps)
		if err != nil {
			return fmt.Errorf("failed to marshal patch: %v", err)
		}
		_, err = ctrl.maroonedpodsCli.CoreV1().Pods(pod.Namespace).Patch(
			context.Background(),
			pod.Name,
			k8stypes.JSONPatchType,
			patchBytes,
			k8smetav1.PatchOptions{},
		)
		if err != nil {
			return err
		}
		klog.Infof("Pod %s/%s scheduling gate removed, ready to schedule", pod.Namespace, pod.Name)
		ctrl.recorder.Eventf(pod, v1.EventTypeNormal, "GateRemoved", "Scheduling gate removed, pod ready to schedule on dedicated node")
	}
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
			ctrl.recorder.Eventf(pod, v1.EventTypeWarning, "VMICreationFailed", "Failed to create VMI: %v", err)
			return err
		}
		klog.Infof("Created VMI %s/%s for pod %s", vmi.Namespace, vmi.Name, pod.Name)
		ctrl.recorder.Eventf(pod, v1.EventTypeNormal, "VMICreated", "Created VirtualMachineInstance %s", vmi.Name)
		return fmt.Errorf("waiting for VMI %s to start", vmi.Name)
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
				klog.V(2).Infof("Waiting for VMI %s to become Running, currently %s", vmi.Name, string(vmi.Status.Phase))
				return fmt.Errorf("waiting for VMI %s to become Running, currently %s", vmi.Name, string(vmi.Status.Phase))
			}
		}

	} else {
		klog.V(2).Infof("VMI %s not yet Running, current phase: %s", vmi.Name, string(vmi.Status.Phase))
		ctrl.recorder.Eventf(pod, v1.EventTypeNormal, "WaitingForVMI", "Waiting for VMI %s to become Running (current: %s)", vmi.Name, string(vmi.Status.Phase))
		return fmt.Errorf("waiting for VMI %s to become Running, currently %s", vmi.Name, string(vmi.Status.Phase))
	}

	_, nodeExist, err := ctrl.nodeInformer.GetStore().GetByKey(vmi.Name)
	if err != nil {
		log.Log.Reason(err).Error("Failed to fetch node from cache.")
		return err
	}
	if !nodeExist {
		klog.V(2).Infof("Waiting for node %s to register", vmi.Name)
		ctrl.recorder.Eventf(pod, v1.EventTypeNormal, "WaitingForNode", "Waiting for node %s to join cluster", vmi.Name)
		return fmt.Errorf("waiting for node %s to register", vmi.Name)
	} else {
		klog.Infof("Node %s is ready, releasing pod %s", vmi.Name, pod.Name)
		ctrl.recorder.Eventf(pod, v1.EventTypeNormal, "NodeReady", "Node %s joined cluster, releasing pod for scheduling", vmi.Name)
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

	// Start group pool reconciler (cleans up stale groups with no remaining pods)
	go wait.Until(ctrl.reconcileGroupPools, 60*time.Second, ctrl.stop)

	// Start CSR auto-approval for nodes created by this controller
	go wait.Until(ctrl.reconcileCSRs, 15*time.Second, ctrl.stop)

	// Start endpoint patcher for Route/LB-exposed services on group VMs
	go wait.Until(ctrl.reconcileEndpointSlices, 30*time.Second, ctrl.stop)

	for i := 0; i < threadiness; i++ {
		go wait.Until(ctrl.runWorker, time.Second, ctrl.stop)
	}

	<-ctrl.stop

}

// getConfig retrieves the MaroonedPodsConfig from the informer cache.
// Returns the first config found, or nil if none exists or if the CRD is not installed.
func (ctrl *MaroonedPodsGateController) getConfig() *v1alpha1.MaroonedPodsConfig {
	if ctrl.configInformer == nil {
		klog.Infof("getConfig: configInformer is nil (CRD not installed)")
		return nil
	}
	configs := ctrl.configInformer.GetStore().List()
	if len(configs) == 0 {
		klog.Infof("getConfig: no MaroonedPodsConfig CRs found in store")
		return nil
	}
	klog.Infof("getConfig: found %d config(s), joinConfig.ignitionSecretRef=%q", len(configs), configs[0].(*v1alpha1.MaroonedPodsConfig).Spec.JoinConfig.IgnitionSecretRef)
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
		Ports: []virtv1.Port{
			{Name: "kubelet", Port: 10250, Protocol: "TCP"},
			{Name: "ssh", Port: 22, Protocol: "TCP"},
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

// calculateVMResourcesFromPod calculates VM resources based on pod requests plus overhead.
// Returns CPU cores and memory in Mi.
func (ctrl *MaroonedPodsGateController) calculateVMResourcesFromPod(pod *v1.Pod) (cpuCores uint32, memoryMi uint64) {
	config := ctrl.getConfig()

	// Get base VM resources as minimum floor
	baseVMCPU := uint32(2)
	baseVMMemory := uint64(3072) // 3Gi in Mi
	if config != nil {
		if config.Spec.BaseVMResources.CPU > 0 {
			baseVMCPU = config.Spec.BaseVMResources.CPU
		}
		if config.Spec.BaseVMResources.MemoryMi > 0 {
			baseVMMemory = config.Spec.BaseVMResources.MemoryMi
		}
	}

	// Default overhead: 500m CPU, 512Mi memory
	overheadCPUMillis := int64(500)
	overheadMemoryBytes := int64(512 * 1024 * 1024) // 512Mi

	// Apply configured overhead if present
	if config != nil && config.Spec.ResourceOverhead != nil {
		if cpu, ok := (*config.Spec.ResourceOverhead)[v1.ResourceCPU]; ok {
			overheadCPUMillis = cpu.MilliValue()
		}
		if mem, ok := (*config.Spec.ResourceOverhead)[v1.ResourceMemory]; ok {
			overheadMemoryBytes = mem.Value()
		}
	}

	// Sum up all container requests
	totalPodCPUMillis := int64(0)
	totalPodMemoryBytes := int64(0)

	for _, container := range pod.Spec.Containers {
		if cpu, ok := container.Resources.Requests[v1.ResourceCPU]; ok {
			totalPodCPUMillis += cpu.MilliValue()
		}
		if mem, ok := container.Resources.Requests[v1.ResourceMemory]; ok {
			totalPodMemoryBytes += mem.Value()
		}
	}

	// Add overhead to pod requests
	totalCPUMillis := totalPodCPUMillis + overheadCPUMillis
	totalMemoryBytes := totalPodMemoryBytes + overheadMemoryBytes

	// Convert to VM units (cores and Mi)
	// Round up CPU to nearest core
	calculatedCPU := uint32((totalCPUMillis + 999) / 1000) // ceiling division
	if calculatedCPU == 0 {
		calculatedCPU = 1 // minimum 1 core
	}

	// Convert bytes to Mi
	calculatedMemoryMi := uint64(totalMemoryBytes / (1024 * 1024))
	if calculatedMemoryMi == 0 {
		calculatedMemoryMi = 512 // minimum 512Mi
	}

	// Use maximum of calculated and base (floor)
	cpuCores = calculatedCPU
	if baseVMCPU > cpuCores {
		cpuCores = baseVMCPU
	}

	memoryMi = calculatedMemoryMi
	if baseVMMemory > memoryMi {
		memoryMi = baseVMMemory
	}

	klog.V(3).Infof("Pod %s/%s resource calculation: pod_cpu=%dm pod_mem=%dMi overhead_cpu=%dm overhead_mem=%dMi -> VM: cpu=%d mem=%dMi",
		pod.Namespace, pod.Name,
		totalPodCPUMillis, totalPodMemoryBytes/(1024*1024),
		overheadCPUMillis, overheadMemoryBytes/(1024*1024),
		cpuCores, memoryMi)

	return
}

func (ctrl *MaroonedPodsGateController) createVMIFromPod(pod *v1.Pod) *virtv1.VirtualMachineInstance {
	// Calculate VM resources based on pod requests + overhead
	cpuCores, memoryMi := ctrl.calculateVMResourcesFromPod(pod)

	// Get node image and taint key from config - use bootc+k3s image
	_, _, _, taintKey := ctrl.getVMResourcesFromConfig()
	// Override with bootc+k3s node image
	nodeImage := "quay.io/vladikr/marooned-node:latest"

	// Get Kubernetes API server endpoint
	// TODO: Make this configurable via config or environment
	serverURL := "https://kubernetes.default.svc:6443"
	// TODO: Get actual token from kubeadm or secret
	// For now using placeholder - in production this should come from kubeadm bootstrap token or service account
	token := "abcdef.1234567890123456"

	// Generate pod UID for unique node identification
	podUID := string(pod.UID)

	// Generate cloud-init userdata that writes k3s join configuration
	userData := fmt.Sprintf(`#!/bin/sh
# MaroonedPods k3s node initialization script

# Create marooned config directory
mkdir -p /etc/marooned

# Write k3s join configuration
cat > /etc/marooned/join-info.yaml <<'JOINEOF'
server_url: %s
token: %s
pod_uid: %s
taint_key: %s
JOINEOF

# Ensure marooned-node-boot service will run
systemctl enable marooned-node-boot.service

echo "MaroonedPods cloud-init complete"
`, serverURL, token, podUID, taintKey)

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

	// Use dynamic VM sizing based on pod requests + overhead
	guestMemory := resource.MustParse(fmt.Sprintf("%dMi", memoryMi))
	vmi.Spec.Domain.Memory = &virtv1.Memory{Guest: &guestMemory}

	// Use dynamically calculated CPU cores
	vmi.Spec.Domain.CPU = &virtv1.CPU{
		Threads: 1,
		Sockets: 1,
		Cores:   cpuCores,
	}

	// Optional: Enable kernelBoot for faster startup
	// TODO: When MaroonedPodsConfig CRD is available:
	// enableKernelBoot := false
	// if config != nil && config.Spec.EnableKernelBoot {
	//     enableKernelBoot = true
	//     kernelPath := "/vmlinuz"
	//     initrdPath := "/initrd.img"
	//     kernelArgs := "console=ttyS0 root=/dev/vda rw"
	//     if config.Spec.KernelBootConfig != nil {
	//         if config.Spec.KernelBootConfig.KernelPath != "" {
	//             kernelPath = config.Spec.KernelBootConfig.KernelPath
	//         }
	//         if config.Spec.KernelBootConfig.InitrdPath != "" {
	//             initrdPath = config.Spec.KernelBootConfig.InitrdPath
	//         }
	//         if config.Spec.KernelBootConfig.KernelArgs != "" {
	//             kernelArgs = config.Spec.KernelBootConfig.KernelArgs
	//         }
	//     }
	//     vmi.Spec.Domain.Firmware = &virtv1.Firmware{
	//         KernelBoot: &virtv1.KernelBoot{
	//             Container: &virtv1.KernelBootContainer{
	//                 Image:      nodeImage,
	//                 KernelPath: kernelPath,
	//                 InitrdPath: initrdPath,
	//             },
	//             KernelArgs: kernelArgs,
	//         },
	//     }
	// }

	// Root disk: bootc-based k3s node image
	vmi.Spec.Domain.Devices.Disks = append(vmi.Spec.Domain.Devices.Disks,
		virtv1.Disk{
			Name: "rootdisk",
			DiskDevice: virtv1.DiskDevice{
				Disk: &virtv1.DiskTarget{Bus: virtv1.DiskBusVirtio}}})
	vmi.Spec.Volumes = append(vmi.Spec.Volumes,
		virtv1.Volume{
			Name: "rootdisk",
			VolumeSource: virtv1.VolumeSource{
				ContainerDisk: &virtv1.ContainerDiskSource{
					Image: nodeImage},
			}},
	)

	// Cloud-init disk: provides k3s join configuration
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

	// Readiness probe: Check if k3s-agent is active
	// This replaces the generic VM running check with k3s-specific health
	vmi.Spec.ReadinessProbe = &virtv1.Probe{
		InitialDelaySeconds: 30,
		TimeoutSeconds:      10,
		PeriodSeconds:       10,
		SuccessThreshold:    1,
		FailureThreshold:    3,
		Handler: virtv1.Handler{
			Exec: &v1.ExecAction{
				Command: []string{
					"/bin/sh",
					"-c",
					"systemctl is-active k3s-agent",
				},
			},
		},
	}

	klog.Infof("Created VMI spec for pod %s/%s: image=%s, cpu=%d, memory=%s",
		pod.Namespace, pod.Name, nodeImage, vmi.Spec.Domain.CPU.Cores, guestMemory.String())

	return vmi
}

// --- Group Mode ---

func (ctrl *MaroonedPodsGateController) executeGroup(pod *v1.Pod, groupName string) (error, enqueueState) {
	if pod.Spec.SchedulingGates == nil || len(pod.Spec.SchedulingGates) == 0 {
		return nil, Forget
	}
	hasGate := false
	for _, g := range pod.Spec.SchedulingGates {
		if g.Name == util.MaroonedPodsGate {
			hasGate = true
			break
		}
	}
	if !hasGate {
		return nil, Forget
	}

	// Look up existing VMI for this group
	groupVMI := ctrl.getGroupVMI(groupName)

	if groupVMI == nil {
		// No VMI for this group yet — create one
		klog.Infof("No VMI found for group %s, creating one", groupName)
		_, err := ctrl.createGroupVMI(groupName, pod.Namespace)
		if err != nil {
			klog.Errorf("Failed to create group VMI for %s: %v", groupName, err)
			return err, BackOff
		}
		return fmt.Errorf("waiting for group %s VMI to start", groupName), BackOff
	}

	if groupVMI.Status.Phase != virtv1.Running {
		klog.V(2).Infof("Group %s VMI %s not yet Running (phase: %s)", groupName, groupVMI.Name, string(groupVMI.Status.Phase))
		return fmt.Errorf("waiting for group %s VMI to become Running", groupName), BackOff
	}

	// VMI is Running — check if node has joined
	_, nodeExists, err := ctrl.nodeInformer.GetStore().GetByKey(groupVMI.Name)
	if err != nil {
		return err, BackOff
	}
	if !nodeExists {
		klog.V(2).Infof("Waiting for node %s to register for group %s", groupVMI.Name, groupName)
		return fmt.Errorf("waiting for node %s to register for group %s", groupVMI.Name, groupName), BackOff
	}

	// Node is ready — ensure it has group labels/taints and blocking taints are removed
	err = ctrl.markGroupVMIReady(groupVMI, groupName)
	if err != nil {
		klog.Errorf("Failed to mark group VMI %s as ready: %v", groupVMI.Name, err)
		return err, BackOff
	}

	// Ungate the pod
	key, _ := KeyFunc(pod)
	err = ctrl.releasePod(key)
	if err != nil {
		return err, BackOff
	}

	ctrl.recorder.Eventf(pod, v1.EventTypeNormal, "GroupNodeReady",
		"Group %s node %s is ready, pod released for scheduling", groupName, groupVMI.Name)
	return nil, Forget
}

func (ctrl *MaroonedPodsGateController) getGroupVMI(groupName string) *virtv1.VirtualMachineInstance {
	vmis := ctrl.vmiInformer.GetStore().List()
	for _, obj := range vmis {
		vmi := obj.(*virtv1.VirtualMachineInstance)
		if vmi.Labels == nil {
			continue
		}
		if g, ok := vmi.Labels[util.GroupLabel]; ok && g == groupName {
			return vmi
		}
	}
	return nil
}

func (ctrl *MaroonedPodsGateController) getGroupVMResourcesFromConfig() (cpuCores uint32, memoryMi uint64, nodeImage string, taintKey string) {
	cpuCores = 4
	memoryMi = 3072
	nodeImage = "quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:024cb7de8d9e21ef7fc2bcb90f8dd77e7f742d937822c95e4ec72b9ab48fea7d"
	taintKey = "maroonedpods.io"

	config := ctrl.getConfig()
	if config == nil {
		return
	}

	if config.Spec.GroupBaseVMResources.CPU > 0 {
		cpuCores = config.Spec.GroupBaseVMResources.CPU
	} else if config.Spec.BaseVMResources.CPU > 0 {
		cpuCores = config.Spec.BaseVMResources.CPU
	}

	if config.Spec.GroupBaseVMResources.MemoryMi > 0 {
		memoryMi = config.Spec.GroupBaseVMResources.MemoryMi
	} else if config.Spec.BaseVMResources.MemoryMi > 0 {
		memoryMi = config.Spec.BaseVMResources.MemoryMi
	}

	if config.Spec.GroupNodeImage != "" {
		nodeImage = config.Spec.GroupNodeImage
	} else if config.Spec.NodeImage != "" {
		nodeImage = config.Spec.NodeImage
	}

	if config.Spec.NodeTaintKey != "" {
		taintKey = config.Spec.NodeTaintKey
	}

	return
}

func sanitizeGroupName(groupName string) string {
	safe := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + 32 // lowercase
		}
		return '-'
	}, groupName)
	if len(safe) > 40 {
		safe = safe[:40]
	}
	return strings.Trim(safe, "-")
}

// ensureIgnitionSecret copies the Ignition Secret from the maroonedpods namespace to the target namespace
// and returns the Secret name in the target namespace.
// Returns the Secret name and whether Ignition is configured.
func (ctrl *MaroonedPodsGateController) ensureIgnitionSecret(targetNamespace string) (string, bool) {
	config := ctrl.getConfig()
	if config == nil {
		klog.Infof("ensureIgnitionSecret: config is nil, falling back to kubeadm")
		return "", false
	}
	if config.Spec.JoinConfig.IgnitionSecretRef == "" {
		klog.Infof("ensureIgnitionSecret: ignitionSecretRef is empty, falling back to kubeadm")
		return "", false
	}
	klog.Infof("ensureIgnitionSecret: using ignitionSecretRef=%q for namespace %s", config.Spec.JoinConfig.IgnitionSecretRef, targetNamespace)

	secretName := config.Spec.JoinConfig.IgnitionSecretRef
	srcSecret, err := ctrl.maroonedpodsCli.CoreV1().Secrets(util.DefaultMaroonedPodsNs).Get(
		context.Background(), secretName, k8smetav1.GetOptions{})
	if err != nil {
		klog.Errorf("Failed to get Ignition secret %s/%s: %v", util.DefaultMaroonedPodsNs, secretName, err)
		return "", false
	}

	if _, ok := srcSecret.Data["userdata"]; !ok {
		if ud, ok2 := srcSecret.Data["userData"]; ok2 {
			srcSecret.Data["userdata"] = ud
		} else {
			klog.Errorf("Ignition secret %s/%s has no 'userdata' or 'userData' key", util.DefaultMaroonedPodsNs, secretName)
			return "", false
		}
	}

	targetSecretName := "maroonedpods-ignition"
	targetSecret := &v1.Secret{
		ObjectMeta: k8smetav1.ObjectMeta{
			Name:      targetSecretName,
			Namespace: targetNamespace,
		},
		Data: map[string][]byte{
			"userdata": srcSecret.Data["userdata"],
		},
	}

	existing, err := ctrl.maroonedpodsCli.CoreV1().Secrets(targetNamespace).Get(
		context.Background(), targetSecretName, k8smetav1.GetOptions{})
	if err != nil {
		_, err = ctrl.maroonedpodsCli.CoreV1().Secrets(targetNamespace).Create(
			context.Background(), targetSecret, k8smetav1.CreateOptions{})
		if err != nil {
			if errors.IsAlreadyExists(err) {
				klog.Infof("Ignition secret %s/%s already exists (race), continuing", targetNamespace, targetSecretName)
				return targetSecretName, true
			}
			klog.Errorf("Failed to create Ignition secret in %s: %v", targetNamespace, err)
			return "", false
		}
		klog.Infof("Created Ignition secret %s/%s, verifying propagation", targetNamespace, targetSecretName)
		for i := 0; i < 10; i++ {
			verified, verErr := ctrl.maroonedpodsCli.CoreV1().Secrets(targetNamespace).Get(
				context.Background(), targetSecretName, k8smetav1.GetOptions{})
			if verErr == nil && len(verified.Data["userdata"]) > 0 {
				klog.Infof("Ignition secret %s/%s verified (%d bytes)", targetNamespace, targetSecretName, len(verified.Data["userdata"]))
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
	} else {
		existing.Data = srcSecret.Data
		_, err = ctrl.maroonedpodsCli.CoreV1().Secrets(targetNamespace).Update(
			context.Background(), existing, k8smetav1.UpdateOptions{})
		if err != nil {
			klog.Errorf("Failed to update Ignition secret in %s: %v", targetNamespace, err)
			return "", false
		}
	}

	return targetSecretName, true
}

func (ctrl *MaroonedPodsGateController) ensurePasstNAD(namespace string) {
	nadName := "primary-udn-kubevirt-binding"
	_, err := ctrl.maroonedpodsCli.CoreV1().RESTClient().Get().
		AbsPath("/apis/k8s.cni.cncf.io/v1/namespaces/" + namespace + "/network-attachment-definitions/" + nadName).
		DoRaw(context.Background())
	if err == nil {
		return
	}

	nadJSON := fmt.Sprintf(`{"apiVersion":"k8s.cni.cncf.io/v1","kind":"NetworkAttachmentDefinition","metadata":{"name":"%s","namespace":"%s"},"spec":{"config":"{\"cniVersion\":\"1.0.0\",\"name\":\"%s\",\"plugins\":[{\"type\":\"kubevirt-passt-binding\"}]}"}}`, nadName, namespace, nadName)
	_, err = ctrl.maroonedpodsCli.CoreV1().RESTClient().Post().
		AbsPath("/apis/k8s.cni.cncf.io/v1/namespaces/" + namespace + "/network-attachment-definitions").
		Body([]byte(nadJSON)).
		SetHeader("Content-Type", "application/json").
		DoRaw(context.Background())
	if err != nil {
		klog.Warningf("Failed to create passt NAD in %s: %v", namespace, err)
	} else {
		klog.Infof("Created passt NAD %s in namespace %s", nadName, namespace)
	}
}

func (ctrl *MaroonedPodsGateController) ensureVMNamespace(podNamespace string) (string, error) {
	vmNs := podNamespace + "-vm"
	_, err := ctrl.maroonedpodsCli.CoreV1().Namespaces().Get(context.Background(), vmNs, k8smetav1.GetOptions{})
	if err == nil {
		return vmNs, nil
	}
	if !errors.IsNotFound(err) {
		return "", err
	}

	ns := &v1.Namespace{
		ObjectMeta: k8smetav1.ObjectMeta{
			Name: vmNs,
			Labels: map[string]string{
				"k8s.ovn.org/primary-user-defined-network": "",
				"kubernetes.io/metadata.name":              vmNs,
			},
		},
	}
	_, err = ctrl.maroonedpodsCli.CoreV1().Namespaces().Create(context.Background(), ns, k8smetav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return "", fmt.Errorf("failed to create VM namespace %s: %v", vmNs, err)
	}
	klog.Infof("Created VM namespace %s with UDN label", vmNs)

	// Create UDN in the VM namespace (applied via unstructured since we don't have the CRD types)
	udnJSON := fmt.Sprintf(`{"apiVersion":"k8s.ovn.org/v1","kind":"UserDefinedNetwork","metadata":{"name":"tenant-vm-net","namespace":"%s"},"spec":{"topology":"Layer2","layer2":{"role":"Primary","subnets":["10.100.0.0/24"],"ipam":{"lifecycle":"Persistent"}}}}`, vmNs)
	_, err = ctrl.maroonedpodsCli.CoreV1().RESTClient().Post().
		AbsPath("/apis/k8s.ovn.org/v1/namespaces/" + vmNs + "/userdefinednetworks").
		Body([]byte(udnJSON)).
		SetHeader("Content-Type", "application/json").
		DoRaw(context.Background())
	if err != nil && !errors.IsAlreadyExists(err) {
		klog.Warningf("Failed to create UDN in %s (may need manual creation): %v", vmNs, err)
	} else {
		klog.Infof("Created UDN tenant-vm-net in namespace %s", vmNs)
	}

	return vmNs, nil
}

func (ctrl *MaroonedPodsGateController) createGroupVMI(groupName string, namespace string) (*virtv1.VirtualMachineInstance, error) {
	cpuCores, memoryMi, nodeImage, _ := ctrl.getGroupVMResourcesFromConfig()

	vmNamespace := namespace

	vmiName := fmt.Sprintf("%s%s", util.GroupVMNamePrefix, sanitizeGroupName(groupName))
	klog.Infof("Creating group VMI %s for group %s in namespace %s (pods in %s)", vmiName, groupName, vmNamespace, namespace)

	ctrl.ensurePasstNAD(vmNamespace)

	// Try to get Ignition config first (for OCP)
	// Must be created well before the VMI so kubelet can sync the secret volume
	ignitionSecretName, useIgnition := ctrl.ensureIgnitionSecret(vmNamespace)
	if useIgnition {
		klog.Infof("Waiting for ignition secret %s to propagate before creating VMI", ignitionSecretName)
		time.Sleep(5 * time.Second)
	}

	var userData string
	if !useIgnition {
		// Fall back to legacy kubeadm cloud-init for dev/non-OCP environments
		userData = fmt.Sprintf(`#!/bin/sh

cat <<EOF >/tmp/kubeadm-join-config.conf
apiVersion: kubeadm.k8s.io/v1beta3
kind: JoinConfiguration
discovery:
  bootstrapToken:
    unsafeSkipCAVerification: true
    apiServerEndpoint: "192.168.66.101:6443"
    token: "abcdef.1234567890123456"
nodeRegistration:
  kubeletExtraArgs:
    node-labels: "%s=%s"
EOF

kubeadm join --config /tmp/kubeadm-join-config.conf --ignore-preflight-errors=all --v=5
`, util.GroupNodeLabel, groupName)
		klog.V(2).Infof("Using legacy kubeadm cloud-init for group VMI %s", vmiName)
	}

	encodedData := base64.StdEncoding.EncodeToString([]byte(userData))

	vmi := virtv1.NewVMIReferenceFromNameWithNS(vmNamespace, vmiName)
	vmi.Spec = virtv1.VirtualMachineInstanceSpec{Domain: virtv1.DomainSpec{}}
	vmi.TypeMeta = k8smetav1.TypeMeta{
		APIVersion: virtv1.GroupVersion.String(),
		Kind:       "VirtualMachineInstance",
	}

	vmi.Labels = map[string]string{
		util.GroupLabel:          groupName,
		util.GroupPoolStateLabel: util.GroupPoolStateCreating,
	}

	passtInterface := virtv1.Interface{
		Name: virtv1.DefaultPodNetwork().Name,
		InterfaceBindingMethod: virtv1.InterfaceBindingMethod{
			PasstBinding: &virtv1.InterfacePasstBinding{},
		},
	}
	vmi.Spec.Domain.Devices.Interfaces = append(vmi.Spec.Domain.Devices.Interfaces, passtInterface)
	vmi.Spec.Networks = append(vmi.Spec.Networks, *virtv1.DefaultPodNetwork())

	guestMemory := resource.MustParse(fmt.Sprintf("%dMi", memoryMi))
	vmi.Spec.Domain.Memory = &virtv1.Memory{Guest: &guestMemory}
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

	if useIgnition {
		vmi.Spec.Domain.Devices.Disks = append(vmi.Spec.Domain.Devices.Disks,
			virtv1.Disk{
				Name: "cloudinitdisk",
				DiskDevice: virtv1.DiskDevice{
					Disk: &virtv1.DiskTarget{Bus: virtv1.DiskBusVirtio}}})
	} else {
		vmi.Spec.Domain.Devices.Disks = append(vmi.Spec.Domain.Devices.Disks,
			virtv1.Disk{
				Name: "cloudinitdisk",
				DiskDevice: virtv1.DiskDevice{
					Disk: &virtv1.DiskTarget{Bus: virtv1.DiskBusVirtio}}})
	}

	if useIgnition {
		vmi.Spec.Volumes = append(vmi.Spec.Volumes,
			virtv1.Volume{
				Name: "cloudinitdisk",
				VolumeSource: virtv1.VolumeSource{
					CloudInitConfigDrive: &virtv1.CloudInitConfigDriveSource{
						UserDataSecretRef: &v1.LocalObjectReference{
							Name: ignitionSecretName,
						},
					},
				}},
		)
	} else {
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
	}

	// Data disk for local storage (etcd, etc.)
	vmi.Spec.Domain.Devices.Disks = append(vmi.Spec.Domain.Devices.Disks,
		virtv1.Disk{
			Name: "datadisk",
			DiskDevice: virtv1.DiskDevice{
				Disk: &virtv1.DiskTarget{Bus: virtv1.DiskBusVirtio},
			},
		})
	vmi.Spec.Volumes = append(vmi.Spec.Volumes,
		virtv1.Volume{
			Name: "datadisk",
			VolumeSource: virtv1.VolumeSource{
				EmptyDisk: &virtv1.EmptyDiskSource{
					Capacity: resource.MustParse("50Gi"),
				},
			},
		})

	createdVMI, err := ctrl.maroonedpodsCli.KubevirtClient().KubevirtV1().VirtualMachineInstances(vmNamespace).Create(
		context.Background(), vmi, k8smetav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create group VMI: %v", err)
	}

	klog.Infof("Created group VMI %s/%s for group %s", createdVMI.Namespace, createdVMI.Name, groupName)
	return createdVMI, nil
}

func (ctrl *MaroonedPodsGateController) markGroupVMIReady(vmi *virtv1.VirtualMachineInstance, groupName string) error {
	klog.Infof("Marking group VMI %s/%s as ready for group %s", vmi.Namespace, vmi.Name, groupName)

	vmiCopy := vmi.DeepCopy()
	vmiCopy.Labels[util.GroupPoolStateLabel] = util.GroupPoolStateReady

	_, err := ctrl.maroonedpodsCli.KubevirtClient().KubevirtV1().VirtualMachineInstances(vmiCopy.Namespace).Update(
		context.Background(), vmiCopy, k8smetav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to mark group VMI as ready: %v", err)
	}

	// Label and taint the node for group scheduling
	nodeName := vmi.Name
	nodeObj, exists, err := ctrl.nodeInformer.GetStore().GetByKey(nodeName)
	if err != nil || !exists {
		return fmt.Errorf("node %s not found for group VMI", nodeName)
	}

	node := nodeObj.(*v1.Node).DeepCopy()
	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}
	node.Labels[util.GroupNodeLabel] = groupName
	// HPP CSI requires this label for PV node affinity
	node.Labels["topology.hostpath.csi/node"] = nodeName
	// Prevent ovnkube-node DaemonSet from running on virtual node
	// The internal OVN stack (br-ex, br-int, OpenFlow rules) interferes with
	// cross-node connectivity for bridge CNI pods inside the VM
	node.Labels["network.operator.openshift.io/dpu-host"] = ""

	// Remove blocking taints and add group taint
	taintsToRemove := map[string]bool{
		"node.cloudprovider.kubernetes.io/uninitialized": true,
		"UpdateInProgress": true,
	}

	cleanedTaints := []v1.Taint{}
	for _, t := range node.Spec.Taints {
		if taintsToRemove[t.Key] {
			klog.Infof("Removing blocking taint %s from node %s", t.Key, nodeName)
		} else {
			cleanedTaints = append(cleanedTaints, t)
		}
	}
	node.Spec.Taints = cleanedTaints

	// Add group taint so only group pods can schedule here
	hasTaint := false
	for _, t := range node.Spec.Taints {
		if t.Key == util.GroupLabel && t.Value == groupName {
			hasTaint = true
			break
		}
	}
	if !hasTaint {
		node.Spec.Taints = append(node.Spec.Taints, v1.Taint{
			Key:    util.GroupLabel,
			Value:  groupName,
			Effect: v1.TaintEffectNoSchedule,
		})
	}

	_, err = ctrl.maroonedpodsCli.CoreV1().Nodes().Update(context.Background(), node, k8smetav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to label/taint node %s for group %s: %v", nodeName, groupName, err)
	}

	// Clean up ghost pods from previous VMI boots (ContainerStatusUnknown)
	// Ghost pods are in the tenant namespace (groupName), not the VM namespace
	ctrl.cleanupGhostPods(nodeName, groupName)

	// Patch EndpointSlices for Route/LB-exposed services so external traffic
	// reaches the VM's host-namespace proxy instead of unreachable bridge pod IPs
	ctrl.ensureEndpointSlices(groupName, nodeName)

	ctrl.updateGroupPoolStatus(groupName, util.GroupPoolStateReady, vmi.Name, nodeName)
	klog.Infof("Node %s labeled and tainted for group %s", nodeName, groupName)
	return nil
}

// ensureEndpointSlices creates custom EndpointSlices that point to the VM's
// node IP instead of bridge pod IPs. Bridge pods (10.244.0.0/24) are inside
// the VM and unreachable from the management cluster's OVN network. A socat
// reverse proxy inside the VM forwards from the node IP port to the bridge pod.
func (ctrl *MaroonedPodsGateController) ensureEndpointSlices(namespace, nodeName string) {
	nodeObj, exists, err := ctrl.nodeInformer.GetStore().GetByKey(nodeName)
	if err != nil || !exists {
		klog.Warningf("ensureEndpointSlices: node %s not found", nodeName)
		return
	}
	node := nodeObj.(*v1.Node)
	nodeIP := ""
	for _, addr := range node.Status.Addresses {
		if addr.Type == v1.NodeInternalIP {
			nodeIP = addr.Address
			break
		}
	}
	if nodeIP == "" {
		klog.Warningf("ensureEndpointSlices: no InternalIP for node %s", nodeName)
		return
	}

	type svcEndpoint struct {
		serviceName string
		port        int32
		portName    string
		proxyPort   int32
	}

	endpoints := []svcEndpoint{
		{serviceName: "ignition-server-proxy", port: 8443, portName: "https", proxyPort: 8443},
		{serviceName: "konnectivity-server", port: 8091, portName: "", proxyPort: 8091},
		{serviceName: "oauth-openshift", port: 6443, portName: "", proxyPort: 16443},
		{serviceName: "kube-apiserver", port: 6443, portName: "", proxyPort: 6443},
	}

	managedBy := "maroonedpods-controller"
	proto := v1.ProtocolTCP
	ready := true

	for _, ep := range endpoints {
		sliceName := fmt.Sprintf("%s-mp-proxy", ep.serviceName)

		existing, err := ctrl.maroonedpodsCli.DiscoveryV1().EndpointSlices(namespace).Get(
			context.Background(), sliceName, k8smetav1.GetOptions{})
		if err == nil {
			needsUpdate := false
			if len(existing.Endpoints) == 0 || len(existing.Endpoints[0].Addresses) == 0 || existing.Endpoints[0].Addresses[0] != nodeIP {
				needsUpdate = true
			}
			if len(existing.Ports) == 0 || *existing.Ports[0].Port != ep.proxyPort {
				needsUpdate = true
			}
			if !needsUpdate {
				continue
			}
			existing.Endpoints = []discoveryv1.Endpoint{{
				Addresses:  []string{nodeIP},
				Conditions: discoveryv1.EndpointConditions{Ready: &ready},
				NodeName:   &nodeName,
			}}
			existing.Ports = []discoveryv1.EndpointPort{{
				Name:     &ep.portName,
				Port:     &ep.proxyPort,
				Protocol: &proto,
			}}
			_, err = ctrl.maroonedpodsCli.DiscoveryV1().EndpointSlices(namespace).Update(
				context.Background(), existing, k8smetav1.UpdateOptions{})
			if err != nil {
				klog.Warningf("ensureEndpointSlices: failed to update %s: %v", sliceName, err)
			}
			continue
		}

		if !errors.IsNotFound(err) {
			klog.Warningf("ensureEndpointSlices: failed to get %s: %v", sliceName, err)
			continue
		}

		epSlice := &discoveryv1.EndpointSlice{
			ObjectMeta: k8smetav1.ObjectMeta{
				Name:      sliceName,
				Namespace: namespace,
				Labels: map[string]string{
					"kubernetes.io/service-name":                ep.serviceName,
					"endpointslice.kubernetes.io/managed-by":    managedBy,
				},
			},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints: []discoveryv1.Endpoint{{
				Addresses:  []string{nodeIP},
				Conditions: discoveryv1.EndpointConditions{Ready: &ready},
				NodeName:   &nodeName,
			}},
			Ports: []discoveryv1.EndpointPort{{
				Name:     &ep.portName,
				Port:     &ep.proxyPort,
				Protocol: &proto,
			}},
		}

		_, err = ctrl.maroonedpodsCli.DiscoveryV1().EndpointSlices(namespace).Create(
			context.Background(), epSlice, k8smetav1.CreateOptions{})
		if err != nil {
			if strings.Contains(err.Error(), "is not allowed") {
				klog.Warningf("ensureEndpointSlices: OCP admission blocked creating %s/%s. "+
					"Admin must create it once: oc apply -f - <<EOF\n"+
					"apiVersion: discovery.k8s.io/v1\nkind: EndpointSlice\nmetadata:\n"+
					"  name: %s\n  namespace: %s\n  labels:\n"+
					"    kubernetes.io/service-name: %s\n"+
					"    endpointslice.kubernetes.io/managed-by: %s\n"+
					"addressType: IPv4\nendpoints:\n- addresses: [\"%s\"]\n  conditions: {ready: true}\n"+
					"  nodeName: %s\nports:\n- port: %d\n  protocol: TCP\nEOF",
					namespace, sliceName, sliceName, namespace, ep.serviceName, managedBy, nodeIP, nodeName, ep.proxyPort)
			} else {
				klog.Warningf("ensureEndpointSlices: failed to create %s: %v", sliceName, err)
			}
		} else {
			klog.Infof("Created proxy EndpointSlice %s/%s -> %s:%d", namespace, sliceName, nodeIP, ep.proxyPort)
		}
	}

}

func (ctrl *MaroonedPodsGateController) cleanupGhostPods(nodeName string, namespace string) {
	pods, err := ctrl.maroonedpodsCli.CoreV1().Pods(namespace).List(
		context.Background(), k8smetav1.ListOptions{
			FieldSelector: "spec.nodeName=" + nodeName,
		})
	if err != nil {
		klog.Warningf("Failed to list pods on node %s for ghost cleanup: %v", nodeName, err)
		return
	}

	deleted := 0
	for _, pod := range pods.Items {
		isGhost := false
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Terminated != nil && cs.State.Terminated.Reason == "ContainerStatusUnknown" {
				isGhost = true
				break
			}
		}
		if !isGhost {
			for _, cs := range pod.Status.InitContainerStatuses {
				if cs.State.Terminated != nil && cs.State.Terminated.Reason == "ContainerStatusUnknown" {
					isGhost = true
					break
				}
			}
		}
		if isGhost {
			klog.Infof("Deleting ghost pod %s/%s on node %s", pod.Namespace, pod.Name, nodeName)
			// Remove our finalizer first — without this, the delete hangs forever
			hasFinalizer := false
			cleanFinalizers := []string{}
			for _, f := range pod.Finalizers {
				if f == util.MaroonedPodsFinalizer {
					hasFinalizer = true
				} else {
					cleanFinalizers = append(cleanFinalizers, f)
				}
			}
			if hasFinalizer {
				patch := map[string]interface{}{
					"metadata": map[string]interface{}{
						"finalizers": cleanFinalizers,
					},
				}
				patchBytes, err := json.Marshal(patch)
				if err == nil {
					_, err = ctrl.maroonedpodsCli.CoreV1().Pods(pod.Namespace).Patch(
						context.Background(), pod.Name, k8stypes.MergePatchType, patchBytes, k8smetav1.PatchOptions{})
					if err != nil {
						klog.Warningf("Failed to remove finalizer from ghost pod %s/%s: %v", pod.Namespace, pod.Name, err)
					}
				}
			}
			grace := int64(0)
			_ = ctrl.maroonedpodsCli.CoreV1().Pods(pod.Namespace).Delete(
				context.Background(), pod.Name, k8smetav1.DeleteOptions{GracePeriodSeconds: &grace})
			deleted++
		}
	}
	if deleted > 0 {
		klog.Infof("Cleaned up %d ghost pods on node %s", deleted, nodeName)
	}
}

func (ctrl *MaroonedPodsGateController) handleGroupPodDeletion(pod *v1.Pod, key string, groupName string) (error, enqueueState) {
	hasFinalizer := false
	for _, f := range pod.Finalizers {
		if f == util.MaroonedPodsFinalizer {
			hasFinalizer = true
			break
		}
	}

	if !hasFinalizer {
		return nil, Forget
	}

	klog.Infof("Group pod %s/%s being deleted (group: %s)", pod.Namespace, pod.Name, groupName)

	remaining := ctrl.countGroupPods(groupName, pod.Name)
	if remaining == 0 {
		klog.Infof("Last pod in group %s deleted, cleaning up group VM", groupName)
		ctrl.cleanupGroupVM(groupName)
	} else {
		klog.V(3).Infof("Group %s still has %d pods, keeping VM alive", groupName, remaining)
	}

	// Remove finalizer using merge patch to avoid SCC re-validation
	cleanFinalizers := []string{}
	for _, f := range pod.Finalizers {
		if f != util.MaroonedPodsFinalizer {
			cleanFinalizers = append(cleanFinalizers, f)
		}
	}
	cleanPatch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"finalizers": cleanFinalizers,
		},
	}
	cleanPatchBytes, err := json.Marshal(cleanPatch)
	if err != nil {
		return fmt.Errorf("failed to marshal finalizer patch: %v", err), BackOff
	}
	_, err = ctrl.maroonedpodsCli.CoreV1().Pods(pod.Namespace).Patch(
		context.Background(),
		pod.Name,
		k8stypes.MergePatchType,
		cleanPatchBytes,
		k8smetav1.PatchOptions{},
	)
	if err != nil {
		return err, BackOff
	}

	return nil, Forget
}

func (ctrl *MaroonedPodsGateController) countGroupPods(groupName string, excludePodName string) int {
	pods := ctrl.podInformer.GetStore().List()
	count := 0
	for _, obj := range pods {
		pod := obj.(*v1.Pod)
		if pod.Name == excludePodName {
			continue
		}
		if pod.DeletionTimestamp != nil {
			continue
		}
		if g, ok := pod.Labels[util.GroupLabel]; ok && g == groupName {
			count++
		}
	}
	return count
}

func (ctrl *MaroonedPodsGateController) cleanupGroupVM(groupName string) {
	vmis := ctrl.vmiInformer.GetStore().List()
	for _, obj := range vmis {
		vmi := obj.(*virtv1.VirtualMachineInstance)
		if vmi.Labels == nil {
			continue
		}
		if g, ok := vmi.Labels[util.GroupLabel]; ok && g == groupName {
			klog.Infof("Deleting group VMI %s/%s (group %s cleanup)", vmi.Namespace, vmi.Name, groupName)
			err := ctrl.maroonedpodsCli.KubevirtClient().KubevirtV1().VirtualMachineInstances(vmi.Namespace).Delete(
				context.Background(), vmi.Name, k8smetav1.DeleteOptions{})
			if err != nil {
				klog.Errorf("Failed to delete group VMI %s: %v", vmi.Name, err)
			}
		}
	}

	ctrl.removeGroupPoolStatus(groupName)
}

func (ctrl *MaroonedPodsGateController) updateGroupPoolStatus(groupName, state, vmiName, nodeName string) {
	config := ctrl.getConfig()
	if config == nil {
		return
	}

	configCopy := config.DeepCopy()
	if configCopy.Status.GroupPools == nil {
		configCopy.Status.GroupPools = make(map[string]v1alpha1.GroupPoolStatus)
	}

	configCopy.Status.GroupPools[groupName] = v1alpha1.GroupPoolStatus{
		State:    state,
		VMIName:  vmiName,
		NodeName: nodeName,
	}

	_, err := ctrl.maroonedpodsCli.RestClient().Put().
		Resource("maroonedpodsconfigs").
		Name(configCopy.Name).
		SubResource("status").
		Body(configCopy).
		Do(context.Background()).
		Get()
	if err != nil {
		klog.V(3).Infof("Failed to update group pool status for %s: %v", groupName, err)
	}
}

func (ctrl *MaroonedPodsGateController) removeGroupPoolStatus(groupName string) {
	config := ctrl.getConfig()
	if config == nil {
		return
	}

	configCopy := config.DeepCopy()
	if configCopy.Status.GroupPools == nil {
		return
	}

	delete(configCopy.Status.GroupPools, groupName)

	_, err := ctrl.maroonedpodsCli.RestClient().Put().
		Resource("maroonedpodsconfigs").
		Name(configCopy.Name).
		SubResource("status").
		Body(configCopy).
		Do(context.Background()).
		Get()
	if err != nil {
		klog.V(3).Infof("Failed to remove group pool status for %s: %v", groupName, err)
	}
}

func (ctrl *MaroonedPodsGateController) reconcileGroupPools() {
	// Find all active groups from VMIs
	groupNames := make(map[string]bool)
	vmis := ctrl.vmiInformer.GetStore().List()
	for _, obj := range vmis {
		vmi := obj.(*virtv1.VirtualMachineInstance)
		if vmi.Labels == nil {
			continue
		}
		if g, ok := vmi.Labels[util.GroupLabel]; ok {
			groupNames[g] = true
		}
	}

	for groupName := range groupNames {
		podCount := ctrl.countGroupPods(groupName, "")
		if podCount == 0 {
			klog.Infof("Group %s has no pods, cleaning up stale VM", groupName)
			ctrl.cleanupGroupVM(groupName)
		}
	}
}

// reconcileCSRs auto-approves CSRs for nodes created by this controller
func (ctrl *MaroonedPodsGateController) reconcileCSRs() {
	csrs, err := ctrl.maroonedpodsCli.CertificatesV1().CertificateSigningRequests().List(
		context.Background(), k8smetav1.ListOptions{})
	if err != nil {
		klog.Errorf("Failed to list CSRs: %v", err)
		return
	}

	for _, csr := range csrs.Items {
		// Skip already approved/denied CSRs
		approved := false
		denied := false
		for _, condition := range csr.Status.Conditions {
			if condition.Type == "Approved" {
				approved = true
			}
			if condition.Type == "Denied" {
				denied = true
			}
		}
		if approved || denied {
			continue
		}

		// Extract node name from CSR
		nodeName := ""
		if csr.Spec.Username != "" {
			// Username format: system:node:<nodeName>
			parts := strings.Split(csr.Spec.Username, ":")
			if len(parts) == 3 && parts[0] == "system" && parts[1] == "node" {
				nodeName = parts[2]
			}
		}

		// Handle bootstrap CSRs (no node name yet, from node-bootstrapper SA)
		if nodeName == "" {
			// Check if this is a bootstrap CSR for kubelet client cert
			if csr.Spec.SignerName == "kubernetes.io/kube-apiserver-client-kubelet" &&
				strings.Contains(csr.Spec.Username, "node-bootstrapper") {
				// Approve if we have any group VMI in creating state
				vmis := ctrl.vmiInformer.GetStore().List()
				hasCreatingGroupVMI := false
				for _, obj := range vmis {
					v := obj.(*virtv1.VirtualMachineInstance)
					if v.Labels != nil {
						if state, ok := v.Labels[util.GroupPoolStateLabel]; ok && state == util.GroupPoolStateCreating {
							hasCreatingGroupVMI = true
							break
						}
					}
				}
				if !hasCreatingGroupVMI {
					continue
				}
				// Approve the bootstrap CSR
				klog.Infof("Auto-approving bootstrap CSR %s (group VMI bootstrapping)", csr.Name)
				csrCopy := csr.DeepCopy()
				csrCopy.Status.Conditions = append(csrCopy.Status.Conditions, certv1.CertificateSigningRequestCondition{
					Type:           certv1.CertificateApproved,
					Status:         v1.ConditionTrue,
					Reason:         "MaroonedPodsAutoApproved",
					Message:        "Auto-approved bootstrap CSR for maroonedpods group node",
					LastUpdateTime: k8smetav1.Now(),
				})
				_, err = ctrl.maroonedpodsCli.CertificatesV1().CertificateSigningRequests().UpdateApproval(
					context.Background(), csr.Name, csrCopy, k8smetav1.UpdateOptions{})
				if err != nil {
					klog.Errorf("Failed to approve bootstrap CSR %s: %v", csr.Name, err)
				} else {
					klog.Infof("Successfully approved bootstrap CSR %s", csr.Name)
				}
			}
			continue
		}

		// Check if this node was created by us (VMI exists with this name)
		// Search across all namespaces since VMIs can be in any namespace
		var vmi *virtv1.VirtualMachineInstance
		vmis := ctrl.vmiInformer.GetStore().List()
		for _, obj := range vmis {
			v := obj.(*virtv1.VirtualMachineInstance)
			if v.Name == nodeName {
				vmi = v
				break
			}
		}
		if vmi == nil {
			continue
		}
		// Verify the VMI has our labels (either group mode or 1:1 mode)
		if vmi.Labels == nil {
			continue
		}
		isOurs := false
		if _, ok := vmi.Labels[util.GroupLabel]; ok {
			isOurs = true
		}
		if vmi.Labels[util.MaroonedPodsLabel] == "true" {
			isOurs = true
		}
		if !isOurs {
			continue
		}

		// Approve the CSR
		klog.Infof("Auto-approving CSR %s for node %s (VMI %s)", csr.Name, nodeName, vmi.Name)
		csrCopy := csr.DeepCopy()
		csrCopy.Status.Conditions = append(csrCopy.Status.Conditions, certv1.CertificateSigningRequestCondition{
			Type:           certv1.CertificateApproved,
			Status:         v1.ConditionTrue,
			Reason:         "MaroonedPodsAutoApproved",
			Message:        fmt.Sprintf("Auto-approved CSR for maroonedpods node %s", nodeName),
			LastUpdateTime: k8smetav1.Now(),
		})

		_, err = ctrl.maroonedpodsCli.CertificatesV1().CertificateSigningRequests().UpdateApproval(
			context.Background(), csr.Name, csrCopy, k8smetav1.UpdateOptions{})
		if err != nil {
			klog.Errorf("Failed to approve CSR %s: %v", csr.Name, err)
		} else {
			klog.Infof("Successfully approved CSR %s for node %s", csr.Name, nodeName)
		}
	}
}

// reconcileEndpointSlices finds all Ready group VMIs and ensures custom
// EndpointSlices exist for Route/LB-exposed services in their namespaces.
func (ctrl *MaroonedPodsGateController) reconcileEndpointSlices() {
	vmis := ctrl.vmiInformer.GetStore().List()
	for _, obj := range vmis {
		vmi := obj.(*virtv1.VirtualMachineInstance)
		if vmi.Labels == nil {
			continue
		}
		groupName, ok := vmi.Labels[util.GroupLabel]
		if !ok || groupName == "" {
			continue
		}
		if state, ok := vmi.Labels[util.GroupPoolStateLabel]; !ok || state != util.GroupPoolStateReady {
			continue
		}
		ctrl.ensureEndpointSlices(groupName, vmi.Name)
	}
}
