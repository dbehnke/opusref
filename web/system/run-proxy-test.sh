#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd "$script_dir/../.." && pwd)"
task_dir="$(mktemp -d "$repo_dir/.opusref-system-test.XXXXXX")"
processes=()

finish() {
  result=$?
  if [[ $result -ne 0 ]]; then
    for log_file in "$task_dir"/*.log; do
      [[ -f "$log_file" ]] || continue
      echo "--- $(basename "$log_file") ---" >&2
      tail -n 80 "$log_file" >&2
    done
  fi
  for process in "${processes[@]}"; do
    kill "$process" 2>/dev/null || true
  done
  wait "${processes[@]}" 2>/dev/null || true
  rm -rf "$task_dir"
  return "$result"
}
trap finish EXIT INT TERM

if ! command -v nginx >/dev/null && ! command -v docker >/dev/null; then
  echo "nginx or Docker is required for the system proxy gate" >&2
  exit 1
fi
mkdir -p "$task_dir/recordings"
sed "s|__SYSTEM_DIR__|$task_dir|g" "$script_dir/opusrefweb.yaml" > "$task_dir/opusrefweb.yaml"
sed "s|__SYSTEM_DIR__|$task_dir|g" "$script_dir/nginx.conf" > "$task_dir/nginx.conf"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=localhost" -addext "subjectAltName=DNS:localhost" -keyout "$task_dir/key.pem" -out "$task_dir/cert.pem" >/dev/null 2>&1

go build -o "$task_dir/opusrefd" "$repo_dir/cmd/opusrefd"
go build -o "$task_dir/opusrefweb" "$repo_dir/cmd/opusrefweb"
"$task_dir/opusrefd" -config "$script_dir/reflector.yaml" >"$task_dir/reflector.log" 2>&1 & processes+=("$!")
"$task_dir/opusrefweb" serve -config "$task_dir/opusrefweb.yaml" >"$task_dir/web.log" 2>&1 & processes+=("$!")
if command -v nginx >/dev/null; then
  nginx -c "$task_dir/nginx.conf" -g "daemon off;" >"$task_dir/nginx.log" 2>&1 & processes+=("$!")
else
  cp "$script_dir/nginx-docker.conf" "$task_dir/nginx-docker.conf"
  docker run --rm --add-host host.docker.internal:host-gateway -p 18443:443 -v "$task_dir:/work:ro" nginx:1.28-alpine nginx -c /work/nginx-docker.conf -g "daemon off;" >"$task_dir/nginx.log" 2>&1 & processes+=("$!")
fi

OPUSREF_SYSTEM_URL="https://localhost:18443" npx playwright test --config system/playwright.config.ts --project=chromium
