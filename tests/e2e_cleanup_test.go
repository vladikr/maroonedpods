package tests

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	virtv1 "kubevirt.io/api/core/v1"

	"maroonedpods.io/maroonedpods/tests/builders"
	"maroonedpods.io/maroonedpods/tests/framework"
	testutils "maroonedpods.io/maroonedpods/tests/utils"
)

var _ = Describe("[e2e] Pod and VMI Cleanup", func() {
	var (
		f  *framework.Framework
		ns string
	)

	BeforeEach(func() {
		f = framework.DefaultFramework
		nsName := testutils.GenerateNamespaceName("cleanup")
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

	It("should delete VMI when marooned pod is deleted", func() {
		podName := "test-cleanup"

		By("Creating a marooned pod")
		pod := builders.NewMaroonedPod(podName, ns)
		_, err := f.CreatePod(pod)
		Expect(err).ToNot(HaveOccurred())

		By("Waiting for VMI to be created and running")
		vmi, err := f.WaitForVMI(ns, podName, testutils.DefaultTimeout)
		Expect(err).ToNot(HaveOccurred())
		Expect(vmi).ToNot(BeNil())

		err = f.WaitForVMIPhase(ns, podName, virtv1.Running, testutils.LongTimeout)
		Expect(err).ToNot(HaveOccurred())

		By("Deleting the pod")
		err = f.DeletePod(podName)
		Expect(err).ToNot(HaveOccurred())

		By("Verifying the VMI is deleted")
		err = f.WaitForVMIDeleted(ns, podName, testutils.DefaultTimeout)
		Expect(err).ToNot(HaveOccurred())

		By("Verifying the pod is deleted")
		err = f.WaitForPodDeleted(podName, testutils.DefaultTimeout)
		Expect(err).ToNot(HaveOccurred())
	})

	It("should handle graceful deletion", func() {
		podName := "test-graceful"

		By("Creating a marooned pod")
		pod := builders.NewMaroonedPod(podName, ns)
		_, err := f.CreatePod(pod)
		Expect(err).ToNot(HaveOccurred())

		By("Waiting for VMI to exist")
		_, err = f.WaitForVMI(ns, podName, testutils.DefaultTimeout)
		Expect(err).ToNot(HaveOccurred())

		By("Deleting the pod with grace period")
		gracePeriod := int64(30)
		deleteOptions := v1.DeleteOptions{
			GracePeriodSeconds: &gracePeriod,
		}
		err = f.K8sClient.CoreV1().Pods(ns).Delete(context.Background(), podName, deleteOptions)
		Expect(err).ToNot(HaveOccurred())

		By("Verifying the pod enters terminating state")
		Eventually(func() bool {
			p, err := f.GetPod(podName)
			if err != nil {
				return true // Pod might already be deleted
			}
			return p.DeletionTimestamp != nil
		}, testutils.ShortTimeout, 2*time.Second).Should(BeTrue())

		By("Verifying the VMI is eventually deleted")
		err = f.WaitForVMIDeleted(ns, podName, testutils.DefaultTimeout)
		Expect(err).ToNot(HaveOccurred())
	})

	It("should clean up VMI even if pod is forcefully deleted", func() {
		podName := "test-force-delete"

		By("Creating a marooned pod")
		pod := builders.NewMaroonedPod(podName, ns)
		_, err := f.CreatePod(pod)
		Expect(err).ToNot(HaveOccurred())

		By("Waiting for VMI to be created")
		_, err = f.WaitForVMI(ns, podName, testutils.DefaultTimeout)
		Expect(err).ToNot(HaveOccurred())

		By("Force deleting the pod")
		gracePeriod := int64(0)
		deleteOptions := v1.DeleteOptions{
			GracePeriodSeconds: &gracePeriod,
		}
		err = f.K8sClient.CoreV1().Pods(ns).Delete(context.Background(), podName, deleteOptions)
		Expect(err).ToNot(HaveOccurred())

		By("Verifying the VMI is cleaned up")
		err = f.WaitForVMIDeleted(ns, podName, testutils.DefaultTimeout)
		Expect(err).ToNot(HaveOccurred())
	})
})
