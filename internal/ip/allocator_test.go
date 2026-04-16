package ip_test

import (
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/Kwutzke/holepunch/internal/ip"
)

func TestAllocateDeterministic(t *testing.T) {
	t.Parallel()

	a := ip.New()
	ip1, err := a.Allocate("dev/opensearch")
	require.NoError(t, err)

	ip2, err := a.Allocate("dev/opensearch")
	require.NoError(t, err)

	assert.True(t, ip1.Equal(ip2), "same key must return same IP: got %v and %v", ip1, ip2)
}

func TestAllocateUniqueness(t *testing.T) {
	t.Parallel()

	a := ip.New()
	seen := make(map[string]string) // IP string -> key

	for i := range 50 {
		key := fmt.Sprintf("profile%d/service%d", i/5, i%5)
		allocated, err := a.Allocate(key)
		require.NoError(t, err, "key: %s", key)

		ipStr := allocated.String()
		if existing, ok := seen[ipStr]; ok {
			t.Fatalf("IP collision: %s assigned to both %q and %q", ipStr, existing, key)
		}
		seen[ipStr] = key
	}
}

func TestAllocateRange(t *testing.T) {
	t.Parallel()

	a := ip.New()

	for i := range 100 {
		key := fmt.Sprintf("p/s%d", i)
		allocated, err := a.Allocate(key)
		require.NoError(t, err)

		assert.True(t, allocated[len(allocated)-4] == 127, "first octet must be 127")
		assert.True(t, allocated[len(allocated)-3] == 0, "second octet must be 0")
		assert.True(t, allocated[len(allocated)-2] == 0, "third octet must be 0")

		lastOctet := allocated[len(allocated)-1]
		assert.True(t, lastOctet >= 2 && lastOctet <= 254, "last octet must be 2-254, got %d for key %s", lastOctet, key)
	}
}

func TestAllocateExhaustion(t *testing.T) {
	t.Parallel()

	a := ip.New()
	// Fill all 253 addresses.
	for i := range 253 {
		_, err := a.Allocate(fmt.Sprintf("key%d", i))
		require.NoError(t, err, "allocation %d should succeed", i)
	}

	// The 254th should fail.
	_, err := a.Allocate("overflow")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exhausted")
}

func TestAllocateRelease(t *testing.T) {
	t.Parallel()

	a := ip.New()
	allocated, err := a.Allocate("dev/redis")
	require.NoError(t, err)

	a.Release("dev/redis")

	// After release, the key should get a fresh allocation (same hash, same IP).
	reallocated, err := a.Allocate("dev/redis")
	require.NoError(t, err)
	assert.True(t, allocated.Equal(reallocated), "re-allocation should return same IP")
}

func TestAllocateAssigned(t *testing.T) {
	t.Parallel()

	a := ip.New()
	_, err := a.Allocate("dev/opensearch")
	require.NoError(t, err)
	_, err = a.Allocate("prod/rds")
	require.NoError(t, err)

	assigned := a.Assigned()
	assert.Len(t, assigned, 2)
	assert.Contains(t, assigned, "dev/opensearch")
	assert.Contains(t, assigned, "prod/rds")
}

func TestAllocateConcurrent(t *testing.T) {
	t.Parallel()

	a := ip.New()
	var wg sync.WaitGroup

	results := make([]net.IP, 50)
	errors := make([]error, 50)

	for i := range 50 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errors[idx] = a.Allocate(fmt.Sprintf("concurrent/svc%d", idx))
		}(i)
	}
	wg.Wait()

	for i, err := range errors {
		require.NoError(t, err, "concurrent allocation %d failed", i)
	}

	// Verify all IPs are unique.
	seen := make(map[string]bool)
	for i, allocated := range results {
		ipStr := allocated.String()
		assert.False(t, seen[ipStr], "duplicate IP %s at index %d", ipStr, i)
		seen[ipStr] = true
	}
}
