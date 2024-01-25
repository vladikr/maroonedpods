package maroonedpods_operator_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestMaroonedPodsOperator(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "MaroonedPodsOperator Suite")
}
