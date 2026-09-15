package implementation_webassembly

import (
	"errors"
	"sync"
)

var ErrReadBudget = errors.New("verified PDF source read budget exhausted")

// FileReadBudget is cumulative from the first verified-open callback until the
// document closes. A failure is sticky; subsequent successful reads cannot hide it.
// Limits are immutable after registration. Attempts resolving to this source are
// charged before range validation, including rejected attempts; counters saturate.
type FileReadBudget struct {
	MaxBytes, MaxCallbacks uint64
	mu                     sync.Mutex
	bytes, callbacks       uint64
	err                    error
}

func (b *FileReadBudget) Err() error { b.mu.Lock(); defer b.mu.Unlock(); return b.err }
func (b *FileReadBudget) Fail(err error) error {
	if b == nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err == nil {
		b.err = err
	}
	return b.err
}
func (b *FileReadBudget) Charge(size uint64) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.callbacks < ^uint64(0) {
		b.callbacks++
	}
	if size > ^uint64(0)-b.bytes {
		b.bytes = ^uint64(0)
	} else {
		b.bytes += size
	}
	if b.err == nil && (b.callbacks > b.MaxCallbacks || b.bytes > b.MaxBytes) {
		b.err = ErrReadBudget
	}
	return b.err
}
