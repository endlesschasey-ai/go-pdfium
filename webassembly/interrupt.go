package webassembly

import (
	"errors"

	"github.com/klippa-app/go-pdfium"
)

var errInterruptUnavailable = errors.New("instance cannot be interrupted")

// Interrupt cancels in-flight WASM execution for a webassembly pool instance.
// The pool must be created with RuntimeConfig WithCloseOnContextDone(true) or
// the cancel will not abort an active WASM call. Interrupt returns without
// waiting for that call to finish and does not synchronously destroy the
// instance; wazero's context watcher may close the module on its own. Close
// the instance after the interrupted call returns. A canceled worker is
// destroyed rather than reused.
func Interrupt(instance pdfium.Pdfium) error {
	if instance == nil {
		return errInterruptUnavailable
	}
	i, ok := instance.(*pdfiumInstance)
	if !ok {
		return errInterruptUnavailable
	}
	return i.Interrupt()
}

// Interrupt cancels this instance's worker context so an in-flight WASM call
// can abort when WithCloseOnContextDone(true) is set. It is safe to call
// concurrently with an in-flight API method and does not wait for that method
// to return. Close disables Interrupt before releasing the worker so a
// previous wrapper cannot cancel a later borrower.
func (i *pdfiumInstance) Interrupt() error {
	if i == nil {
		return errInterruptUnavailable
	}
	i.interruptMu.Lock()
	defer i.interruptMu.Unlock()
	if i.interruptCancel == nil {
		return errInterruptUnavailable
	}
	i.interrupted = true
	i.interruptCancel()
	return nil
}
