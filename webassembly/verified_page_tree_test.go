package webassembly

import (
	"bytes"
	"errors"
	"testing"

	impl "github.com/klippa-app/go-pdfium/internal/implementation_webassembly"
)

func TestVerifiedOpenRequiresExport(t *testing.T) {
	pool, err := Init(Config{MinIdle: 0, MaxIdle: 0, MaxTotal: 1})
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
