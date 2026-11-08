#!/usr/bin/env bash
# Exercise the final scratch images, including lib/pq's TLS verification.
# Requires Docker, OpenSSL, and images built for the requested architectures.
set -euo pipefail

if (( $# == 0 )); then
  echo "usage: $0 IMAGE [IMAGE ...]" >&2
  exit 1
fi

work=$(mktemp -d)
network="pg-exporter-tls-$(basename "$work" | tr '[:upper:]' '[:lower:]')"
postgres_id=
image_id=
cleanup() {
  if [[ -n "$image_id" ]]; then docker rm -f "$image_id" >/dev/null 2>&1 || true; fi
  if [[ -n "$postgres_id" ]]; then docker rm -f -v "$postgres_id" >/dev/null 2>&1 || true; fi
  docker network rm "$network" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

mkdir "$work/tls"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
  -subj '/CN=PG Exporter container test CA' \
  -addext 'basicConstraints=critical,CA:TRUE' \
  -addext 'keyUsage=critical,keyCertSign,cRLSign' \
  -keyout "$work/ca.key" -out "$work/tls/ca.crt" >"$work/openssl.log" 2>&1
openssl req -new -newkey rsa:2048 -nodes -subj '/CN=pg-tls' \
  -keyout "$work/tls/server.key" -out "$work/server.csr" >>"$work/openssl.log" 2>&1
cat >"$work/server.ext" <<'EOF'
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=DNS:pg-tls
EOF
openssl x509 -req -days 1 -in "$work/server.csr" \
  -CA "$work/tls/ca.crt" -CAkey "$work/ca.key" -CAcreateserial \
  -extfile "$work/server.ext" -out "$work/tls/server.crt" >>"$work/openssl.log" 2>&1

docker network create "$network" >/dev/null
postgres_id=$(docker run -d --rm --network "$network" \
  --network-alias pg-tls --network-alias wrong-host \
  -e POSTGRES_HOST_AUTH_METHOD=trust \
  -v "$work/tls:/tls:ro" postgres:18-alpine sh -ec '
    cp /tls/server.crt /tls/server.key /tmp/
    chown postgres /tmp/server.key
    chmod 600 /tmp/server.key
    exec docker-entrypoint.sh postgres -c ssl=on \
      -c ssl_cert_file=/tmp/server.crt -c ssl_key_file=/tmp/server.key
  ')
ready=false
for (( attempt=0; attempt<60; attempt++ )); do
  if docker exec "$postgres_id" pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 1
done
if [[ "$ready" != true ]]; then
  docker logs "$postgres_id" >&2
  exit 1
fi

for image in "$@"; do
  echo "Verifying $image"
  platform=$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$image")
  image_id=$(docker create --platform "$platform" "$image")
  docker cp "$image_id:/etc/ssl/certs/ca-certificates.crt" "$work/image-ca.crt"
  docker cp "$image_id:/LICENSE" "$work/LICENSE"
  docker cp "$image_id:/etc/pg_exporter.yml" "$work/pg_exporter.yml"
  docker rm "$image_id" >/dev/null
  image_id=
  cmp LICENSE "$work/LICENSE"
  cmp pg_exporter.yml "$work/pg_exporter.yml"
  test -s "$work/image-ca.crt"
  # Parse the bundled roots, rather than accepting an arbitrary nonempty file.
  openssl crl2pkcs7 -nocrl -certfile "$work/image-ca.crt" -out "$work/roots.p7b"
  openssl pkcs7 -in "$work/roots.p7b" -print_certs -out "$work/roots.pem"
  grep -q '^-----BEGIN CERTIFICATE-----$' "$work/roots.pem"
  cat "$work/image-ca.crt" "$work/tls/ca.crt" >"$work/system-ca.crt"

  docker run --rm --platform "$platform" "$image" --version
  for mode in verify-ca verify-full; do
    url="postgres://postgres@pg-tls:5432/postgres?sslmode=$mode"
    args=(--fail-fast --no-auto-discovery --connect-timeout=5000 --explain)

    # The generated private CA must not be trusted by the unmodified image.
    if docker run --rm --platform "$platform" --network "$network" \
      "$image" "${args[@]}" --url "$url" >"$work/untrusted.log" 2>&1; then
      echo "$image accepted an untrusted CA ($mode)" >&2
      exit 1
    fi
    if ! grep -q 'certificate signed by unknown authority' "$work/untrusted.log"; then
      cat "$work/untrusted.log" >&2
      exit 1
    fi

    # An explicit private CA remains supported.
    if ! docker run --rm --platform "$platform" --network "$network" \
      -v "$work/tls/ca.crt:/test-ca.crt:ro" "$image" "${args[@]}" \
      --url "$url&sslrootcert=/test-ca.crt" >"$work/trusted.log" 2>&1; then
      cat "$work/trusted.log" >&2
      exit 1
    fi

    # Exercise the default system-root lookup, without sslrootcert. Only this
    # test mount adds the private CA; the published image remains unmodified.
    if ! docker run --rm --platform "$platform" --network "$network" \
      -v "$work/system-ca.crt:/etc/ssl/certs/ca-certificates.crt:ro" \
      "$image" "${args[@]}" --url "$url" >"$work/system.log" 2>&1; then
      cat "$work/system.log" >&2
      exit 1
    fi
  done

  if docker run --rm --platform "$platform" --network "$network" \
    -v "$work/tls/ca.crt:/test-ca.crt:ro" "$image" "${args[@]}" \
    --url 'postgres://postgres@wrong-host:5432/postgres?sslmode=verify-full&sslrootcert=/test-ca.crt' \
    >"$work/hostname.log" 2>&1; then
    echo "$image accepted a mismatched hostname" >&2
    exit 1
  fi
  if ! grep -q 'certificate is valid for pg-tls, not wrong-host' "$work/hostname.log"; then
    cat "$work/hostname.log" >&2
    exit 1
  fi
  echo "$image: CA bundle, license, configuration, and TLS checks passed"
done
