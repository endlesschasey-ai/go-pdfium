package webassembly

import (
	"github.com/klippa-app/go-pdfium"
	impl "github.com/klippa-app/go-pdfium/internal/implementation_webassembly"
)

// VerifiedOpenRequest bounds tree validation and all source reads starting at
// the first open callback. Reader must not change and must return from Read/Seek;
// an arbitrary blocked reader cannot be forcibly canceled by WASM interruption.
type VerifiedOpenRequest = impl.VerifiedOpenRequest
type VerifiedOpenResponse = impl.VerifiedOpenResponse
type VerifiedOpenError = impl.VerifiedOpenError

var ErrVerifiedOpenUnsupported = impl.ErrVerifiedOpenUnsupported
var ErrVerifiedReadBudget = impl.ErrReadBudget

// OpenDocumentVerified is WASM-only. It never falls back to ordinary OpenDocument.
// Close the returned Document or its instance; retain it for subsequent trusted
// operations instead of reopening the source through an ordinary loader.
func OpenDocumentVerified(instance pdfium.Pdfium, request VerifiedOpenRequest) (*VerifiedOpenResponse, error) {
	i, ok := instance.(*pdfiumInstance)
	if !ok || i == nil || i.closed {
		return nil, ErrVerifiedOpenUnsupported
	}
	return i.worker.Instance.OpenDocumentVerified(request)
}
