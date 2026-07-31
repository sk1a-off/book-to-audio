from __future__ import annotations

import asyncio
import hashlib
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

from .audio import encode_audio
from .body_limit import limited_body_route
from .config import Settings
from .errors import ErrorCode, WorkerError, invalid_request
from .runtime import OmniVoiceRuntime, RealOmniVoiceRuntime, VoiceReference
from .schemas import (
    ErrorBody,
    ErrorEnvelope,
    GenerateRequest,
    HealthResponse,
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
                    code=ErrorCode.VOICE_REFERENCE_ERROR,
                    public_message="Voice reference exceeds the configured size limit",
                    status_code=413,
                    request_id=request_id,
                )
    finally:
        await upload.close()

    if not data:
        raise WorkerError(
            code=ErrorCode.VOICE_REFERENCE_ERROR,
            public_message="Voice reference is empty",
            status_code=422,
            request_id=request_id,
        )
    return bytes(data)


def create_app(
    *,
    settings: Settings | None = None,
    runtime: OmniVoiceRuntime | None = None,
) -> FastAPI:
    resolved_settings = settings or Settings.from_env()
    resolved_runtime = runtime or RealOmniVoiceRuntime(resolved_settings)
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
                name="omnivoice-model-loader",
                daemon=True,
            ).start()
        else:
            resolved_runtime.start()
        yield

    app = FastAPI(
        title="OmniVoice inference worker",
        version="0.1.0",
        description=(
            "Internal HTTP test transport for the audiobook.ml.v1 OmniVoice worker contract."
        ),
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
            "Unhandled OmniVoice worker error",
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
                public_message="OmniVoice model is not ready",
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
        "/v1/generate",
        responses={
            200: {"content": {"application/octet-stream": {}}},
            422: {"model": ErrorEnvelope},
            429: {"model": ErrorEnvelope},
            503: {"model": ErrorEnvelope},
        },
        tags=["worker"],
    )
    async def generate(
        http_request: Request,
        metadata: Annotated[str, Form(...)],
        voice_reference: Annotated[UploadFile | None, File()] = None,
    ) -> Response:
        try:
            parsed = GenerateRequest.model_validate_json(metadata)
        except (ValidationError, ValueError, json.JSONDecodeError) as exc:
            raise invalid_request(
                "metadata must match the GenerateRequest schema",
                request_id=_request_id(http_request),
            ) from exc

        http_request.state.request_id = parsed.request_id
        if not request_slot.acquire(blocking=False):
            if voice_reference is not None:
                await voice_reference.close()
            raise WorkerError(
                code=ErrorCode.RESOURCE_EXHAUSTED,
                public_message="OmniVoice worker is busy",
                status_code=429,
                retryable=True,
                request_id=parsed.request_id,
            )
        try:
            reference: VoiceReference | None = None
            if voice_reference is not None:
                content_type = voice_reference.content_type or "application/octet-stream"
                if content_type not in {
                    "application/octet-stream",
                    "audio/flac",
                    "audio/wav",
                    "audio/x-flac",
                    "audio/x-wav",
                    "audio/vnd.wave",
                }:
                    await voice_reference.close()
                    raise WorkerError(
                        code=ErrorCode.VOICE_REFERENCE_ERROR,
                        public_message="Voice reference media type is not supported",
                        status_code=415,
                        request_id=parsed.request_id,
                    )
                reference = VoiceReference(
                    audio=await _read_upload(
                        voice_reference,
                        limit=resolved_settings.max_reference_bytes,
                        request_id=parsed.request_id,
                    ),
                    content_type=content_type,
                )

            result = await asyncio.to_thread(resolved_runtime.generate, parsed, reference)
        finally:
            request_slot.release()
        body, media_type, filename = encode_audio(
            result.pcm_s16le,
            sample_rate=result.sample_rate,
            encoding=parsed.output_encoding,
        )
        audio_hash = hashlib.sha256(body).hexdigest()
        headers = {
            "Cache-Control": "no-store",
            "Content-Disposition": f'attachment; filename="{filename}"',
            "X-Request-ID": parsed.request_id,
            "X-Audio-Encoding": parsed.output_encoding.value,
            "X-Audio-Sample-Rate": str(result.sample_rate),
            "X-Audio-Channels": "1",
            "X-Audio-Bits-Per-Sample": "16",
            "X-Audio-SHA256": audio_hash,
            "X-Audio-Duration-Ms": str(result.duration_ms),
            "X-Generation-Duration-Ms": str(result.generation_duration_ms),
            "X-Seed-Used": str(result.seed_used),
            "X-Voice-Cache-Hit": str(result.voice_cache_hit).lower(),
            "X-Model-ID": result.model_id,
            "X-Model-Version": result.model_version,
        }
        if result.warnings:
            headers["X-Audio-Warnings"] = ",".join(result.warnings)
        return Response(content=body, media_type=media_type, headers=headers)

    return app
