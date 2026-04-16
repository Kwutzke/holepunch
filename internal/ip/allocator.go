package ip

import (
	"fmt"
	"hash/fnv"
	"net"
	"sync"
)

const (
	minOctet  = 2                       // skip 127.0.0.0 (network) and 127.0.0.1 (standard loopback)
	maxOctet  = 254                     // skip 127.0.0.255 (broadcast)
	rangeSize = maxOctet - minOctet + 1 // 253 usable addresses
)

// Allocator assigns deterministic loopback IPs from the 127.0.0.x range.
type Allocator struct {
	mu       sync.Mutex
	assigned map[byte]string // octet -> key that owns it
}

func New() *Allocator {
	return &Allocator{
		assigned: make(map[byte]string),
	}
}

// Allocate returns a deterministic 127.0.0.x IP for the given key.
// The key should be "profile/service_name" for uniqueness.
// Repeated calls with the same key return the same IP.
// Collisions are resolved via linear probing.
func (a *Allocator) Allocate(key string) (net.IP, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Check if this key already has an allocation.
	for octet, owner := range a.assigned {
		if owner == key {
			return net.IPv4(127, 0, 0, octet), nil
		}
	}

	if len(a.assigned) >= rangeSize {
		return nil, fmt.Errorf("IP range exhausted: all %d addresses in 127.0.0.2-254 are assigned", rangeSize)
	}

	startOctet := hashToOctet(key)
	octet := startOctet
	for {
		if _, taken := a.assigned[octet]; !taken {
			a.assigned[octet] = key
			return net.IPv4(127, 0, 0, octet), nil
		}
		octet = nextOctet(octet)
		if octet == startOctet {
			return nil, fmt.Errorf("IP range exhausted: linear probe wrapped around")
		}
	}
}

// Release frees the IP assigned to the given key.
func (a *Allocator) Release(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for octet, owner := range a.assigned {
		if owner == key {
			delete(a.assigned, octet)
			return
		}
	}
}

// Assigned returns a copy of current allocations (key -> IP).
func (a *Allocator) Assigned() map[string]net.IP {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := make(map[string]net.IP, len(a.assigned))
	for octet, key := range a.assigned {
		result[key] = net.IPv4(127, 0, 0, octet)
	}
	return result
}

func hashToOctet(key string) byte {
	h := fnv.New32a()
	h.Write([]byte(key))
	return byte(h.Sum32()%uint32(rangeSize)) + minOctet
}

func nextOctet(o byte) byte {
	if o >= maxOctet {
		return minOctet
	}
	return o + 1
}
