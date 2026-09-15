# PDFium WASM build inputs

Run `python3 run-bounded.py /absolute/new-output-directory` from a frozen copy of this directory. The output directory must be new. The launcher uses the pinned Linux amd64 image,2CPUs,4GiB memory+swap,Ninja-j2 and90-minute total timeout. Both memory checks require5GiB available in the Docker VM. Build inputs are read-only; the SDK cache uses the dedicated container filesystem.

The source patch adds the WASM verified-open entry point. Ordinary loaders retain their existing behavior. Count verification uses the original page tree, strict dependent metadata-stream decoding and actual indexed-page identity. It rejects truncated/overflowed Count tokens, checks traversal limits, and retains decode failure across recovery. It does not validate arbitrary page contents.

`build-info.json` identifies the shipped binary, exact source/toolchain inputs and actual verification. Build002 compiled and linked this binary but exited at a missing HarfBuzz license mapping. The included license collector contains that isolated correction and subsequently collected notices successfully without rebuilding the binary. This distinction is retained in the record. A second identical successful build has not been run, so bit-for-bit reproducibility is not claimed.

The PDFium and SDK notices accompanying the binary are under `../pdfium-licenses/`. SDK notice collection includes a conservative source superset; the build did not emit a final archive-member map.
