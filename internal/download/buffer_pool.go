/*
Copyright 2024 eh-ops.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package download

import (
	"sync"
)

const (
	// DefaultBufferSize is the default buffer size for streaming (64KB).
	DefaultBufferSize = 64 * 1024
)

// BufferPool provides reusable byte buffers for memory-efficient streaming.
// Using a pool reduces memory allocations and GC pressure during large file downloads.
type BufferPool struct {
	pool    sync.Pool
	bufSize int
}

// NewBufferPool creates a new buffer pool with the specified buffer size.
// If bufSize is 0, DefaultBufferSize (64KB) is used.
func NewBufferPool(bufSize int) *BufferPool {
	if bufSize <= 0 {
		bufSize = DefaultBufferSize
	}
	return &BufferPool{
		bufSize: bufSize,
		pool: sync.Pool{
			New: func() interface{} {
				return make([]byte, bufSize)
			},
		},
	}
}

// Get retrieves a buffer from the pool. The buffer is guaranteed to be
// of the pool's configured size, but contents are not zeroed.
func (p *BufferPool) Get() []byte {
	return p.pool.Get().([]byte)
}

// Put returns a buffer to the pool for reuse. The buffer should not be
// used after being returned to the pool.
func (p *BufferPool) Put(buf []byte) {
	// Only return buffers that match our expected size
	// This prevents accidental returns of differently-sized slices
	if cap(buf) >= p.bufSize {
		p.pool.Put(buf[:p.bufSize])
	}
}

// BufferSize returns the configured buffer size for this pool.
func (p *BufferPool) BufferSize() int {
	return p.bufSize
}
