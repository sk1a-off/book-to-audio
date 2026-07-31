from __future__ import annotations

import hashlib
import io
import json
import wave
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager

import httpx2
import pytest

from omnivoice_worker.api import create_app
from omnivoice_worker.config import Settings
from omnivoice_worker.errors import ErrorCode, WorkerError
from omnivoice_worker.runtime import GenerationResult, VoiceReference
from omnivoice_worker.schemas import GenerateRequest, VoiceMode, WorkerCapabilities


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
        self.last_request: GenerateRequest | None = None
        self.last_reference: VoiceReference | None = None
        self.error: WorkerError | None = None

    def start(self) -> None:
        self.started += 1

    def capabilities(self) -> WorkerCapabilities:
        return WorkerCapabilities(
            api_version="v1",
            worker_id="test-omnivoice",
            service_type="omnivoice",
            model_id="fake",
            model_version="test",
            device_type="cpu",
            device_name="test",
            total_vram_bytes=0,
            available_vram_bytes=0,
            max_concurrency=1,
            supported_sample_rates=[24_000],
            supported_languages=["ru"],
            supported_modes=list(VoiceMode),
            ready=True,
            warnings=[],
        )

    def generate(
        self,
        request: GenerateRequest,
        reference: VoiceReference | None,
    ) -> GenerationResult:
        self.last_request = request
        self.last_reference = reference
        if self.error is not None:
            raise self.error
        return GenerationResult(
            pcm_s16le=b"\x00\x00\xff\x7f\x01\x80",
            sample_rate=24_000,
            seed_used=request.seed or 17,
            voice_cache_hit=False,
            generation_duration_ms=12,
            model_id="fake",
            model_version="test",
            device="cpu",
        )


def _metadata(**overrides: object) -> dict[str, object]:
    metadata: dict[str, object] = {
        "api_version": "v1",
        "request_id": "request-1",
        "text": "Короткий тест.",
        "language": "ru",
        "mode": "AUTO_VOICE",
        "output_encoding": "PCM_S16LE",
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
    assert capabilities.json()["supported_modes"] == [
        "VOICE_CLONE",
        "VOICE_DESIGN",
        "AUTO_VOICE",
    ]


@pytest.mark.anyio
async def test_generate_returns_pcm_with_integrity_headers() -> None:
    runtime = FakeRuntime()
    app = create_app(
        settings=Settings(background_load=False, warmup_enabled=False),
        runtime=runtime,
    )

    async with _client(app) as client:
        response = await client.post(
            "/v1/generate",
            data={"metadata": json.dumps(_metadata(seed=42))},
        )

    assert response.status_code == 200
    assert response.content == b"\x00\x00\xff\x7f\x01\x80"
    assert response.headers["content-type"].startswith("application/octet-stream")
    assert response.headers["x-audio-sha256"] == hashlib.sha256(response.content).hexdigest()
    assert response.headers["x-seed-used"] == "42"
    assert response.headers["cache-control"] == "no-store"
    assert runtime.last_request is not None
    assert runtime.last_request.text == "Короткий тест."
    assert runtime.last_request.normalize_text is True


@pytest.mark.anyio
async def test_generate_can_wrap_pcm_as_wav() -> None:
    runtime = FakeRuntime()
    app = create_app(
        settings=Settings(background_load=False, warmup_enabled=False),
        runtime=runtime,
    )

    async with _client(app) as client:
        response = await client.post(
            "/v1/generate",
            data={
                "metadata": json.dumps(
                    _metadata(output_encoding="WAV_PCM_S16LE"),
                ),
            },
        )

    assert response.status_code == 200
    with wave.open(io.BytesIO(response.content), "rb") as wav_file:
        assert wav_file.getnchannels() == 1
        assert wav_file.getsampwidth() == 2
        assert wav_file.getframerate() == 24_000
        assert wav_file.readframes(3) == b"\x00\x00\xff\x7f\x01\x80"


@pytest.mark.anyio
async def test_clone_reference_is_forwarded_without_a_filesystem_path() -> None:
    runtime = FakeRuntime()
    app = create_app(
        settings=Settings(background_load=False, warmup_enabled=False),
        runtime=runtime,
    )
    metadata = _metadata(
        mode="VOICE_CLONE",
        voice_reference_text="Точный референс.",
    )

    async with _client(app) as client:
        response = await client.post(
            "/v1/generate",
            data={"metadata": json.dumps(metadata)},
            files={"voice_reference": ("reference.flac", b"bounded-bytes", "audio/flac")},
        )

    assert response.status_code == 200
    assert runtime.last_reference == VoiceReference(
        audio=b"bounded-bytes",
        content_type="audio/flac",
    )


@pytest.mark.anyio
async def test_invalid_metadata_uses_stable_error_envelope() -> None:
    app = create_app(
        settings=Settings(background_load=False, warmup_enabled=False),
        runtime=FakeRuntime(),
    )

    async with _client(app) as client:
        response = await client.post("/v1/generate", data={"metadata": "{}"})

    assert response.status_code == 422
    assert response.json() == {
        "error": {
            "code": "INVALID_REQUEST",
            "message": "metadata must match the GenerateRequest schema",
            "retryable": False,
            "request_id": response.headers["x-request-id"],
            "details": {},
        }
    }


@pytest.mark.anyio
async def test_reference_upload_limit_is_enforced() -> None:
    app = create_app(
        settings=Settings(
            max_reference_bytes=3,
            background_load=False,
            warmup_enabled=False,
        ),
        runtime=FakeRuntime(),
    )

    async with _client(app) as client:
        response = await client.post(
            "/v1/generate",
            data={
                "metadata": json.dumps(
                    _metadata(
                        mode="VOICE_CLONE",
                        voice_reference_text="reference",
                    )
                )
            },
            files={"voice_reference": ("reference.wav", b"1234", "audio/wav")},
        )

    assert response.status_code == 413
    assert response.json()["error"]["code"] == "VOICE_REFERENCE_ERROR"


@pytest.mark.anyio
async def test_runtime_error_is_sanitized() -> None:
    runtime = FakeRuntime()
    runtime.error = WorkerError(
        code=ErrorCode.INFERENCE_ERROR,
        public_message="OmniVoice inference failed",
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
            "/v1/generate",
            data={"metadata": json.dumps(_metadata())},
        )

    assert response.status_code == 500
    assert response.json()["error"]["code"] == "INFERENCE_ERROR"
    assert response.json()["error"]["retryable"] is True


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
            "/v1/generate",
            content=oversized_form(),
            headers={"Content-Type": "application/x-www-form-urlencoded"},
        )

    assert response.status_code == 413
    assert response.json()["error"]["message"] == (
        "Request body exceeds the configured size limit"
    )
