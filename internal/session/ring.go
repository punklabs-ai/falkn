package session

import "sync"

type byteRing struct {
	mu       sync.RWMutex
	data     []byte
	capacity int
}

func newByteRing(capacity int) *byteRing {
	return &byteRing{capacity: capacity}
}

func (r *byteRing) Write(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(p) >= r.capacity {
		r.data = append(r.data[:0], p[len(p)-r.capacity:]...)
		return
	}

	overflow := len(r.data) + len(p) - r.capacity
	if overflow > 0 {
		copy(r.data, r.data[overflow:])
		r.data = r.data[:len(r.data)-overflow]
	}
	r.data = append(r.data, p...)
}

func (r *byteRing) Bytes() []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]byte(nil), r.data...)
}
