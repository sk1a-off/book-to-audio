from __future__ import annotations

import asyncio
import logging
import re
import uuid
from collections.abc import AsyncIterator
from concurrent.futures import ThreadPoolExecutor
from contextlib import asynccontextmanager
from typing import Any

from fastapi import FastAPI, Request
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse, Response
from starlette.exceptions import HTTPException as StarletteHTTPException

from .body_limit import BODY_TOO_LARGE_MESSAGE, limited_body_route
from .config import MAX_REQUEST_MAX_TOKENS, MIN_REQUEST_MAX_TOKENS, Settings
from .errors import ErrorCode, WorkerError, invalid_request
from .runtime import LlamaCppRuntime, RewriterRuntime
from .schemas import (
    ErrorBody,
    ErrorEnvelope,
    FloatSettingRange,
    HealthResponse,
    IntegerSettingRange,
    ModelResource,
    ModelsResponse,
    RewriteLimitsResource,
    RewriteRequest,
    RewriteResponse,
    RewriteSettingsResource,
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
            "X-Content-Type-Options": "nosniff",
            "X-Request-ID": body.error.request_id,
        },
    )


def create_app(
    *,
    settings: Settings | None = None,
    runtime: RewriterRuntime | None = None,
) -> FastAPI:
    resolved_settings = settings or Settings.from_env()
    resolved_runtime = runtime or LlamaCppRuntime(resolved_settings)
    inference_executor: ThreadPoolExecutor | None = None

    @asynccontextmanager
    async def lifespan(_: FastAPI) -> AsyncIterator[None]:
        nonlocal inference_executor
        inference_executor = ThreadPoolExecutor(
            max_workers=2,
            thread_name_prefix="rewrite-call",
        )
        resolved_runtime.start()
        try:
            yield
        finally:
            resolved_runtime.stop()
            inference_executor.shutdown(wait=True, cancel_futures=True)
            inference_executor = None

    app = FastAPI(
        title="Audiobook rewrite worker",
        version="0.1.0",
        description=(
            "Stateless, CPU-only HTTP transport for bounded Russian text rewriting."
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
        if (
            supplied_request_id
            and SAFE_REQUEST_ID.fullmatch(supplied_request_id) is None
        ):
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
                too_large = (
                    int(content_length) < 0
                    or int(content_length) > resolved_settings.max_request_bytes
                )
            except ValueError:
                too_large = True
            if too_large:
                return _error_response(
                    WorkerError(
                        code=ErrorCode.INVALID_REQUEST,
                        public_message=BODY_TOO_LARGE_MESSAGE,
                        status_code=413,
                        request_id=_request_id(request),
                    ),
                    _request_id(request),
                )

        response = await call_next(request)
        response.headers["Cache-Control"] = "no-store"
        response.headers["X-Content-Type-Options"] = "nosniff"
        response.headers["X-Request-ID"] = _request_id(request)
        return response

    @app.exception_handler(WorkerError)
    async def worker_error_handler(
        request: Request,
        exc: WorkerError,
    ) -> JSONResponse:
        return _error_response(exc, _request_id(request))

    @app.exception_handler(RequestValidationError)
    async def validation_error_handler(
        request: Request,
        _: RequestValidationError,
    ) -> JSONResponse:
        return _error_response(
            invalid_request(
                "Request validation failed",
                request_id=_request_id(request),
            ),
            _request_id(request),
        )

    @app.exception_handler(StarletteHTTPException)
    async def http_error_handler(
        request: Request,
        exc: StarletteHTTPException,
    ) -> JSONResponse:
        message = "Route not found" if exc.status_code == 404 else "Request is not supported"
        return _error_response(
            WorkerError(
                code=ErrorCode.INVALID_REQUEST,
                public_message=message,
                status_code=exc.status_code,
                request_id=_request_id(request),
            ),
            _request_id(request),
        )

    @app.exception_handler(Exception)
    async def internal_error_handler(
        request: Request,
        exc: Exception,
    ) -> JSONResponse:
        logger.exception(
            "Unhandled rewrite worker error",
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
        return HealthResponse(
            status="alive",
            ready=True,
        )

    @app.get(
        "/health/ready",
        response_model=HealthResponse,
        tags=["health"],
    )
    async def readiness() -> JSONResponse:
        state = resolved_runtime.readiness()
        body = HealthResponse(
            status="ready" if state.ready else "not_ready",
            ready=state.ready,
            loaded_model_id=state.loaded_model_id,
            available_models=state.available_models,
            reason=state.reason,
        )
        return JSONResponse(
            status_code=200 if state.ready else 503,
            content=body.model_dump(mode="json"),
        )

    @app.get(
        "/v1/models",
        response_model=ModelsResponse,
        tags=["worker"],
    )
    async def models() -> ModelsResponse:
        statuses = resolved_runtime.models()
        loaded_model_id = next(
            (status.spec.model_id for status in statuses if status.loaded),
            None,
        )
        return ModelsResponse(
            api_version="v1",
            worker_id=resolved_settings.worker_id,
            default_model_id=resolved_settings.default_model_id,
            default_prompt=resolved_settings.default_prompt,
            loaded_model_id=loaded_model_id,
            settings=RewriteSettingsResource(
                temperature=FloatSettingRange(
                    default=resolved_settings.temperature,
                    minimum=0.0,
                    maximum=1.0,
                ),
                top_k=IntegerSettingRange(
                    default=resolved_settings.top_k,
                    minimum=0,
                    maximum=200,
                ),
                top_p=FloatSettingRange(
                    default=resolved_settings.top_p,
                    minimum=0.0,
                    maximum=1.0,
                ),
                min_p=FloatSettingRange(
                    default=resolved_settings.min_p,
                    minimum=0.0,
                    maximum=1.0,
                ),
                repeat_penalty=FloatSettingRange(
                    default=resolved_settings.repeat_penalty,
                    minimum=0.5,
                    maximum=2.0,
                ),
                max_tokens=IntegerSettingRange(
                    default=resolved_settings.max_tokens,
                    minimum=MIN_REQUEST_MAX_TOKENS,
                    maximum=MAX_REQUEST_MAX_TOKENS,
                ),
            ),
            limits=RewriteLimitsResource(
                max_request_bytes=resolved_settings.max_request_bytes,
                max_text_chars=resolved_settings.max_text_chars,
                max_stt_text_chars=resolved_settings.max_stt_text_chars,
                max_prompt_chars=resolved_settings.max_prompt_chars,
                max_output_chars=resolved_settings.max_output_chars,
                max_reason_chars=resolved_settings.max_reason_chars,
            ),
            models=[
                ModelResource(
                    model_id=status.spec.model_id,
                    display_name=status.spec.display_name,
                    repo_id=status.spec.repo_id,
                    revision=status.spec.revision,
                    filename=status.spec.filename,
                    sha256=status.spec.sha256,
                    size_bytes=status.spec.size_bytes,
                    quantization=status.spec.quantization,
                    recommended=status.spec.recommended,
                    available=status.available,
                    loaded=status.loaded,
                    integrity_verified=status.integrity_verified,
                    device="cpu",
                )
                for status in statuses
            ],
        )

    @app.post(
        "/v1/rewrite",
        response_model=RewriteResponse,
        responses={
            413: {"model": ErrorEnvelope},
            422: {"model": ErrorEnvelope},
            429: {"model": ErrorEnvelope},
            502: {"model": ErrorEnvelope},
            503: {"model": ErrorEnvelope},
        },
        tags=["worker"],
    )
    async def rewrite(
        http_request: Request,
        rewrite_request: RewriteRequest,
    ) -> JSONResponse:
        http_request.state.request_id = rewrite_request.request_id
        executor = inference_executor
        if executor is None:
            raise WorkerError(
                code=ErrorCode.MODEL_NOT_READY,
                public_message="Rewrite worker is not running",
                status_code=503,
                retryable=True,
                request_id=rewrite_request.request_id,
            )
        result = await asyncio.get_running_loop().run_in_executor(
            executor,
            resolved_runtime.rewrite,
            rewrite_request,
        )
        return JSONResponse(
            content=result.model_dump(mode="json"),
            headers={"X-Request-ID": rewrite_request.request_id},
        )

    return app
