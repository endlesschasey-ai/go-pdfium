package implementation_webassembly

import (
	"bytes"
	"context"
	"errors"
	"testing"

	pdfErrors "github.com/klippa-app/go-pdfium/errors"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

func leb(v uint32) []byte {
	var b []byte
	for {
		a := byte(v & 127)
		v >>= 7
		if v != 0 {
			a |= 128
		}
		b = append(b, a)
		if v == 0 {
			return b
		}
	}
}
func wstr(s string) []byte { return append(leb(uint32(len(s))), []byte(s)...) }
func iconst(v uint32) []byte {
	b := leb(v)
	if b[len(b)-1]&64 != 0 {
		b = append(b, 0)
	}
	return append([]byte{0x41}, b...)
}
func section(id byte, b []byte) []byte {
	return append(append([]byte{id}, leb(uint32(len(b)))...), b...)
}

// Tiny ABI failure stub, not a PDF parser or evidence of verified-open success.
func failureABI(status, handle uint32) []byte {
	out := []byte{0, 97, 115, 109, 1, 0, 0, 0}
	types := []byte{5, 0x60, 6, 0x7f, 0x7f, 0x7f, 0x7f, 0x7f, 0x7f, 1, 0x7f, 0x60, 2, 0x7f, 0x7f, 1, 0x7f, 0x60, 0, 1, 0x7f, 0x60, 1, 0x7f, 0, 0x60, 1, 0x7f, 1, 0x7f}
	out = append(out, section(1, types)...)
	out = append(out, section(3, []byte{6, 0, 1, 2, 3, 4, 3})...)
	out = append(out, section(5, []byte{1, 0, 2})...)
	out = append(out, section(6, []byte{1, 0x7f, 1, 0x41, 0xc0, 0, 0x0b})...)
	exports := []byte{7}
	for i, name := range []string{"FPDF_LoadCustomDocumentVerified", "FPDF_FILEACCESS_Create", "FPDF_GetLastError", "FPDF_CloseDocument", "malloc", "free"} {
		exports = append(exports, wstr(name)...)
		exports = append(exports, 0, byte(i))
	}
	exports = append(exports, wstr("memory")...)
	exports = append(exports, 2, 0)
	out = append(out, section(7, exports)...)
	load := []byte{0, 0x20, 5}
	load = append(load, iconst(status)...)
	load = append(load, 0x36, 2, 0)
	load = append(load, iconst(handle)...)
	load = append(load, 0x0b)
	create := append([]byte{0}, iconst(512)...)
	create = append(create, 0x0b)
	last := []byte{0, 0x41, 4, 0x0b}
	close := []byte{0, 0x41, 0, 0x41, 1, 0x36, 2, 0, 0x0b}
	malloc := []byte{1, 1, 0x7f, 0x23, 0, 0x22, 1, 0x20, 0, 0x6a, 0x24, 0, 0x20, 1, 0x0b}
	free := []byte{0, 0x41, 4, 0x41, 4, 0x28, 2, 0, 0x41, 1, 0x6a, 0x36, 2, 0, 0x0b}
	bodies := []byte{6}
	for _, body := range [][]byte{load, create, last, close, malloc, free} {
		bodies = append(bodies, leb(uint32(len(body)))...)
		bodies = append(bodies, body...)
	}
	return append(out, section(10, bodies)...)
}
func abiImplementation(t *testing.T, status, handle uint32) (*PdfiumImplementation, api.Module) {
	t.Helper()
	ctx := context.Background()
	runtime := wazero.NewRuntime(ctx)
	t.Cleanup(func() { runtime.Close(ctx) })
	mod, err := runtime.Instantiate(ctx, failureABI(status, handle))
	if err != nil {
		t.Fatal(err)
	}
	p := GetInstance(ctx, map[string]api.Function{"malloc": mod.ExportedFunction("malloc"), "free": mod.ExportedFunction("free")}, mod)
	return p, mod
}
func TestVerifiedOpenPreservesParserErrorAndReleasesReader(t *testing.T) {
	p, mod := abiImplementation(t, 12, 0)
	_, err := p.OpenDocumentVerified(VerifiedOpenRequest{Reader: bytes.NewReader([]byte("abc")), Size: 3, MaxDepth: 256, MaxVisits: 1000000, MaxPages: 1000000})
	if !errors.Is(err, pdfErrors.ErrPassword) {
		t.Fatalf("parser cause lost: %v", err)
	}
	if len(p.fileReaders) != 0 || len(p.documentRefs) != 0 {
		t.Fatal("failed open retained source/document")
	}
	frees, _ := mod.Memory().ReadUint32Le(4)
	if frees < 3 {
		t.Fatalf("free calls=%d, expected param/access/result release", frees)
	}
}

func TestVerifiedOpenClosesFailureHandle(t *testing.T) {
	p, mod := abiImplementation(t, 4, 700)
	_, err := p.OpenDocumentVerified(VerifiedOpenRequest{Reader: bytes.NewReader([]byte("abc")), Size: 3, MaxDepth: 256, MaxVisits: 1000000, MaxPages: 1000000})
	if err == nil {
		t.Fatal("invalid tree accepted")
	}
	closed, _ := mod.Memory().ReadUint32Le(0)
	if closed != 1 {
		t.Fatal("failure document not closed")
	}
	if len(p.fileReaders) != 0 || len(p.documentRefs) != 0 {
		t.Fatal("failed open retained source/document")
	}
}

func TestVerifiedOpenReadPolicy(t *testing.T) {
	for _, tt := range []struct{ size, want uint64 }{{1, 64 << 20}, {4 << 20, 128 << 20}, {1 << 30, 16 << 30}, {^uint64(0), 16 << 30}} {
		b, e := verifiedReadLimits(tt.size, 0, 0)
		if e != nil || b.MaxBytes != tt.want || b.MaxCallbacks != 1000000 {
			t.Fatalf("size=%d budget=%+v error=%v", tt.size, b, e)
		}
	}
	if _, e := verifiedReadLimits(1, 64<<20|1, 0); !errors.Is(e, ErrReadBudget) {
		t.Fatalf("raised byte limit: %v", e)
	}
	if _, e := verifiedReadLimits(1, 0, 1000001); !errors.Is(e, ErrReadBudget) {
		t.Fatalf("raised callback limit: %v", e)
	}
}
