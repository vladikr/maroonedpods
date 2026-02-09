package tests

import (
	"flag"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"maroonedpods.io/maroonedpods/tests/flags"
	"maroonedpods.io/maroonedpods/tests/framework"
)

func TestTests(t *testing.T) {
	flags.NormalizeFlags()
	flag.Parse()

	RegisterFailHandler(Fail)
	RunSpecs(t, "MaroonedPods E2E Test Suite")
}

var _ = BeforeSuite(func() {
	framework.InitFramework()
})

var _ = AfterSuite(func() {
	// Cleanup if needed
})
