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
		queue:           workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "maroonedpods-queue"),

		recorder:                     eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: util.ControllerPodName}),
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
	for i := 0; i < threadiness; i++ {
		go wait.Until(ctrl.runWorker, time.Second, ctrl.stop)
	}

	<-ctrl.stop

}

func (ctrl *MaroonedPodsGateController) createVMIFromPod(pod *v1.Pod) *virtv1.VirtualMachineInstance {

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
useradd -s /bin/bash -d /home/vladik/ -m -G sudo vladik
passwd vladik
`, pod.Name)

	encodedData := base64.StdEncoding.EncodeToString([]byte(userData))
	nodeVmImageTemplate := "quay.io/capk/ubuntu-2004-container-disk:v1.26.0"
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

	guestMemory := resource.MustParse("3Gi")
	vmi.Spec.Domain.Memory = &virtv1.Memory{Guest: &guestMemory}

	vmi.Spec.Domain.CPU = &virtv1.CPU{
		Threads: 1,
		Sockets: 1,
		Cores:   2,
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
					Image: nodeVmImageTemplate},
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
