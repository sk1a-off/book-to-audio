#!/usr/bin/env bash

set -Eeuo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
manage_script="${project_root}/scripts/manage.sh"
temporary_dir="$(mktemp -d)"
trap 'rm -rf -- "${temporary_dir}"' EXIT

fake_bin="${temporary_dir}/bin"
docker_log="${temporary_dir}/docker.log"
test_env="${temporary_dir}/compose.env"
mkdir -p -- "${fake_bin}"

cat >"${fake_bin}/docker" <<'FAKE_DOCKER'
#!/usr/bin/env bash
set -Eeuo pipefail

printf '%q ' "$@" >>"${DOCKER_TEST_LOG}"
printf '\n' >>"${DOCKER_TEST_LOG}"

if [[ "${1:-}" == "image" && "${2:-}" == "ls" ]]; then
  printf '%s\n' "sha256:managed-dangling" "sha256:managed-dangling"
fi

if [[ "${1:-}" == "volume" &&
  "${2:-}" == "inspect" &&
  "${3:-}" == "--format" ]]; then
  if [[ "${4:-}" == *"com.tts-microservice-mvp.managed"* ]]; then
    printf 'true\n'
  elif [[ "${4:-}" == *"com.docker.compose.project"* ]]; then
    printf 'tts-microservice-mvp\n'
  fi
fi
FAKE_DOCKER
chmod 0755 "${fake_bin}/docker"

export PATH="${fake_bin}:${PATH}"
export DOCKER_TEST_LOG="${docker_log}"
export TTS_MVP_ENV_FILE="${test_env}"
export TTS_MVP_COMPOSE_FILE="${project_root}/deploy/compose/compose.yaml"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_log_contains() {
  local expected="$1"
  grep --fixed-strings --quiet -- "${expected}" "${docker_log}" ||
    fail "docker log does not contain: ${expected}"
}

assert_log_excludes() {
  local unexpected="$1"
  if grep --fixed-strings --quiet -- "${unexpected}" "${docker_log}"; then
    fail "docker log unexpectedly contains: ${unexpected}"
  fi
}

first_line() {
  local expected="$1"
  local result
  result="$(
    grep --fixed-strings --line-number -- "${expected}" "${docker_log}" |
      head -n 1 |
      cut -d: -f1
  )"
  [[ -n "${result}" ]] || fail "cannot find command order marker: ${expected}"
  printf '%s\n' "${result}"
}

assert_before() {
  local first="$1"
  local second="$2"
  local first_number
  local second_number
  first_number="$(first_line "${first}")"
  second_number="$(first_line "${second}")"
  ((first_number < second_number)) ||
    fail "expected '${first}' before '${second}'"
}

reset_log() {
  : >"${docker_log}"
}

run_manage() {
  reset_log
  # All Docker calls are handled by the fake binary above. Keep normal test
  # output concise so simulated destructive commands cannot be mistaken for
  # real cleanup of the developer's Docker resources.
  bash "${manage_script}" "$@" >/dev/null 2>&1
}

bash -n "${manage_script}"

help_output="$(bash "${manage_script}" help)"
[[ "${help_output}" == *"четыре именованных образа"* ]] ||
  fail "help does not document four project images"
[[ "${help_output}" == *"четыре явно именованных project volumes"* ]] ||
  fail "help does not document four project volumes"

run_manage rebuild
[[ -f "${test_env}" ]] || fail ".env bootstrap did not create an env file"
[[ "$(stat -c '%a' "${test_env}")" == "600" ]] ||
  fail "bootstrapped env permissions are not 600"
grep --fixed-strings --quiet -- "REWRITER_N_CTX=8192" "${test_env}" ||
  fail "bootstrapped env misses the CPU-first rewriter context"
assert_log_contains "compose --project-name tts-microservice-mvp"
assert_log_contains "down --remove-orphans"
assert_log_contains \
  "image rm tts-microservice-mvp/audiobook-api:local"
assert_log_contains "tts-microservice-mvp/omnivoice-worker:local"
assert_log_contains "tts-microservice-mvp/stt-worker:local"
assert_log_contains "tts-microservice-mvp/rewriter-worker:local"
assert_log_contains "dangling=true"
assert_log_contains "com.tts-microservice-mvp.managed=true"
assert_log_contains "image rm sha256:managed-dangling"
assert_log_contains "build --pull --no-cache"
assert_log_contains "up --detach --wait --wait-timeout 900"
assert_before "down --remove-orphans" "image rm tts-microservice-mvp/audiobook-api"
assert_before "image rm sha256:managed-dangling" "build --pull --no-cache"
assert_before "build --pull --no-cache" "up --detach --wait"

run_manage clean
assert_log_contains "down --remove-orphans"
assert_log_contains "image rm tts-microservice-mvp/audiobook-api:local"
assert_log_excludes "volume rm"
assert_log_excludes "--volumes"

reset_log
if bash "${manage_script}" reset >/dev/null 2>&1; then
  fail "reset without --yes unexpectedly succeeded"
fi
[[ ! -s "${docker_log}" ]] ||
  fail "reset without --yes invoked Docker"

run_manage reset --yes
assert_log_contains "down --remove-orphans"
assert_log_contains "volume rm tts-microservice-mvp_postgres-data"
assert_log_contains "volume rm tts-microservice-mvp_omnivoice-models"
assert_log_contains "volume rm tts-microservice-mvp_stt-models"
assert_log_contains "volume rm tts-microservice-mvp_rewriter-models"
assert_log_excludes "image rm"

reset_log
if bash "${manage_script}" full-reset >/dev/null 2>&1; then
  fail "full-reset without --yes unexpectedly succeeded"
fi
[[ ! -s "${docker_log}" ]] ||
  fail "full-reset without --yes invoked Docker"

run_manage full-reset --yes
assert_log_contains "down --remove-orphans"
assert_log_contains "volume rm tts-microservice-mvp_postgres-data"
assert_log_contains "volume rm tts-microservice-mvp_omnivoice-models"
assert_log_contains "volume rm tts-microservice-mvp_stt-models"
assert_log_contains "volume rm tts-microservice-mvp_rewriter-models"
assert_log_contains "image rm tts-microservice-mvp/audiobook-api:local"
assert_log_contains "tts-microservice-mvp/omnivoice-worker:local"
assert_log_contains "tts-microservice-mvp/stt-worker:local"
assert_log_contains "tts-microservice-mvp/rewriter-worker:local"
assert_log_contains "image rm sha256:managed-dangling"
assert_before "down --remove-orphans" "volume rm tts-microservice-mvp_postgres-data"
assert_before "volume rm tts-microservice-mvp_rewriter-models" "image rm tts-microservice-mvp/audiobook-api"

run_manage restart
assert_log_contains "config --quiet"
assert_log_contains "down --remove-orphans"
assert_log_contains "up --detach --wait --wait-timeout 900"

run_manage stop
assert_log_contains "compose --project-name tts-microservice-mvp"
assert_log_contains "stop"

run_manage down
assert_log_contains "down --remove-orphans"
assert_log_excludes "volume rm"
assert_log_excludes "image rm"

run_manage status
assert_log_contains "ps --all"

run_manage ps
assert_log_contains "ps --all"

run_manage logs stt-worker
assert_log_contains "logs --follow --tail 200 stt-worker"

run_manage logs rewriter-worker
assert_log_contains "logs --follow --tail 200 rewriter-worker"

reset_log
if bash "${manage_script}" logs unknown-service >/dev/null 2>&1; then
  fail "logs accepted an unknown service"
fi
[[ ! -s "${docker_log}" ]] ||
  fail "unknown logs service invoked Docker"

if grep --extended-regexp --quiet \
  '(system|image|container|volume|builder)[[:space:]]+prune' \
  "${manage_script}" "${docker_log}"; then
  fail "global Docker cleanup command detected"
fi

for command in up rebuild restart stop down status ps logs smoke clean reset full-reset; do
  grep --fixed-strings --quiet -- "${command}" "${manage_script}" ||
    fail "manage script does not mention command: ${command}"
done

printf 'manage.sh static command tests: PASS\n'
