package utils

import (
	"fmt"
	"math/rand"
	"time"
)

const (
	// DefaultTimeout for operations
	DefaultTimeout = 5 * time.Minute
	// ShortTimeout for quick operations
	ShortTimeout = 30 * time.Second
	// LongTimeout for slow operations
	LongTimeout = 10 * time.Minute
)

// GenerateTestName generates a unique name for test resources
func GenerateTestName(prefix string) string {
	rand.Seed(time.Now().UnixNano())
	return fmt.Sprintf("%s-%d", prefix, rand.Int31())
}

// GenerateNamespaceName generates a unique namespace name for tests
func GenerateNamespaceName(prefix string) string {
	return GenerateTestName(prefix)
}
