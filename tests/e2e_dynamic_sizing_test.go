package tests

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/resource"
	virtv1 "kubevirt.io/api/core/v1"

	"maroonedpods.io/maroonedpods/tests/builders"
	"maroonedpods.io/maroonedpods/tests/framework"
	testutils "maroonedpods.io/maroonedpods/tests/utils"
)

var _ = Describe("[e2e] Dynamic VM Sizing", func() {
	var (
		f  *framework.Framework
		ns string
	)

	BeforeEach(func() {
		f = framework.DefaultFramework
		nsName := testutils.GenerateNamespaceName("dynamic-sizing")
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

	It("should size VMI based on pod resource requests", func() {
		podName := "test-sizing"
		cpuRequest := "2"
		memoryRequest := "4Gi"

		By("Creating a marooned pod with specific resource requests")
		pod := builders.NewMaroonedPodWithResources(podName, ns, cpuRequest, memoryRequest)
		_, err := f.CreatePod(pod)
		Expect(err).ToNot(HaveOccurred())

		By("Waiting for VMI to be created")
		vmi, err := f.WaitForVMI(ns, podName, testutils.DefaultTimeout)
		Expect(err).ToNot(HaveOccurred())

		By("Verifying VMI has appropriate resources")
		// The VMI should have resources >= pod requests (accounting for overhead)
		// Base VM is typically 1 CPU / 1Gi, overhead is 500m CPU / 512Mi
		// So for 2 CPU / 4Gi pod, VMI should have at least 3 CPUs (2 + 1 for overhead rounded up)
		// and at least 4.5Gi memory (4Gi + 512Mi)

		// Check CPU cores (should be >= 3)
		totalCores := vmi.Spec.Domain.CPU.Cores * vmi.Spec.Domain.CPU.Sockets * vmi.Spec.Domain.CPU.Threads
		Expect(totalCores).To(BeNumerically(">=", uint32(3)), "VMI should have at least 3 CPU cores")

		// Check memory (should be >= 4608Mi which is 4.5Gi)
		guestMemory := vmi.Spec.Domain.Resources.Requests.Memory()
		minMemory := resource.MustParse("4608Mi") // 4.5Gi
		Expect(guestMemory.Cmp(minMemory)).To(BeNumerically(">=", 0), "VMI memory should be at least 4.5Gi")
	})

	It("should use minimum base resources for small pods", func() {
		podName := "test-small-pod"
		cpuRequest := "100m"
		memoryRequest := "128Mi"

		By("Creating a marooned pod with minimal resource requests")
		pod := builders.NewMaroonedPodWithResources(podName, ns, cpuRequest, memoryRequest)
		_, err := f.CreatePod(pod)
		Expect(err).ToNot(HaveOccurred())

		By("Waiting for VMI to be created")
		vmi, err := f.WaitForVMI(ns, podName, testutils.DefaultTimeout)
		Expect(err).ToNot(HaveOccurred())

		By("Verifying VMI uses base minimum resources")
		// Even though pod requests are tiny, VMI should have base resources
		// Base is typically 1 CPU / 1Gi minimum
		totalCores := vmi.Spec.Domain.CPU.Cores * vmi.Spec.Domain.CPU.Sockets * vmi.Spec.Domain.CPU.Threads
		Expect(totalCores).To(BeNumerically(">=", uint32(1)), "VMI should have at least 1 CPU core")

		guestMemory := vmi.Spec.Domain.Resources.Requests.Memory()
		minMemory := resource.MustParse("1Gi")
		Expect(guestMemory.Cmp(minMemory)).To(BeNumerically(">=", 0), "VMI memory should be at least 1Gi")
	})

	It("should handle pods without resource requests", func() {
		podName := "test-no-resources"

		By("Creating a marooned pod without resource requests")
		pod := builders.NewMaroonedPod(podName, ns)
		_, err := f.CreatePod(pod)
		Expect(err).ToNot(HaveOccurred())

		By("Waiting for VMI to be created")
		vmi, err := f.WaitForVMI(ns, podName, testutils.DefaultTimeout)
		Expect(err).ToNot(HaveOccurred())

		By("Verifying VMI uses default base resources")
		totalCores := vmi.Spec.Domain.CPU.Cores * vmi.Spec.Domain.CPU.Sockets * vmi.Spec.Domain.CPU.Threads
		Expect(totalCores).To(BeNumerically(">=", uint32(1)))

		// Should still be able to reach running state
		err = f.WaitForVMIPhase(ns, podName, virtv1.Running, testutils.LongTimeout)
		Expect(err).ToNot(HaveOccurred())
	})
})
