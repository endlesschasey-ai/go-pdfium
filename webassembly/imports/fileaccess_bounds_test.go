package imports

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/tetratelabs/wazero/api"

	impl "github.com/klippa-app/go-pdfium/internal/implementation_webassembly"
	"github.com/tetratelabs/wazero"
)

type observedReader struct {
	*strings.Reader
	reads int
}

func (r *observedReader) Read(p []byte) (int, error) { r.reads++; return r.Reader.Read(p) }

func TestFileAccessRejectsGuestRangeBeforeRead(t *testing.T) {
	ctx := context.Background()
	runtime := wazero.NewRuntime(ctx)
	defer runtime.Close(ctx)
	// Two memory pages, exported memory; no executable guest code.
	wasm := []byte{0, 97, 115, 109, 1, 0, 0, 0, 5, 3, 1, 0, 2, 7, 10, 1, 6, 'm', 'e', 'm', 'o', 'r', 'y', 2, 0}
	mod, err := runtime.Instantiate(ctx, wasm)
	if err != nil {
		t.Fatal(err)
	}
	reader := &observedReader{Reader: strings.NewReader("abcd")}
	ref := &impl.FileReaderRef{Reader: reader, Size: uint64(reader.Len()), Module: mod}
	impl.FileReaders.Mutex.Lock()
	id := impl.FileReaders.Counter
	impl.FileReaders.Counter++
	impl.FileReaders.Refs[id] = ref
	impl.FileReaders.Mutex.Unlock()
	defer func() {
		impl.FileReaders.Mutex.Lock()
		delete(impl.FileReaders.Refs, id)
		impl.FileReaders.Mutex.Unlock()
	}()
	mod.Memory().WriteUint32Le(0, id)
	stack := []uint64{0, 0, 131071, 2}
	(&FPDF_FILEACCESS_CB{}).Call(ctx, mod, stack)
	if stack[0] != 0 {
		t.Fatalf("accepted invalid guest range: %d", stack[0])
	}
	if reader.reads != 0 {
		t.Fatalf("invalid destination triggered %d underlying reads", reader.reads)
	}
}

type chunkReader struct {
	*strings.Reader
	maxRead int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(p) > r.maxRead {
		r.maxRead = len(p)
	}
	return r.Reader.Read(p)
}
func TestFileAccessChunksLargeCallback(t *testing.T) {
	ctx := context.Background()
	runtime := wazero.NewRuntime(ctx)
	defer runtime.Close(ctx)
	mod, err := runtime.Instantiate(ctx, []byte{0, 97, 115, 109, 1, 0, 0, 0, 5, 3, 1, 0, 2, 7, 10, 1, 6, 'm', 'e', 'm', 'o', 'r', 'y', 2, 0})
	if err != nil {
		t.Fatal(err)
	}
	reader := &chunkReader{Reader: strings.NewReader(strings.Repeat("a", 100000))}
	ref := &impl.FileReaderRef{Reader: reader, Size: uint64(reader.Len()), Module: mod}
	impl.FileReaders.Mutex.Lock()
	id := impl.FileReaders.Counter
	impl.FileReaders.Counter++
	impl.FileReaders.Refs[id] = ref
	impl.FileReaders.Mutex.Unlock()
	defer func() {
		impl.FileReaders.Mutex.Lock()
		delete(impl.FileReaders.Refs, id)
		impl.FileReaders.Mutex.Unlock()
	}()
	mod.Memory().WriteUint32Le(0, id)
	stack := []uint64{0, 0, 16, 100000}
	(&FPDF_FILEACCESS_CB{}).Call(ctx, mod, stack)
	if stack[0] == 0 {
		t.Fatal("valid callback failed")
	}
	if reader.maxRead > 65536 {
		t.Fatalf("underlying read=%d exceeds 64KiB", reader.maxRead)
	}
}

func TestFileAccessRejectsSourceRangeBeforeRead(t *testing.T) {
	ctx := context.Background()
	runtime := wazero.NewRuntime(ctx)
	defer runtime.Close(ctx)
	mod, err := runtime.Instantiate(ctx, []byte{0, 97, 115, 109, 1, 0, 0, 0, 5, 3, 1, 0, 2, 7, 10, 1, 6, 'm', 'e', 'm', 'o', 'r', 'y', 2, 0})
	if err != nil {
		t.Fatal(err)
	}
	reader := &observedReader{Reader: strings.NewReader("abcd")}
	ref := &impl.FileReaderRef{Reader: reader, Size: 4, Module: mod}
	impl.FileReaders.Mutex.Lock()
	id := impl.FileReaders.Counter
	impl.FileReaders.Counter++
	impl.FileReaders.Refs[id] = ref
	impl.FileReaders.Mutex.Unlock()
	defer func() {
		impl.FileReaders.Mutex.Lock()
		delete(impl.FileReaders.Refs, id)
		impl.FileReaders.Mutex.Unlock()
	}()
	mod.Memory().WriteUint32Le(0, id)
	stack := []uint64{0, 3, 16, 2}
	(&FPDF_FILEACCESS_CB{}).Call(ctx, mod, stack)
	if stack[0] != 0 || reader.reads != 0 {
		t.Fatalf("source range result=%d reads=%d", stack[0], reader.reads)
	}
}

func TestFileAccessCanceledBeforeRead(t *testing.T) {
	ctx := context.Background()
	runtime := wazero.NewRuntime(ctx)
	defer runtime.Close(ctx)
	mod, err := runtime.Instantiate(ctx, []byte{0, 97, 115, 109, 1, 0, 0, 0, 5, 3, 1, 0, 2, 7, 10, 1, 6, 'm', 'e', 'm', 'o', 'r', 'y', 2, 0})
	if err != nil {
		t.Fatal(err)
	}
	reader := &observedReader{Reader: strings.NewReader("abcd")}
	ref := &impl.FileReaderRef{Reader: reader, Size: 4, Module: mod}
	impl.FileReaders.Mutex.Lock()
	id := impl.FileReaders.Counter
	impl.FileReaders.Counter++
	impl.FileReaders.Refs[id] = ref
	impl.FileReaders.Mutex.Unlock()
	defer func() {
		impl.FileReaders.Mutex.Lock()
		delete(impl.FileReaders.Refs, id)
		impl.FileReaders.Mutex.Unlock()
	}()
	mod.Memory().WriteUint32Le(0, id)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	stack := []uint64{0, 0, 16, 4}
	(&FPDF_FILEACCESS_CB{}).Call(canceled, mod, stack)
	if stack[0] != 0 || reader.reads != 0 {
		t.Fatalf("canceled result=%d reads=%d", stack[0], reader.reads)
	}
}

func TestFileAccessBudgetFailureSticks(t *testing.T) {
	ctx := context.Background()
	runtime := wazero.NewRuntime(ctx)
	defer runtime.Close(ctx)
	mod, err := runtime.Instantiate(ctx, []byte{0, 97, 115, 109, 1, 0, 0, 0, 5, 3, 1, 0, 2, 7, 10, 1, 6, 'm', 'e', 'm', 'o', 'r', 'y', 2, 0})
	if err != nil {
		t.Fatal(err)
	}
	reader := &observedReader{Reader: strings.NewReader("abcd")}
	budget := &impl.FileReadBudget{MaxBytes: 3, MaxCallbacks: 10}
	ref := &impl.FileReaderRef{Reader: reader, Size: 4, Module: mod, Budget: budget}
	impl.FileReaders.Mutex.Lock()
	id := impl.FileReaders.Counter
	impl.FileReaders.Counter++
	impl.FileReaders.Refs[id] = ref
	impl.FileReaders.Mutex.Unlock()
	defer func() {
		impl.FileReaders.Mutex.Lock()
		delete(impl.FileReaders.Refs, id)
		impl.FileReaders.Mutex.Unlock()
	}()
	mod.Memory().WriteUint32Le(0, id)
	for _, n := range []uint64{4, 1} {
		stack := []uint64{0, 0, 16, n}
		(&FPDF_FILEACCESS_CB{}).Call(ctx, mod, stack)
		if stack[0] != 0 {
			t.Fatalf("over-budget or later callback succeeded: %d", n)
		}
	}
	if reader.reads != 0 || budget.Err() == nil {
		t.Fatalf("reads=%d sticky error=%v", reader.reads, budget.Err())
	}
}

func callbackHarness(t *testing.T, reader io.ReadSeeker, size uint64, budget *impl.FileReadBudget) (api.Module, func(context.Context, uint64) uint64) {
	t.Helper()
	ctx := context.Background()
	runtime := wazero.NewRuntime(ctx)
	t.Cleanup(func() { runtime.Close(ctx) })
	mod, err := runtime.Instantiate(ctx, []byte{0, 97, 115, 109, 1, 0, 0, 0, 5, 3, 1, 0, 2, 7, 10, 1, 6, 'm', 'e', 'm', 'o', 'r', 'y', 2, 0})
	if err != nil {
		t.Fatal(err)
	}
	ref := &impl.FileReaderRef{Reader: reader, Size: size, Module: mod, Budget: budget}
	impl.FileReaders.Mutex.Lock()
	id := impl.FileReaders.Counter
	impl.FileReaders.Counter++
	impl.FileReaders.Refs[id] = ref
	impl.FileReaders.Mutex.Unlock()
	t.Cleanup(func() {
		impl.FileReaders.Mutex.Lock()
		delete(impl.FileReaders.Refs, id)
		impl.FileReaders.Mutex.Unlock()
	})
	mod.Memory().WriteUint32Le(0, id)
	return mod, func(ctx context.Context, n uint64) uint64 {
		stack := []uint64{0, 0, 16, n}
		(&FPDF_FILEACCESS_CB{}).Call(ctx, mod, stack)
		return stack[0]
	}
}

type shortReader struct {
	*strings.Reader
	cancel context.CancelFunc
	zero   bool
}

func (r *shortReader) Read(p []byte) (int, error) {
	if r.zero {
		return 0, nil
	}
	if len(p) > 2 {
		p = p[:2]
	}
	n, e := r.Reader.Read(p)
	if r.cancel != nil {
		r.cancel()
	}
	return n, e
}
func TestFileAccessFullShortReads(t *testing.T) {
	mod, call := callbackHarness(t, &shortReader{Reader: strings.NewReader("abcd")}, 4, nil)
	if call(context.Background(), 4) != 1 {
		t.Fatal("complete short reads rejected")
	}
	got, _ := mod.Memory().Read(16, 4)
	if string(got) != "abcd" {
		t.Fatalf("data=%q", got)
	}
}
func TestFileAccessShortEOFSticks(t *testing.T) {
	b := &impl.FileReadBudget{MaxBytes: 100, MaxCallbacks: 10}
	_, call := callbackHarness(t, strings.NewReader("ab"), 4, b)
	if call(context.Background(), 4) != 0 || !errors.Is(b.Err(), io.ErrUnexpectedEOF) {
		t.Fatalf("short EOF=%v", b.Err())
	}
	if call(context.Background(), 1) != 0 {
		t.Fatal("later read erased failure")
	}
}
func TestFileAccessZeroProgress(t *testing.T) {
	b := &impl.FileReadBudget{MaxBytes: 100, MaxCallbacks: 10}
	_, call := callbackHarness(t, &shortReader{Reader: strings.NewReader("abcd"), zero: true}, 4, b)
	if call(context.Background(), 4) != 0 || !errors.Is(b.Err(), io.ErrNoProgress) {
		t.Fatalf("zero progress=%v", b.Err())
	}
}
func TestFileAccessCancelAfterChunk(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &impl.FileReadBudget{MaxBytes: 100, MaxCallbacks: 10}
	_, call := callbackHarness(t, &shortReader{Reader: strings.NewReader("abcd"), cancel: cancel}, 4, b)
	if call(ctx, 4) != 0 || !errors.Is(b.Err(), context.Canceled) {
		t.Fatalf("cancel=%v", b.Err())
	}
}
func TestFileAccessCallbackLimit(t *testing.T) {
	b := &impl.FileReadBudget{MaxBytes: 100, MaxCallbacks: 1}
	_, call := callbackHarness(t, strings.NewReader("abcd"), 4, b)
	if call(context.Background(), 1) != 1 || call(context.Background(), 1) != 0 || !errors.Is(b.Err(), impl.ErrReadBudget) {
		t.Fatalf("callback budget=%v", b.Err())
	}
}

type blockedSource struct {
	*strings.Reader
	entered chan struct{}
	release chan struct{}
}

func (r *blockedSource) Read(p []byte) (int, error) {
	close(r.entered)
	<-r.release
	return r.Reader.Read(p)
}
func TestFileAccessCancelWaitsForActiveReader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := wazero.NewRuntime(context.Background())
	defer runtime.Close(context.Background())
	mod, err := runtime.Instantiate(context.Background(), []byte{0, 97, 115, 109, 1, 0, 0, 0, 5, 3, 1, 0, 2, 7, 10, 1, 6, 'm', 'e', 'm', 'o', 'r', 'y', 2, 0})
	if err != nil {
		t.Fatal(err)
	}
	reader := &blockedSource{Reader: strings.NewReader("abcd"), entered: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-reader.release:
		default:
			close(reader.release)
		}
	}()
	ref := &impl.FileReaderRef{Reader: reader, Size: 4, Module: mod, Budget: &impl.FileReadBudget{MaxBytes: 64, MaxCallbacks: 8}}
	impl.FileReaders.Mutex.Lock()
	id := impl.FileReaders.Counter
	impl.FileReaders.Counter++
	impl.FileReaders.Refs[id] = ref
	impl.FileReaders.Mutex.Unlock()
	defer func() {
		impl.FileReaders.Mutex.Lock()
		delete(impl.FileReaders.Refs, id)
		impl.FileReaders.Mutex.Unlock()
	}()
	mod.Memory().WriteUint32Le(0, id)
	done := make(chan struct{})
	stack := []uint64{0, 0, 16, 4}
	go func() { (&FPDF_FILEACCESS_CB{}).Call(ctx, mod, stack); close(done) }()
	<-reader.entered
	cancel()
	// Cancel cannot make a synchronous caller's buffer or permit reusable while
	// the application reader still owns the destination slice.
	select {
	case <-done:
		t.Fatal("callback returned while application reader remained blocked")
	case <-time.After(20 * time.Millisecond):
	}
	close(reader.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("callback did not exit after reader returned")
	}
	if stack[0] != 0 || !errors.Is(ref.Budget.Err(), context.Canceled) {
		t.Fatalf("result=%d cause=%v", stack[0], ref.Budget.Err())
	}
}
