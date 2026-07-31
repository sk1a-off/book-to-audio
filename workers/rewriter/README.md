# Rewrite worker

Внутренний stateless-воркер для минимальной подготовки русских фрагментов,
которые после автоматических попыток синтеза и STT-проверки остались в
`warning`. Он запускает Qwen3 GGUF через `llama.cpp` **только на CPU** и
возвращает структурированный вариант текста для повторного синтеза.

Воркер не подключается к PostgreSQL, не читает книги и задания из файловой
системы и не принимает решений о сохранении результата. Go API формирует
запрос, хранит задания и историю редакций, проверяет допустимость изменений и
решает, создавать ли новую ревизию фрагмента.

## HTTP API

- `GET /health/live` — процесс отвечает;
- `GET /health/ready` — каталог моделей доступен, а выбранную модель можно
  загрузить или скачать;
- `GET /v1/models` — безопасный allowlist моделей, immutable revision/SHA256,
  состояние загрузки, диапазоны request-scoped параметров и лимиты;
- `POST /v1/rewrite` — одна синхронная операция переписывания.

Пример запроса:

```bash
curl --fail-with-body \
  --header 'Content-Type: application/json' \
  --data '{
    "api_version": "v1",
    "request_id": "rewrite-1",
    "fragment_id": "fragment-1",
    "text": "Замок стоял у дороги.",
    "stt_text": "Замок стоял у дороги.",
    "warning_code": "word_error",
    "model_id": "qwen3-4b-q4-k-m",
    "prompt": "",
    "temperature": 0.1,
    "top_k": 20,
    "top_p": 0.8,
    "min_p": 0.0,
    "repeat_penalty": 1.05,
    "max_tokens": 1024
  }' \
  http://127.0.0.1:8003/v1/rewrite
```

Каждый request-scoped параметр можно не передавать: тогда используется
процессный default. Допустимые диапазоны публикует `/v1/models`:

| Поле | Default | Диапазон |
|---|---:|---:|
| `temperature` | `0.1` | `0..1` |
| `top_k` | `20` | `0..200` |
| `top_p` | `0.8` | `0..1` |
| `min_p` | `0.0` | `0..1` |
| `repeat_penalty` | `1.05` | `0.5..2` |
| `max_tokens` | `1024` | `16..2048` |

Это параметры sampling, а не обучение модели: эпох у локального inference
нет. При занятом единственном inference-слоте второй запрос получает
`429 RESOURCE_EXHAUSTED`; очередь и retry policy принадлежат Go scheduler.

## CPU, RAM и жизненный цикл

В образ намеренно не пробрасывается GPU: runtime создаёт `llama.cpp` с
`n_gpu_layers=0`, `use_mmap=true`, `use_mlock=false`, без K/Q/V offload.
GGUF отображается в память через mmap, поэтому resident memory растёт по мере
доступа к страницам и может быть возвращена ОС после выгрузки. Образ собирает
`llama-cpp-python` с OpenBLAS; количество BLAS build jobs ограничено двумя.

По умолчанию модель загружается лениво при первом запросе и выгружается через
300 секунд простоя. Переключение модели закрывает предыдущий instance. Для
предзагрузки задайте `REWRITER_PRELOAD_MODEL_ID`; значение `0` у
`REWRITER_IDLE_UNLOAD_SECONDS` отключает idle-unload.

Для машины с 8 физическими ядрами и 32 ГиБ RAM практичная отправная точка:

```text
REWRITER_N_THREADS=8
REWRITER_N_CTX=8192
REWRITER_N_BATCH=128
REWRITER_IDLE_UNLOAD_SECONDS=300
```

Ограничение контейнера в 8 ГиБ оставляет достаточно места для рекомендуемого
4B Q4_K_M и KV/cache при коротких фрагментах. Быстрый 1.7B Q8_0 занимает
меньше файлового cache и обычно даёт меньшую задержку. Контекст, batch,
потоки, каталог моделей, сетевые лимиты и lifecycle — process-scoped
операторские параметры; их нельзя менять отдельным HTTP-запросом.

## Разрешённые модели

Сервис не принимает произвольные repository/path. В коде зафиксированы два
официальных GGUF:

| ID | Hugging Face файл | Revision | Размер | SHA256 |
|---|---|---|---:|---|
| `qwen3-4b-q4-k-m` | `Qwen/Qwen3-4B-GGUF` / `Qwen3-4B-Q4_K_M.gguf` | `bc640142c66e1fdd12af0bd68f40445458f3869b` | `2497280256` | `7485fe6f11af29433bc51cab58009521f205840f5b4ae3a32fa7f92e8534fdf5` |
| `qwen3-1.7b-q8-0` | `Qwen/Qwen3-1.7B-GGUF` / `Qwen3-1.7B-Q8_0.gguf` | `90862c4b9d2787eaed51d12237eafdfe7c5f6077` | `1834426016` | `061b54daade076b5d3362dac252678d17da8c68f07560be70818cace6590cb1a` |

`qwen3-4b-q4-k-m` выбран по умолчанию как более качественный вариант.
`qwen3-1.7b-q8-0` — облегчённая альтернатива. При
`REWRITER_AUTO_DOWNLOAD=true` отсутствующий файл скачивается по immutable
revision в Hugging Face cache, копируется во временный файл, проверяется по
размеру и SHA256 и только затем атомарно публикуется в
`REWRITER_MODELS_DIR`. Символические ссылки и файлы вне разрешённого корня не
принимаются.

## Промпт и сохранность фразы

Неизменяемый system prompt требует:

- полностью сохранять смысловые единицы, имена, буквенно-цифровое и символьное
  содержимое и их порядок;
- ничего не добавлять, не удалять и не перефразировать;
- буквально сохранять уже существующие inline-теги OmniVoice;
- разрешать только пунктуацию/пробелы, регистр, `е↔ё` и best-effort
  combining acute `U+0301` внутри русского слова;
- не раскрывать цифры, даты, единицы и аббревиатуры — это задача
  `OmniVoice normalize_text`;
- считать STT только диагностической подсказкой;
- вернуть закрытый JSON `{rewritten_text, reason}` без chain-of-thought.

Исходный текст, STT и дополнительная подсказка сериализуются как недоверенный
JSON, чтобы команды внутри них не становились системными инструкциями.
Промпт снижает риск, но не является доказательством сохранности: authoritative
детерминированную проверку допустимых изменений выполняет Go API до записи
новой ревизии. Пользователь может затем прослушать результат и вручную выбрать
или одобрить нужную ревизию.

## Окружение

Основные process-scoped настройки:

```text
REWRITER_API_VERSION=v1
REWRITER_WORKER_ID=<hostname>
REWRITER_MODELS_DIR=/models/rewriter
REWRITER_DEFAULT_MODEL_ID=qwen3-4b-q4-k-m
REWRITER_AUTO_DOWNLOAD=true
REWRITER_PRELOAD_MODEL_ID=
REWRITER_N_CTX=8192
REWRITER_N_BATCH=128
REWRITER_N_THREADS=<число CPU>
REWRITER_MAX_TOKENS=1024
REWRITER_TEMPERATURE=0.1
REWRITER_TOP_K=20
REWRITER_TOP_P=0.8
REWRITER_MIN_P=0.0
REWRITER_REPEAT_PENALTY=1.05
REWRITER_IDLE_UNLOAD_SECONDS=300
REWRITER_MAX_REQUEST_BYTES=32768
REWRITER_MAX_TEXT_CHARS=8000
REWRITER_MAX_STT_TEXT_CHARS=8000
REWRITER_MAX_PROMPT_CHARS=2000
REWRITER_MAX_OUTPUT_CHARS=12000
REWRITER_MAX_REASON_CHARS=1000
```

Revision, SHA256 и ожидаемый размер моделей можно переопределить только
согласованным набором `REWRITER_QWEN3_*`; это операторская функция для
осознанного обновления pin, а не пользовательский выбор произвольного файла.

## Локальная проверка

```bash
uv sync --frozen --group dev
PYTHONPATH=src uv run --frozen --group dev pytest
uv run --frozen --group dev ruff check src tests
```

Тесты используют fake runtime/model/downloader: они проверяют HTTP-контракт,
лимиты, prompt isolation, immutable allowlist, SHA256/atomic publish,
CPU-only параметры, lazy load, model switch, idle unload и взаимное
исключение inference. Реальные GGUF для unit-тестов не скачиваются.

