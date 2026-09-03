# Russian TTS quality suite

Этот каталог фиксирует входы и provenance для воспроизводимых сравнений TTS. Он не является официальным MOS-набором: оценки называются `internal_subjective_quality_score`.

## Инварианты

- `source_text` — канонический текст. Нормализованный `tts_text` хранится отдельно в manifest конкретного sample.
- Один experiment меняет одну основную переменную.
- Скрытая метка sample не должна содержать название движка до завершения оценки.
- RTF, ASR/CER и DSP-метрики не заменяют прослушивание.
- Любой результат без model revision, reference hash, inference config и artifact hash считается невоспроизводимым.

## Структура

- `corpus.json` — version-controlled русский regression corpus.
- `baselines/` — неизменяемые снимки фактических запусков.
- `manifests/schema.json` — контракт provenance одного audio sample.
- `ratings/schema.json` — контракт слепой субъективной оценки.
- `ratings/preference-schema.json` — контракт preference-only результата, когда
  пользователь выбрал варианты, но не выставлял числовые MOS-like оценки.
- `references/manifest.json` — ссылки и хеши speaker reference, без дублирования бинарных файлов.
- `outputs/` — будущие controlled samples; большие наборы должны использовать согласованное artifact storage или Git LFS.

## Проверка

Из каталога `book-text-editor`:

```bash
go run ./cmd/quality-corpus -corpus ../testdata/tts_quality/corpus.json
go test ./internal/quality ./cmd/quality-corpus
```

## Controlled OmniVoice preview

Runner обращается непосредственно к внутреннему OmniVoice worker и не создаёт
книгу, job или записи в PostgreSQL. По умолчанию используются пять
репрезентативных случаев; полный корпус требует явного `-all`.

```bash
cd book-text-editor
go run ./cmd/quality-runner \
  -experiment omnivoice-baseline-001 \
  -endpoint http://127.0.0.1:8001
```

Worker можно запустить на loopback по инструкции в `workers/omnivoice/README.md`.
Runner никогда не перезаписывает существующий experiment directory. Для каждого
sample он атомарно сохраняет WAV и JSON manifest с SHA-256, seed, model revision,
end-to-end RTF, worker inference time, peak, RMS, clipping и silence metrics.
Manifest содержит каждый фактически отправленный chunk, его текст, seed, request
ID, worker hash и timing. По умолчанию несколько PCM chunks объединяются без
добавления искусственной тишины: так измеряются реальные паузы и resets,
созданные моделью. Экспериментальный `-inter-chunk-min-pause-ms` включает adaptive
minimum pause: runner измеряет trailing silence левого chunk и leading silence
правого, а затем добавляет только недостающую тишину. Естественная пауза
модели не обрезается. Политика и фактические existing/added/resulting seconds
сохраняются в manifest.
`lufs_i` пока остаётся `null`: корректный ITU-R BS.1770/R128 анализ не подменяется
обычным RMS.

## Controlled adaptive chapter-pause A/B/C

Пилот меняет только итоговый minimum pause. Текст заголовка уже
зафиксирован как `Глава седьмая`, чтобы не смешивать тест паузы с тестом
нормализации числительного:

```bash
for pause_ms in 500 800 1100; do
  go run ./cmd/quality-runner \
    -experiment "heading-pause-${pause_ms}ms" \
    -endpoint http://127.0.0.1:8001 \
    -corpus ../testdata/tts_quality/corpus.json \
    -reference ../voice_data/voice_reference_omnivoice.flac \
    -reference-text ../voice_data/transcription.txt \
    -cases heading-chapter-seven-words \
    -segmenter prosody-v2 \
    -inter-chunk-min-pause-ms "$pause_ms"
done
```

Целевое значение не переносится в production до слепого прослушивания.
Альтернатива `-boundary-min-pauses-ms 800,500` задаёт точный minimum
для каждой последовательной границы controlled sample. Число значений должно
совпадать с числом фактических chunk boundaries. Это experiment-only механизм:
он не заменяет будущую классификацию `sentence` / `paragraph` / `chapter`.

Для Compose worker, который намеренно не публикуется на host, используется
profile-service в той же внутренней сети:

```bash
docker compose -f deploy/compose/compose.yaml run --rm quality-runner \
  -experiment omnivoice-baseline-001 \
  -endpoint http://omnivoice-worker:8000 \
  -corpus /quality/corpus.json \
  -output /output \
  -reference /data/voice_reference_omnivoice.flac \
  -reference-text /data/transcription.txt
```

## Controlled segmentation A/B

Production-профиль `legacy-v1` не переключается автоматически. Кандидат
`prosody-v2` защищает русские инициалы и сокращения, сохраняет структурные
переносы и гарантирует worker hard limits: не более 120 слов и 2000 Unicode
символов на chunk. Для чистого A/B меняются только профиль и experiment ID:

```bash
export QUALITY_OUTPUT_DIR=/tmp/tts-segmentation-ab

docker compose -f deploy/compose/compose.yaml run --rm quality-runner \
  -experiment segmentation-a-legacy-v1 \
  -endpoint http://omnivoice-worker:8000 \
  -corpus /quality/corpus.json \
  -output /output \
  -reference /data/voice_reference_omnivoice.flac \
  -reference-text /data/transcription.txt \
  -segmenter legacy-v1 \
  -segmenter-max-words 12 \
  -cases initials-spaced-and-compact,abbreviations-context,chapter-transition-scene

docker compose -f deploy/compose/compose.yaml run --rm quality-runner \
  -experiment segmentation-b-prosody-v2 \
  -endpoint http://omnivoice-worker:8000 \
  -corpus /quality/corpus.json \
  -output /output \
  -reference /data/voice_reference_omnivoice.flac \
  -reference-text /data/transcription.txt \
  -segmenter prosody-v2 \
  -segmenter-max-words 12 \
  -cases initials-spaced-and-compact,abbreviations-context,chapter-transition-scene
```

Значение `12` намеренно усиливает различия алгоритмов в коротком controlled
наборе и не является рекомендацией для production. В обоих запусках должны
оставаться одинаковыми corpus, voice, seed и inference parameters.

## Controlled explicit-stress A/B

`experiments/stress-overrides-v1.json` добавляет только combining acute
`U+0301` после русских гласных. Runner проверяет, что после удаления этих меток
получается точный canonical `source_text`. Значения не переносятся в production
и не доказывают корректность RUAccent. Fail-closed sparse policy запрещает
пустой override и разметку ударениями половины или более слов во фразе.

Для A/B используются одинаковые `legacy-v1`, voice, seed и inference settings:

```bash
export QUALITY_OUTPUT_DIR=/tmp/tts-stress-ab
HOMOGRAPHS=homograph-zamok,homograph-muka,homograph-stoit,homograph-plachu,homograph-atlas,homograph-organ,homograph-strelki,homograph-doroga,homograph-uzhe,homograph-belki

docker compose -f deploy/compose/compose.yaml run --rm quality-runner \
  -experiment stress-a-unmarked \
  -endpoint http://omnivoice-worker:8000 \
  -corpus /quality/corpus.json -output /output \
  -reference /data/voice_reference_omnivoice.flac \
  -reference-text /data/transcription.txt \
  -segmenter legacy-v1 -cases "$HOMOGRAPHS"

docker compose -f deploy/compose/compose.yaml run --rm quality-runner \
  -experiment stress-b-curated-u0301 \
  -endpoint http://omnivoice-worker:8000 \
  -corpus /quality/corpus.json -output /output \
  -reference /data/voice_reference_omnivoice.flac \
  -reference-text /data/transcription.txt \
  -segmenter legacy-v1 \
  -text-overrides /quality/experiments/stress-overrides-v1.json \
  -cases "$HOMOGRAPHS"
```

Успешная генерация B подтверждает только техническую совместимость worker/model
с U+0301. Корректность ударений определяется слепым прослушиванием каждой пары.

## Слепой пакет для segmentation A/B

После двух запусков runner пакет собирается только если corpus, case IDs,
speaker reference, transcript, seed, normalizer, max words и inference config
совпадают, а `segmenter_version` различается:

```bash
cd book-text-editor
go run ./cmd/quality-blind \
  -session segmentation-blind-001 \
  -experiment-a /tmp/tts-segmentation-ab/segmentation-a-legacy-v1 \
  -experiment-b /tmp/tts-segmentation-ab/segmentation-b-prosody-v2 \
  -output /tmp/tts-segmentation-blind \
  -shuffle-seed 42
```

`index.html`, `session.json` и каталог `samples/` не содержат названий
алгоритмов. Соответствие A/B хранится отдельно в `reveal.json`; его открывают
только после сохранения оценки. Страница сохраняет незавершённый черновик в
браузере и выгружает результат по `ratings/session-schema.json`.

## Слепой пакет для engine A/B

Между движками нельзя уравнять внутренние inference parameters: они имеют разный
смысл и диапазоны. Режим `engine` поэтому требует одинаковые corpus, case IDs,
canonical/TTS text, chunks, speaker reference, reference transcript, seed, normalizer и
segmenter, но разрешает engine-specific config. Движок, model ID и immutable revision
попадают только в reveal:

```bash
go run ./cmd/quality-blind \
  -session qwen-vs-omnivoice-001 \
  -experiment-a /path/to/omnivoice \
  -experiment-b /path/to/qwen \
  -output /path/to/blind \
  -primary-variable engine \
  -shuffle-seed 42
```

## Минимальный эксперимент

1. Выбрать неизменный subset `case_id` и speaker reference.
2. Записать baseline manifest до генерации.
3. Сгенерировать все samples с одной конфигурацией.
4. Проверить WAV/FLAC, duration, peak, RMS, LUFS, clipping, silence и artifact hash.
5. Рандомизировать скрытые labels и собрать ratings 1–5.
6. Раскрыть engine mapping только после сохранения оценки.

ElevenLabs и Chatterbox не включены в baseline 2026-08-18: controlled samples с тем же текстом и speaker intent ещё не созданы.
