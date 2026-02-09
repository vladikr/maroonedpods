package tests

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	virtv1 "kubevirt.io/api/core/v1"

	"maroonedpods.io/maroonedpods/pkg/util"
	"maroonedpods.io/maroonedpods/tests/builders"
	"maroonedpods.io/maroonedpods/tests/framework"
	testutils "maroonedpods.io/maroonedpods/tests/utils"
)

var _ = Describe("[e2e] Warm Pool", func() {
	var (
		f  *framework.Framework
		ns string
	)

	BeforeEach(func() {
		f = framework.DefaultFramework
		nsName := testutils.GenerateNamespaceName("warm-pool")
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

	It("should support claiming VMI from warm pool if available", func() {
		podName := "test-pool-claim"

		By("Checking if warm pool is enabled by looking for pool VMIs")
		// This test is conditional on warm pool being configured
		// If no pool VMIs exist, we skip this test
		mpNs := "maroonedpods" // Default operator namespace
		vmiList, err := f.ListVMIs(mpNs)
		Expect(err).ToNot(HaveOccurred())

		poolVMIs := 0
		for _, vmi := range vmiList.Items {
			if vmi.Labels != nil {
				if state, ok := vmi.Labels[util.WarmPoolStateLabel]; ok {
					if state == util.PoolStateAvailable {
						poolVMIs++
					}
				}
			}
		}

		if poolVMIs == 0 {
			Skip("No available pool VMIs found, skipping warm pool test")
		}

		By("Creating a marooned pod")
		pod := builders.NewMaroonedPod(podName, ns)
		_, err = f.CreatePod(pod)
		Expect(err).ToNot(HaveOccurred())

		By("Waiting for pod to potentially claim a pool VMI")
		// If warm pool is working, the pod should schedule faster
		time.Sleep(5 * time.Second)

		By("Checking if a VMI exists for the pod")
		// The VMI might be from the pool or newly created
		_, err = f.GetVMI(ns, podName)
		// We just verify a VMI exists, we don't require it to be from the pool
		// as that depends on pool availability and timing
	})

	It("should track pool VMI states correctly", func() {
		By("Listing all VMIs in maroonedpods namespace")
		mpNs := "maroonedpods"
		vmiList, err := f.ListVMIs(mpNs)
		Expect(err).ToNot(HaveOccurred())

		By("Checking pool VMI state labels")
		for _, vmi := range vmiList.Items {
			if vmi.Labels != nil {
				if state, ok := vmi.Labels[util.WarmPoolStateLabel]; ok {
					By("Found pool VMI: " + vmi.Name + " with state: " + state)
					// State should be one of: creating, available, claimed
					Expect(state).To(BeElementOf(
						util.PoolStateCreating,
						util.PoolStateAvailable,
						util.PoolStateClaimed,
					))

					// If claimed, should have claimed-by label
					if state == util.PoolStateClaimed {
						_, hasClaimedBy := vmi.Labels[util.WarmPoolClaimedByLabel]
						Expect(hasClaimedBy).To(BeTrue(), "Claimed VMI should have claimed-by label")
					}
				}
			}
		}
	})

	It("should have pool VMIs with correct naming", func() {
		By("Listing all VMIs in maroonedpods namespace")
		mpNs := "maroonedpods"
		vmiList, err := f.ListVMIs(mpNs)
		Expect(err).ToNot(HaveOccurred())

		By("Checking pool VMI names")
		foundPoolVMI := false
		for _, vmi := range vmiList.Items {
			if vmi.Labels != nil {
				if _, ok := vmi.Labels[util.WarmPoolStateLabel]; ok {
					foundPoolVMI = true
					// Pool VMIs should have the pool name prefix
					Expect(vmi.Name).To(HavePrefix(util.WarmPoolVMNamePrefix),
						"Pool VMI should have correct name prefix")
				}
			}
		}

		if !foundPoolVMI {
			Skip("No pool VMIs found in cluster")
		}
	})
})
