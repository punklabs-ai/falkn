#!/bin/sh
set -eu

# Install Falkn on the development machines from the current checkout.
#
# install.sh downloads a published release, and no release exists yet, so this
# script covers local deployment instead. It builds the binaries, copies them to
# every account that runs Falkn, and follows the same upgrade ordering install.sh
# uses: stop the account's notification watcher, replace both binaries
# atomically, restart the daemon, then bring the watcher back.
#
# Targets are written host:account, where the host is "local" or an SSH
# destination. Deploying to an account other than the logged-in one uses sudo
# and will ask for that host's password. With no arguments the script installs
# on this machine for the current account; set FALKN_DEPLOY_TARGETS to keep a
# standing list of machines outside the repository.
#
#   scripts/deploy-local.sh                      this machine, current account
#   scripts/deploy-local.sh local:alice          one account here
#   scripts/deploy-local.sh build-host:falkn     one account on another host

DEFAULT_TARGETS="${FALKN_DEPLOY_TARGETS:-local:$(id -un)}"

fail() {
  printf 'deploy-local: %s\n' "$1" >&2
  exit 1
}

repository_root="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
[ -f "${repository_root}/VERSION" ] || fail "VERSION file is missing"

version="$(cat "${repository_root}/VERSION")-local.$(
  git -C "$repository_root" rev-parse --short HEAD 2>/dev/null || echo unknown
)"
if [ -n "$(git -C "$repository_root" status --porcelain 2>/dev/null)" ]; then
  # A build from uncommitted sources cannot be reproduced from any commit, so
  # the version says so rather than passing for the revision it names.
  version="${version}.dirty"
fi

targets="${*:-$DEFAULT_TARGETS}"
staging_root="$(mktemp -d "${TMPDIR:-/tmp}/falkn-deploy.XXXXXX")"
trap 'rm -rf "$staging_root"' EXIT HUP INT TERM

# Runs on the target host as the target account. Mirrors install.sh's ordering.
cat > "${staging_root}/install-account.sh" <<'ACCOUNT_SCRIPT'
set -eu
staged_directory="$1"
install_directory="${HOME}/.local/bin"
mkdir -p "$install_directory"

# Stop only this account's watcher, and only one started from this install
# directory, so no watcher outlives the binary it was launched from.
watcher_pids="$(ps -Ao user=,pid=,command= | awk -v account="$(id -un)" -v expected="${install_directory}/falkn notify-watch" '
  {
    user = $1
    pid = $2
    $1 = ""
    $2 = ""
    sub(/^[[:space:]]+/, "", $0)
    if (user == account && $0 == expected && pid ~ /^[0-9]+$/) print pid
  }
')"
for watcher_pid in $watcher_pids; do
  kill -TERM "$watcher_pid" 2>/dev/null || true
done
attempt=0
while [ "$attempt" -lt 5 ]; do
  watchers_stopped=true
  for watcher_pid in $watcher_pids; do
    if kill -0 "$watcher_pid" 2>/dev/null; then watchers_stopped=false; fi
  done
  [ "$watchers_stopped" = true ] && break
  attempt=$((attempt + 1))
  sleep 1
done
[ "${watchers_stopped:-true}" = true ] || {
  printf 'deploy-local: could not stop the previous notification watcher\n' >&2
  exit 1
}

for binary in falkn falknd; do
  cp "${staged_directory}/${binary}" "${install_directory}/.${binary}.new.$$"
  chmod 0755 "${install_directory}/.${binary}.new.$$"
done
mv "${install_directory}/.falknd.new.$$" "${install_directory}/falknd"
mv "${install_directory}/.falkn.new.$$" "${install_directory}/falkn"

# The daemon serves from its old process image until it restarts, and it refuses
# to restart while sessions are running. Let that refusal surface as a failure.
"${install_directory}/falkn" daemon restart

runtime_directory="${FALKND_RUNTIME_DIR:-}"
if [ -z "$runtime_directory" ]; then
  case "$(uname -s)" in
    Darwin) runtime_directory="${HOME}/Library/Caches/falkn" ;;
    *) runtime_directory="${XDG_CACHE_HOME:-${HOME}/.cache}/falkn" ;;
  esac
fi
if [ -f "${runtime_directory}/notifications.json" ]; then
  nohup "${install_directory}/falknd" notify-watch \
    >>"${runtime_directory}/notification-watcher.log" 2>&1 </dev/null &
fi

printf 'deploy-local:   %s is now %s\n' "$(id -un)" "$("${install_directory}/falkn" version)"
ACCOUNT_SCRIPT

# Build once per platform; several targets usually share a host.
build_for_platform() {
  platform_directory="${staging_root}/${1}_${2}"
  [ ! -d "$platform_directory" ] || return 0
  mkdir -p "$platform_directory"
  printf 'deploy-local: building %s for %s/%s\n' "$version" "$1" "$2"
  for command in falkn falknd; do
    CGO_ENABLED=0 GOOS="$1" GOARCH="$2" go build \
      -trimpath \
      -ldflags "-s -w -X github.com/punklabs-ai/falkn/internal/buildinfo.Version=${version}" \
      -o "${platform_directory}/${command}" \
      "${repository_root}/cmd/${command}" \
      || fail "could not build ${command} for ${1}/${2}"
  done
}

resolve_platform() {
  if [ "$1" = "local" ]; then
    host_description="$(uname -s) $(uname -m)"
  else
    host_description="$(ssh "$1" 'uname -s; uname -m' | tr '\n' ' ')"
  fi
  case "$host_description" in
    Darwin*) host_os="darwin" ;;
    Linux*) host_os="linux" ;;
    *) fail "unsupported operating system on $1: ${host_description}" ;;
  esac
  case "$host_description" in
    *arm64*|*aarch64*) host_arch="arm64" ;;
    *x86_64*|*amd64*) host_arch="amd64" ;;
    *) fail "unsupported architecture on $1: ${host_description}" ;;
  esac
}

login_account_for_host() {
  if [ "$1" = "local" ]; then
    id -un
  else
    # ssh -G reports the effective configuration; its warnings are not ours.
    ssh -G "$1" 2>/dev/null | awk '/^user /{ print $2; exit }'
  fi
}

run_on_host() {
  if [ "$1" = "local" ]; then
    shift
    sh -c "$*"
  else
    host_name="$1"
    shift
    ssh "$host_name" "$*"
  fi
}

for target in $targets; do
  case "$target" in
    *:*) ;;
    *) fail "target ${target} is not written host:account" ;;
  esac
  host="${target%%:*}"
  account="${target#*:}"

  resolve_platform "$host"
  build_for_platform "$host_os" "$host_arch"
  built_directory="${staging_root}/${host_os}_${host_arch}"

  printf 'deploy-local: installing on %s\n' "$target"
  staged_directory="$(run_on_host "$host" "mktemp -d /tmp/falkn-staged.XXXXXX")"
  if [ "$host" = "local" ]; then
    cp "${built_directory}/falkn" "${built_directory}/falknd" \
      "${staging_root}/install-account.sh" "$staged_directory"
  else
    # Plain scp fails on hosts whose SFTP subsystem is unavailable; -O selects
    # the original protocol, which those hosts still accept.
    scp -O -q "${built_directory}/falkn" "${built_directory}/falknd" \
      "${staging_root}/install-account.sh" "${host}:${staged_directory}/"
  fi
  # Another account must be able to read the staged files through sudo.
  run_on_host "$host" "chmod 0755 ${staged_directory} && chmod 0644 ${staged_directory}/falkn ${staged_directory}/falknd ${staged_directory}/install-account.sh"

  install_command="sh ${staged_directory}/install-account.sh ${staged_directory}"
  if [ "$account" != "$(login_account_for_host "$host")" ]; then
    install_command="sudo -u ${account} ${install_command}"
    # sudo may prompt for a password, which needs a terminal on a remote host.
    [ "$host" = "local" ] || install_command="TERMINAL_REQUIRED ${install_command}"
  fi
  case "$install_command" in
    TERMINAL_REQUIRED*)
      ssh -t "$host" "${install_command#TERMINAL_REQUIRED }" || fail "installation failed on ${target}"
      ;;
    *)
      run_on_host "$host" "$install_command" || fail "installation failed on ${target}"
      ;;
  esac

  run_on_host "$host" "rm -rf ${staged_directory}"
done

printf 'deploy-local: installed %s on:%s\n' "$version" "$(printf ' %s' $targets)"
