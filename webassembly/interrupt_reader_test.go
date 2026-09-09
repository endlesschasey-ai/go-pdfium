package webassembly_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
	"weak"

	"github.com/klippa-app/go-pdfium"
	implementation "github.com/klippa-app/go-pdfium/internal/implementation_webassembly"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/tetratelabs/wazero"
)

// wasmInterrupter is a test-only assertion so these tests compile against the
// unmodified library and fail when Interrupt is missing.
type wasmInterrupter interface {
	Interrupt() error
}

func interruptInstance(t *testing.T, instance pdfium.Pdfium) error {
	t.Helper()
	i, ok := instance.(wasmInterrupter)
	if !ok {
		t.Fatal("pdfium instance does not implement Interrupt(); WASM cancel is unavailable")
	}
	return i.Interrupt()
}

func testWASMPool(t *testing.T) pdfium.Pool {
	t.Helper()
	pool, err := webassembly.Init(webassembly.Config{
		MinIdle:      0,
		MaxIdle:      1,
		MaxTotal:     1,
		FSConfig:     wazero.NewFSConfig(),
		ReuseWorkers: true,
		Stdout:       io.Discard,
		Stderr:       io.Discard,
		RuntimeConfig: wazero.NewRuntimeConfig().
			WithCloseOnContextDone(true),
	})
	if err != nil {
		t.Fatalf("webassembly.Init: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}

func getInstance(t *testing.T, pool pdfium.Pool) pdfium.Pdfium {
	t.Helper()
	inst, err := pool.GetInstance(30 * time.Second)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	return inst
}

func testdataPDF(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("..", "shared_tests", "testdata", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read testdata %s: %v", name, err)
	}
	return data
}

func forceGC() {
	for i := 0; i < 5; i++ {
		runtime.GC()
		runtime.Gosched()
	}
}

func mustOpenFile(t *testing.T, inst pdfium.Pdfium, data []byte) {
	t.Helper()
	doc, err := inst.OpenDocument(&requests.OpenDocument{File: &data})
	if err != nil {
		t.Fatalf("OpenDocument: %v", err)
	}
	_, _ = inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document})
}

func TestInterrupt_AvailableOnWASMInstance(t *testing.T) {
	pool := testWASMPool(t)
	inst := getInstance(t, pool)
	t.Cleanup(func() { _ = inst.Close() })
	if _, ok := inst.(wasmInterrupter); !ok {
		t.Fatal("pdfium instance does not implement Interrupt(); WASM cancel is unavailable")
	}
}

func TestInterrupt_ActiveWASMSaveCancelsAndRecovers(t *testing.T) {
	pool := testWASMPool(t)
	inst := getInstance(t, pool)
	if _, ok := inst.(wasmInterrupter); !ok {
		_ = inst.Close()
		t.Fatal("pdfium instance does not implement Interrupt(); WASM cancel is unavailable")
	}

	data := testdataPDF(t, "hello_world.pdf")
	doc, err := inst.OpenDocument(&requests.OpenDocument{File: &data})
	if err != nil {
		_ = inst.Close()
		t.Fatalf("OpenDocument: %v", err)
	}

	gate := &saveWriteGate{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	releaseOnce := sync.Once{}
	release := func() { releaseOnce.Do(func() { close(gate.release) }) }
	t.Cleanup(release)

	done := make(chan error, 1)
	go func() {
		_, saveErr := inst.FPDF_SaveAsCopy(&requests.FPDF_SaveAsCopy{
			Document:   doc.Document,
			FileWriter: gate,
			Flags:      requests.SaveFlagNoIncremental,
		})
		done <- saveErr
	}()

	select {
	case <-gate.started:
	case err := <-done:
		_ = inst.Close()
		t.Fatalf("FPDF_SaveAsCopy returned before the first Write: %v", err)
	case <-time.After(30 * time.Second):
		_ = inst.Close()
		t.Fatal("timed out waiting for FPDF_SaveAsCopy first Write")
	}

	if err := interruptInstance(t, inst); err != nil {
		release()
		_ = inst.Close()
		t.Fatalf("Interrupt: %v", err)
	}
	// Keep the host callback blocked until the runtime observes cancellation.
	module := inst.GetImplementation().(*implementation.PdfiumImplementation).Module
	deadline := time.Now().Add(5 * time.Second)
	for !module.IsClosed() {
		if time.Now().After(deadline) {
			release()
			t.Fatal("runtime did not observe Interrupt")
		}
		runtime.Gosched()
	}
	release()

	select {
	case err := <-done:
		if err == nil {
			_ = inst.Close()
			t.Fatal("in-flight save succeeded after Interrupt")
		}
	case <-time.After(30 * time.Second):
		_ = inst.Close()
		t.Fatal("FPDF_SaveAsCopy did not return after Interrupt")
	}

	if err := inst.Close(); err != nil {
		t.Fatalf("Close after Interrupt: %v", err)
	}

	inst2 := getInstance(t, pool)
	mustOpenFile(t, inst2, testdataPDF(t, "hello_world.pdf"))
	if err := inst2.Close(); err != nil {
		t.Fatalf("Close recovered instance: %v", err)
	}
}

func TestInterrupt_APIEntryCancelsAndRecovers(t *testing.T) {
	pool := testWASMPool(t)
	data := testdataPDF(t, "hello_world.pdf")

	const iterations = 20
	for i := 0; i < iterations; i++ {
		inst := getInstance(t, pool)
		if err := interruptInstance(t, inst); err != nil {
			_ = inst.Close()
			t.Fatalf("Interrupt at API entry (i=%d): %v", i, err)
		}
		_, openErr := inst.OpenDocument(&requests.OpenDocument{File: &data})
		closeErr := inst.Close()
		if closeErr != nil {
			t.Fatalf("Close after API-entry Interrupt (i=%d): %v", i, closeErr)
		}
		if openErr == nil {
			t.Fatalf("OpenDocument succeeded after Interrupt (i=%d)", i)
		}
	}

	inst := getInstance(t, pool)
	mustOpenFile(t, inst, data)
	if err := inst.Close(); err != nil {
		t.Fatalf("Close recovered instance: %v", err)
	}
}

func TestInterrupt_AfterCloseDoesNotKillNewBorrower(t *testing.T) {
	pool := testWASMPool(t)
	data := testdataPDF(t, "hello_world.pdf")

	const rounds = 20
	for round := 0; round < rounds; round++ {
		old := getInstance(t, pool)
		mustOpenFile(t, old, data)
		worker := old.GetImplementation()
		if err := old.Close(); err != nil {
			t.Fatalf("Close healthy instance (round %d): %v", round, err)
		}

		fresh := getInstance(t, pool)
		if fresh.GetImplementation() != worker {
			_ = fresh.Close()
			t.Fatalf("healthy worker was not reused (round %d)", round)
		}
		if err := interruptClosedInstance(t, old); err == nil {
			_ = fresh.Close()
			t.Fatal("Interrupt after Close succeeded; old wrapper can still cancel a worker")
		}
		mustOpenFile(t, fresh, data)
		if err := fresh.Close(); err != nil {
			t.Fatalf("Close reused instance (round %d): %v", round, err)
		}
	}
}

func interruptClosedInstance(t *testing.T, instance pdfium.Pdfium) error {
	t.Helper()
	i, ok := instance.(wasmInterrupter)
	if !ok {
		t.Fatal("pdfium instance does not implement Interrupt(); WASM cancel is unavailable")
	}
	return i.Interrupt()
}

func TestInterrupt_RepeatedInterrupt(t *testing.T) {
	pool := testWASMPool(t)
	inst := getInstance(t, pool)
	if err := interruptInstance(t, inst); err != nil {
		_ = inst.Close()
		t.Fatalf("first Interrupt: %v", err)
	}
	if err := interruptInstance(t, inst); err != nil {
		_ = inst.Close()
		t.Fatalf("second Interrupt: %v", err)
	}
	if err := inst.Close(); err != nil {
		t.Fatalf("Close after repeated Interrupt: %v", err)
	}

	inst2 := getInstance(t, pool)
	mustOpenFile(t, inst2, testdataPDF(t, "hello_world.pdf"))
	if err := inst2.Close(); err != nil {
		t.Fatalf("Close recovered instance: %v", err)
	}
}

func TestInterrupt_HealthyReuseThenRecover(t *testing.T) {
	pool := testWASMPool(t)
	data := testdataPDF(t, "hello_world.pdf")

	inst := getInstance(t, pool)
	mustOpenFile(t, inst, data)
	worker := inst.GetImplementation()
	if err := inst.Close(); err != nil {
		t.Fatalf("Close healthy instance: %v", err)
	}

	inst2 := getInstance(t, pool)
	if inst2.GetImplementation() != worker {
		_ = inst2.Close()
		t.Fatal("healthy worker was not reused")
	}
	mustOpenFile(t, inst2, data)
	if err := inst2.Close(); err != nil {
		t.Fatalf("Close reused instance: %v", err)
	}
}

func TestOpenDocument_InvalidFileReaderIsReleased(t *testing.T) {
	pool := testWASMPool(t)
	const n = 8
	refs := make([]weak.Pointer[bytes.Reader], n)
	for i := 0; i < n; i++ {
		refs[i] = invalidReaderWeakRef(t, pool)
	}
	forceGC()
	for i, ref := range refs {
		if ref.Value() != nil {
			t.Fatalf("invalid FileReader %d retained after Close and GC", i)
		}
	}

	inst := getInstance(t, pool)
	mustOpenFile(t, inst, testdataPDF(t, "hello_world.pdf"))
	if err := inst.Close(); err != nil {
		t.Fatalf("Close recovered instance: %v", err)
	}
}

func invalidReaderWeakRef(t *testing.T, pool pdfium.Pool) weak.Pointer[bytes.Reader] {
	t.Helper()
	inst, err := pool.GetInstanceWithContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader([]byte("%PDF-1.4 not a real document"))
	ref := weak.Make(r)
	_, err = inst.OpenDocument(&requests.OpenDocument{
		FileReader:     r,
		FileReaderSize: r.Size(),
	})
	if err == nil {
		_ = inst.Close()
		t.Fatal("invalid PDF was accepted")
	}
	if err = inst.Close(); err != nil {
		t.Fatalf("Close after invalid OpenDocument: %v", err)
	}
	return ref
}

func TestOpenDocument_SuccessfulFileReaderIsReleased(t *testing.T) {
	pool := testWASMPool(t)
	ref := successfulReaderWeakRef(t, pool)
	forceGC()
	if ref.Value() != nil {
		t.Fatal("successful FileReader retained after Close and GC")
	}
}

func successfulReaderWeakRef(t *testing.T, pool pdfium.Pool) weak.Pointer[bytes.Reader] {
	t.Helper()
	inst, err := pool.GetInstanceWithContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(testdataPDF(t, "hello_world.pdf"))
	ref := weak.Make(r)
	doc, err := inst.OpenDocument(&requests.OpenDocument{
		FileReader:     r,
		FileReaderSize: r.Size(),
	})
	if err != nil {
		_ = inst.Close()
		t.Fatalf("OpenDocument: %v", err)
	}
	_, _ = inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document})
	if err = inst.Close(); err != nil {
		t.Fatalf("Close after successful OpenDocument: %v", err)
	}
	return ref
}

func TestOpenDocument_CanceledFileReaderIsReleased(t *testing.T) {
	pool := testWASMPool(t)
	ref := canceledReaderWeakRef(t, pool)
	forceGC()
	if ref.Value() != nil {
		t.Fatal("canceled FileReader retained after Close and GC")
	}

	inst := getInstance(t, pool)
	mustOpenFile(t, inst, testdataPDF(t, "hello_world.pdf"))
	if err := inst.Close(); err != nil {
		t.Fatalf("Close recovered instance: %v", err)
	}
}

func canceledReaderWeakRef(t *testing.T, pool pdfium.Pool) weak.Pointer[blockingReadSeeker] {
	t.Helper()
	inst, err := pool.GetInstanceWithContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := inst.(wasmInterrupter); !ok {
		_ = inst.Close()
		t.Fatal("pdfium instance does not implement Interrupt(); WASM cancel is unavailable")
	}

	r := &blockingReadSeeker{
		inner:   bytes.NewReader(testdataPDF(t, "hello_world.pdf")),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	ref := weak.Make(r)
	releaseOnce := sync.Once{}
	releaseChannel := r.release
	release := func() { releaseOnce.Do(func() { close(releaseChannel) }) }
	t.Cleanup(release)

	done := make(chan error, 1)
	go func() {
		_, openErr := inst.OpenDocument(&requests.OpenDocument{
			FileReader:     r,
			FileReaderSize: r.inner.Size(),
		})
		done <- openErr
	}()

	select {
	case <-r.started:
	case err := <-done:
		release()
		_ = inst.Close()
		t.Fatalf("OpenDocument returned before FileReader.Read: %v", err)
	case <-time.After(30 * time.Second):
		release()
		_ = inst.Close()
		t.Fatal("timed out waiting for FileReader.Read")
	}

	if err := interruptInstance(t, inst); err != nil {
		release()
		_ = inst.Close()
		t.Fatalf("Interrupt during OpenDocument: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("OpenDocument returned while Read still blocked after Interrupt: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	release()

	select {
	case err := <-done:
		if err == nil {
			_ = inst.Close()
			t.Fatal("OpenDocument succeeded after Interrupt")
		}
	case <-time.After(30 * time.Second):
		_ = inst.Close()
		t.Fatal("OpenDocument did not return after Interrupt")
	}

	if err := inst.Close(); err != nil {
		t.Fatalf("Close after canceled OpenDocument: %v", err)
	}
	return ref
}

type saveWriteGate struct {
	started chan struct{}
	release chan struct{}
	start   sync.Once
}

func (w *saveWriteGate) Write(p []byte) (int, error) {
	w.start.Do(func() { close(w.started) })
	<-w.release
	return len(p), nil
}

type blockingReadSeeker struct {
	inner   *bytes.Reader
	started chan struct{}
	release chan struct{}
	start   sync.Once
}

func (r *blockingReadSeeker) Read(p []byte) (int, error) {
	r.start.Do(func() { close(r.started) })
	<-r.release
	return r.inner.Read(p)
}

func (r *blockingReadSeeker) Seek(offset int64, whence int) (int64, error) {
	return r.inner.Seek(offset, whence)
}

var _ io.ReadSeeker = (*blockingReadSeeker)(nil)
var _ io.Writer = (*saveWriteGate)(nil)
