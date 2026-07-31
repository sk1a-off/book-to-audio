# TTS microservice MVP

Реализация следует тут
[`tts_audiobook_api_architecture_report_ru.md`](./tts_audiobook_api_architecture_report_ru.md).
Текущий slice уже включает публичный Go API, PostgreSQL и три изолированных
Python ML-worker с временным HTTP transport.

```text
client
  -> audiobook-api (Go)
       -> PostgreSQL: books, voices, jobs, fragments, attempts, PCM
       -> omnivoice-worker: text + voice reference -> PCM S16LE 24 kHz
       -> stt-worker: PCM -> transcript/words/timings
       -> rewriter-worker: warning fragment + prompt -> text candidate
       -> Go validation/retry/export -> ZIP with one merged FLAC per chapter
```

Go — единственный владелец бизнес-состояния и единственный компонент с
доступом к PostgreSQL. Workers не знают о книгах, lifecycle job, retry,
решении ACCEPT/RETRY, persistent paths или export. Они принимают
self-contained задания, возвращают результат inference и могут использовать
только внутренние временные файлы библиотеки.

Сейчас voice reference и PCM хранятся в PostgreSQL `BYTEA`: это намеренное
упрощение тестового MVP. Следующий шаг — Go-owned content-addressed artifact
storage из архитектурного отчёта; Python по-прежнему не получит persistent
storage или DB credentials.

## Быстрый запуск

```bash
./scripts/manage.sh up
```

При первом запуске `manage.sh` создаст `deploy/compose/.env` из
`.env.example` с правами `0600` и выведет предупреждение. Перед использованием
вне локальной машины проверьте пароль PostgreSQL и параметры моделей.

Публичный endpoint доступен только на loopback:

- Web UI: <http://127.0.0.1:8080/>
- Go API health: <http://127.0.0.1:8080/healthz>

OmniVoice, STT и Rewriter не публикуют host ports и доступны только Go-сервису
внутри Compose network. Перед запуском API отдельный контейнер `migrate`
применяет versioned SQL migrations; каждый обычный startup API миграции не
выполняет.

Rewriter настроен прежде всего для CPU/RAM: Qwen3 4B Q4_K_M является моделью
по умолчанию, `n_gpu_layers=0` закреплён в worker-коде, контейнер не получает
GPU. Для него заданы `REWRITER_N_THREADS=8`, `REWRITER_N_CTX=8192`,
`REWRITER_N_BATCH=128`, лимиты `8 CPU` и `8 GiB RAM`. Пустой
`REWRITER_PRELOAD_MODEL_ID` оставляет загрузку модели до первого запроса;
значение можно задать в `.env`, чтобы загрузить одну из allowlisted моделей
при старте. Контекст 8192 покрывает максимально разрешённый фрагмент,
служебный prompt и bounded ответ без неявного обрезания. По умолчанию worker
выгружает неиспользуемую модель через
`REWRITER_IDLE_UNLOAD_SECONDS=300`. Модельный кэш хранится в отдельном named
volume.

Web UI встроен в Go-бинарник и не требует Node.js, CDN или отдельного
frontend-контейнера. Он загружает книгу и голос, запускает job, показывает
прогресс и проблемные фрагменты, позволяет исправить текст, повторить
генерацию и скачать ZIP всей книги либо отдельные ZIP по главам. Каждый такой
архив содержит не WAV-фрагменты, а объединённый FLAC для каждой входящей в него
главы. В UI доступны все request-scoped настройки OmniVoice, Whisper и
Rewriter: шаги генерации (`num_steps`, это не эпохи обучения), sampling,
нормализация текста, chunking, VAD/beam search и число автоматических повторов
warning. Выбранные значения сохраняются в браузере, а точный immutable snapshot
генерации — в PostgreSQL вместе с job. Браузер обращается только к same-origin
Go API.

Process-scoped параметры — device/dtype, распределение GPU, модель Whisper,
потоки/контекст Rewriter, каталоги и resource limits — остаются в
`deploy/compose/.env`, потому что их изменение требует безопасного
перезапуска worker. Rewriter получает модель и параметры sampling из каждой
задачи, работает без GPU, лениво загружает только allowlisted GGUF и
выгружается после простоя.

Полный последовательный smoke только через public API:

```bash
./scripts/manage.sh smoke
```

Он загружает небольшую FB2 и существующий voice reference, создаёт job,
дожидается TTS → STT → Go validation, скачивает ZIP и проверяет объединённые
FLAC по главам.

## Экспорт аудиокниги

Полный ZIP содержит по одному объединённому FLAC-файлу на каждую главу книги.
ZIP отдельной главы содержит один такой FLAC. Фрагменты объединяются в порядке
их следования внутри главы и не экспортируются отдельными WAV-файлами.

Имя аудиофайла включает порядковый номер и заголовок главы. Например:

```text
character_0001_Глава 1 Прибытие.flac
```

Endpoint `/v1/fragment/{fragmentID}/audio.wav` остаётся отдельным
диагностическим способом прослушать конкретный warning-фрагмент в UI и не
определяет содержимое экспортных ZIP.

## Управление Compose-контуром

Все повседневные операции выполняются из корня проекта:

```bash
./scripts/manage.sh up
./scripts/manage.sh status
./scripts/manage.sh logs audiobook-api
./scripts/manage.sh restart
./scripts/manage.sh stop
./scripts/manage.sh down
```

Команды:

| Команда | Действие |
|---|---|
| `up` | Проверяет Compose config, запускает в фоне и ждёт health/readiness. |
| `rebuild` | Выполняет `down`, удаляет только четыре project images и managed dangling images, затем `build --pull --no-cache` и `up --wait`. |
| `restart` | Пересоздаёт контейнеры без пересборки; images и volumes сохраняются. |
| `stop` | Только останавливает существующие контейнеры. |
| `down` | Удаляет project containers/network, но сохраняет images и volumes. |
| `status` / `ps` | Показывает все project containers, включая остановленные. |
| `logs [service]` | Следит за логами всего контура или выбранного сервиса. |
| `smoke` | Запускает полный цикл через public Go API и проверяет ZIP с объединёнными FLAC по главам. |
| `clean` | Удаляет project containers и images, но сохраняет все volumes. |
| `reset --yes` | Удаляет только project volumes с PostgreSQL и кэшами моделей. Images сохраняются. |

`COMPOSE_WAIT_TIMEOUT` меняет ожидание readiness (по умолчанию 900 секунд),
`COMPOSE_LOGS_TAIL` — начальное число строк для `logs` (по умолчанию 200).
Для `smoke` можно передать другой same-origin адрес через `API_URL`.

Очистка намеренно ограничена allowlist:

- `tts-microservice-mvp/audiobook-api:local`;
- `tts-microservice-mvp/omnivoice-worker:local`;
- `tts-microservice-mvp/stt-worker:local`;
- `tts-microservice-mvp/rewriter-worker:local`;
- dangling images только с label
  `com.tts-microservice-mvp.managed=true`;
- volumes
  `tts-microservice-mvp_postgres-data`,
  `tts-microservice-mvp_omnivoice-models`,
  `tts-microservice-mvp_stt-models`,
  `tts-microservice-mvp_rewriter-models`.

Скрипт не выполняет глобальную очистку Docker. `reset` требует буквальный
флаг `--yes`, дополнительно проверяет ownership label каждого volume и не
удаляет посторонние volumes. После `reset` модели загружаются заново, поэтому
первый readiness может занять несколько минут.

## Endpoint-ы

```text
GET   /
GET   /healthz
POST  /v1/book
GET   /v1/books/{bookID}
POST  /v1/voice
GET   /v1/voices
GET   /v1/voices/{voiceID}
POST  /v1/generate/book/{bookID}/voice/{voiceID}
GET   /v1/jobs?status=active|all|queued|running|completed|completed_with_warnings|failed
GET   /v1/job/{jobID}
GET   /v1/job/{jobID}/warnings
PATCH /v1/fragment/{fragmentID}
POST  /v1/job/{jobID}/retry/warnings
GET   /v1/fragment/{fragmentID}/audio.wav
POST  /v1/fragment/{fragmentID}/approve
GET   /v1/fragment/{fragmentID}/reviews
GET   /v1/fragment/{fragmentID}/revisions
POST  /v1/fragment/{fragmentID}/revisions/{revisionID}/restore
GET   /v1/rewrite/models
POST  /v1/job/{jobID}/rewrite/warnings
GET   /v1/rewrite/{rewriteID}
GET   /v1/job/{jobID}/audio.zip
GET   /v1/job/{jobID}/chapters
GET   /v1/job/{jobID}/chapters/{chapterNumber}/audio.zip
```

Книга передаётся multipart-полем `file`. Голос — полями
`reference_audio`, `reference_text` и необязательным `name`. POST генерации
принимает необязательный строгий JSON со снимком настроек; пустое тело
использует рекомендуемые defaults, в том числе пять повторов после первой
warning-попытки.

## Структура

```text
book-text-editor/
  api/            # public API, orchestration, PostgreSQL, worker clients
  cmd/api/
  cmd/migrate/
  db/migrations/
workers/
  omnivoice/  # clone/design/auto, bounded prompt LRU, one inference slot
  stt/        # Faster Whisper, PCM validation/resample, words/segments
  rewriter/   # CPU-first Qwen GGUF rewrite candidates, no business state
deploy/
  compose/
scripts/
data/         # существующие тестовые book/reference fixtures
```

## Проверки

```bash
cd book-text-editor
go test -shuffle=on -race ./...
go vet ./...
go build -buildvcs=false ./cmd/api ./cmd/migrate

cd ..
docker compose -f deploy/compose/compose.yaml config --quiet
bash -n scripts/manage.sh scripts/test-manage.sh
./scripts/test-manage.sh
```

Python suites находятся в каждой папке worker. Все dependency/model revisions
зафиксированы lock-файлами и immutable commit SHA.
