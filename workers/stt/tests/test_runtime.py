from __future__ import annotations

from types import SimpleNamespace
from typing import ClassVar

import numpy as np
import pytest

from stt_worker.config import Settings
from stt_worker.errors import ErrorCode, WorkerError
from stt_worker.runtime import AudioPayload, FasterWhisperRuntime
from stt_worker.schemas import TranscribeRequest


class LazyFakeModel:
    supported_languages: ClassVar[list[str]] = ["ru"]

    def __init__(self) -> None:
        self.materialized = False
        self.last_kwargs: dict[str, object] | None = None

    def transcribe(self, *_: object, **kwargs: object) -> tuple[object, object]:
        self.last_kwargs = kwargs

        def segments() -> object:
            self.materialized = True
            yield SimpleNamespace(
                text=" Тест.",
                start=0.0,
                end=0.1,
                avg_logprob=-0.1,
                words=[
                    SimpleNamespace(
                        word=" Тест.",
                        start=0.0,
                        end=0.1,
                        probability=0.9,
                    )
                ],
            )

        info = SimpleNamespace(language="ru", language_probability=0.99)
        return segments(), info


def test_faster_whisper_generator_is_materialized_before_slot_release() -> None:
    runtime = FasterWhisperRuntime(
        Settings(
            device="cpu",
            compute_type="int8",
            warmup_enabled=False,
        )
    )
    model = LazyFakeModel()
    runtime._model = model
    runtime._ready = True
    runtime._supported_languages = ["ru"]
    samples = np.sin(np.linspace(0, 10, 2400)) * 1000
    payload = AudioPayload(
        data=samples.astype("<i2").tobytes(),
        content_type="application/octet-stream",
    )
    request = TranscribeRequest(
        api_version="v1",
        request_id="request-1",
        language="ru",
        beam_size=7,
        patience=1.5,
        temperature=0.25,
        vad_filter=True,
        word_timestamps_required=False,
    )

    response = runtime.transcribe(request, payload)

    assert model.materialized is True
    assert model.last_kwargs == {
        "word_timestamps": False,
        "language": "ru",
        "beam_size": 7,
        "patience": 1.5,
        "temperature": 0.25,
        "vad_filter": True,
    }
    assert response.transcript == "Тест."
    assert response.words[0].probability == 0.9


def test_busy_worker_rejects_before_audio_decode() -> None:
    runtime = FasterWhisperRuntime(
        Settings(
            device="cpu",
            compute_type="int8",
            warmup_enabled=False,
        )
    )
    runtime._model = LazyFakeModel()
    runtime._ready = True
    runtime._supported_languages = ["ru"]
    runtime._inference_slot.acquire()

    try:
        with pytest.raises(WorkerError) as raised:
            runtime.transcribe(
                TranscribeRequest(
                    api_version="v1",
                    request_id="request-1",
                    language="ru",
                ),
                AudioPayload(
                    data=b"not-even-aligned-pcm",
                    content_type="application/octet-stream",
                ),
            )
    finally:
        runtime._inference_slot.release()

    assert raised.value.code == ErrorCode.RESOURCE_EXHAUSTED
