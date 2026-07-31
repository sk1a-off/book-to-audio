from __future__ import annotations

import asyncio
import json
import logging
import re
import threading
import time
import uuid
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from typing import Annotated, Any

from fastapi import FastAPI, File, Form, Request, UploadFile
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse, Response
from pydantic import ValidationError

from .body_limit import limited_body_route
from .config import Settings
from .errors import ErrorCode, WorkerError, invalid_request
from .runtime import AudioPayload, FasterWhisperRuntime, STTRuntime
from .schemas import (
    AudioEncoding,
    ErrorBody,
    ErrorEnvelope,
    HealthResponse,
    TranscribeRequest,
    TranscribeResponse,
    WorkerCapabilities,
)

logger = logging.getLogger(__name__)
SAFE_REQUEST_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$")


def _request_id(request: Request) -> str:
    return str(getattr(request.state, "request_id", uuid.uuid4().hex))


def _error_response(error: WorkerError, fallback_request_id: str) -> JSONResponse:
    body = ErrorEnvelope(
        error=ErrorBody(
            code=error.code,
            message=error.public_message,
            retryable=error.retryable,
            request_id=error.request_id or fallback_request_id,
            details=error.details,
        )
    )
    return JSONResponse(
        status_code=error.status_code,
        content=body.model_dump(mode="json"),
        headers={
            "Cache-Control": "no-store",
            "X-Request-ID": body.error.request_id,
        },
    )


async def _read_upload(
    upload: UploadFile,
    *,
    limit: int,
    request_id: str,
) -> bytes:
    data = bytearray()
    try:
        while chunk := await upload.read(1024 * 1024):
            data.extend(chunk)
            if len(data) > limit:
                raise WorkerError(
                    code=ErrorCode.INVALID_AUDIO,
                    public_message="Audio payload exceeds the configured size limit",
                    status_code=413,
                    request_id=request_id,
                )
    finally:
        await upload.close()

    if not data:
        raise WorkerError(
            code=ErrorCode.INVALID_AUDIO,
            public_message="Audio payload is empty",
            status_code=422,
            request_id=request_id,
        )
    return bytes(data)


def create_app(
    *,
    settings: Settings | None = None,
    runtime: STTRuntime | None = None,
) -> FastAPI:
    resolved_settings = settings or Settings.from_env()
    resolved_runtime = runtime or FasterWhisperRuntime(resolved_settings)
    request_slot = threading.BoundedSemaphore(value=1)

    def load_until_ready() -> None:
        while not resolved_runtime.capabilities().ready:
            resolved_runtime.start()
            if resolved_runtime.capabilities().ready:
                return
            time.sleep(max(1.0, resolved_settings.load_retry_seconds))

    @asynccontextmanager
    async def lifespan(_: FastAPI) -> AsyncIterator[None]:
        if resolved_settings.background_load:
            threading.Thread(
                target=load_until_ready,
                name="stt-model-loader",
                daemon=True,
            ).start()
        else:
            resolved_runtime.start()
        yield

    app = FastAPI(
        title="STT inference worker",
        version="0.1.0",
        description="Internal HTTP test transport for the audiobook.ml.v1 STT worker contract.",
        lifespan=lifespan,
    )
    app.router.route_class = limited_body_route(resolved_settings.max_request_bytes)
    app.state.settings = resolved_settings
    app.state.runtime = resolved_runtime

    @app.middleware("http")
    async def request_context(request: Request, call_next: Any) -> Response:
        supplied_request_id = request.headers.get("X-Request-ID")
        request.state.request_id = supplied_request_id or uuid.uuid4().hex
        if supplied_request_id and SAFE_REQUEST_ID.fullmatch(supplied_request_id) is None:
            request.state.request_id = uuid.uuid4().hex
            return _error_response(
                WorkerError(
                    code=ErrorCode.INVALID_REQUEST,
                    public_message="X-Request-ID contains unsupported characters",
                    status_code=400,
                    request_id=_request_id(request),
                ),
                _request_id(request),
            )
        content_length = request.headers.get("content-length")
        if content_length:
            try:
                too_large = int(content_length) > resolved_settings.max_request_bytes
            except ValueError:
                too_large = True
            if too_large:
                return _error_response(
                    WorkerError(
                        code=ErrorCode.INVALID_REQUEST,
                        public_message="Request body exceeds the configured size limit",
                        status_code=413,
                        request_id=_request_id(request),
                    ),
                    _request_id(request),
                )

        response = await call_next(request)
        response.headers["X-Request-ID"] = _request_id(request)
        return response

    @app.exception_handler(WorkerError)
    async def worker_error_handler(request: Request, exc: WorkerError) -> JSONResponse:
        return _error_response(exc, _request_id(request))

    @app.exception_handler(RequestValidationError)
    async def validation_error_handler(
        request: Request,
        _: RequestValidationError,
    ) -> JSONResponse:
        return _error_response(
            invalid_request("Request validation failed", request_id=_request_id(request)),
            _request_id(request),
        )

    @app.exception_handler(Exception)
    async def internal_error_handler(request: Request, exc: Exception) -> JSONResponse:
        logger.exception(
            "Unhandled STT worker error",
            exc_info=exc,
            extra={"request_id": _request_id(request)},
        )
        return _error_response(
            WorkerError(
                code=ErrorCode.INTERNAL_ERROR,
                public_message="Internal worker error",
                status_code=500,
                request_id=_request_id(request),
            ),
            _request_id(request),
        )

    @app.get("/health/live", response_model=HealthResponse, tags=["health"])
    async def liveness() -> HealthResponse:
        return HealthResponse(status="alive", ready=resolved_runtime.capabilities().ready)

    @app.get(
        "/health/ready",
        response_model=HealthResponse,
        responses={503: {"model": ErrorEnvelope}},
        tags=["health"],
    )
    async def readiness(request: Request) -> HealthResponse | JSONResponse:
        if resolved_runtime.capabilities().ready:
            return HealthResponse(status="ready", ready=True)
        return _error_response(
            WorkerError(
                code=ErrorCode.MODEL_NOT_READY,
                public_message="STT model is not ready",
                status_code=503,
                retryable=True,
                request_id=_request_id(request),
            ),
            _request_id(request),
        )

    @app.get(
        "/v1/capabilities",
        response_model=WorkerCapabilities,
        tags=["worker"],
    )
    async def capabilities() -> WorkerCapabilities:
        return resolved_runtime.capabilities()

    @app.post(
        "/v1/transcribe",
        response_model=TranscribeResponse,
        responses={
            422: {"model": ErrorEnvelope},
            429: {"model": ErrorEnvelope},
            503: {"model": ErrorEnvelope},
        },
        tags=["worker"],
    )
    async def transcribe(
        http_request: Request,
        metadata: Annotated[str, Form(...)],
        audio: Annotated[UploadFile, File(...)],
    ) -> JSONResponse:
        try:
            parsed = TranscribeRequest.model_validate_json(metadata)
        except (ValidationError, ValueError, json.JSONDecodeError) as exc:
            await audio.close()
            raise invalid_request(
                "metadata must match the TranscribeRequest schema",
                request_id=_request_id(http_request),
            ) from exc

        http_request.state.request_id = parsed.request_id
        if not request_slot.acquire(blocking=False):
            await audio.close()
            raise WorkerError(
                code=ErrorCode.RESOURCE_EXHAUSTED,
                public_message="STT worker is busy",
                status_code=429,
                retryable=True,
                request_id=parsed.request_id,
            )
        try:
            content_type = audio.content_type or "application/octet-stream"
            allowed_types = {
                AudioEncoding.PCM_S16LE: {
                    "application/octet-stream",
                    "audio/pcm",
                },
                AudioEncoding.WAV_PCM_S16LE: {
                    "application/octet-stream",
                    "audio/wav",
                    "audio/x-wav",
                    "audio/vnd.wave",
                },
            }
            if content_type not in allowed_types[parsed.audio.encoding]:
                await audio.close()
                raise WorkerError(
                    code=ErrorCode.INVALID_AUDIO,
                    public_message="Audio media type does not match AudioSpec",
                    status_code=415,
                    request_id=parsed.request_id,
                )

            payload = AudioPayload(
                data=await _read_upload(
                    audio,
                    limit=resolved_settings.max_audio_bytes,
                    request_id=parsed.request_id,
                ),
                content_type=content_type,
            )
            result = await asyncio.to_thread(
                resolved_runtime.transcribe,
                parsed,
                payload,
            )
        finally:
            request_slot.release()
        return JSONResponse(
            content=result.model_dump(mode="json"),
            headers={
                "Cache-Control": "no-store",
                "X-Request-ID": parsed.request_id,
            },
        )

    return app
