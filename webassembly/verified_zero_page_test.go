package webassembly_test

import (
	"bytes"
	"fmt"
	"testing"

	impl "github.com/klippa-app/go-pdfium/internal/implementation_webassembly"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
)

func TestVerifiedEmptyTreeRetainsZeroCount(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	a := b.Len()
	b.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	c := b.Len()
	b.WriteString("2 0 obj\n<< /Type /Pages /Kids [] /Count 0 >>\nendobj\n")
	x := b.Len()
	fmt.Fprintf(&b, "xref\n0 3\n0000000000 65535 f \n%010d 00000 n \n%010d 00000 n \ntrailer\n<< /Size 3 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", a, c, x)
	p, e := webassembly.Init(webassembly.Config{MaxTotal: 1, MaxIdle: 0})
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	i, e := p.GetInstanceWithContext(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	defer i.Close()
	d, e := webassembly.OpenDocumentVerified(i, webassembly.VerifiedOpenRequest{Reader: bytes.NewReader(b.Bytes()), Size: int64(b.Len()), MaxDepth: 256, MaxVisits: 1000000, MaxPages: 1000000})
	if e != nil {
		t.Fatalf("zero page verified open: %T %v", e, e)
	}
	if d.PageCount != 0 {
		t.Fatalf("count=%d", d.PageCount)
	}
	count, e := i.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: d.Document})
	if e != nil || count.PageCount != 0 {
		t.Fatalf("ordinary count=%+v err=%v", count, e)
	}
	if _, e := i.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: d.Document}); e != nil {
		t.Fatal(e)
	}
	impl.FileReaders.Mutex.Lock()
	n := len(impl.FileReaders.Refs)
	impl.FileReaders.Mutex.Unlock()
	if n != 0 {
		t.Fatalf("closed empty document retained %d readers", n)
	}
}
