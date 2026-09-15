package webassembly_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
)

// Tree: Pages 2 -> [Pages 3 -> Page 4, Page 5]. Four dictionary
// visits plus four indirect-reference resolutions = eight visits, and two
// leaves. /Type, /Kids and /Count are direct objects, so add no reference
// resolutions; the catalog itself is outside the page tree.
func budgetTreePDF() []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.7\n")
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R 5 0 R] /Count 2 >>",
		"<< /Type /Pages /Parent 2 0 R /Kids [4 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 3 0 R /MediaBox [0 0 300 200] >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 200] >>",
	}
	offsets := make([]int, len(objects))
	for n, body := range objects {
		offsets[n] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", n+1, body)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 6\n0000000000 65535 f \n")
	for _, offset := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref)
	return b.Bytes()
}

func TestVerifiedTreeBudgetBoundaries(t *testing.T) {
	pool := lifecyclePool(t)
	data := budgetTreePDF()
	for _, tc := range []struct {
		name                  string
		visits, pages, status uint32
	}{
		{"visits_insufficient", 7, 100, 8}, {"visits_exact", 8, 100, 0},
		{"pages_insufficient", 100, 1, 9}, {"pages_exact", 100, 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst, cleanup := lifecycleInstance(t, pool)
			defer cleanup()
			doc, err := webassembly.OpenDocumentVerified(inst, webassembly.VerifiedOpenRequest{Reader: bytes.NewReader(data), Size: int64(len(data)), MaxDepth: 256, MaxVisits: tc.visits, MaxPages: tc.pages})
			if tc.status != 0 {
				var detail *webassembly.VerifiedOpenError
				if doc != nil || !errors.As(err, &detail) || detail.Status != tc.status {
					t.Fatalf("doc=%+v err=%v; want nil and status %d", doc, err, tc.status)
				}
				return
			}
			if err != nil || doc == nil {
				t.Fatalf("exact limit open: doc=%+v err=%v", doc, err)
			}
			defer inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document})
			if doc.Visits != 8 || doc.PageCount != 2 || doc.ReadError() != nil {
				t.Fatalf("visits=%d pages=%d source=%v", doc.Visits, doc.PageCount, doc.ReadError())
			}
			count, err := inst.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc.Document})
			if err != nil || count.PageCount != 2 {
				t.Fatalf("ordinary count=%+v err=%v", count, err)
			}
		})
	}
}

// The callback seeks exactly once before its chunked reads. Count successful
// source-visible SeekStart calls, not Read calls or private budget counters.
// A rejected budget attempt is not visible here because it never reaches Seek.
// This reader is used only synchronously and never sought by the test itself.
type callbackObservedReader struct {
	*bytes.Reader
	seeks, reads, readBytes uint64
}

func (r *callbackObservedReader) Seek(offset int64, whence int) (int64, error) {
	if whence != io.SeekStart {
		return 0, fmt.Errorf("unexpected seek origin %d", whence)
	}
	r.seeks++
	return r.Reader.Seek(offset, whence)
}
func (r *callbackObservedReader) Read(p []byte) (int, error) {
	r.reads++
	n, e := r.Reader.Read(p)
	r.readBytes += uint64(n)
	return n, e
}

func TestVerifiedLifecycleCumulativeCallbackBudget(t *testing.T) {
	pool := lifecyclePool(t)
	data := lifecyclePDF()
	session := func(limit uint64, expectedOpen uint64, exhaust bool) (uint64, uint64) {
		inst, cleanup := lifecycleInstance(t, pool)
		defer cleanup()
		reader := &callbackObservedReader{Reader: bytes.NewReader(data)}
		doc, err := webassembly.OpenDocumentVerified(inst, webassembly.VerifiedOpenRequest{Reader: reader, Size: int64(len(data)), MaxDepth: 256, MaxVisits: 1000000, MaxPages: 1000000, MaxReadCallbacks: limit})
		if err != nil || doc == nil {
			t.Fatalf("callback limit %d open: doc=%+v err=%v", limit, doc, err)
		}
		defer inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document})
		opened := reader.seeks
		if expectedOpen != 0 && opened != expectedOpen {
			t.Fatalf("open callbacks=%d calibrated=%d", opened, expectedOpen)
		}
		lifecycleLoadAndClosePage(t, inst, doc, !exhaust)
		later := reader.seeks - opened
		if !exhaust {
			if doc.ReadError() != nil || opened == 0 || later == 0 {
				t.Fatalf("control source=%v callbacks open=%d later=%d", doc.ReadError(), opened, later)
			}
			// Default byte allowance is at least 64 MiB, independent of callback cap.
			if reader.readBytes >= 64<<20 {
				t.Fatalf("control bytes=%d approach byte ceiling", reader.readBytes)
			}
		} else {
			if !errors.Is(doc.ReadError(), webassembly.ErrVerifiedReadBudget) {
				t.Fatalf("later callback exhaustion missing: %v", doc.ReadError())
			}
			if reader.seeks != limit {
				t.Fatalf("source reached %d callbacks, want ceiling %d", reader.seeks, limit)
			}
			first := doc.ReadError()
			seeks, reads := reader.seeks, reader.reads
			lifecycleLoadAndClosePage(t, inst, doc, false)
			if !errors.Is(doc.ReadError(), first) || reader.seeks != seeks || reader.reads != reads {
				t.Fatalf("sticky error/source boundary lost: %v seeks=%d reads=%d", doc.ReadError(), reader.seeks, reader.reads)
			}
		}
		t.Logf("limit=%d open=%d later-source-callbacks=%d bytes=%d exhausted=%t", limit, opened, later, reader.readBytes, exhaust)
		return opened, later
	}
	opened, later := session(0, 0, false)
	budget := max(opened, later)
	if opened > budget || later > budget || opened > ^uint64(0)-later || opened+later <= budget {
		t.Fatalf("invalid calibration open=%d later=%d limit=%d", opened, later, budget)
	}
	session(budget, opened, true)
	// An explicit sufficient ceiling validates the same complete phase sequence.
	controlOpen, controlLater := session(opened+later, opened, false)
	if controlOpen != opened || controlLater != later {
		t.Fatalf("control callbacks changed: %d/%d vs %d/%d", controlOpen, controlLater, opened, later)
	}
}
