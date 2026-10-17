package webassembly

import (
	"errors"
	"fmt"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

const (
	// wasmPageSize is the WebAssembly linear-memory page size.
	wasmPageSize uint64 = 65536

	// runtimeMemoryPagesForAllocator is wazero's maximum page count (4GiB of
	// address space). It is applied only when MemoryLimitBytes is set.
	//
	// wazero clamps a module's encoded memory maximum down to
	// WithMemoryLimitPages at compile time, and MemoryInstance.Grow returns
	// false when the new page count exceeds that maximum before it calls
	// LinearMemory.Reallocate. pdfium.wasm encodes a maximum of 32,768 pages
	// (2GiB). A lower runtime cap, such as the historical 8,192 pages, therefore
	// refuses growth with no allocator record.
	//
	// 65,536 is the largest value WithMemoryLimitPages accepts, so the encoded
	// maximum is left intact and every in-range growth reaches the allocator.
	// WithMemoryCapacityFromMax stays false, so this does not allocate those
	// pages: capacity remains the module minimum.
	runtimeMemoryPagesForAllocator uint32 = 65536
)

// MemoryLimitError is a refused wasm linear-memory growth.
// Requested, Current, and Limit are byte counts. Phase is the public call
// in progress, or "instantiate" when the refusal happened while creating the
// instance. The value is a snapshot: it stays readable after the instance is
// closed.
type MemoryLimitError struct {
	Requested uint64
	Current   uint64
	Limit     uint64
	Phase     string
}

func (e *MemoryLimitError) Error() string {
	if e == nil {
		return "pdfium wasm memory limit exceeded"
	}
	return fmt.Sprintf("pdfium wasm memory limit exceeded in %s: requested %d bytes, current %d bytes, limit %d bytes", e.Phase, e.Requested, e.Current, e.Limit)
}

// memoryLimiter is one instance's backing-buffer cap. Each instance has its own
// limiter, so concurrent instances do not share refusal state.
type memoryLimiter struct {
	mu        sync.Mutex
	limit     uint64
	buf       []byte
	highWater uint64
	phase     string
	rejected  *MemoryLimitError
}

func newMemoryLimiter(limit uint64) *memoryLimiter {
	return &memoryLimiter{limit: limit, phase: "instantiate"}
}

func (m *memoryLimiter) begin(phase string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.phase = phase
	m.rejected = nil
}

func (m *memoryLimiter) finish(err error) error {
	if m == nil {
		return err
	}
	m.mu.Lock()
	snap := m.rejected
	m.rejected = nil
	m.mu.Unlock()
	if err == nil || snap == nil {
		return err
	}
	copied := *snap
	return errors.Join(err, &copied)
}

func (m *memoryLimiter) peak() uint64 {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.highWater
}

// Allocate implements experimental.MemoryAllocator.
// The suggested capacity and the module maximum are not reserved.
func (m *memoryLimiter) Allocate(_, _ uint64) experimental.LinearMemory {
	return m
}

// Reallocate implements experimental.LinearMemory.
// A request above the limit records the refusal and returns nil so wazero
// reports the growth as unsuccessful. The installed buffer is left unchanged.
func (m *memoryLimiter) Reallocate(size uint64) []byte {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if size > m.limit {
		m.note(size)
		return nil
	}
	if size > uint64(cap(m.buf)) {
		next := make([]byte, size)
		copy(next, m.buf)
		if uint64(cap(next)) > m.limit {
			m.note(size)
			return nil
		}
		m.buf = next
	} else if size > uint64(len(m.buf)) {
		old := uint64(len(m.buf))
		m.buf = m.buf[:size]
		clear(m.buf[old:size])
	} else {
		m.buf = m.buf[:size]
	}
	m.observe()
	return m.buf
}

// Free implements experimental.LinearMemory.
func (m *memoryLimiter) Free() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.buf = nil
	m.mu.Unlock()
}

func (m *memoryLimiter) note(requested uint64) {
	if m.rejected != nil {
		return
	}
	phase := m.phase
	if phase == "" {
		phase = "allocate"
	}
	m.rejected = &MemoryLimitError{
		Requested: requested,
		Current:   uint64(len(m.buf)),
		Limit:     m.limit,
		Phase:     phase,
	}
}

func (m *memoryLimiter) observe() {
	if n := uint64(len(m.buf)); n > m.highWater {
		m.highWater = n
	}
	if n := uint64(cap(m.buf)); n > m.highWater {
		m.highWater = n
	}
}

func declaredMinimumBytes(mod wazero.CompiledModule) (uint64, bool) {
	var min uint64
	found := false
	consider := func(def api.MemoryDefinition) {
		if def == nil {
			return
		}
		n := uint64(def.Min()) * wasmPageSize
		if !found || n > min {
			min = n
			found = true
		}
	}
	if mod == nil {
		return 0, false
	}
	for _, def := range mod.ExportedMemories() {
		consider(def)
	}
	for _, def := range mod.ImportedMemories() {
		consider(def)
	}
	return min, found
}
