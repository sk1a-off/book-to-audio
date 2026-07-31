# STT worker

Внутренний speech-to-text worker из архитектурного отчёта. Primary backend —
Faster Whisper. Worker принимает bounded mono PCM/WAV, выполняет decode,
sanitation, peak normalization, resample до 16 кГц и возвращает transcript,
words, segments и confidence. Он не принимает решений ACCEPT/RETRY и не
владеет validation policy.

HTTP — временный тестовый transport, близкий к
`audiobook.ml.v1.TranscribeRequest`. Production transport позднее заменяется
на gRPC client-streaming без изменения backend/runtime.

## Запуск

```bash
uv sync --frozen --group dev
PYTHONPATH=src uv run uvicorn stt_worker.main:app \
  --host 127.0.0.1 --port 8002 --workers 1
```

```bash
curl --fail http://127.0.0.1:8002/health/live
curl --fail http://127.0.0.1:8002/health/ready
curl --fail http://127.0.0.1:8002/v1/capabilities
```

Модель загружается в background: liveness доступен во время загрузки, а
readiness возвращает `503` до завершения model load и warm-up.

## Транскрипция PCM от OmniVoice

```bash
curl --fail-with-body \
  -F 'metadata={"api_version":"v1","request_id":"demo-stt-1","job_id":"demo-job","segment_id":"segment-1","attempt_id":"attempt-1","language":"ru","audio":{"encoding":"PCM_S16LE","sample_rate_hz":24000,"channels":1,"bits_per_sample":16},"word_timestamps_required":true}' \
  -F 'audio=@/tmp/segment.pcm;type=application/octet-stream' \
  http://127.0.0.1:8002/v1/transcribe
```

`expected_text` можно передать для correlation/debugging, но worker его не
сравнивает с результатом. Поле `normalized_transcript` диагностическое; все
authoritative normalization и validation остаются в Go.

По умолчанию для быстрого локального smoke используется cached-friendly
`small`. Production profile можно переключить на `large-v3` только через
окружение:

```text
STT_MODEL_ID=large-v3
STT_MODEL_REVISION=<immutable Hugging Face commit>
STT_DEVICE=cuda
STT_COMPUTE_TYPE=float16
```

Модель загружается один раз. Lock охватывает и `transcribe()`, и полную
материализацию lazy generator. Второй одновременный запрос получает
`RESOURCE_EXHAUSTED`, поскольку реальная очередь должна жить в Go.

## Проверки

```bash
PYTHONPATH=src uv run --group dev pytest
uv run --group dev ruff check src tests
```
