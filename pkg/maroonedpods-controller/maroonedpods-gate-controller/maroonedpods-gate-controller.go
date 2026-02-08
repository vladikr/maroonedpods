package mp_controller

import (
	"context"
	"encoding/base64"
	"fmt"
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

func (ctrl *MaroonedPodsGateController) deletePod(obj interface{}) {
	pod := obj.(*v1.Pod)
	klog.V(3).Infof("Pod %s/%s deleted", pod.Namespace, pod.Name)
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

	// Handle pod deletion with finalizer
	if pod.DeletionTimestamp != nil {
		return ctrl.handlePodDeletion(pod, podKey)
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

	// Remove our finalizer
	podCopy := pod.DeepCopy()
	newFinalizers := []string{}
	for _, f := range podCopy.Finalizers {
		if f != util.MaroonedPodsFinalizer {
			newFinalizers = append(newFinalizers, f)
		}
	}
	podCopy.Finalizers = newFinalizers

	_, err = ctrl.maroonedpodsCli.CoreV1().Pods(podCopy.Namespace).Update(context.Background(), podCopy, k8smetav1.UpdateOptions{})
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
	pod := obj.(*v1.Pod).DeepCopy()
	if pod.Spec.SchedulingGates != nil && len(pod.Spec.SchedulingGates) == 1 && pod.Spec.SchedulingGates[0].Name == util.MaroonedPodsGate {
		pod.Spec.SchedulingGates = []v1.PodSchedulingGate{}
		_, err = ctrl.maroonedpodsCli.CoreV1().Pods(pod.Namespace).Update(context.Background(), pod, k8smetav1.UpdateOptions{})
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
		klog.Infof("Creating VMI for pod %s/%s", pod.Namespace, pod.Name)
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

	// Get node image and taint key from config
	_, _, nodeImage, taintKey := ctrl.getVMResourcesFromConfig()

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
