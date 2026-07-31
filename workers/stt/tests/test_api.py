from __future__ import annotations

import json
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager

import httpx2
import pytest

from stt_worker.api import create_app
from stt_worker.config import Settings
from stt_worker.errors import ErrorCode, WorkerError
from stt_worker.runtime import AudioPayload
from stt_worker.schemas import (
    TranscribeRequest,
    TranscribeResponse,
    WorkerCapabilities,
)


@pytest.fixture
def anyio_backend() -> str:
    return "asyncio"


@asynccontextmanager
async def _client(
    app: object,
    *,
    raise_app_exceptions: bool = True,
) -> AsyncIterator[httpx2.AsyncClient]:
    async with app.router.lifespan_context(app):
        transport = httpx2.ASGITransport(
            app=app,
            raise_app_exceptions=raise_app_exceptions,
        )
        async with httpx2.AsyncClient(
            transport=transport,
            base_url="http://testserver",
        ) as client:
            yield client


class FakeRuntime:
    def __init__(self) -> None:
        self.started = 0
        self.last_request: TranscribeRequest | None = None
        self.last_payload: AudioPayload | None = None
        self.error: WorkerError | None = None

    def start(self) -> None:
        self.started += 1

    def capabilities(self) -> WorkerCapabilities:
        return WorkerCapabilities(
            api_version="v1",
            worker_id="test-stt",
            service_type="stt",
            model_id="fake",
            model_version="test",
            device_type="cpu",
            device_name="cpu",
            total_vram_bytes=0,
            available_vram_bytes=0,
            max_concurrency=1,
            supported_sample_rates=[24_000],
            supported_languages=["ru"],
            supported_modes=["TRANSCRIBE"],
            ready=True,
            warnings=[],
        )

    def transcribe(
        self,
        request: TranscribeRequest,
        payload: AudioPayload,
    ) -> TranscribeResponse:
        self.last_request = request
        self.last_payload = payload
        if self.error is not None:
            raise self.error
        return TranscribeResponse(
            request_id=request.request_id,
            segment_id=request.segment_id or "",
            attempt_id=request.attempt_id or "",
            attempt_number=request.attempt_number,
            transcript="Тест.",
            normalized_transcript="тест.",
            detected_language="ru",
            language_probability=0.99,
            duration_ms=100,
            words=[],
            segments=[],
            confidence=0.9,
            processing_duration_ms=10,
            warnings=[],
        )


def _metadata(**overrides: object) -> dict[str, object]:
    metadata: dict[str, object] = {
        "api_version": "v1",
        "request_id": "request-1",
        "segment_id": "segment-1",
        "attempt_id": "attempt-1",
        "language": "ru",
        "audio": {
            "encoding": "PCM_S16LE",
            "sample_rate_hz": 24_000,
            "channels": 1,
            "bits_per_sample": 16,
        },
    }
    metadata.update(overrides)
    return metadata


@pytest.mark.anyio
async def test_health_and_lifespan_start() -> None:
    runtime = FakeRuntime()
    app = create_app(
        settings=Settings(background_load=False, warmup_enabled=False),
        runtime=runtime,
    )

    async with _client(app) as client:
        live = await client.get("/health/live")
        ready = await client.get("/health/ready")
        capabilities = await client.get("/v1/capabilities")

    assert runtime.started == 1
    assert live.json() == {"status": "alive", "ready": True}
    assert ready.json() == {"status": "ready", "ready": True}
    assert capabilities.json()["service_type"] == "stt"


@pytest.mark.anyio
async def test_transcription_forwards_bounded_pcm_and_returns_stable_schema() -> None:
    runtime = FakeRuntime()
    app = create_app(
        settings=Settings(background_load=False, warmup_enabled=False),
        runtime=runtime,
    )

    async with _client(app) as client:
        response = await client.post(
            "/v1/transcribe",
            data={"metadata": json.dumps(_metadata())},
            files={"audio": ("segment.pcm", b"\x01\x00\x02\x00", "application/octet-stream")},
        )

    assert response.status_code == 200
    assert response.json()["transcript"] == "Тест."
    assert response.headers["cache-control"] == "no-store"
    assert runtime.last_request is not None
    assert runtime.last_request.language == "ru"
    assert runtime.last_request.beam_size == 5
    assert runtime.last_request.patience == 1.0
    assert runtime.last_request.temperature == 0.0
    assert runtime.last_request.vad_filter is False
    assert runtime.last_request.word_timestamps_required is True
    assert runtime.last_payload == AudioPayload(
        data=b"\x01\x00\x02\x00",
        content_type="application/octet-stream",
    )


@pytest.mark.anyio
async def test_invalid_metadata_closes_request_with_stable_error() -> None:
    app = create_app(
        settings=Settings(background_load=False, warmup_enabled=False),
        runtime=FakeRuntime(),
    )

    async with _client(app) as client:
        response = await client.post(
            "/v1/transcribe",
            data={"metadata": "{}"},
            files={"audio": ("segment.pcm", b"\x01\x00", "application/octet-stream")},
        )

    assert response.status_code == 422
    assert response.json()["error"]["code"] == "INVALID_REQUEST"
    assert response.json()["error"]["message"] == (
        "metadata must match the TranscribeRequest schema"
    )


@pytest.mark.anyio
async def test_media_type_must_match_audio_spec() -> None:
    app = create_app(
        settings=Settings(background_load=False, warmup_enabled=False),
        runtime=FakeRuntime(),
    )

    async with _client(app) as client:
        response = await client.post(
            "/v1/transcribe",
            data={"metadata": json.dumps(_metadata())},
            files={"audio": ("segment.flac", b"not-pcm", "audio/flac")},
        )

    assert response.status_code == 415
    assert response.json()["error"]["code"] == "INVALID_AUDIO"


@pytest.mark.anyio
async def test_audio_limit_is_enforced_while_reading() -> None:
    app = create_app(
        settings=Settings(
            max_audio_bytes=3,
            background_load=False,
            warmup_enabled=False,
        ),
        runtime=FakeRuntime(),
    )

    async with _client(app) as client:
        response = await client.post(
            "/v1/transcribe",
            data={"metadata": json.dumps(_metadata())},
            files={"audio": ("segment.pcm", b"1234", "application/octet-stream")},
        )

    assert response.status_code == 413
    assert response.json()["error"]["code"] == "INVALID_AUDIO"


@pytest.mark.anyio
async def test_runtime_error_does_not_leak_internal_details() -> None:
    runtime = FakeRuntime()
    runtime.error = WorkerError(
        code=ErrorCode.TRANSCRIPTION_ERROR,
        public_message="STT inference failed",
        status_code=500,
        retryable=True,
        request_id="request-1",
    )
    app = create_app(
        settings=Settings(background_load=False, warmup_enabled=False),
        runtime=runtime,
    )

    async with _client(app, raise_app_exceptions=False) as client:
        response = await client.post(
            "/v1/transcribe",
            data={"metadata": json.dumps(_metadata())},
            files={"audio": ("segment.pcm", b"\x01\x00", "application/octet-stream")},
        )

    assert response.status_code == 500
    assert response.json()["error"] == {
        "code": "TRANSCRIPTION_ERROR",
        "message": "STT inference failed",
        "retryable": True,
        "request_id": "request-1",
        "details": {},
    }


@pytest.mark.anyio
async def test_request_id_header_rejects_unsafe_characters() -> None:
    app = create_app(
        settings=Settings(background_load=False, warmup_enabled=False),
        runtime=FakeRuntime(),
    )

    async with _client(app) as client:
        response = await client.get(
            "/health/live",
            headers={"X-Request-ID": "unsafe request id"},
        )

    assert response.status_code == 400
    assert response.json()["error"]["code"] == "INVALID_REQUEST"
    assert " " not in response.headers["x-request-id"]


@pytest.mark.anyio
async def test_chunked_request_is_limited_before_form_parsing() -> None:
    app = create_app(
        settings=Settings(
            max_request_bytes=16,
            background_load=False,
            warmup_enabled=False,
        ),
        runtime=FakeRuntime(),
    )

    async def oversized_form() -> AsyncIterator[bytes]:
        yield b"metadata="
        yield b"x" * 32

    async with _client(app) as client:
        response = await client.post(
            "/v1/transcribe",
            content=oversized_form(),
            headers={"Content-Type": "application/x-www-form-urlencoded"},
        )

    assert response.status_code == 413
    assert response.json()["error"]["message"] == (
        "Request body exceeds the configured size limit"
    )
