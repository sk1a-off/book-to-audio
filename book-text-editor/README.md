# Audiobook API / Book Text Editor

Go-сервис принимает FB2 и голосовой референс, хранит бизнес-состояние в
PostgreSQL, управляет генерацией и проверкой сегментов, а затем выдаёт ZIP с
WAV-файлами. Python-сервисы используются только как stateless inference
workers: OmniVoice получает текст и референс, STT получает PCM, а локальный
Rewriter предлагает минимальную TTS-правку проблемного текста. Они не
обращаются к PostgreSQL и не управляют books/jobs/retry/history/review/export.

Существующий CLI для разбора FB2 также сохранён.

## Запуск API

Требуется Go 1.26 или новее.

```bash
export DATABASE_URL='postgres://audiobook:audiobook@127.0.0.1:5432/audiobook?sslmode=disable'
export OMNIVOICE_URL='http://127.0.0.1:8001'
export STT_URL='http://127.0.0.1:8002'
export REWRITER_URL='http://127.0.0.1:8003'

go run ./cmd/migrate
go run ./cmd/api
```

По умолчанию API слушает только `127.0.0.1:8080`; адрес меняется через
`HTTP_ADDR`. После запуска Web UI доступен на
[`http://127.0.0.1:8080/`](http://127.0.0.1:8080/). Frontend встроен в
Go-бинарник через `go:embed`, поэтому Node.js и отдельная сборка не требуются.

Основной контракт:

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

Книга загружается как multipart-поле `file`. Голос загружается полями
`reference_audio` и `reference_text`. Генерация асинхронная: клиент получает
`202 Accepted`, затем читает состояние job. Необязательное JSON-тело запуска
фиксирует полный снимок request-scoped настроек:

```json
{
  "omnivoice": {
    "num_steps": 32,
    "guidance_scale": 2,
    "speed": 1,
    "normalize_text": true,
    "denoise": true,
    "t_shift": 0.1,
    "layer_penalty_factor": 5,
    "position_temperature": 5,
    "class_temperature": 0,
    "preprocess_prompt": true,
    "postprocess_output": true,
    "audio_chunk_duration": 15,
    "audio_chunk_threshold": 30,
    "pad_duration": 0.1,
    "fade_duration": 0.1
  },
  "whisper": {
    "beam_size": 5,
    "patience": 1,
    "temperature": 0,
    "vad_filter": false,
    "word_timestamps": true
  },
  "automatic_warning_retries": 5
}
```

`num_steps` — число итераций генерации, а не эпох обучения: Python workers
ничего не дообучают во время job. После первой попытки warning по умолчанию
допускается ещё пять TTS → STT попыток; каждая попытка сохраняется отдельно.
Транспортные и прочие технические ошибки этим циклом не маскируются.

## Запуск CLI

```bash
go run ./cmd/book-text-editor -- book.fb2
cat book.fb2 | go run ./cmd/book-text-editor -- -
go run ./cmd/book-text-editor -max-words=45 book.fb2
```

## Архитектура

- `cmd/api` — public HTTP API и graceful shutdown;
- `cmd/migrate` — отдельный, идемпотентный runner SQL-миграций;
- `api/endpoints.go` — transport и строгие HTTP-контракты;
- `api/postgres_store.go` — единственная production-реализация хранения;
- `api/jobs.go` — bounded Go-side orchestration TTS → STT → validation;
- `api/workers.go` — ограниченные HTTP-клиенты Python workers;
- `api/rewrite*.go` — очередь AI-правок, строгая проверка сохранности фразы и
  история версий;
- `api/manual_reviews.go` — прослушивание и ручное одобрение false-positive
  warning;
- `api/archive.go` — формирование PCM WAV и безопасного ZIP;
- `api/ui.go`, `api/web` — встроенный Web UI без внешних runtime-зависимостей;
- `db/migrations` — versioned PostgreSQL schema;
- `cmd/book-text-editor` — локальный FB2 CLI;
- `internal/book` — единственная доменная модель книги;
- `internal/fb2` — потоковый XML-парсер и преобразование FB2 в модель;
- `internal/segment` — независимая, детерминированная сегментация текста.

Production startup требует `DATABASE_URL` и выполняет `Ping`, но не запускает
миграции автоматически. Составные изменения выполняются транзакционно.
`fragment_attempts` сохраняет результаты попыток, а job counters обновляются
после каждого завершённого фрагмента. In-memory repository используется только
в изолированных HTTP-тестах.

Текущий тестовый MVP хранит voice reference и PCM в PostgreSQL `BYTEA`.
Следующий production-шаг из архитектурного отчёта — вынести крупные immutable
blobs в content-addressed artifact storage, оставив ownership и metadata в Go.

## Контракт FB2

- принимается несжатый XML FictionBook 2.0 с официальным namespace;
- разбирается только первый `body`; отдельный `body` с примечаниями не
  смешивается с основным текстом;
- контейнерные секции сохраняются в названии главы, например
  `Часть I — Глава 1`;
- текст эпиграфов перед первой секцией присоединяется к первой главе без
  создания ложной главы;
- сохраняется текст абзацев, подзаголовков, стихов, подписей авторов, дат и
  ячеек таблиц;
- маркер смены сцены `* * *` удаляется из результата, но всегда создаёт
  принудительную границу между сегментами;
- фраза длиннее лимита дополнительно делится по словам, поэтому каждый сегмент
  всегда укладывается в `max-words`;
- поддерживаются UTF-8 и кодировки с ASCII-совместимой XML-декларацией,
  например Windows-1251; UTF-16, FB2.ZIP и полная XSD-валидация сейчас не
  поддерживаются.

## Проверки

```bash
gofmt -l .
go mod tidy -diff
go mod verify
go vet ./...
go test -shuffle=on -race -cover ./...
go build -buildvcs=false ./cmd/api ./cmd/migrate ./cmd/book-text-editor
```

PostgreSQL integration test запускается отдельно:

```bash
TEST_DATABASE_URL='postgres://…' go test -race ./api \
  -run '^TestPostgresStoreIntegration$' -count=1
```

Тесты покрывают HTTP lifecycle, границы `* * *`, worker-контракты, warnings,
редактирование/retry, job counters, WAV/ZIP, PostgreSQL transactions и
immutable attempts.

Web UI использует только перечисленные публичные Go endpoints. В нём меняются
все безопасные request-scoped параметры OmniVoice, Whisper и Rewriter,
включая число шагов, sampling и число повторов warning. Выбранные параметры
сохраняются в `localStorage`, а точный снимок запуска — в PostgreSQL вместе с
job. Модель, device/dtype, GPU/CPU, число потоков, каталоги и лимиты памяти —
process-scoped параметры worker; они задаются окружением и требуют
перезапуска, поэтому не подменяются настройками отдельной задачи.

В `localStorage` также сохраняются метаданные загруженных книг, ID последней
job и ID активной AI-правки для восстановления рабочего контекста после
обновления страницы. Голосовые референсы, PCM и результаты генерации в
браузере не сохраняются.
Общий монитор заданий по умолчанию показывает активную очередь, автоматически
обновляет прогресс и счётчики, поддерживает фильтрацию по статусу и
постраничную навигацию. Из карточки можно открыть полное состояние задачи.
После успешного завершения доступны как единый ZIP всей книги, так и отдельные
ZIP-архивы по главам. Для warning можно прослушать WAV, вручную одобрить
результат, посмотреть историю текста, восстановить любую версию либо поставить
выбранные фрагменты в очередь CPU-only локального Rewriter. Go принимает
AI-кандидат только после детерминированной проверки: разрешены минимальные
правки пунктуации/пробелов/регистра, `е` ↔ `ё` и combining acute U+0301;
смысловые и буквенно-цифровые единицы, порядок и inline-теги должны сохраниться.
