package webassembly_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"weak"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/tetratelabs/wazero"
)

// Exercise the packaged artifact or an explicitly selected candidate.
func lifecyclePool(t *testing.T) pdfium.Pool {
	t.Helper()
	path := os.Getenv("PDFIUM_VERIFIED_WASM")
	if path == "" {
		path = "pdfium.wasm"
	}
	wasm, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("artifact=%s sha256=%x", path, sha256.Sum256(wasm))
	pool, err := webassembly.Init(webassembly.Config{
		WASM: wasm, MaxTotal: 1, MaxIdle: 1, MinIdle: 0, ReuseWorkers: true,
		FSConfig:      wazero.NewFSConfig(),
		RuntimeConfig: wazero.NewRuntimeConfig().WithMemoryLimitPages(8192).WithCloseOnContextDone(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}

func lifecycleInstance(t *testing.T, pool pdfium.Pool) (pdfium.Pdfium, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	inst, err := pool.GetInstanceWithContext(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = webassembly.Interrupt(inst); close(stopped) })
	cleanup := func() {
		if !stop() {
			<-stopped
		}
		cancel()
		_ = inst.Close()
	}
	return inst, cleanup
}

// A complete xref table keeps open on the normal loader path. The large plain
// Contents stream is intentionally outside tree validation and initial read-ahead.
// Each test checks actual later reads rather than assuming PDFium stays lazy.
func lifecyclePDF() []byte {
	var out bytes.Buffer
	out.WriteString("%PDF-1.7\n")
	offsets := []int{0}
	writeObject := func(number int, body string) {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", number, body)
	}
	writeObject(1, "<< /Type /Catalog /Pages 2 0 R >>")
	writeObject(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	writeObject(3, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 200] /Resources << >> /Contents 4 0 R >>")
	content := append(bytes.Repeat([]byte(" "), 1<<20), []byte("q Q\n")...)
	writeObject(4, fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content))
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 5\n0000000000 65535 f \n")
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size 5 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref)
	return out.Bytes()
}

type lifecycleBarrier struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *lifecycleBarrier) unblock() { b.once.Do(func() { close(b.release) }) }

type lifecycleReader struct {
	*bytes.Reader
	readBytes atomic.Uint64
	mu        sync.Mutex
	barrier   *lifecycleBarrier
}

func (r *lifecycleReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	barrier := r.barrier
	r.barrier = nil
	r.mu.Unlock()
	if barrier != nil {
		close(barrier.entered)
		<-barrier.release
	}
	n, err := r.Reader.Read(p)
	r.readBytes.Add(uint64(n))
	return n, err
}

func lifecycleOpen(t *testing.T, inst pdfium.Pdfium, r *lifecycleReader, budget uint64) *webassembly.VerifiedOpenResponse {
	t.Helper()
	doc, err := webassembly.OpenDocumentVerified(inst, webassembly.VerifiedOpenRequest{
		Reader: r, Size: r.Size(), MaxDepth: 256, MaxVisits: 1000000, MaxPages: 1000000, MaxReadBytes: budget,
	})
	if errors.Is(err, webassembly.ErrVerifiedOpenUnsupported) {
		t.Fatalf("selected artifact does not support verified open; lifecycle behavior has not executed: %v", err)
	}
	if err != nil {
		t.Fatalf("real verified open: %v", err)
	}
	if doc.PageCount != 1 || doc.ReadError() != nil {
		t.Fatalf("verified count=%d source error=%v", doc.PageCount, doc.ReadError())
	}
	return doc
}

func lifecycleLoadAndClosePage(t *testing.T, inst pdfium.Pdfium, doc *webassembly.VerifiedOpenResponse, requireSuccess bool) {
	t.Helper()
	page, err := inst.FPDF_LoadPage(&requests.FPDF_LoadPage{Document: doc.Document, Index: 0})
	if requireSuccess && err != nil {
		t.Fatalf("ordinary LoadPage after verified success: %v", err)
	}
	// PDFium may produce a partial page when a content read fails; ReadError is
	// the acceptance authority for source failures, not LoadPage's return alone.
	if page != nil {
		if _, closeErr := inst.FPDF_ClosePage(&requests.FPDF_ClosePage{Page: page.Page}); closeErr != nil && requireSuccess {
			t.Fatal(closeErr)
		}
	}
}

func lifecycleReleased(t *testing.T, ref weak.Pointer[lifecycleReader]) {
	t.Helper()
	for range 3 {
		runtime.GC()
	}
	if ref.Value() != nil {
		t.Fatal("Reader still strongly reachable after completed Close and GC")
	}
}

// Keep all strong Reader locals inside this helper. Only its public weak
// reference escapes; no registry, reflection or private pointer layout is read.
func lifecycleSuccessfulSession(t *testing.T, pool pdfium.Pool, data []byte) (weak.Pointer[lifecycleReader], uint64, uint64) {
	t.Helper()
	inst, cleanup := lifecycleInstance(t, pool)
	defer cleanup()
	r := &lifecycleReader{Reader: bytes.NewReader(data)}
	ref := weak.Make(r)
	doc := lifecycleOpen(t, inst, r, 0)
	opened := r.readBytes.Load()
	count, err := inst.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc.Document})
	if err != nil || count.PageCount != 1 {
		t.Fatalf("ordinary Count=%+v err=%v", count, err)
	}
	size, err := inst.FPDF_GetPageSizeByIndex(&requests.FPDF_GetPageSizeByIndex{Document: doc.Document, Index: 0})
	if err != nil || size.Width != 300 || size.Height != 200 {
		t.Fatalf("ordinary dimensions=%+v err=%v", size, err)
	}
	lifecycleLoadAndClosePage(t, inst, doc, true)
	total := r.readBytes.Load()
	if total <= opened {
		t.Fatal("fixture did not produce post-open reads; cannot validate deferred-read lifecycle")
	}
	if err := doc.ReadError(); err != nil {
		t.Fatal(err)
	}
	if _, err := inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document}); err != nil {
		t.Fatal(err)
	}
	if err := inst.Close(); err != nil {
		t.Fatal(err)
	}
	runtime.KeepAlive(r)
	t.Logf("real source bytes: open=%d later=%d", opened, total-opened)
	return ref, opened, total - opened
}

func TestVerifiedLifecycleSuccessAndReaderRelease(t *testing.T) {
	pool := lifecyclePool(t)
	ref, _, _ := lifecycleSuccessfulSession(t, pool, lifecyclePDF())
	lifecycleReleased(t, ref)
	// The same capacity-one pool must remain usable after document/instance close.
	ref, _, _ = lifecycleSuccessfulSession(t, pool, lifecyclePDF())
	lifecycleReleased(t, ref)
}

func TestVerifiedLifecycleCumulativeReadBudget(t *testing.T) {
	pool := lifecyclePool(t)
	data := lifecyclePDF()
	ref, opened, later := lifecycleSuccessfulSession(t, pool, data)
	lifecycleReleased(t, ref)
	budget := max(opened, later)
	if opened > budget || later > budget || opened > ^uint64(0)-later || opened+later <= budget {
		t.Fatalf("invalid cumulative-only calibration: opened=%d later=%d budget=%d", opened, later, budget)
	}
	inst, cleanup := lifecycleInstance(t, pool)
	defer cleanup()
	r := &lifecycleReader{Reader: bytes.NewReader(data)}
	doc := lifecycleOpen(t, inst, r, budget)
	if current := r.readBytes.Load(); current != opened {
		t.Fatalf("opening read accounting changed: current=%d calibrated=%d", current, opened)
	}
	t.Logf("calibrated successful-open bytes=%d later bytes=%d cumulative ceiling=%d current-open bytes=%d", opened, later, budget, r.readBytes.Load())
	lifecycleLoadAndClosePage(t, inst, doc, false)
	if !errors.Is(doc.ReadError(), webassembly.ErrVerifiedReadBudget) {
		t.Fatalf("later ordinary read did not expose cumulative budget error: %v", doc.ReadError())
	}
	first := doc.ReadError()
	before := r.readBytes.Load()
	lifecycleLoadAndClosePage(t, inst, doc, false)
	if !errors.Is(doc.ReadError(), first) || r.readBytes.Load() != before {
		t.Fatalf("sticky failure lost or reader called again: error=%v bytes=%d before=%d", doc.ReadError(), r.readBytes.Load(), before)
	}
	if err := inst.Close(); err != nil {
		t.Fatal(err)
	}
	ref, _, _ = lifecycleSuccessfulSession(t, pool, data)
	lifecycleReleased(t, ref)
}

func lifecycleCanceledSession(t *testing.T, pool pdfium.Pool, data []byte) weak.Pointer[lifecycleReader] {
	t.Helper()
	inst, cleanup := lifecycleInstance(t, pool)
	defer cleanup()
	r := &lifecycleReader{Reader: bytes.NewReader(data)}
	ref := weak.Make(r)
	doc := lifecycleOpen(t, inst, r, 0)
	barrier := &lifecycleBarrier{entered: make(chan struct{}), release: make(chan struct{})}
	defer barrier.unblock() // Release a blocked callback before cleanup can Close.
	r.mu.Lock()
	r.barrier = barrier
	r.mu.Unlock()
	callDone := make(chan error, 1)
	go func() {
		_, err := inst.FPDF_LoadPage(&requests.FPDF_LoadPage{Document: doc.Document, Index: 0})
		callDone <- err
	}()
	select {
	case <-barrier.entered:
	case err := <-callDone:
		t.Fatalf("ordinary page load made no deferred callback: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("no deferred callback entered")
	}
	if err := webassembly.Interrupt(inst); err != nil {
		t.Fatal(err)
	}
	closeStarted := make(chan struct{})
	closeDone := make(chan error, 1)
	go func() { close(closeStarted); closeDone <- inst.Close() }()
	<-closeStarted
	// A real synchronous host Read is still blocked. Neither Close nor the
	// capacity-one pool may report release merely because WASM was interrupted.
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	next, borrowErr := pool.GetInstanceWithContext(ctx)
	cancel()
	if next != nil {
		_ = next.Close()
		t.Fatal("pool capacity returned before the active Reader.Read exited")
	}
	if borrowErr == nil {
		t.Fatal("blocked capacity-one pool returned neither instance nor error")
	}
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned while Reader.Read was blocked: %v", err)
	default:
	}
	select {
	case err := <-callDone:
		t.Fatalf("LoadPage returned while Reader.Read was blocked: %v", err)
	default:
	}
	barrier.unblock()
	select {
	case err := <-callDone:
		if err == nil {
			t.Fatal("interrupted ordinary call returned success")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("LoadPage did not finish after releasing Reader.Read")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Close did not finish after synchronous callback exit")
	}
	if !errors.Is(doc.ReadError(), context.Canceled) {
		t.Fatalf("post-open canceled read cause not retained after Close: %v", doc.ReadError())
	}
	runtime.KeepAlive(r)
	return ref
}

func TestVerifiedLifecycleCanceledReadHoldsOwnership(t *testing.T) {
	pool := lifecyclePool(t)
	data := lifecyclePDF()
	ref := lifecycleCanceledSession(t, pool, data)
	lifecycleReleased(t, ref)
	ref, _, _ = lifecycleSuccessfulSession(t, pool, data)
	lifecycleReleased(t, ref)
}
