# Notices for the embedded PDFium WASM

`pdfium/` contains the PDFium and linked third-party notices collected from the exact source revisions in `../pdfium-build/`. `sdk/` contains Emscripten runtime and standard-library license texts, referenced credits, and a conservative superset of source notices from the pinned SDK. `sdk-manifest.json` records their provenance and hashes; source paths are relative to the SDK root.

The build did not record a final archive-member map. The SDK notice collection therefore includes sources that may have been removed by the linker, rather than claiming every collected source is present in the binary. The complete CC0 text accompanies the dlmalloc reference. The emsdk license records build-tool provenance.

The original build linked the binary, then failed on an unmapped HarfBuzz notice. The corrected collector subsequently passed without rebuilding the binary. The original build status is preserved in `../pdfium-build/build-info.json`.
