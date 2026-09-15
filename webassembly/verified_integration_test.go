package webassembly_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/tetratelabs/wazero"
)

// PDFIUM_VERIFIED_WASM selects the independently built artifact; absent means
// test the embedded old artifact, which must fail this acceptance suite.
func TestVerifiedFixtureMatrix(t *testing.T) {
	var wasm []byte
	if path := os.Getenv("PDFIUM_VERIFIED_WASM"); path != "" {
		var err error
		wasm, err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	pool, err := webassembly.Init(webassembly.Config{WASM: wasm, MaxTotal: 1, MaxIdle: 0, MinIdle: 0, ReuseWorkers: true, FSConfig: wazero.NewFSConfig(), RuntimeConfig: wazero.NewRuntimeConfig().WithMemoryLimitPages(8192).WithCloseOnContextDone(true)})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	cases := []struct {
		name   string
		status uint32
		pages  int
	}{
		{"rebuilt-valid-object-stream.pdf", 0, 1}, {"rebuilt-broken-tree-filter-raw-fallback.pdf", 10, 0},
		{"valid-compressed-xref.pdf", 0, 1}, {"truncated-compressed-xref.pdf", 10, 0},
		{"negative-truncated-count.pdf", 3, 0}, {"positive-truncated-count.pdf", 3, 0}, {"exact-256-zero-count.pdf", 0, 1},
		{"negative-overflow.pdf", 3, 0}, {"positive-overflow.pdf", 3, 0}, {"zero-empty-branch.pdf", 0, 1},
		{"encrypted-object-stream-normal-xref.pdf", 0, 1}, {"encrypted-object-stream-rebuilt-xref.pdf", 0, 1},
		{"empty-filter-array.pdf", 0, 1},
		{"null-filter.pdf", 0, 1}, {"unresolved-optional-filter.pdf", 0, 1}, {"nonzero-first-page-mapping.pdf", 0, 3},
		{"valid-ascii85-object-stream.pdf", 0, 1}, {"truncated-ascii85-object-stream.pdf", 10, 0},
		{"valid-hex-object-stream.pdf", 0, 1}, {"truncated-hex-object-stream.pdf", 10, 0},
		{"valid-runlength-object-stream.pdf", 0, 1}, {"truncated-runlength-object-stream.pdf", 10, 0},
		{"valid-lzw-object-stream.pdf", 0, 1}, {"truncated-lzw-object-stream.pdf", 10, 0},
		{"valid-object-stream.pdf", 0, 1}, {"valid-uncompressed-object-stream.pdf", 0, 1},
		{"broken-tree-object-stream.pdf", 10, 0}, {"broken-tree-filter-raw-fallback.pdf", 10, 0},
		{"truncated-tree-object-stream.pdf", 10, 0}, {"forged-root-count.pdf", 4, 0},
		{"interior-count-mismatch.pdf", 4, 0}, {"cycle.pdf", 5, 0},
		{"zero-count-nonempty.pdf", 4, 0}, {"missing-count.pdf", 0, 1},
		{"missing-type.pdf", 0, 1}, {"unknown-type.pdf", 0, 1},
		{"fractional-count.pdf", 3, 0}, {"negative-count.pdf", 3, 0}, {"string-count.pdf", 3, 0},
		{"page-with-kids.pdf", 2, 0}, {"duplicate-leaf.pdf", 6, 0}, {"direct-child.pdf", 0, 1},
		{"depth-257.pdf", 7, 0}, {"valid-linearized.pdf", 0, 3},
		{"linearized-count-mismatch.pdf", 0, 3}, {"linearized-first-page-mismatch.pdf", 0, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			instance, err := pool.GetInstanceWithContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var f *os.File
			defer func() {
				_ = instance.Close()
				if f != nil {
					_ = f.Close()
				}
			}()
			// Force active calls to stop at the same operation deadline. Close retains
			// ownership until the synchronous call and any host callback have returned.
			stopped := make(chan struct{})
			stop := context.AfterFunc(ctx, func() { _ = webassembly.Interrupt(instance); close(stopped) })
			defer func() {
				if !stop() {
					<-stopped
				}
			}()
			f, err = os.Open(filepath.Join("testdata/verified", tc.name))
			if err != nil {
				t.Fatal(err)
			}
			info, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			var password *string
			if tc.name == "encrypted-object-stream-normal-xref.pdf" || tc.name == "encrypted-object-stream-rebuilt-xref.pdf" {
				value := "verified-test"
				password = &value
			}
			doc, err := webassembly.OpenDocumentVerified(instance, webassembly.VerifiedOpenRequest{Reader: f, Size: info.Size(), Password: password, MaxDepth: 256, MaxVisits: 1000000, MaxPages: 1000000})
			if tc.status != 0 {
				var failure *webassembly.VerifiedOpenError
				if !errors.As(err, &failure) || failure.Status != tc.status || doc != nil {
					t.Fatalf("wanted status %d and no document, got doc=%+v err=%v", tc.status, doc, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if int(doc.PageCount) != tc.pages {
				t.Fatalf("verified count=%d want=%d", doc.PageCount, tc.pages)
			}
			count, err := instance.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc.Document})
			if err != nil {
				t.Fatal(err)
			}
			if count.PageCount != tc.pages {
				t.Fatalf("engine count=%d want=%d", count.PageCount, tc.pages)
			}
			for i := 0; i < tc.pages; i++ {
				size, err := instance.FPDF_GetPageSizeByIndex(&requests.FPDF_GetPageSizeByIndex{Document: doc.Document, Index: i})
				if err != nil {
					t.Fatalf("page %d: %v", i, err)
				}
				if size.Width <= 0 || size.Height <= 0 {
					t.Fatalf("page %d invalid dimensions %+v", i, size)
				}
				if tc.name == "nonzero-first-page-mapping.pdf" {
					// Independent fixture inspection: leaves [1, 31, 8] have
					// annotation counts [4, 2, 6], despite identical dimensions.
					wantAnnotations := [...]int{4, 2, 6}
					page, err := instance.FPDF_LoadPage(&requests.FPDF_LoadPage{Document: doc.Document, Index: i})
					if err != nil {
						t.Fatalf("mapping page %d load: %v", i, err)
					}
					annotations, annotationErr := instance.FPDFPage_GetAnnotCount(&requests.FPDFPage_GetAnnotCount{Page: requests.Page{ByReference: &page.Page}})
					_, closeErr := instance.FPDF_ClosePage(&requests.FPDF_ClosePage{Page: page.Page})
					if annotationErr != nil {
						t.Fatalf("mapping page %d annotations: %v", i, annotationErr)
					}
					if closeErr != nil {
						t.Fatalf("mapping page %d close: %v", i, closeErr)
					}
					if annotations.Count != wantAnnotations[i] {
						t.Fatalf("mapping page %d annotations=%d want=%d", i, annotations.Count, wantAnnotations[i])
					}
				}
			}
			if err := doc.ReadError(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
