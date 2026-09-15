package implementation_webassembly

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	pdfiumErrors "github.com/klippa-app/go-pdfium/errors"
	"github.com/tetratelabs/wazero/api"

	"github.com/klippa-app/go-pdfium/references"
)

var ErrVerifiedOpenUnsupported = errors.New("verified PDF open is not supported by this WASM instance")

type VerifiedOpenRequest struct {
	Reader                        io.ReadSeeker
	Size                          int64
	Password                      *string
	MaxDepth, MaxVisits, MaxPages uint32
	// Zero uses the process policy ceiling; positive values can only lower it.
	MaxReadBytes, MaxReadCallbacks uint64
}
type VerifiedOpenResponse struct {
	Document                      references.FPDF_DOCUMENT
	PageCount, Visits, Recoveries uint32
	// ReadError returns the sticky source failure, including later document reads.
	// The source budget survives through standard operations until document close.
	ReadError func() error
}

func (p *PdfiumImplementation) OpenDocumentVerified(request VerifiedOpenRequest) (*VerifiedOpenResponse, error) {
	p.Lock()
	defer p.Unlock()
	loader := p.Module.ExportedFunction("FPDF_LoadCustomDocumentVerified")
	if loader == nil {
		return nil, ErrVerifiedOpenUnsupported
	}
	definition := loader.Definition()
	if len(definition.ParamTypes()) != 6 || len(definition.ResultTypes()) != 1 {
		return nil, ErrVerifiedOpenUnsupported
	}
	for _, kind := range append(append([]api.ValueType{}, definition.ParamTypes()...), definition.ResultTypes()...) {
		if kind != api.ValueTypeI32 {
			return nil, ErrVerifiedOpenUnsupported
		}
	}
	if request.Reader == nil || request.Size <= 0 || uint64(request.Size) > math.MaxUint32 || request.MaxDepth == 0 || request.MaxVisits == 0 || request.MaxPages == 0 {
		return nil, fmt.Errorf("verified open requires a reader, WASM32 source size and positive tree limits")
	}
	if request.Password != nil && (strings.IndexByte(*request.Password, 0) >= 0 || uint64(len(*request.Password)) >= math.MaxUint32) {
		return nil, fmt.Errorf("invalid verified-open password")
	}
	budget, err := verifiedReadLimits(uint64(request.Size), request.MaxReadBytes, request.MaxReadCallbacks)
	if err != nil {
		return nil, err
	}
	var password uint64
	if request.Password != nil {
		value, err := p.CString(*request.Password)
		if err != nil {
			return nil, err
		}
		password = value.Pointer
		defer func() { _ = p.Free(password) }()
	}
	access, index, err := p.CreateFileAccessReader(request.Size, request.Reader)
	if err != nil {
		return nil, err
	}
	p.fileReaders[*index].Budget = budget
	retained := false
	defer func() {
		if !retained {
			p.releaseFileReader(*index)
		}
	}()
	resultPtr, err := p.Malloc(16)
	if err != nil {
		return nil, err
	}
	if resultPtr == 0 {
		return nil, fmt.Errorf("verified result allocation failed")
	}
	defer func() { _ = p.Free(resultPtr) }()
	result, callErr := loader.Call(p.Context, *access, password, uint64(request.MaxDepth), uint64(request.MaxVisits), uint64(request.MaxPages), resultPtr)
	var handle uint64
	if len(result) == 1 {
		handle = result[0]
	}
	// A misbehaving loader returning a handle with failure must not leak it.
	defer func() {
		if !retained && handle != 0 {
			if closeFn := p.Module.ExportedFunction("FPDF_CloseDocument"); closeFn != nil {
				_, _ = closeFn.Call(p.Context, handle)
			}
		}
	}()
	if err := budget.Err(); err != nil {
		return nil, &VerifiedOpenError{Cause: err}
	}
	if callErr != nil {
		return nil, &VerifiedOpenError{Cause: callErr}
	}
	data, ok := p.Module.Memory().Read(uint32(resultPtr), 16)
	if !ok {
		return nil, &VerifiedOpenError{Cause: fmt.Errorf("verified result outside guest memory")}
	}
	status := binary.LittleEndian.Uint32(data)
	count := binary.LittleEndian.Uint32(data[4:])
	visits := binary.LittleEndian.Uint32(data[8:])
	recoveries := binary.LittleEndian.Uint32(data[12:])
	if status != 0 || handle == 0 {
		cause := p.parserOpenError()
		if cause == nil {
			cause = fmt.Errorf("page-tree verification failed")
		}
		return nil, &VerifiedOpenError{Status: status, Cause: cause}
	}
	if count > request.MaxPages || visits > request.MaxVisits {
		return nil, &VerifiedOpenError{Cause: fmt.Errorf("inconsistent verified result")}
	}
	doc := p.registerDocument(&handle)
	doc.fileHandleRef = index
	retained = true
	return &VerifiedOpenResponse{Document: doc.nativeRef, PageCount: count, Visits: visits, Recoveries: recoveries, ReadError: budget.Err}, nil
}

// VerifiedOpenError preserves both the C verifier status and the original parser
// or sticky source cause. Status 0 with an error represents a host/ABI failure.
type VerifiedOpenError struct {
	Status uint32
	Cause  error
}

func (e *VerifiedOpenError) Error() string {
	return fmt.Sprintf("verified PDF open status=%d: %v", e.Status, e.Cause)
}
func (e *VerifiedOpenError) Unwrap() error { return e.Cause }

func verifiedReadLimits(size uint64, bytes, callbacks uint64) (*FileReadBudget, error) {
	const ceiling = uint64(16 << 30)
	limit := ceiling
	if size <= ceiling/32 {
		limit = max(uint64(64<<20), size*32)
	}
	if bytes == 0 {
		bytes = limit
	}
	if callbacks == 0 {
		callbacks = 1000000
	}
	if bytes > limit || callbacks > 1000000 {
		return nil, fmt.Errorf("verified read limits exceed policy: %w", ErrReadBudget)
	}
	return &FileReadBudget{MaxBytes: bytes, MaxCallbacks: callbacks}, nil
}

func (p *PdfiumImplementation) parserOpenError() error {
	f := p.Module.ExportedFunction("FPDF_GetLastError")
	if f == nil {
		return ErrVerifiedOpenUnsupported
	}
	result, err := f.Call(p.Context)
	if err != nil {
		return err
	}
	if len(result) != 1 {
		return fmt.Errorf("invalid FPDF_GetLastError result")
	}
	switch FPDF_ERR(result[0]) {
	case FPDF_ERR_SUCCESS:
		return nil
	case FPDF_ERR_UNKNOWN:
		return pdfiumErrors.ErrUnknown
	case FPDF_ERR_FILE:
		return pdfiumErrors.ErrFile
	case FPDF_ERR_FORMAT:
		return pdfiumErrors.ErrFormat
	case FPDF_ERR_PASSWORD:
		return pdfiumErrors.ErrPassword
	case FPDF_ERR_SECURITY:
		return pdfiumErrors.ErrSecurity
	case FPDF_ERR_PAGE:
		return pdfiumErrors.ErrPage
	default:
		return pdfiumErrors.ErrUnexpected
	}
}
