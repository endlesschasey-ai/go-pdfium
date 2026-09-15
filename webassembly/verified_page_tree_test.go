package webassembly

import (
	"bytes"
	"errors"
	"testing"

	impl "github.com/klippa-app/go-pdfium/internal/implementation_webassembly"
)

func TestVerifiedOpenRequiresExport(t *testing.T) {
	// Rename only the verified export in a private binary copy. The packaged
	// engine provides the export, so the unsupported-engine control must remove
	// that capability explicitly while preserving a valid ordinary engine.
	export := []byte("FPDF_LoadCustomDocumentVerified")
	if bytes.Count(pdfiumWasm, export) != 1 {
		t.Fatal("expected one verified export name in packaged WASM")
	}
	withoutVerified := bytes.Replace(pdfiumWasm, export, []byte("FPDF_LoadCustomDocumentVerifieX"), 1)
	pool, err := Init(Config{WASM: withoutVerified, MinIdle: 0, MaxIdle: 0, MaxTotal: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	instance, err := pool.GetInstanceWithContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	_, err = OpenDocumentVerified(instance, VerifiedOpenRequest{Reader: bytes.NewReader([]byte("pdf")), Size: 3, MaxDepth: 256, MaxVisits: 1000000, MaxPages: 1000000})
	if !errors.Is(err, ErrVerifiedOpenUnsupported) {
		t.Fatalf("missing export: %v", err)
	}
	impl.FileReaders.Mutex.Lock()
	defer impl.FileReaders.Mutex.Unlock()
	if len(impl.FileReaders.Refs) != 0 {
		t.Fatal("missing export retained source registration")
	}
}
