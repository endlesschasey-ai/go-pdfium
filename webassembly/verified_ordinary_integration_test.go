package webassembly_test

import (
	"context"
	"crypto/sha256"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"os"
	"testing"
	"time"
)

func TestOrdinaryEncryptedRecoveryControls(t *testing.T) {
	var wasm []byte
	if path := os.Getenv("PDFIUM_VERIFIED_WASM"); path != "" {
		var err error
		wasm, err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("ordinary API selected artifact=%s sha256=%x", path, sha256.Sum256(wasm))
	} else {
		t.Log("ordinary API uses embedded old artifact; not a new-artifact acceptance run")
	}
	pool, err := webassembly.Init(webassembly.Config{WASM: wasm, MaxTotal: 1, MaxIdle: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, name := range []string{"encrypted-object-stream-normal-xref.pdf", "encrypted-object-stream-rebuilt-xref.pdf"} {
		t.Run(name, func(t *testing.T) {
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
			f, err = os.Open("testdata/verified/" + name)
			if err != nil {
				t.Fatal(err)
			}
			info, _ := f.Stat()
			password := "verified-test"
			doc, err := instance.FPDF_LoadCustomDocument(&requests.FPDF_LoadCustomDocument{Reader: f, Size: info.Size(), Password: &password})
			if err != nil {
				t.Fatal(err)
			}
			count, err := instance.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc.Document})
			if err != nil || count.PageCount != 1 {
				t.Fatalf("count=%+v err=%v", count, err)
			}
			page, err := instance.FPDF_GetPageSizeByIndex(&requests.FPDF_GetPageSizeByIndex{Document: doc.Document, Index: 0})
			if err != nil || page.Width != 300 || page.Height != 200 {
				t.Fatalf("page=%+v err=%v", page, err)
			}
			t.Logf("ordinary API: count=%d dimensions=%gx%g", count.PageCount, page.Width, page.Height)
		})
	}
}
