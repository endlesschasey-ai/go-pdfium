#!/bin/bash
set -euo pipefail
# Run only in the disposable /work mount inside the pinned build container.
[[ "$PWD" == /work && ! -e /work/source ]] || exit 2
(cd /inputs && sha256sum --check SHA256SUMS)
export DEPOT_TOOLS_UPDATE=0 DEPOT_TOOLS_METRICS=0
export SOURCE_DATE_EPOCH=1780617600 TZ=UTC LC_ALL=C
export PDFium_TARGET_OS=emscripten PDFium_TARGET_CPU=wasm-standalone
export PDFium_ENABLE_V8=false PDFium_IS_DEBUG=false
export PDFium_SOURCE_DIR=/work/source/pdfium PDFium_BUILD_DIR=/work/source/pdfium/out
mkdir -p /work/evidence
exec > >(tee /work/evidence/build.log) 2>&1
trap 'status=$?; cat /sys/fs/cgroup/memory.peak > /work/evidence/container-memory-peak.txt; cat /sys/fs/cgroup/cpu.stat > /work/evidence/container-cpu.stat; exit "$status"' EXIT
check_available_memory() {
  python3 - <<'MEMORY'
from pathlib import Path
mem = dict(line.split(':', 1) for line in Path('/proc/meminfo').read_text().splitlines())
available_kib = int(mem['MemAvailable'].split()[0])
print(f'VM MemAvailable={available_kib} KiB; required=5242880 KiB')
if available_kib < 5 * 1024 * 1024:
    raise SystemExit('Insufficient VM memory for 4 GiB build plus 1 GiB margin')
MEMORY
}
check_available_memory
uname -a
python3 --version
git --version
/emsdk/upstream/emscripten/emcc --version | tee /work/evidence/emcc-version.txt
grep -F '5.0.5' /work/evidence/emcc-version.txt
[[ $(node --version) == v22.16.0 ]]
/emsdk/upstream/bin/clang --version
/emsdk/upstream/bin/wasm-opt --version
# The immutable image supplies SDK contents. Do not install or activate a
# different SDK. Hash the actual input files before patching its copied SDK.
find /emsdk -type f -print0 | sort -z | xargs -0 sha256sum > /work/evidence/sdk-files.sha256
git clone --filter=blob:none --no-checkout https://chromium.googlesource.com/chromium/tools/depot_tools.git /work/depot_tools
git -C /work/depot_tools checkout --detach 46afe8bfbb57583700c01d1584e7a49638d586ed
[[ $(git -C /work/depot_tools rev-parse HEAD) == 46afe8bfbb57583700c01d1584e7a49638d586ed ]]
touch /work/depot_tools/.disable_auto_update
export PATH=/work/depot_tools:$PATH
mkdir /work/source
cd /work/source
gclient config --unmanaged https://pdfium.googlesource.com/pdfium.git --custom-var checkout_configuration=small
printf "target_os = [ 'emscripten' ]\n" >> .gclient
gclient sync --revision pdfium@91b9d569b34be4f38eed7b3c49b227356c3aadad --no-history --shallow --jobs=2
[[ $(git -C pdfium rev-parse HEAD) == 91b9d569b34be4f38eed7b3c49b227356c3aadad ]]
[[ $(git -C pdfium/build rev-parse HEAD) == 613f5c13bccbc15bd7ce8da9acb13ac06459f8cb ]]
gclient revinfo -a > /work/evidence/source-revisions.txt
# Resolve no moving GN/Ninja versions; retrieve immutable package instances.
printf 'gn/gn/linux-amd64 2w75WgzO2nc2UMXt6PDqnK3LCqbHZ-11ME5WIr4hJQQC\n' > /work/gn.ensure
printf 'infra/3pp/tools/ninja/linux-amd64 Px8cwPaaG8_fZ_tsK8dBmx3YEruNDnmvqb-oo1U7UIIC\n' > /work/ninja.ensure
cipd ensure -root /work/tools/gn -ensure-file /work/gn.ensure
cipd ensure -root /work/tools/ninja -ensure-file /work/ninja.ensure
export PATH=/work/tools/gn:/work/tools/ninja:$PATH
command -v gn ninja
gn --version
ninja --version
sha256sum "$(command -v gn)" "$(command -v ninja)" > /work/evidence/build-tools.sha256
for name in public_headers wasm/pdfium wasm/callbacks; do
  patch --batch --fuzz=0 -p1 -d pdfium -i "/inputs/patches/$name.patch"
done
patch --batch --fuzz=0 -p1 -d pdfium/build -i /inputs/patches/wasm/build.patch
mkdir -p pdfium/build/config/wasm
cp /inputs/patches/wasm/config.gn pdfium/build/config/wasm/BUILD.gn
patch --batch --fuzz=0 -p1 -d pdfium -i /inputs/patches/wasm/verified_page_tree.patch
[[ ! -e pdfium/third_party/emsdk ]]
cp -a /emsdk pdfium/third_party/emsdk
patch --batch --fuzz=0 -p1 -d pdfium/third_party/emsdk/upstream/emscripten -i /inputs/patches/wasm/emscripten.patch
# A new container-local cache avoids sharing file locks through the host bind.
export EM_CACHE=/var/tmp/pdfium-emscripten-cache
export EM_CONFIG=/work/emscripten-config.py
python3 - <<'CONFIG'
from pathlib import Path
import shutil
sdk='/work/source/pdfium/third_party/emsdk'
Path('/work/emscripten-config.py').write_text(
    f'LLVM_ROOT = {sdk + "/upstream/bin"!r}\n'
    f'BINARYEN_ROOT = {sdk + "/upstream"!r}\n'
    f'NODE_JS = {[shutil.which("node")]!r}\n')
CONFIG
export PATH=/work/source/pdfium/third_party/emsdk/upstream/emscripten:/work/source/pdfium/third_party/emsdk/upstream/bin:$PATH
bash -euo pipefail /inputs/steps/05-configure.sh
check_available_memory
bash -euo pipefail /inputs/steps/06-build.sh
bash -euo pipefail /inputs/steps/08-licenses.sh
cp pdfium/out/args.gn /work/evidence/args.gn
cp pdfium/out/build.ninja /work/evidence/build.ninja
sha256sum pdfium/out/pdfium.wasm > /work/evidence/wasm.sha256
[[ $(git -C /work/depot_tools rev-parse HEAD) == 46afe8bfbb57583700c01d1584e7a49638d586ed ]]
git -C pdfium diff --binary > /work/evidence/pdfium.diff
git -C pdfium/build diff --binary > /work/evidence/chromium-build.diff
