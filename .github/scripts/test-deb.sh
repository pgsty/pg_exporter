#!/usr/bin/env bash
# Run in a Debian container from the repository root, with artifacts in $1.
set -euo pipefail
shopt -s nullglob
debs=("${1:?usage: test-deb.sh ARTIFACT_DIRECTORY}"/*.deb)
test "${#debs[@]}" -gt 0
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
native_arch=$(dpkg --print-architecture)
installed=false

for deb in "${debs[@]}"; do
  echo "Verifying $deb"
  test "$(dpkg-deb --field "$deb" Package)" = pg-exporter
  arch=$(dpkg-deb --field "$deb" Architecture)
  case "$arch" in amd64|arm64|ppc64el) ;; *) exit 1 ;; esac
  # The filename uses Go's ppc64le spelling; Debian metadata uses ppc64el.
  file_arch=$arch
  if [[ "$arch" == ppc64el ]]; then file_arch=ppc64le; fi
  [[ "$(basename "$deb")" == pg-exporter_*_"$file_arch".deb ]]

  root="$work/$arch"
  dpkg-deb --extract "$deb" "$root"
  dpkg-deb --control "$deb" "$root/DEBIAN"
  test -x "$root/usr/bin/pg_exporter"
  cmp LICENSE "$root/usr/share/doc/pg_exporter/LICENSE"
  cmp pg_exporter.yml "$root/etc/pg_exporter.yml"
  cmp package/pg_exporter.default "$root/etc/default/pg_exporter"
  cmp package/pg_exporter.service "$root/lib/systemd/system/pg_exporter.service"
  test "$(stat -c %a "$root/etc/pg_exporter.yml")" = 700
  test "$(stat -c %a "$root/etc/default/pg_exporter")" = 700
  test "$(stat -c %a "$root/usr/share/doc/pg_exporter/LICENSE")" = 644
  grep -qx /etc/pg_exporter.yml "$root/DEBIAN/conffiles"
  grep -qx /etc/default/pg_exporter "$root/DEBIAN/conffiles"
  test -x "$root/DEBIAN/preinst"

  if [[ "$arch" == "$native_arch" ]]; then
    dpkg --install "$deb"
    test "$(stat -c %U:%G /etc/pg_exporter.yml)" = prometheus:prometheus
    test "$(stat -c %U:%G /etc/default/pg_exporter)" = prometheus:prometheus
    runuser -u prometheus -- /usr/bin/pg_exporter --version
    installed=true
  fi
done
test "$installed" = true
