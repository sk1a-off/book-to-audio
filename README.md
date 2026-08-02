# Голос Книги

Локальный сервис, который превращает книгу в формате FB2 в аудиокнигу:
разбирает главы, озвучивает фрагменты клонированным голосом, проверяет результат
через STT, позволяет прослушивать и исправлять отдельные фрагменты и собирает
готовые главы в FLAC только по запросу пользователя.

> Проект рассчитан на локальный запуск. Публичный Web UI и API принадлежат
> Go-сервису; ML-worker'ы изолированы во внутренней Compose-сети и не хранят
> бизнес-состояние.

## Что умеет сервис

- загружать и разбирать `.fb2`, `.zip` и `.fb2.zip`;
- создавать голос по референсу `.flac` или PCM `.wav` и точной расшифровке;
- асинхронно озвучивать книгу через OmniVoice;
- ставить несколько книг в общую FIFO-очередь;
- останавливать и возобновлять отдельную задачу без потери готовых фрагментов;
- автоматически подхватывать незавершённую озвучку после `stop`, `down` или
  перезапуска API;
- проверять произношение через Whisper/STT;
- показывать **все фрагменты по мере генерации**, а не только warning;
- прослушивать уже готовый фрагмент прямо во время озвучки книги;
- редактировать готовый, warning- или failed-фрагмент;
- автоматически ставить отредактированный фрагмент на переозвучивание;
- вручную принимать корректное аудио с ложным warning;
- повторять выбранные проблемные фрагменты;
- исправлять одной кнопкой все warning-фрагменты книги через локальную LLM,
  сохраняя отдельную правку на странице каждой главы;
- скачивать отдельную готовую главу как **прямой `.flac`**;
- скачивать всю готовую книгу как ZIP с одним FLAC на главу;
- хранить историю текстовых ревизий и ручных решений.

## Проверка и экспорт больших книг

- Главная страница показывает только сводку и карточки глав.
- Фрагменты и предупреждения открываются на отдельной странице главы.
- Предупреждение Whisper не блокирует FLAC главы или ZIP книги, если аудио есть у каждого фрагмента.
- При повторной генерации сохраняется попытка с лучшим сходством Whisper; худший retry остаётся только в истории.
- Проверка текста намеренно щадящая: warning создаётся только при сильном общем расхождении.

## Быстрый запуск

```bash
./scripts/manage.sh up
```

После readiness:

- Web UI: `http://127.0.0.1:8080/`
- Health check: `http://127.0.0.1:8080/healthz`

При первом запуске `scripts/manage.sh` создаёт
`deploy/compose/.env` из `.env.example` с правами `0600`. Перед запуском вне
изолированной локальной среды обязательно смените пароль PostgreSQL и
проверьте параметры моделей.

Полный smoke-тест через публичный API:

```bash
./scripts/manage.sh smoke
```

Smoke загружает небольшую FB2 и голосовой референс, запускает TTS → STT,
дожидается завершения, скачивает прямой FLAC первой главы и ZIP всей книги и
проверяет сигнатуры аудиофайлов.

## Рабочий процесс

```text
1. Загрузить FB2
2. Загрузить голосовой референс и его расшифровку
3. Поставить одну или несколько книг в очередь
4. Следить за книгами и фрагментами в отдельном окне «Очередь»
   ├─ остановить задачу с сохранением прогресса
   ├─ возобновить её с первого незавершённого фрагмента
   └─ открыть нужную книгу, не дожидаясь остальных
5. Проверить предупреждения
   ├─ прослушать готовый WAV-preview
   ├─ исправить текст → фрагмент автоматически переозвучится
   ├─ принять корректный warning вручную
   ├─ исправить все warnings книги через локальную LLM
   └─ повторить warning/failed
6. Скачать готовую главу FLAC или ZIP всей книги
```

### Просмотр во время генерации

`GET /v1/job/{jobID}/chapters` — лёгкий metadata endpoint. Он возвращает:

- прогресс каждой главы;
- состояние каждого фрагмента;
- исходный и распознанный текст;
- наличие аудио-preview;
- возможность редактирования;
- ссылку на FLAC только для полностью готовой главы.

Этот endpoint **не склеивает PCM и не кодирует FLAC**, поэтому UI может
безопасно опрашивать его во время генерации.

### Устойчивая очередь, остановка и возобновление

PostgreSQL является источником истины для очереди. Процессная Go-очередь лишь
будит последовательный dispatcher; порядок книг, незавершённые фрагменты и
статусы не пропадают вместе с контейнером API.
Штатная конфигурация запускает один `audiobook-api`; несколько API-replica без
lease/heartbeat не поддерживаются, чтобы не отправлять две книги на один GPU.

- `GET /v1/queue` возвращает книги `running`, `queued`, `paused` и их
  `pending`/`generating` фрагменты в фактическом порядке обработки;
- `POST /v1/job/{jobID}/pause` отменяет текущий worker-вызов, отбрасывает только
  незавершённый PCM и сохраняет задачу как `paused`;
- `POST /v1/job/{jobID}/resume` возвращает её в `queued`;
- `POST /v1/job/{jobID}/cancel` окончательно исключает задачу из очереди, но не
  удаляет уже готовые фрагменты.

При старте API и между задачами recovery-транзакция возвращает прерванные
`running/generating` в `queued/pending`. Она также понимает состояния
`generation interrupted` и `validation interrupted`, оставленные предыдущей
версией сервиса, поэтому первое обновление не теряет активный фрагмент.

### Редактирование озвученного фрагмента

```http
PATCH /v1/fragment/{fragmentID}
Content-Type: application/json

{"new_text":"Исправленный текст фрагмента."}
```

Редактировать можно статусы `ready`, `warning` и `failed`. В одной
транзакции PostgreSQL — либо в одной критической секции memory-adapter — сервис:

1. создаёт новую текстовую ревизию;
2. удаляет устаревшее аудио текущей версии;
3. переводит фрагмент в `pending`;
4. пересчитывает состояние job и подготавливает задачу повторной генерации.

После фиксации состояния application service будит durable dispatcher. Клиент
получает `X-Fragment-Regeneration-Queued: true`, затем TTS и STT выполняются
асинхронно. Даже если API остановится сразу после ответа, `pending`-фрагмент
останется в PostgreSQL и будет обработан после следующего запуска.

### Массовая LLM-правка warnings

`POST /v1/job/{jobID}/rewrite/warnings` с `{}`, пустым телом или пустым
`fragment_ids` создаёт неизменяемый снимок всех текущих warning-фрагментов во
всех главах книги. Непустой `fragment_ids` сохраняет прежний сценарий правки
выбранных фрагментов одной главы. Операция отвечает `202 Accepted`; прогресс и
частичные ошибки читаются через `GET /v1/rewrite/{rewriteID}`.

## Экспорт без преждевременной сборки

Аудио глав не хранится как заранее собранный файл. До скачивания PostgreSQL
содержит PCM отдельных фрагментов. Склейка и FLAC-кодирование происходят только
в обработчике download-запроса.

### Одна глава

```text
GET /v1/job/{jobID}/chapters/{chapterNumber}/audio.flac
Content-Type: audio/flac
```

Ответ — один непосредственный FLAC-файл, например:

```text
character_0001_Глава 1 Прибытие.flac
```

Архив для отдельной главы не создаётся. Старый путь с окончанием
`/audio.zip` оставлен как deprecated alias для совместимости, но также
возвращает прямой `audio/flac`, а не ZIP.

### Вся книга

```text
GET /v1/job/{jobID}/audio.zip
Content-Type: application/zip
```

ZIP создаётся только по этому запросу и содержит:

```text
character_0001_....flac
character_0002_....flac
...
manifest.json
```

Каждая глава собирается из фрагментов в книжном порядке. Отдельные WAV-preview
в экспорт не попадают.

## Почему warning стало меньше

Раньше STT-проверка требовала почти полного совпадения нормализованных строк.
Такой подход создавал ложные warning из-за пунктуации, `е/ё`, регистра и одной
неидеально распознанной служебной частицы.

Теперь проверка детерминированная, но более устойчивая:

- Unicode приводится к совместимой нормальной форме;
- регистр, пунктуация и диакритика не учитываются;
- `ё` и `е` считаются эквивалентными;
- для достаточно длинных фраз разрешена небольшая token edit distance;
- дополнительно проверяется символьная похожесть;
- цифры и русские числительные сравниваются по значению (`25` = «двадцать
  пять», но «пять» ≠ «пятьсот»);
- edit distance вычисляется полосовым алгоритмом с жёстким бюджетом, поэтому
  большой вход не вызывает квадратичного потребления CPU/памяти;
- короткие фразы остаются строгими;
- пустая или существенно другая расшифровка по-прежнему даёт warning.

Также сегментатор больше не разрезает одну слишком длинную фразу по
произвольному слову. Фраза сохраняется целиком и возвращается как non-fatal
segmentation warning. Это сохраняет естественную интонацию и уменьшает число
ложных STT-расхождений на искусственных обрывах.

## Архитектура

```text
Browser
  │ same-origin HTTP
  ▼
audiobook-api (Go)
  ├─ HTTP transport / embedded UI
  ├─ durable FIFO generation dispatcher + startup recovery
  ├─ generation and review application services
  ├─ fragment read model + focused persistence port
  ├─ PostgreSQL fragment adapter
  ├─ in-memory fragment adapter for tests
  ├─ existing repository implementations
  ├─ TTS/STT/rewrite clients
  └─ on-demand FLAC/ZIP exporter
       │
       ├── PostgreSQL
       ├── omnivoice-worker
       ├── stt-worker
       └── rewriter-worker
```

### Границы ответственности

**Go API**

- единственный владелец job/fragment lifecycle;
- единственный компонент с доступом к PostgreSQL;
- валидирует входные данные и worker-ответы;
- управляет retries, ревизиями и manual review;
- формирует WAV-preview, FLAC и ZIP;
- не отдаёт браузеру адреса внутренних worker'ов.

**Python worker'ы**

- получают self-contained inference request;
- не знают о главах, статусах job и экспорте;
- не имеют DB credentials;
- не владеют persistent artifact paths.

**Application service и focused persistence port**

HTTP-обработчики не выполняют storage orchestration самостоятельно. Сценарий
просмотра/редактирования/повторной генерации выделен в `fragmentService`, а его
зависимость описана небольшим `fragmentWorkflowStore`:

- metadata-only fragment catalog;
- атомарная правка текста и подготовка retry task;
- durable постановка правки на повторную генерацию;
- snapshot одной полностью готовой главы для on-demand экспорта.

Memory и PostgreSQL находятся за отдельными adapter-файлами. В результате
transport отвечает за HTTP-контракт, application service — за последовательность
use case, repository adapters — за транзакции и блокировки, runner — за фоновые
TTS/STT задания, exporter — только за PCM/FLAC/ZIP. Это не полная миграция всего
старого пакета `api` на textbook Clean Architecture, но новый workflow больше не
расширяет монолитный handler/storage coupling и имеет отдельные unit-тесты.

## Основные endpoint'ы

```text
GET   /
GET   /healthz

POST  /v1/book
GET   /v1/books/{bookID}

POST  /v1/voice
GET   /v1/voices
GET   /v1/voices/{voiceID}

POST  /v1/generate/book/{bookID}/voice/{voiceID}
GET   /v1/jobs?status=active|all|queued|running|paused|canceled|completed|completed_with_warnings|failed
GET   /v1/queue
GET   /v1/job/{jobID}
POST  /v1/job/{jobID}/pause
POST  /v1/job/{jobID}/resume
POST  /v1/job/{jobID}/cancel

GET   /v1/job/{jobID}/chapters
GET   /v1/job/{jobID}/chapters/{chapterNumber}/audio.flac
GET   /v1/job/{jobID}/audio.zip

GET   /v1/job/{jobID}/warnings
PATCH /v1/fragment/{fragmentID}
GET   /v1/fragment/{fragmentID}/audio.wav
POST  /v1/fragment/{fragmentID}/approve
POST  /v1/job/{jobID}/retry/warnings

GET   /v1/fragment/{fragmentID}/reviews
GET   /v1/fragment/{fragmentID}/revisions
POST  /v1/fragment/{fragmentID}/revisions/{revisionID}/restore

GET   /v1/rewrite/models
POST  /v1/job/{jobID}/rewrite/warnings
GET   /v1/rewrite/{rewriteID}
```

Все JSON endpoint'ы отклоняют неизвестные поля. Upload книги и голоса использует
`multipart/form-data` с bounded body size.

## Структура репозитория

```text
book-text-editor/
  api/
    endpoints.go              # HTTP transport and routing
    jobs.go                   # durable generation dispatcher
    job_queue_store.go        # FIFO, pause/resume/cancel and recovery
    fragment_service.go       # fragment review application service
    fragment_store_memory.go  # in-memory adapter
    fragment_store_postgres.go # PostgreSQL adapter and transactions
    transcript_validation.go  # bounded deterministic STT comparison
    chapter_archive.go        # metadata catalog and direct FLAC download
    archive.go                # PCM concatenation and encoding
    postgres_store.go         # PostgreSQL implementation
    store.go                  # memory implementation and legacy contract
    web/                      # embedded same-origin UI
  cmd/api/
  cmd/migrate/
  db/migrations/
  internal/
    book/
    fb2/
    segment/
workers/
  omnivoice/
  stt/
  rewriter/
deploy/compose/
scripts/
data/
```

## Настройки генерации

Request-scoped параметры OmniVoice и Whisper передаются при создании job и
сохраняются вместе с ним как immutable snapshot. UI хранит последние значения
локально в браузере.

Process-scoped параметры — модель, device/dtype, потоки, каталоги, GPU и лимиты
ресурсов — задаются в `deploy/compose/.env` и применяются после перезапуска.

## Проверки

```bash
cd book-text-editor

gofmt -w ./api ./cmd ./internal
go test -shuffle=on -race ./...
go vet ./...
go build -buildvcs=false ./cmd/api ./cmd/migrate

cd ..
node --check book-text-editor/api/web/app.js
bash -n scripts/manage.sh scripts/smoke-api.sh scripts/test-manage.sh
./scripts/test-manage.sh

docker compose -f deploy/compose/compose.yaml config --quiet
```

PostgreSQL integration tests запускаются при наличии `TEST_DATABASE_URL`.
Python test suites и lock-файлы находятся в каталогах соответствующих worker'ов.

## Управление Compose

```bash
./scripts/manage.sh up
./scripts/manage.sh status
./scripts/manage.sh logs audiobook-api
./scripts/manage.sh restart
./scripts/manage.sh stop
./scripts/manage.sh down
./scripts/manage.sh smoke
```

Команды очистки намеренно разделены:

```bash
./scripts/manage.sh clean
./scripts/manage.sh reset --yes
./scripts/manage.sh full-reset --yes
```

- `clean` удаляет containers/network и только allowlisted project images,
  сохраняя volumes;
- существующий `reset --yes` удаляет containers/network и четыре project
  volumes PostgreSQL/model cache, но **сохраняет images**;
- новый `full-reset --yes` удаляет containers/network, четыре project volumes,
  четыре project images и managed dangling images.

Даже `full-reset` не выполняет глобальный Docker prune, не удаляет исходники,
`deploy/compose/.env` и посторонние images/volumes. Для обеих команд с удалением
данных обязателен явный `--yes`.

## Безопасность

- API публикуется только на loopback по умолчанию;
- inference worker'ы не публикуют host ports;
- браузер работает только с same-origin API;
- UI не использует inline scripts и небезопасный `innerHTML`;
- CSP запрещает внешние scripts/styles и встраивание во frame;
- multipart и worker response ограничены по размеру;
- SQL migrations запускаются отдельным контейнером до старта API;
- секреты и process settings не возвращаются в Web UI.

## Ограничения MVP

Voice reference и PCM фрагментов пока хранятся в PostgreSQL `BYTEA`. Для очень
больших библиотек следующий архитектурный шаг — Go-owned content-addressed
artifact storage. Worker'ы при этом должны остаться stateless и не получать
доступ к постоянному хранилищу или базе данных.
