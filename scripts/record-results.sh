#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
result_dir="test/results"
mkdir -p "$result_dir"
run_id="$(date -u +%Y%m%dT%H%M%SZ)-$(go env GOOS)-$(go env GOARCH)-$$"
result_path="$result_dir/$run_id"
mkdir "$result_path"
exec > >(tee "$result_path/checks.log") 2>&1
printf 'UTC: %s\n' "$(date -u +%FT%TZ)"
go version
go env GOOS GOARCH CGO_ENABLED
printf 'Parent commit: %s\n' "$(git rev-parse HEAD 2>/dev/null || printf unknown)"
python3 - <<'PY'
import hashlib
from pathlib import Path
root = Path('.')
digest = hashlib.sha256()
for p in sorted(root.rglob('*')):
    if not p.is_file() or '.git' in p.parts or p.parts[:2] == ('test', 'results'):
        continue
    digest.update(p.as_posix().encode() + b'\0' + p.read_bytes() + b'\0')
print('Source tree SHA256:', digest.hexdigest())
PY
go_files="$(find cmd inspect transfer internal -name '*.go' -type f)"
unformatted="$(gofmt -l $go_files)"
if [ -n "$unformatted" ]; then
  printf 'Unformatted files:\n%s\n' "$unformatted"
  exit 1
fi
python3 -m unittest discover -s scripts -p "test_*.py" -v
go vet ./...
go test -race -v -count=1 -timeout=60s ./...
build_dir="$(mktemp -d)"
trap 'rm -rf "$build_dir"' EXIT
go build -o "$build_dir/bitspeed-inspect" ./cmd/bitspeed-inspect
"$build_dir/bitspeed-inspect" --version
python3 scripts/smoke-inspect.py "$build_dir/bitspeed-inspect"
go build -o "$build_dir/bitspeed-transfer" ./cmd/bitspeed-transfer
"$build_dir/bitspeed-transfer" --version
python3 scripts/smoke-transfer.py "$build_dir/bitspeed-transfer"
printf 'PASS: formatting, vet, race tests, both command builds and CLI smoke\n'
