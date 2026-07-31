#!/usr/bin/env bash

set -Eeuo pipefail

readonly project_name="tts-microservice-mvp"
readonly managed_label="com.tts-microservice-mvp.managed=true"

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly project_root
readonly compose_file="${TTS_MVP_COMPOSE_FILE:-${project_root}/deploy/compose/compose.yaml}"
readonly env_file="${TTS_MVP_ENV_FILE:-${project_root}/deploy/compose/.env}"
readonly env_example="${project_root}/deploy/compose/.env.example"
readonly smoke_script="${project_root}/scripts/smoke-api.sh"
readonly wait_timeout="${COMPOSE_WAIT_TIMEOUT:-900}"
readonly logs_tail="${COMPOSE_LOGS_TAIL:-200}"

readonly -a project_images=(
  "tts-microservice-mvp/audiobook-api:local"
  "tts-microservice-mvp/omnivoice-worker:local"
  "tts-microservice-mvp/stt-worker:local"
  "tts-microservice-mvp/rewriter-worker:local"
)

readonly -a project_volumes=(
  "tts-microservice-mvp_postgres-data"
  "tts-microservice-mvp_omnivoice-models"
  "tts-microservice-mvp_stt-models"
  "tts-microservice-mvp_rewriter-models"
)

readonly -a compose_services=(
  "postgres"
  "migrate"
  "omnivoice-worker"
  "stt-worker"
  "rewriter-worker"
  "audiobook-api"
)

compose_command=()

info() {
  printf '[manage] %s\n' "$*"
}

warn() {
  printf '[manage] WARNING: %s\n' "$*" >&2
}

die() {
  printf '[manage] ERROR: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'USAGE'
Управление TTS microservice MVP:

  ./scripts/manage.sh up
      Проверить конфигурацию и запустить контур с ожиданием readiness.

  ./scripts/manage.sh rebuild
      Остановить контур, удалить только его четыре именованных образа и
      managed dangling images, пересобрать с --pull --no-cache и запустить.

  ./scripts/manage.sh restart
      Пересоздать контейнеры без пересборки, сохранив образы и volumes.

  ./scripts/manage.sh stop
      Остановить контейнеры, не удаляя их.

  ./scripts/manage.sh down
      Удалить контейнеры и network проекта, сохранив образы и volumes.

  ./scripts/manage.sh status
  ./scripts/manage.sh ps
      Показать все контейнеры проекта, включая остановленные.

  ./scripts/manage.sh logs [service]
      Следить за последними логами всего контура или одного сервиса.

  ./scripts/manage.sh smoke
      Выполнить полный smoke через публичный Go API.

  ./scripts/manage.sh clean
      Удалить контейнеры и только project images; volumes сохраняются.

  ./scripts/manage.sh reset --yes
      Удалить только четыре явно именованных project volumes с PostgreSQL
      и моделями. Образы сохраняются. Флаг --yes обязателен.

Переменные:
  COMPOSE_WAIT_TIMEOUT  ожидание health в секундах, по умолчанию 900
  COMPOSE_LOGS_TAIL     число начальных строк logs, по умолчанию 200
  API_URL               public API URL для smoke
USAGE
}

bootstrap_env() {
  [[ -f "${env_example}" ]] ||
    die "env template not found: ${env_example}"

  if [[ -e "${env_file}" ]]; then
    [[ -f "${env_file}" ]] ||
      die "env path exists but is not a regular file: ${env_file}"
    return
  fi

  warn "создаю ${env_file} из .env.example"
  warn "проверьте пароли и model settings перед внешним использованием"
  mkdir -p -- "$(dirname "${env_file}")"
  (
    umask 077
    cp -- "${env_example}" "${env_file}"
  )
}

preflight() {
  [[ -f "${compose_file}" ]] ||
    die "compose file not found: ${compose_file}"
  [[ "${wait_timeout}" =~ ^[1-9][0-9]*$ ]] ||
    die "COMPOSE_WAIT_TIMEOUT must be a positive integer"
  [[ "${logs_tail}" =~ ^[1-9][0-9]*$ ]] ||
    die "COMPOSE_LOGS_TAIL must be a positive integer"
  command -v docker >/dev/null 2>&1 ||
    die "docker executable not found"
  docker compose version >/dev/null

  compose_command=(
    docker compose
    --project-name "${project_name}"
    --env-file "${env_file}"
    --file "${compose_file}"
  )
}

compose_run() {
  "${compose_command[@]}" "$@"
}

validate_compose() {
  info "проверяю Compose-конфигурацию"
  compose_run config --quiet
}

up_and_wait() {
  info "запускаю контур и ожидаю readiness (до ${wait_timeout} с)"
  compose_run up \
    --detach \
    --wait \
    --wait-timeout "${wait_timeout}"
}

down_contour() {
  info "останавливаю и удаляю project containers"
  compose_run down --remove-orphans
}

remove_project_images() {
  local image
  local -a existing=()

  for image in "${project_images[@]}"; do
    if docker image inspect "${image}" >/dev/null 2>&1; then
      existing+=("${image}")
    fi
  done

  if ((${#existing[@]} == 0)); then
    info "именованные project images уже отсутствуют"
    return
  fi

  info "удаляю только именованные project images"
  docker image rm "${existing[@]}"
}

remove_managed_dangling_images() {
  local image_id
  local output
  local -a image_ids=()
  declare -A seen=()

  output="$(
    docker image ls \
      --quiet \
      --filter "dangling=true" \
      --filter "label=${managed_label}"
  )"
  while IFS= read -r image_id; do
    [[ -n "${image_id}" ]] || continue
    [[ -z "${seen[${image_id}]:-}" ]] || continue
    seen["${image_id}"]=1
    image_ids+=("${image_id}")
  done <<<"${output}"

  if ((${#image_ids[@]} == 0)); then
    info "managed dangling images отсутствуют"
    return
  fi

  info "удаляю только managed dangling images"
  docker image rm "${image_ids[@]}"
}

remove_project_volumes() {
  local volume
  local managed
  local compose_project

  for volume in "${project_volumes[@]}"; do
    if ! docker volume inspect "${volume}" >/dev/null 2>&1; then
      info "volume ${volume} уже отсутствует"
      continue
    fi

    managed="$(
      docker volume inspect \
        --format '{{ index .Labels "com.tts-microservice-mvp.managed" }}' \
        "${volume}"
    )"
    compose_project="$(
      docker volume inspect \
        --format '{{ index .Labels "com.docker.compose.project" }}' \
        "${volume}"
    )"
    if [[ "${managed}" != "true" &&
      "${compose_project}" != "${project_name}" ]]; then
      warn "пропускаю ${volume}: ownership labels не подтверждены"
      continue
    fi

    info "удаляю project volume ${volume}"
    docker volume rm "${volume}"
  done
}

known_service() {
  local requested="$1"
  local service

  for service in "${compose_services[@]}"; do
    if [[ "${requested}" == "${service}" ]]; then
      return 0
    fi
  done
  return 1
}

run_smoke() {
  [[ -x "${smoke_script}" ]] ||
    die "smoke script is not executable: ${smoke_script}"

  if [[ -n "${API_URL:-}" ]]; then
    info "запускаю smoke через ${API_URL}"
    "${smoke_script}"
    return
  fi

  local api_port
  api_port="$(
    awk -F= '
      $1 ~ /^[[:space:]]*API_HTTP_PORT[[:space:]]*$/ {
        value = $2
      }
      END {
        gsub(/[[:space:]]/, "", value)
        print value
      }
    ' "${env_file}"
  )"
  api_port="${api_port:-8080}"
  [[ "${api_port}" =~ ^[1-9][0-9]*$ ]] ||
    die "API_HTTP_PORT in ${env_file} must be a positive integer"

  info "запускаю smoke через публичный Go API"
  API_HTTP_PORT="${api_port}" "${smoke_script}"
}

require_no_arguments() {
  local command="$1"
  shift
  (($# == 0)) || die "${command} does not accept arguments"
}

command_name="${1:-help}"
if (($# > 0)); then
  shift
fi

case "${command_name}" in
  help | -h | --help)
    (($# == 0)) || die "help does not accept arguments"
    usage
    exit 0
    ;;
  up | rebuild | restart | stop | down | status | ps | smoke | clean)
    require_no_arguments "${command_name}" "$@"
    ;;
  logs)
    (($# <= 1)) || die "logs accepts at most one service"
    if (($# == 1)) && ! known_service "$1"; then
      die "unknown service: $1"
    fi
    ;;
  reset)
    (($# == 1)) && [[ "$1" == "--yes" ]] ||
      die "reset is destructive; run: ./scripts/manage.sh reset --yes"
    ;;
  *)
    usage >&2
    die "unknown command: ${command_name}"
    ;;
esac

bootstrap_env
preflight

case "${command_name}" in
  up)
    validate_compose
    up_and_wait
    ;;
  rebuild)
    validate_compose
    down_contour
    remove_project_images
    remove_managed_dangling_images
    info "пересобираю project images без cache"
    compose_run build --pull --no-cache
    up_and_wait
    ;;
  restart)
    validate_compose
    down_contour
    up_and_wait
    ;;
  stop)
    info "останавливаю project containers без удаления"
    compose_run stop
    ;;
  down)
    down_contour
    ;;
  status | ps)
    compose_run ps --all
    ;;
  logs)
    if (($# == 1)); then
      exec "${compose_command[@]}" logs --follow --tail "${logs_tail}" "$1"
    fi
    exec "${compose_command[@]}" logs --follow --tail "${logs_tail}"
    ;;
  smoke)
    run_smoke
    ;;
  clean)
    down_contour
    remove_project_images
    remove_managed_dangling_images
    info "clean завершён; project volumes сохранены"
    ;;
  reset)
    warn "будут удалены только PostgreSQL data и model volumes проекта"
    down_contour
    remove_project_volumes
    info "reset завершён; project images сохранены"
    ;;
esac
