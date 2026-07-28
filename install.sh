#!/bin/sh
set -eu

REPOSITORY="${FALKN_REPOSITORY:-punklabs-ai/falkn}"
INSTALL_DIR="${FALKN_INSTALL_DIR:-$HOME/.local/bin}"
RELEASE_BASE_URL="${FALKN_RELEASE_BASE_URL:-https://github.com/${REPOSITORY}/releases/latest/download}"

fail() {
  printf 'falkn: %s\n' "$1" >&2
  exit 1
}

need() {
  command -v "$1" >/dev/null 2>&1 || fail "installation requires $1"
}

need curl
need tar
need awk
need mktemp

case "$(uname -s)" in
  Darwin) operating_system="darwin" ;;
  Linux) operating_system="linux" ;;
  *) fail "unsupported operating system: $(uname -s)" ;;
esac

case "$(uname -m)" in
  arm64|aarch64) architecture="arm64" ;;
  x86_64|amd64) architecture="amd64" ;;
  *) fail "unsupported architecture: $(uname -m)" ;;
esac

asset="falkn_${operating_system}_${architecture}.tar.gz"
temporary_directory="$(mktemp -d "${TMPDIR:-/tmp}/falkn-install.XXXXXX")"
cleanup() {
  [ ! -d "$temporary_directory" ] || rm -r "$temporary_directory"
  rm -f "${INSTALL_DIR}/.falkn.new.$$" "${INSTALL_DIR}/.falknd.new.$$"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

printf 'falkn: downloading %s/%s\n' "$operating_system" "$architecture"
curl -fsSL --retry 3 --connect-timeout 10 --max-time 120 \
  "${RELEASE_BASE_URL}/${asset}" -o "${temporary_directory}/${asset}" \
  || fail "could not download ${asset}"
curl -fsSL --retry 3 --connect-timeout 10 --max-time 30 \
  "${RELEASE_BASE_URL}/checksums.txt" -o "${temporary_directory}/checksums.txt" \
  || fail "could not download checksums.txt"

expected_checksum="$(awk -v asset="$asset" '$2 == asset || $2 == "*" asset { print $1; exit }' "${temporary_directory}/checksums.txt")"
[ -n "$expected_checksum" ] || fail "checksums.txt does not contain ${asset}"

if command -v sha256sum >/dev/null 2>&1; then
  actual_checksum="$(sha256sum "${temporary_directory}/${asset}" | awk '{ print $1 }')"
elif command -v shasum >/dev/null 2>&1; then
  actual_checksum="$(shasum -a 256 "${temporary_directory}/${asset}" | awk '{ print $1 }')"
else
  fail "installation requires sha256sum or shasum"
fi
[ "$actual_checksum" = "$expected_checksum" ] || fail "checksum verification failed for ${asset}"

mkdir "${temporary_directory}/archive"
tar -xzf "${temporary_directory}/${asset}" -C "${temporary_directory}/archive"
for binary in falkn falknd; do
  [ -f "${temporary_directory}/archive/${binary}" ] || fail "${asset} does not contain ${binary}"
done

mkdir -p "$INSTALL_DIR"
for binary in falkn falknd; do
  staged="${INSTALL_DIR}/.${binary}.new.$$"
  cp "${temporary_directory}/archive/${binary}" "$staged"
  chmod 0755 "$staged"
done

# The notification watcher is a separate long-running process. Stop only
# watchers launched from this install directory so an upgrade cannot leave old
# notification policy running beside the new daemon.
watcher_pids=""
for installed_binary in "${INSTALL_DIR}/falkn" "${INSTALL_DIR}/falknd"; do
  [ -x "$installed_binary" ] || continue
  matching_pids="$(ps -Ao pid=,command= | awk -v expected="${installed_binary} notify-watch" '
    {
      pid = $1
      $1 = ""
      sub(/^[[:space:]]+/, "", $0)
      if ($0 == expected && pid ~ /^[0-9]+$/) print pid
    }
  ')"
  [ -z "$matching_pids" ] || watcher_pids="${watcher_pids} ${matching_pids}"
done
if [ -n "$watcher_pids" ]; then
  for watcher_pid in $watcher_pids; do
    kill -TERM "$watcher_pid" 2>/dev/null || true
  done
  attempt=0
  while [ "$attempt" -lt 5 ]; do
    watchers_stopped=true
    for watcher_pid in $watcher_pids; do
      if kill -0 "$watcher_pid" 2>/dev/null; then
        watchers_stopped=false
      fi
    done
    [ "$watchers_stopped" = true ] && break
    attempt=$((attempt + 1))
    sleep 1
  done
  [ "$watchers_stopped" = true ] || fail "could not stop the previous notification watcher"
fi

mv "${INSTALL_DIR}/.falknd.new.$$" "${INSTALL_DIR}/falknd"
mv "${INSTALL_DIR}/.falkn.new.$$" "${INSTALL_DIR}/falkn"

if [ -n "${FALKND_RUNTIME_DIR:-}" ]; then
  runtime_directory="$FALKND_RUNTIME_DIR"
elif [ "$operating_system" = "darwin" ]; then
  runtime_directory="${HOME}/Library/Caches/falkn"
else
  runtime_directory="${XDG_CACHE_HOME:-${HOME}/.cache}/falkn"
fi
if [ -f "${runtime_directory}/notifications.json" ]; then
  mkdir -p "$runtime_directory"
  nohup "${INSTALL_DIR}/falknd" notify-watch \
    >>"${runtime_directory}/notification-watcher.log" 2>&1 </dev/null &
fi

printf 'falkn: installed falkn and falknd in %s\n' "$INSTALL_DIR"
case ":${PATH}:" in
  *":${INSTALL_DIR}:"*) ;;
  *)
    printf 'falkn: add %s to PATH:\n' "$INSTALL_DIR"
    printf '  export PATH="%s:$PATH"\n' "$INSTALL_DIR"
    ;;
esac
printf 'falkn: run falkn to get started\n'
printf 'falkn: after an upgrade, run falkn daemon restart when no sessions are active\n'
