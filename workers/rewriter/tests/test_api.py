from __future__ import annotations

from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from pathlib import Path

import httpx2
import pytest

from rewriter_worker.api import create_app
from rewriter_worker.config import ModelSpec, Settings
from rewriter_worker.errors import ErrorCode, WorkerError
from rewriter_worker.runtime import ModelRuntimeStatus, RuntimeReadiness
from rewriter_worker.schemas import RewriteRequest, RewriteResponse


@pytest.fixture
def anyio_backend() -> str:
    return "asyncio"


def _model() -> ModelSpec:
    return ModelSpec(
        model_id="test-model",
        display_name="Test model",
        repo_id="test/model",
        revision="1" * 40,
        filename="model.gguf",
        sha256="a" * 64,
        size_bytes=4,
        quantization="TEST",
        recommended=True,
    )


def _settings(tmp_path: Path, **overrides: object) -> Settings:
    values: dict[str, object] = {
        "worker_id": "test-rewriter",
        "models_dir": tmp_path,
        "models": (_model(),),
        "default_model_id": "test-model",
        "default_prompt": "Проверяй только произносимость.",
        "idle_unload_seconds": 0,
    }
    values.update(overrides)
    return Settings(**values)


def _payload(**overrides: object) -> dict[str, object]:
    payload: dict[str, object] = {
        "api_version": "v1",
        "request_id": "request-1",
        "fragment_id": "fragment-1",
        "text": "В 2026 году.",
        "stt_text": "",
        "warning_code": "",
        "model_id": "test-model",
        "prompt": "",
    }
    payload.update(overrides)
    return payload


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
        self.stopped = 0
        self.last_request: RewriteRequest | None = None
        self.error: Exception | None = None
        self.ready = RuntimeReadiness(
            ready=True,
            loaded_model_id=None,
            available_models=1,
        )

    def start(self) -> None:
        self.started += 1

    def stop(self) -> None:
        self.stopped += 1

    def readiness(self) -> RuntimeReadiness:
        return self.ready

    def models(self) -> list[ModelRuntimeStatus]:
        return [
            ModelRuntimeStatus(
                spec=_model(),
                available=True,
                loaded=False,
                integrity_verified=False,
            )
        ]

    def rewrite(self, request: RewriteRequest) -> RewriteResponse:
        self.last_request = request
        if self.error is not None:
            raise self.error
        return RewriteResponse(
            api_version="v1",
            request_id=request.request_id,
            fragment_id=request.fragment_id,
            rewritten_text="В две тысячи двадцать шестом году.",
            reason="Число раскрыто словами.",
            model_id=request.model_id,
            model_revision="1" * 40,
            duration_ms=7,
        )


@pytest.mark.anyio
async def test_health_lifecycle_and_lazy_ready_payload(tmp_path: Path) -> None:
    runtime = FakeRuntime()
    app = create_app(settings=_settings(tmp_path), runtime=runtime)

    async with _client(app) as client:
        live = await client.get("/health/live")
        ready = await client.get("/health/ready")

    assert runtime.started == 1
    assert runtime.stopped == 1
    assert live.status_code == 200
    assert live.json()["status"] == "alive"
    assert ready.status_code == 200
    assert ready.json() == {
        "status": "ready",
        "ready": True,
        "loaded_model_id": None,
        "available_models": 1,
        "reason": "",
    }


@pytest.mark.anyio
async def test_not_ready_is_503_without_restart_unsafe_error_shape(
    tmp_path: Path,
) -> None:
    runtime = FakeRuntime()
    runtime.ready = RuntimeReadiness(
        ready=False,
        loaded_model_id=None,
        available_models=0,
        reason="MODEL_INTEGRITY_ERROR",
    )
    app = create_app(settings=_settings(tmp_path), runtime=runtime)

    async with _client(app) as client:
        response = await client.get("/health/ready")

    assert response.status_code == 503
    assert response.json()["ready"] is False
    assert response.json()["reason"] == "MODEL_INTEGRITY_ERROR"


@pytest.mark.anyio
async def test_models_exposes_safe_metadata_defaults_and_limits(
    tmp_path: Path,
) -> None:
    settings = _settings(
        tmp_path,
        temperature=0.25,
        top_k=12,
        top_p=0.7,
        min_p=0.05,
        repeat_penalty=1.1,
        max_tokens=512,
    )
    app = create_app(settings=settings, runtime=FakeRuntime())

    async with _client(app) as client:
        response = await client.get("/v1/models")

    assert response.status_code == 200
    body = response.json()
    assert body["api_version"] == "v1"
    assert body["default_prompt"] == settings.default_prompt
    assert body["loaded_model_id"] is None
    assert body["models"][0]["device"] == "cpu"
    assert body["models"][0]["display_name"] == "Test model"
    assert body["settings"] == {
        "temperature": {
            "default": 0.25,
            "minimum": 0.0,
            "maximum": 1.0,
        },
        "top_k": {
            "default": 12,
            "minimum": 0,
            "maximum": 200,
        },
        "top_p": {
            "default": 0.7,
            "minimum": 0.0,
            "maximum": 1.0,
        },
        "min_p": {
            "default": 0.05,
            "minimum": 0.0,
            "maximum": 1.0,
        },
        "repeat_penalty": {
            "default": 1.1,
            "minimum": 0.5,
            "maximum": 2.0,
        },
        "max_tokens": {
            "default": 512,
            "minimum": 16,
            "maximum": 2048,
        },
    }
    assert body["limits"]["max_request_bytes"] == settings.max_request_bytes
    assert "models_dir" not in response.text


@pytest.mark.anyio
async def test_rewrite_forwards_full_contract_and_per_request_settings(
    tmp_path: Path,
) -> None:
    runtime = FakeRuntime()
    app = create_app(settings=_settings(tmp_path), runtime=runtime)

    async with _client(app) as client:
        response = await client.post(
            "/v1/rewrite",
            json=_payload(
                temperature=0.4,
                top_k=10,
                top_p=0.65,
                min_p=0.04,
                repeat_penalty=1.15,
                max_tokens=256,
            ),
            headers={"X-Request-ID": "transport-id"},
        )

    assert response.status_code == 200
    assert response.headers["x-request-id"] == "request-1"
    assert response.headers["cache-control"] == "no-store"
    assert response.headers["x-content-type-options"] == "nosniff"
    assert response.json()["api_version"] == "v1"
    assert response.json()["fragment_id"] == "fragment-1"
    assert runtime.last_request is not None
    assert runtime.last_request.temperature == 0.4
    assert runtime.last_request.top_k == 10
    assert runtime.last_request.top_p == 0.65
    assert runtime.last_request.min_p == 0.04
    assert runtime.last_request.repeat_penalty == 1.15
    assert runtime.last_request.max_tokens == 256


@pytest.mark.anyio
@pytest.mark.parametrize(
    ("field", "value"),
    [
        ("temperature", -0.01),
        ("temperature", 1.01),
        ("top_k", -1),
        ("top_k", 201),
        ("top_p", -0.01),
        ("top_p", 1.01),
        ("min_p", -0.01),
        ("min_p", 1.01),
        ("repeat_penalty", 0.49),
        ("repeat_penalty", 2.01),
        ("max_tokens", 15),
        ("max_tokens", 2_049),
        ("api_version", "v2"),
    ],
)
async def test_request_settings_and_version_are_bounded(
    tmp_path: Path,
    field: str,
    value: object,
) -> None:
    app = create_app(settings=_settings(tmp_path), runtime=FakeRuntime())

    async with _client(app) as client:
        response = await client.post(
            "/v1/rewrite",
            json=_payload(**{field: value}),
        )

    assert response.status_code == 422
    assert response.json()["error"]["code"] == "INVALID_REQUEST"
    assert "input" not in response.text


@pytest.mark.anyio
async def test_runtime_error_is_stable_and_does_not_leak_details(
    tmp_path: Path,
) -> None:
    runtime = FakeRuntime()
    runtime.error = WorkerError(
        code=ErrorCode.REWRITE_ERROR,
        public_message="Rewrite inference failed",
        status_code=500,
        retryable=True,
        request_id="request-1",
    )
    app = create_app(settings=_settings(tmp_path), runtime=runtime)

    async with _client(app, raise_app_exceptions=False) as client:
        response = await client.post("/v1/rewrite", json=_payload())

    assert response.status_code == 500
    assert response.json()["error"] == {
        "code": "REWRITE_ERROR",
        "message": "Rewrite inference failed",
        "retryable": True,
        "request_id": "request-1",
        "details": {},
    }


@pytest.mark.anyio
async def test_request_id_header_rejects_unsafe_characters(tmp_path: Path) -> None:
    app = create_app(settings=_settings(tmp_path), runtime=FakeRuntime())

    async with _client(app) as client:
        response = await client.get(
            "/health/live",
            headers={"X-Request-ID": "unsafe request id"},
        )

    assert response.status_code == 400
    assert response.json()["error"]["code"] == "INVALID_REQUEST"
    assert " " not in response.headers["x-request-id"]


@pytest.mark.anyio
async def test_chunked_request_is_limited_before_json_parsing(tmp_path: Path) -> None:
    app = create_app(
        settings=_settings(tmp_path, max_request_bytes=16),
        runtime=FakeRuntime(),
    )

    async def oversized_json() -> AsyncIterator[bytes]:
        yield b'{"api_version":'
        yield b'"v1","padding":"' + b"x" * 64 + b'"}'

    async with _client(app) as client:
        response = await client.post(
            "/v1/rewrite",
            content=oversized_json(),
            headers={"Content-Type": "application/json"},
        )

    assert response.status_code == 413
    assert response.json()["error"]["message"] == (
        "Request body exceeds the configured size limit"
    )


@pytest.mark.anyio
async def test_unknown_route_uses_stable_error_envelope(tmp_path: Path) -> None:
    app = create_app(settings=_settings(tmp_path), runtime=FakeRuntime())

    async with _client(app) as client:
        response = await client.get("/unknown")

    assert response.status_code == 404
    assert response.json()["error"]["code"] == "INVALID_REQUEST"
