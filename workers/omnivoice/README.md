# OmniVoice worker

Внутренний ML-worker из архитектурного отчёта. Он загружает одну серверную
модель OmniVoice, выполняет `AUTO_VOICE`, `VOICE_DESIGN` и `VOICE_CLONE`, а
затем возвращает mono PCM S16LE. Worker ничего не знает о проектах, БД,
retry, validation или постоянных путях к артефактам.

HTTP — временный тестовый transport. Поля запроса намеренно близки к
`audiobook.ml.v1.GenerateRequest`, чтобы позднее заменить его на gRPC без
переноса model/runtime-кода.

## Запуск

```bash
uv sync --frozen --group dev
PYTHONPATH=src uv run uvicorn omnivoice_worker.main:app \
  --host 127.0.0.1 --port 8001 --workers 1
```

Модель загружается в background при startup: liveness отвечает сразу, а
readiness становится успешным только после load и warm-up:

```bash
curl --fail http://127.0.0.1:8001/health/live
curl --fail http://127.0.0.1:8001/health/ready
curl --fail http://127.0.0.1:8001/v1/capabilities
```

## Voice clone

Запрос self-contained: worker получает bounded bytes референса, а не путь к
файлу. Текст референса обязателен, чтобы OmniVoice не загружал внутреннюю ASR
модель.

```bash
curl --fail-with-body \
  -F 'metadata={"api_version":"v1","request_id":"demo-tts-1","job_id":"demo-job","segment_id":"segment-1","attempt_id":"attempt-1","text":"Это короткая тестовая фраза.","language":"ru","mode":"VOICE_CLONE","voice_reference_text":"Мы шагали с Хаосом по морозной заснеженной пустыне. Всюду торчали ледяные торосы, при должной доле фантазии похожие на скульптуры животных и людей.","speed":1.0,"guidance_scale":2.0,"num_steps":8,"seed":42,"output_encoding":"PCM_S16LE"}' \
  -F 'voice_reference=@../../data/voice_reference_omnivoice.flac;type=audio/flac' \
  http://127.0.0.1:8001/v1/generate \
  --output /tmp/segment.pcm
```

Для ручного прослушивания укажите
`"output_encoding":"WAV_PCM_S16LE"` и сохраните ответ как WAV.

## Настройки генерации

Все параметры ниже request-scoped: Go API валидирует и сохраняет snapshot
настройки задания, а worker повторно проверяет диапазон и передаёт значения в
`OmniVoiceGenerationConfig`.

| Поле | Default | Диапазон |
|---|---:|---:|
| `num_steps` | `32` | `1..100` |
| `guidance_scale` | `2.0` | `0..10` |
| `speed` | `1.0` | `0.5..2` |
| `t_shift` | `0.1` | `0.001..10` |
| `layer_penalty_factor` | `5.0` | `0..20` |
| `position_temperature` | `5.0` | `0..20` |
| `class_temperature` | `0.0` | `0..10` |
| `audio_chunk_duration` | `15.0` | `1..120` |
| `audio_chunk_threshold` | `30.0` | `1..600` |
| `pad_duration` | `0.1` | `0..5` |
| `fade_duration` | `0.1` | `0..5` |

Также доступны boolean-переключатели `normalize_text`, `denoise`,
`preprocess_prompt`, `postprocess_output` (все по умолчанию `true`) и
опциональный `seed` в диапазоне `0..2^32-1`.

`num_steps` — число итеративных шагов генерации, а не эпохи обучения: worker
выполняет только inference. `normalize_text=true` оставляет раскрытие чисел
OmniVoice; явная зависимость `num2words` закреплена в lock-файлах, чтобы этот
путь работал и в минимальном Docker image.

## Другие режимы

- `AUTO_VOICE`: без `voice_reference` и `instruction`.
- `VOICE_DESIGN`: без `voice_reference`, но с непустым `instruction`.
- `VOICE_CLONE`: с файлом, `voice_reference_text`; `instruction` допустим как
  дополнительный стиль.

Модель, device и dtype задаются только окружением оператора:

```text
OMNIVOICE_MODEL_ID=k2-fsa/OmniVoice
OMNIVOICE_MODEL_REVISION=c5fdb5ccb189668d56333f77ba2629f4cd7535f4
OMNIVOICE_DEVICE=cuda:0
OMNIVOICE_DTYPE=float16
```

`MODEL_REVISION` — immutable Hugging Face commit, который входит в
`model_version` и cache identity.

На одну GPU запускается ровно один Uvicorn worker. Одновременный второй
inference получает `RESOURCE_EXHAUSTED`; очередь остаётся ответственностью
Go scheduler.

## Проверки

```bash
PYTHONPATH=src uv run --group dev pytest
uv run --group dev ruff check src tests
```
