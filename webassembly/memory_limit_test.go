package webassembly

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/tetratelabs/wazero"
)

func TestS1MemoryLimitRejectionIsTyped(t *testing.T) {
	const limit = 64 << 20
	pool := newPool(t, Config{
		MinIdle:          0,
		MaxIdle:          1,
		MaxTotal:         1,
		MemoryLimitBytes: limit,
		// The same 64MiB expressed as pages. This is the historical refusal
		// point; with MemoryLimitBytes set, growth has to pass it and stop in
		// the allocator instead.
		RuntimeConfig: wazero.NewRuntimeConfig().WithMemoryLimitPages(uint32(limit / wasmPageSize)),
		Stdout:        io.Discard,
		Stderr:        io.Discard,
	})
	inst := borrow(t, pool)
	mem := inst.worker.Module.Memory()
	minBytes := uint64(mem.Definition().Min()) * wasmPageSize
	if got := uint64(mem.Size()); got != minBytes {
		t.Fatalf("initial backing %d, module minimum %d", got, minBytes)
	}
	maxPages, hasMax := mem.Definition().Max()
	if !hasMax || maxPages <= uint32(limit/wasmPageSize) {
		t.Fatalf("module max pages=%d hasMax=%v; a lower page cap can refuse growth before the allocator", maxPages, hasMax)
	}

	err := renderImage(inst, writeImagePDF(t))
	if err == nil {
		t.Fatal("S1: rendering the oversized image succeeded")
	}
	lim := requireLimit(t, err)
	if lim.Limit != limit || lim.Requested <= lim.Limit || lim.Current > lim.Limit || lim.Phase != "RenderPageInDPI" {
		t.Fatalf("S1: evidence = %+v", lim)
	}
	if got := uint64(inst.worker.Module.Memory().Size()); got > limit || got != lim.Current {
		t.Fatalf("S1: backing length %d, evidence current %d, limit %d", got, lim.Current, limit)
	}
	if inst.worker.memory == nil {
		t.Fatal("S1: allocator was not installed")
	}
	if peak := inst.worker.memory.peak(); peak > limit {
		t.Fatalf("S1: backing peak %d exceeds limit %d", peak, limit)
	}
	if err := inst.Close(); err != nil {
		t.Fatal(err)
	}
	if again := requireLimit(t, err); again.Requested != lim.Requested || again.Current != lim.Current || again.Limit != lim.Limit || again.Phase != lim.Phase {
		t.Fatalf("S1: evidence changed after close: %+v", again)
	}
}

func TestS2MemoryLimitResetsBetweenCalls(t *testing.T) {
	pool := newPool(t, limitConfig(64<<20, 1))
	inst := borrow(t, pool)
	if err := openSmall(inst); err != nil {
		t.Fatal(err)
	}
	if err := openSmall(inst); err != nil {
		t.Fatal(err)
	}
	renderErr := renderImage(inst, writeImagePDF(t))
	_ = requireLimit(t, renderErr)
	if err := openSmall(inst); err != nil {
		t.Fatalf("S2: same instance after refusal: %v", err)
	}
	if err := inst.Close(); err != nil {
		t.Fatal(err)
	}
	if again := requireLimit(t, renderErr); again.Phase == "" {
		t.Fatal("S2: evidence lost after close")
	}
	next := borrow(t, pool)
	if err := openSmall(next); err != nil {
		t.Fatalf("S2: new instance after refusal: %v", err)
	}
}

func TestS3MemoryLimitEvidenceDoesNotCrossInstances(t *testing.T) {
	const limit = 64 << 20
	pool := newPool(t, limitConfig(limit, 2))
	rejected := borrow(t, pool)
	healthy := borrow(t, pool)
	path := writeImagePDF(t)

	var wg sync.WaitGroup
	start := make(chan struct{})
	var errRejected, errHealthy error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errRejected = renderImage(rejected, path)
	}()
	go func() {
		defer wg.Done()
		<-start
		errHealthy = openSmall(healthy)
	}()
	close(start)
	wg.Wait()

	lim := requireLimit(t, errRejected)
	if lim.Limit != limit {
		t.Fatalf("S3: rejected instance limit = %d", lim.Limit)
	}
	if errHealthy != nil {
		t.Fatalf("S3: healthy instance failed: %v", errHealthy)
	}
	var crossed *MemoryLimitError
	if errors.As(errHealthy, &crossed) {
		t.Fatalf("S3: healthy instance carried %+v", crossed)
	}
	if peak := healthy.worker.memory.peak(); peak > limit {
		t.Fatalf("S3: healthy backing peak %d", peak)
	}
	if peak := rejected.worker.memory.peak(); peak > limit {
		t.Fatalf("S3: rejected backing peak %d", peak)
	}
}

func TestS4NonMemoryTrapHasNoLimitEvidence(t *testing.T) {
	pool := newPool(t, limitConfig(64<<20, 2))
	fresh := borrow(t, pool)
	trapErr := nonMemoryTrap(fresh)
	if trapErr == nil || !strings.Contains(trapErr.Error(), "invalid table access") {
		t.Fatalf("S4: expected invalid table access, got %v", trapErr)
	}
	var lim *MemoryLimitError
	if errors.As(trapErr, &lim) {
		t.Fatalf("S4: fresh trap carried %+v", lim)
	}

	used := borrow(t, pool)
	if renderErr := renderImage(used, writeImagePDF(t)); renderErr == nil {
		t.Fatal("S4: expected a prior memory refusal")
	} else {
		_ = requireLimit(t, renderErr)
	}
	trapErr = nonMemoryTrap(used)
	if trapErr == nil || !strings.Contains(trapErr.Error(), "invalid table access") {
		t.Fatalf("S4: expected invalid table access after refusal, got %v", trapErr)
	}
	if errors.As(trapErr, &lim) {
		t.Fatalf("S4: trap after refusal carried %+v", lim)
	}
}

func TestS5UnsetMemoryLimitKeepsPageCapBehavior(t *testing.T) {
	const pages = uint32(64 << 20 / wasmPageSize)
	pool := newPool(t, Config{
		MinIdle:       0,
		MaxIdle:       1,
		MaxTotal:      1,
		RuntimeConfig: wazero.NewRuntimeConfig().WithMemoryLimitPages(pages),
		Stdout:        io.Discard,
		Stderr:        io.Discard,
	})
	inst := borrow(t, pool)
	if inst.worker.memory != nil {
		t.Fatal("S5: allocator installed without MemoryLimitBytes")
	}
	err := renderImage(inst, writeImagePDF(t))
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("S5: expected the historical unreachable trap, got %v", err)
	}
	var lim *MemoryLimitError
	if errors.As(err, &lim) {
		t.Fatalf("S5: page cap attached allocator evidence %+v", lim)
	}
}

func TestMemoryLimitBelowModuleMinimum(t *testing.T) {
	_, err := Init(Config{
		MinIdle:          0,
		MaxIdle:          0,
		MaxTotal:         1,
		MemoryLimitBytes: 1,
		Stdout:           io.Discard,
		Stderr:           io.Discard,
	})
	lim := requireLimit(t, err)
	if lim.Phase != "instantiate" || lim.Current != 0 || lim.Limit != 1 || lim.Requested != 287*wasmPageSize {
		t.Fatalf("minimum refusal = %+v", lim)
	}
}

func TestMemoryLimiterCapsBackingWithoutPreallocatingMax(t *testing.T) {
	const limit = 64 << 20
	m := newMemoryLimiter(limit)
	m.begin("grow")
	mem := m.Allocate(2<<30, 4<<30)
	got := mem.Reallocate(287 * wasmPageSize)
	if got == nil || uint64(len(got)) != 287*wasmPageSize || uint64(cap(got)) > limit {
		t.Fatalf("initial buffer len=%d cap=%d", len(got), cap(got))
	}
	if mem.Reallocate(limit+wasmPageSize) != nil {
		t.Fatal("growth past the limit was installed")
	}
	if uint64(cap(m.buf)) > limit || m.peak() > limit {
		t.Fatalf("backing cap=%d peak=%d", cap(m.buf), m.peak())
	}
	err := m.finish(errors.New("wasm error: unreachable"))
	lim := requireLimit(t, err)
	if lim.Phase != "grow" || lim.Requested != limit+wasmPageSize || lim.Current != 287*wasmPageSize || lim.Limit != limit {
		t.Fatalf("limiter evidence = %+v", lim)
	}
	m.begin("next")
	next := m.finish(errors.New("wasm error: invalid table access"))
	var again *MemoryLimitError
	if errors.As(next, &again) {
		t.Fatalf("next call kept %+v", again)
	}
	if !errors.As(err, &again) {
		t.Fatal("snapshot was cleared from the earlier error")
	}
}

func limitConfig(limit uint64, instances int) Config {
	return Config{
		MinIdle:          0,
		MaxIdle:          instances,
		MaxTotal:         instances,
		MemoryLimitBytes: limit,
		RuntimeConfig:    wazero.NewRuntimeConfig().WithMemoryLimitPages(uint32(limit / wasmPageSize)),
		Stdout:           io.Discard,
		Stderr:           io.Discard,
	}
}

func newPool(t *testing.T, cfg Config) pdfium.Pool {
	t.Helper()
	pool, err := Init(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}

func borrow(t *testing.T, pool pdfium.Pool) *pdfiumInstance {
	t.Helper()
	raw, err := pool.GetInstance(30 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return raw.(*pdfiumInstance)
}

func writeImagePDF(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "uncompressed-image.pdf")
	if err := os.WriteFile(path, uncompressedImagePDF(4096, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func renderImage(inst *pdfiumInstance, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	doc, err := inst.OpenDocument(&requests.OpenDocument{FileReader: f, FileReaderSize: info.Size()})
	if err != nil {
		return err
	}
	_, err = inst.RenderPageInDPI(&requests.RenderPageInDPI{
		Page: requests.Page{ByIndex: &requests.PageByIndex{Document: doc.Document, Index: 0}},
		DPI:  72,
	})
	_, _ = inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document})
	return err
}

func openSmall(inst *pdfiumInstance) error {
	pdf := minimalPDF()
	doc, err := inst.OpenDocument(&requests.OpenDocument{File: &pdf})
	if err != nil {
		return err
	}
	count, err := inst.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc.Document})
	if err != nil {
		return err
	}
	if count.PageCount != 1 {
		return fmt.Errorf("page count %d", count.PageCount)
	}
	_, err = inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document})
	return err
}

func nonMemoryTrap(inst *pdfiumInstance) error {
	inst.beginCall("FPDF_GetPageWidth")
	fn := inst.worker.Module.ExportedFunction("FPDF_GetPageWidth")
	if fn == nil {
		return errors.New("FPDF_GetPageWidth missing")
	}
	_, err := fn.Call(inst.worker.Context, 1)
	return inst.finishCall(err)
}

func requireLimit(t *testing.T, err error) *MemoryLimitError {
	t.Helper()
	var lim *MemoryLimitError
	if !errors.As(err, &lim) || lim == nil {
		t.Fatalf("expected *MemoryLimitError, got %v", err)
	}
	return lim
}

func minimalPDF() []byte {
	return []byte("%PDF-1.4\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n2 0 obj<</Type/Pages/Count 1/Kids[3 0 R]>>endobj\n3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 3 3]>>endobj\nxref\n0 4\n0000000000 65535 f \n0000000009 00000 n \n0000000058 00000 n \n0000000115 00000 n \ntrailer<</Size 4/Root 1 0 R>>\nstartxref\n182\n%%EOF\n")
}

func uncompressedImagePDF(width, height int) []byte {
	streamLen := width * height * 3
	var b []byte
	b = append(b, "%PDF-1.4\n"...)
	offsets := []int{0}
	add := func(s string) {
		offsets = append(offsets, len(b))
		b = append(b, s...)
	}
	add("1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n")
	add("2 0 obj<</Type/Pages/Count 1/Kids[3 0 R]>>endobj\n")
	add("3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]/Resources<</XObject<</Im0 4 0 R>>>>/Contents 5 0 R>>endobj\n")
	header := fmt.Sprintf("4 0 obj<</Type/XObject/Subtype/Image/Width %d/Height %d/ColorSpace/DeviceRGB/BitsPerComponent 8/Length %d>>stream\n", width, height, streamLen)
	offsets = append(offsets, len(b))
	b = append(b, header...)
	pixel := make([]byte, streamLen)
	pixel[0] = 1
	b = append(b, pixel...)
	b = append(b, "\nendstream\nendobj\n"...)
	content := "q 612 0 0 792 0 0 cm /Im0 Do Q\n"
	add(fmt.Sprintf("5 0 obj<</Length %d>>stream\n%s\nendstream\nendobj\n", len(content), content))
	xrefAt := len(b)
	b = append(b, fmt.Sprintf("xref\n0 %d\n0000000000 65535 f \n", len(offsets))...)
	for i := 1; i < len(offsets); i++ {
		b = append(b, fmt.Sprintf("%010d 00000 n \n", offsets[i])...)
	}
	b = append(b, fmt.Sprintf("trailer<</Size %d/Root 1 0 R>>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xrefAt)...)
	return b
}
