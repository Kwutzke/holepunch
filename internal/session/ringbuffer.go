package session

import "sync"

// ringBuffer is a bounded, thread-safe io.Writer that retains the most
// recently written bytes up to a fixed capacity. Older bytes are discarded
// as newer ones arrive. It exists to capture a bounded tail of a child
// process's stderr for post-exit inspection without risking unbounded memory.
type ringBuffer struct {
	mu   sync.Mutex
	buf  []byte
	size int
	full bool
	pos  int
}

func newRingBuffer(capacity int) *ringBuffer {
	return &ringBuffer{buf: make([]byte, capacity), size: capacity}
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(p)
	if n >= r.size {
		copy(r.buf, p[n-r.size:])
		r.pos = 0
		r.full = true
		return n, nil
	}
	end := r.pos + n
	if end <= r.size {
		copy(r.buf[r.pos:], p)
	} else {
		first := r.size - r.pos
		copy(r.buf[r.pos:], p[:first])
		copy(r.buf, p[first:])
		r.full = true
	}
	r.pos = end % r.size
	if end >= r.size {
		r.full = true
	}
	return n, nil
}

// Bytes returns a copy of the buffered content in write order.
func (r *ringBuffer) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		out := make([]byte, r.pos)
		copy(out, r.buf[:r.pos])
		return out
	}
	out := make([]byte, r.size)
	copy(out, r.buf[r.pos:])
	copy(out[r.size-r.pos:], r.buf[:r.pos])
	return out
}
