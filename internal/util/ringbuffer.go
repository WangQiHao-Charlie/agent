package util

import "sync"

// RingBuffer is a fixed-size byte ring storing last N bytes written.
type RingBuffer struct {
    mu     sync.Mutex
    buf    []byte
    size   int
    pos    int
    filled bool
}

func NewRingBuffer(size int) *RingBuffer {
    if size <= 0 {
        size = 4096
    }
    return &RingBuffer{buf: make([]byte, size), size: size}
}

func (r *RingBuffer) Write(p []byte) (int, error) {
    r.mu.Lock()
    defer r.mu.Unlock()
    n := len(p)
    if n >= r.size {
        copy(r.buf, p[n-r.size:])
        r.pos = 0
        r.filled = true
        return n, nil
    }
    remaining := r.size - r.pos
    if n <= remaining {
        copy(r.buf[r.pos:], p)
        r.pos += n
        if r.pos == r.size {
            r.pos = 0
            r.filled = true
        }
        return n, nil
    }
    // wrap around
    copy(r.buf[r.pos:], p[:remaining])
    copy(r.buf, p[remaining:])
    r.pos = n - remaining
    r.filled = true
    return n, nil
}

// Bytes returns a copy of the bytes in order.
func (r *RingBuffer) Bytes() []byte {
    r.mu.Lock()
    defer r.mu.Unlock()
    if !r.filled {
        out := make([]byte, r.pos)
        copy(out, r.buf[:r.pos])
        return out
    }
    out := make([]byte, r.size)
    copy(out, r.buf[r.pos:])
    copy(out[r.size-r.pos:], r.buf[:r.pos])
    return out
}

