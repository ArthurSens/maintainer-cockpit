#!/usr/bin/env bash
set -euo pipefail

image="${IMAGE:-maintainer-cockpit:smoke}"
platform="${PLATFORM:-linux/amd64}"
expected_version="${EXPECTED_VERSION:-dev}"
container=""
volume="maintainer-cockpit-smoke-${RANDOM}-$$"

cleanup() {
  if [[ -n "${container}" ]]; then
    docker rm --force "${container}" >/dev/null 2>&1 || true
  fi
  docker volume rm --force "${volume}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker buildx build --load --platform "${platform}" \
  --build-arg "VERSION=${expected_version}" \
  --tag "${image}" .
docker volume create "${volume}" >/dev/null
container="$(docker run --detach --platform "${platform}" --publish 127.0.0.1::8765 \
  --volume "${volume}:/var/lib/maintainer-cockpit" "${image}" \
  serve \
  --config /etc/maintainer-cockpit/container-smoke.yaml \
  --database /var/lib/maintainer-cockpit/maintainer-cockpit.db \
  --listen 0.0.0.0:8765 \
  --fixture /usr/share/maintainer-cockpit/fixtures/demo-pull-requests.json)"
port="$(docker port "${container}" 8765/tcp)"
port="${port##*:}"

payload=""
for _ in {1..100}; do
  if payload="$(curl --fail --silent "http://127.0.0.1:${port}/api/collections")"; then
    break
  fi
  sleep 0.1
done

if [[ "${payload}" != *'"id":"prometheus"'* ]]; then
  docker logs "${container}"
  echo "collection API did not return the configured Prometheus collection" >&2
  exit 1
fi

for probe in healthy ready; do
  probe_payload="$(curl --fail --silent "http://127.0.0.1:${port}/-/${probe}")"
  if [[ "${probe_payload}" != '{"status":"ok"}' ]]; then
    docker logs "${container}"
    echo "/-/${probe} disclosed an unexpected response" >&2
    exit 1
  fi
done

pull_requests="$(curl --fail --silent "http://127.0.0.1:${port}/api/collections/prometheus/pull-requests")"
if [[ "${pull_requests}" != *'"title":"Demonstrate the SQLite-backed collection"'* ]]; then
  docker logs "${container}"
  echo "pull-request API did not return the persisted demo fixture" >&2
  exit 1
fi

page="$(curl --fail --silent "http://127.0.0.1:${port}/collections/prometheus")"
if [[ "${page}" != *"Maintainer Cockpit"* ]]; then
  docker logs "${container}"
  echo "collection page did not render the product shell" >&2
  exit 1
fi

actual_version="$(docker run --rm --platform "${platform}" "${image}" version --short)"
if [[ "${actual_version}" != "${expected_version}" ]]; then
  echo "image version ${actual_version} does not match ${expected_version}" >&2
  exit 1
fi

if docker run --rm --platform "${platform}" --entrypoint /bin/sh "${image}" -c true >/dev/null 2>&1; then
  echo "runtime image unexpectedly contains a shell" >&2
  exit 1
fi

docker stop --time 10 "${container}" >/dev/null
if [[ "$(docker inspect --format '{{.State.ExitCode}}' "${container}")" != "0" ]]; then
  docker logs "${container}"
  echo "container did not shut down gracefully" >&2
  exit 1
fi

docker rm "${container}" >/dev/null
container="$(docker run --detach --platform "${platform}" --publish 127.0.0.1::8765 \
  --volume "${volume}:/var/lib/maintainer-cockpit" "${image}" \
  serve \
  --config /etc/maintainer-cockpit/container-smoke.yaml \
  --database /var/lib/maintainer-cockpit/maintainer-cockpit.db \
  --listen 0.0.0.0:8765 \
  --fixture -)"
port="$(docker port "${container}" 8765/tcp)"
port="${port##*:}"
ready=""
for _ in {1..100}; do
  if ready="$(curl --fail --silent "http://127.0.0.1:${port}/-/ready")"; then
    break
  fi
  sleep 0.1
done
if [[ "${ready}" != '{"status":"ok"}' ]]; then
  docker logs "${container}"
  echo "restarted container did not become ready" >&2
  exit 1
fi
persisted="$(curl --fail --silent "http://127.0.0.1:${port}/api/collections/prometheus/pull-requests")"
if [[ "${persisted}" != *'"title":"Demonstrate the SQLite-backed collection"'* ]]; then
  docker logs "${container}"
  echo "persisted data was not available after restart" >&2
  exit 1
fi
