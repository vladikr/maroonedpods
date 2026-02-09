package tests

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	virtv1 "kubevirt.io/api/core/v1"

	"maroonedpods.io/maroonedpods/pkg/util"
	"maroonedpods.io/maroonedpods/tests/builders"
	"maroonedpods.io/maroonedpods/tests/framework"
	testutils "maroonedpods.io/maroonedpods/tests/utils"
)

var _ = Describe("[e2e] Pod Isolation", func() {
	var (
		f  *framework.Framework
		ns string
	)

	BeforeEach(func() {
		f = framework.DefaultFramework
		nsName := testutils.GenerateNamespaceName("pod-isolation")
		createdNs, err := f.CreateNamespace(nsName)
		Expect(err).ToNot(HaveOccurred())
		ns = createdNs.Name
	})

	AfterEach(func() {
		if ns != "" {
			err := f.DeleteNamespace(ns)
			Expect(err).ToNot(HaveOccurred())
		}
	})

	It("should create a VMI when a marooned pod is created", func() {
		podName := "test-nginx"

		By("Creating a marooned pod")
		pod := builders.NewMaroonedPod(podName, ns)
		createdPod, err := f.CreatePod(pod)
		Expect(err).ToNot(HaveOccurred())
		Expect(createdPod).ToNot(BeNil())

		By("Verifying the pod has the marooned label")
		Expect(createdPod.Labels[util.MaroonedPodLabel]).To(Equal("true"))

		By("Verifying the pod gets a scheduling gate")
		Eventually(func() bool {
			p, err := f.GetPod(podName)
			if err != nil {
				return false
			}
			for _, gate := range p.Spec.SchedulingGates {
				if gate.Name == util.MaroonedPodsGate {
					return true
				}
			}
			return false
		}, testutils.ShortTimeout, 2*time.Second).Should(BeTrue())

		By("Waiting for VMI to be created")
		vmi, err := f.WaitForVMI(ns, podName, testutils.DefaultTimeout)
		Expect(err).ToNot(HaveOccurred())
		Expect(vmi).ToNot(BeNil())
		Expect(vmi.Name).To(Equal(podName))

		By("Waiting for VMI to be running")
		err = f.WaitForVMIPhase(ns, podName, virtv1.Running, testutils.LongTimeout)
		Expect(err).ToNot(HaveOccurred())

		By("Verifying the node joins the cluster")
		err = f.WaitForNodeReady(podName, testutils.DefaultTimeout)
		Expect(err).ToNot(HaveOccurred())

		By("Verifying the scheduling gate is removed")
		Eventually(func() bool {
			p, err := f.GetPod(podName)
			if err != nil {
				return false
			}
			return len(p.Spec.SchedulingGates) == 0
		}, testutils.DefaultTimeout, 2*time.Second).Should(BeTrue())

		By("Waiting for pod to be running")
		err = f.WaitForPodPhase(podName, v1.PodRunning, testutils.DefaultTimeout)
		Expect(err).ToNot(HaveOccurred())

		By("Verifying the pod is running on the dedicated node")
		finalPod, err := f.GetPod(podName)
		Expect(err).ToNot(HaveOccurred())
		Expect(finalPod.Spec.NodeName).To(Equal(podName))
		Expect(finalPod.Status.Phase).To(Equal(v1.PodRunning))
	})

	It("should handle multiple marooned pods in the same namespace", func() {
		pod1Name := "test-nginx-1"
		pod2Name := "test-nginx-2"

		By("Creating first marooned pod")
		pod1 := builders.NewMaroonedPod(pod1Name, ns)
		_, err := f.CreatePod(pod1)
		Expect(err).ToNot(HaveOccurred())

		By("Creating second marooned pod")
		pod2 := builders.NewMaroonedPod(pod2Name, ns)
		_, err = f.CreatePod(pod2)
		Expect(err).ToNot(HaveOccurred())

		By("Waiting for both VMIs to be created")
		vmi1, err := f.WaitForVMI(ns, pod1Name, testutils.DefaultTimeout)
		Expect(err).ToNot(HaveOccurred())
		Expect(vmi1).ToNot(BeNil())

		vmi2, err := f.WaitForVMI(ns, pod2Name, testutils.DefaultTimeout)
		Expect(err).ToNot(HaveOccurred())
		Expect(vmi2).ToNot(BeNil())

		By("Verifying both VMIs are distinct")
		Expect(vmi1.Name).ToNot(Equal(vmi2.Name))
	})

	It("should not create a VMI for a pod without the marooned label", func() {
		podName := "regular-nginx"

		By("Creating a regular pod without marooned label")
		pod := builders.NewPod(podName, ns).
			WithContainer("nginx", "nginx:latest").
			Build()
		_, err := f.CreatePod(pod)
		Expect(err).ToNot(HaveOccurred())

		By("Verifying no VMI is created")
		time.Sleep(10 * time.Second) // Give some time for VMI creation if it would happen
		_, err = f.GetVMI(ns, podName)
		Expect(err).To(HaveOccurred()) // Should not find the VMI
	})

	It("should add finalizer to marooned pod", func() {
		podName := "test-finalizer"

		By("Creating a marooned pod")
		pod := builders.NewMaroonedPod(podName, ns)
		_, err := f.CreatePod(pod)
		Expect(err).ToNot(HaveOccurred())

		By("Verifying the pod gets a finalizer")
		Eventually(func() bool {
			p, err := f.GetPod(podName)
			if err != nil {
				return false
			}
			for _, finalizer := range p.Finalizers {
				if finalizer == util.MaroonedPodsFinalizer {
					return true
				}
			}
			return false
		}, testutils.ShortTimeout, 2*time.Second).Should(BeTrue())
	})
})
