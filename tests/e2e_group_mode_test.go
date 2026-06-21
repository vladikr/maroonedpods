package tests

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"maroonedpods.io/maroonedpods/pkg/util"
	"maroonedpods.io/maroonedpods/tests/builders"
	"maroonedpods.io/maroonedpods/tests/framework"
	testutils "maroonedpods.io/maroonedpods/tests/utils"
)

var _ = Describe("[e2e] Group Mode", func() {
	var (
		f  *framework.Framework
		ns string
	)

	BeforeEach(func() {
		f = framework.DefaultFramework
		nsName := testutils.GenerateNamespaceName("group-mode")
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

	It("should create a single VMI for a group of pods", func() {
		groupName := "test-cp-1"

		By("Creating multiple pods with the same group label")
		for i := 0; i < 3; i++ {
			podName := fmt.Sprintf("cp-pod-%d", i)
			pod := builders.NewGroupPod(podName, ns, groupName)
			_, err := f.CreatePod(pod)
			Expect(err).ToNot(HaveOccurred())
		}

		By("Verifying all pods have the scheduling gate")
		for i := 0; i < 3; i++ {
			podName := fmt.Sprintf("cp-pod-%d", i)
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
		}

		By("Waiting for the group VMI to be created")
		expectedVMIName := util.GroupVMNamePrefix + groupName
		vmi, err := f.WaitForVMI(ns, expectedVMIName, testutils.DefaultTimeout)
		Expect(err).ToNot(HaveOccurred())
		Expect(vmi).ToNot(BeNil())

		By("Verifying the VMI has group labels")
		Expect(vmi.Labels[util.GroupLabel]).To(Equal(groupName))
		Expect(vmi.Labels[util.GroupPoolStateLabel]).To(Equal(util.GroupPoolStateCreating))

		By("Verifying only one VMI exists for the group")
		vmis, err := f.ListVMIs(ns)
		Expect(err).ToNot(HaveOccurred())
		groupVMICount := 0
		for _, v := range vmis.Items {
			if g, ok := v.Labels[util.GroupLabel]; ok && g == groupName {
				groupVMICount++
			}
		}
		Expect(groupVMICount).To(Equal(1))
	})

	It("should ungate all group pods when the group VM is ready", func() {
		groupName := "test-cp-2"
		podNames := []string{"etcd-0", "kube-apiserver", "kube-controller-manager"}

		By("Creating control plane pods with the same group label")
		for _, name := range podNames {
			pod := builders.NewGroupPod(name, ns, groupName)
			_, err := f.CreatePod(pod)
			Expect(err).ToNot(HaveOccurred())
		}

		By("Waiting for the group VMI to become Running")
		expectedVMIName := util.GroupVMNamePrefix + groupName
		err := f.WaitForVMIPhase(ns, expectedVMIName, "Running", testutils.LongTimeout)
		Expect(err).ToNot(HaveOccurred())

		By("Waiting for the group node to join the cluster")
		err = f.WaitForNodeReady(expectedVMIName, testutils.DefaultTimeout)
		Expect(err).ToNot(HaveOccurred())

		By("Verifying all pods get ungated")
		for _, name := range podNames {
			Eventually(func() bool {
				p, err := f.GetPod(name)
				if err != nil {
					return false
				}
				return len(p.Spec.SchedulingGates) == 0
			}, testutils.DefaultTimeout, 2*time.Second).Should(BeTrue(),
				fmt.Sprintf("Pod %s should have scheduling gate removed", name))
		}
	})

	It("should not delete the VMI when one group pod is deleted", func() {
		groupName := "test-cp-3"

		By("Creating two pods with the same group label")
		pod1 := builders.NewGroupPod("pod-stay", ns, groupName)
		_, err := f.CreatePod(pod1)
		Expect(err).ToNot(HaveOccurred())

		pod2 := builders.NewGroupPod("pod-delete", ns, groupName)
		_, err = f.CreatePod(pod2)
		Expect(err).ToNot(HaveOccurred())

		By("Waiting for the group VMI to be created")
		expectedVMIName := util.GroupVMNamePrefix + groupName
		_, err = f.WaitForVMI(ns, expectedVMIName, testutils.DefaultTimeout)
		Expect(err).ToNot(HaveOccurred())

		By("Deleting one of the group pods")
		err = f.DeletePod("pod-delete")
		Expect(err).ToNot(HaveOccurred())

		By("Verifying the VMI still exists")
		time.Sleep(10 * time.Second)
		vmi, err := f.GetVMI(ns, expectedVMIName)
		Expect(err).ToNot(HaveOccurred())
		Expect(vmi).ToNot(BeNil())
	})

	It("should clean up the VMI when the last group pod is deleted", func() {
		groupName := "test-cp-4"

		By("Creating a single pod with a group label")
		pod := builders.NewGroupPod("solo-pod", ns, groupName)
		_, err := f.CreatePod(pod)
		Expect(err).ToNot(HaveOccurred())

		By("Waiting for the group VMI to be created")
		expectedVMIName := util.GroupVMNamePrefix + groupName
		_, err = f.WaitForVMI(ns, expectedVMIName, testutils.DefaultTimeout)
		Expect(err).ToNot(HaveOccurred())

		By("Deleting the last pod in the group")
		err = f.DeletePod("solo-pod")
		Expect(err).ToNot(HaveOccurred())

		By("Verifying the VMI is deleted")
		err = f.WaitForVMIDeleted(ns, expectedVMIName, testutils.DefaultTimeout)
		Expect(err).ToNot(HaveOccurred())
	})

	It("should translate HyperShift cluster label to group label", func() {
		clusterName := "clusters-tenant-1"

		By("Creating a pod with only the HyperShift cluster label")
		pod := builders.NewHypershiftPod("hs-pod", ns, clusterName)
		createdPod, err := f.CreatePod(pod)
		Expect(err).ToNot(HaveOccurred())

		By("Verifying the pod gets the canonical group label injected")
		Eventually(func() string {
			p, err := f.GetPod(createdPod.Name)
			if err != nil {
				return ""
			}
			return p.Labels[util.GroupLabel]
		}, testutils.ShortTimeout, 2*time.Second).Should(Equal(clusterName))

		By("Verifying the pod gets a group nodeSelector (not pod-name-based)")
		Eventually(func() string {
			p, err := f.GetPod(createdPod.Name)
			if err != nil {
				return ""
			}
			return p.Spec.NodeSelector[util.GroupNodeLabel]
		}, testutils.ShortTimeout, 2*time.Second).Should(Equal(clusterName))
	})

	It("should still use 1:1 mode for pods without group label", func() {
		By("Creating a marooned pod without group label")
		pod := builders.NewMaroonedPod("regular-marooned", ns)
		_, err := f.CreatePod(pod)
		Expect(err).ToNot(HaveOccurred())

		By("Verifying a pod-named VMI is created (1:1 mode)")
		vmi, err := f.WaitForVMI(ns, "regular-marooned", testutils.DefaultTimeout)
		Expect(err).ToNot(HaveOccurred())
		Expect(vmi.Name).To(Equal("regular-marooned"))

		By("Verifying no group labels on the VMI")
		_, hasGroupLabel := vmi.Labels[util.GroupLabel]
		Expect(hasGroupLabel).To(BeFalse())
	})

	It("should handle group mode and 1:1 mode simultaneously", func() {
		groupName := "test-cp-mixed"

		By("Creating a group pod")
		groupPod := builders.NewGroupPod("group-pod", ns, groupName)
		_, err := f.CreatePod(groupPod)
		Expect(err).ToNot(HaveOccurred())

		By("Creating a 1:1 pod")
		regularPod := builders.NewMaroonedPod("regular-pod", ns)
		_, err = f.CreatePod(regularPod)
		Expect(err).ToNot(HaveOccurred())

		By("Waiting for the group VMI")
		expectedGroupVMI := util.GroupVMNamePrefix + groupName
		groupVMI, err := f.WaitForVMI(ns, expectedGroupVMI, testutils.DefaultTimeout)
		Expect(err).ToNot(HaveOccurred())
		Expect(groupVMI.Labels[util.GroupLabel]).To(Equal(groupName))

		By("Waiting for the 1:1 VMI")
		regularVMI, err := f.WaitForVMI(ns, "regular-pod", testutils.DefaultTimeout)
		Expect(err).ToNot(HaveOccurred())
		_, hasGroupLabel := regularVMI.Labels[util.GroupLabel]
		Expect(hasGroupLabel).To(BeFalse())

		By("Verifying they are independent VMIs")
		Expect(groupVMI.Name).ToNot(Equal(regularVMI.Name))
	})

	Context("node labeling and tainting", func() {
		It("should label and taint the group node when it becomes ready", func() {
			groupName := "test-cp-node"

			By("Creating a group pod")
			pod := builders.NewGroupPod("node-test-pod", ns, groupName)
			_, err := f.CreatePod(pod)
			Expect(err).ToNot(HaveOccurred())

			By("Waiting for the group VMI and node")
			expectedVMIName := util.GroupVMNamePrefix + groupName
			err = f.WaitForVMIPhase(ns, expectedVMIName, "Running", testutils.LongTimeout)
			Expect(err).ToNot(HaveOccurred())

			err = f.WaitForNodeReady(expectedVMIName, testutils.DefaultTimeout)
			Expect(err).ToNot(HaveOccurred())

			By("Verifying the node has the group label")
			Eventually(func() string {
				node, err := f.K8sClient.CoreV1().Nodes().Get(
					context.Background(), expectedVMIName, metav1.GetOptions{})
				if err != nil {
					return ""
				}
				return node.Labels[util.GroupNodeLabel]
			}, testutils.DefaultTimeout, 2*time.Second).Should(Equal(groupName))

			By("Verifying the node has the group taint")
			Eventually(func() bool {
				node, err := f.K8sClient.CoreV1().Nodes().Get(
					context.Background(), expectedVMIName, metav1.GetOptions{})
				if err != nil {
					return false
				}
				for _, taint := range node.Spec.Taints {
					if taint.Key == util.GroupLabel && taint.Value == groupName {
						return true
					}
				}
				return false
			}, testutils.DefaultTimeout, 2*time.Second).Should(BeTrue())
		})
	})
})
