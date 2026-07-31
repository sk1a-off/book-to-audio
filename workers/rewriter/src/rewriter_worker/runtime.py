from __future__ import annotations

import gc
import hashlib
import json
import logging
import os
import shutil
import stat
import tempfile
import threading
import time
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Protocol

from pydantic import ValidationError

from .config import ModelSpec, Settings
from .errors import ErrorCode, WorkerError
from .prompt import build_messages, rewrite_output_schema
from .schemas import RewriteModelOutput, RewriteRequest, RewriteResponse

logger = logging.getLogger(__name__)

ModelFactory = Callable[..., Any]


@dataclass(frozen=True, slots=True)
class FileIdentity:
    device: int
    inode: int
    size: int
    modified_ns: int


@dataclass(frozen=True, slots=True)
class ModelRuntimeStatus:
    spec: ModelSpec
    available: bool
    loaded: bool
    integrity_verified: bool


@dataclass(frozen=True, slots=True)
class RuntimeReadiness:
    ready: bool
    loaded_model_id: str | None
    available_models: int
    reason: str = ""


class RewriterRuntime(Protocol):
    def start(self) -> None: ...

    def stop(self) -> None: ...

    def readiness(self) -> RuntimeReadiness: ...

    def models(self) -> list[ModelRuntimeStatus]: ...

    def rewrite(self, request: RewriteRequest) -> RewriteResponse: ...


class ModelDownloader(Protocol):
    def fetch(self, spec: ModelSpec) -> Path: ...


class HuggingFaceDownloader:
    """Fetch one immutable allowlisted model file into the shared HF cache."""

    def fetch(self, spec: ModelSpec) -> Path:
        from huggingface_hub import hf_hub_download

        return Path(
            hf_hub_download(
                repo_id=spec.repo_id,
                filename=spec.filename,
                revision=spec.revision,
                repo_type="model",
            )
        )


def _llama_factory(**kwargs: Any) -> Any:
    from llama_cpp import Llama

    return Llama(**kwargs)


class LlamaCppRuntime:
    """One-slot, CPU-only llama.cpp runtime with an allowlisted model switch."""

    def __init__(
        self,
        settings: Settings,
        *,
        model_factory: ModelFactory | None = None,
        downloader: ModelDownloader | None = None,
    ) -> None:
        self._settings = settings
        self._model_factory = model_factory or _llama_factory
        self._downloader = downloader or HuggingFaceDownloader()
        self._model: Any | None = None
        self._loaded_spec: ModelSpec | None = None
        self._inference_lock = threading.Lock()
        self._state_lock = threading.RLock()
        self._verified_identities: dict[str, FileIdentity] = {}
        self._bad_identities: dict[str, FileIdentity] = {}
        self._readiness_failure: WorkerError | None = None
        self._failure_model_id: str | None = None
        self._failure_identity: FileIdentity | None = None
        self._idle_timer: threading.Timer | None = None
        self._idle_generation = 0

    def start(self) -> None:
        try:
            self._prepare_models_dir()
        except WorkerError as error:
            self._record_readiness_failure(
                error,
                self._settings.default_model_id,
                None,
            )
            logger.error(
                "Rewrite model directory is not usable",
                extra={"error_code": error.code.value},
            )
            return

        model_id = self._settings.preload_model_id
        if model_id is None:
            return

        self._inference_lock.acquire()
        try:
            self._cancel_idle_timer_locked()
            try:
                self._ensure_model_locked(model_id, request_id="startup")
                self._schedule_idle_unload_locked()
            except WorkerError:
                logger.exception(
                    "Configured rewrite model preload failed",
                    extra={"model_id": model_id},
                )
        finally:
            self._inference_lock.release()

    def stop(self) -> None:
        self._inference_lock.acquire()
        try:
            self._cancel_idle_timer_locked()
            self._unload_model_locked()
        finally:
            self._inference_lock.release()

    def unload(self) -> bool:
        if not self._inference_lock.acquire(blocking=False):
            return False
        try:
            self._cancel_idle_timer_locked()
            self._unload_model_locked()
            return True
        finally:
            self._inference_lock.release()

    def readiness(self) -> RuntimeReadiness:
        statuses = self.models()
        with self._state_lock:
            self._clear_failure_if_model_changed_locked()
            failure = self._readiness_failure
            loaded_model_id = (
                self._loaded_spec.model_id
                if self._loaded_spec is not None
                else None
            )

        available_models = sum(status.available for status in statuses)
        if failure is not None:
            return RuntimeReadiness(
                ready=False,
                loaded_model_id=loaded_model_id,
                available_models=available_models,
                reason=failure.code.value,
            )
        if loaded_model_id is not None:
            return RuntimeReadiness(
                ready=True,
                loaded_model_id=loaded_model_id,
                available_models=available_models,
            )

        default_status = next(
            (
                status
                for status in statuses
                if status.spec.model_id == self._settings.default_model_id
            ),
            None,
        )
        if default_status is not None and default_status.available:
            return RuntimeReadiness(
                ready=True,
                loaded_model_id=None,
                available_models=available_models,
            )
        if self._settings.auto_download and self._models_root_ready():
            return RuntimeReadiness(
                ready=True,
                loaded_model_id=None,
                available_models=available_models,
            )
        return RuntimeReadiness(
            ready=False,
            loaded_model_id=None,
            available_models=available_models,
            reason=ErrorCode.MODEL_NOT_READY.value,
        )

    def models(self) -> list[ModelRuntimeStatus]:
        with self._state_lock:
            loaded_model_id = (
                self._loaded_spec.model_id
                if self._loaded_spec is not None
                else None
            )
            verified = dict(self._verified_identities)
            bad = dict(self._bad_identities)

        statuses: list[ModelRuntimeStatus] = []
        for spec in self._settings.models:
            identity = self._safe_identity(spec)
            available = (
                identity is not None
                and identity.size == spec.size_bytes
                and bad.get(spec.model_id) != identity
            )
            statuses.append(
                ModelRuntimeStatus(
                    spec=spec,
                    available=available,
                    loaded=loaded_model_id == spec.model_id,
                    integrity_verified=verified.get(spec.model_id) == identity,
                )
            )
        return statuses

    def rewrite(self, request: RewriteRequest) -> RewriteResponse:
        self._validate_request_limits(request)
        if request.model_id not in self._settings.model_by_id:
            raise WorkerError(
                code=ErrorCode.MODEL_NOT_ALLOWED,
                public_message="Requested rewrite model is not allowlisted",
                status_code=422,
                request_id=request.request_id,
            )
        if not self._inference_lock.acquire(blocking=False):
            raise WorkerError(
                code=ErrorCode.RESOURCE_EXHAUSTED,
                public_message="Rewrite worker is busy",
                status_code=429,
                retryable=True,
                request_id=request.request_id,
            )

        started_at = time.monotonic()
        try:
            self._cancel_idle_timer_locked()
            spec = self._ensure_model_locked(
                request.model_id,
                request_id=request.request_id,
            )
            effective_request = request
            if not request.prompt:
                effective_request = request.model_copy(
                    update={"prompt": self._settings.default_prompt}
                )
            raw = self._run_inference_locked(effective_request)
            parsed = self._parse_output(raw, request_id=request.request_id)
            duration_ms = round((time.monotonic() - started_at) * 1_000)
            response = RewriteResponse(
                api_version="v1",
                request_id=request.request_id,
                fragment_id=request.fragment_id,
                rewritten_text=parsed.rewritten_text,
                reason=parsed.reason,
                model_id=spec.model_id,
                model_revision=spec.revision,
                duration_ms=duration_ms,
            )
            return response
        except MemoryError as exc:
            error = WorkerError(
                code=ErrorCode.OUT_OF_MEMORY,
                public_message="Rewrite model exhausted available memory",
                status_code=503,
                retryable=True,
                request_id=request.request_id,
            )
            self._record_readiness_failure(
                error,
                request.model_id,
                self._safe_identity(self._settings.model_by_id[request.model_id]),
            )
            self._unload_model_locked()
            raise error from exc
        except WorkerError:
            raise
        except Exception as exc:
            logger.exception(
                "Unexpected rewrite inference failure",
                extra={
                    "request_id": request.request_id,
                    "model_id": request.model_id,
                },
            )
            raise WorkerError(
                code=ErrorCode.REWRITE_ERROR,
                public_message="Rewrite inference failed",
                status_code=500,
                retryable=True,
                request_id=request.request_id,
            ) from exc
        finally:
            with self._state_lock:
                model_is_loaded = self._model is not None
            if model_is_loaded:
                self._schedule_idle_unload_locked()
            self._inference_lock.release()

    def _validate_request_limits(self, request: RewriteRequest) -> None:
        limits = (
            ("text", request.text, self._settings.max_text_chars),
            ("stt_text", request.stt_text, self._settings.max_stt_text_chars),
            ("prompt", request.prompt, self._settings.max_prompt_chars),
        )
        for name, value, limit in limits:
            if len(value) > limit:
                raise WorkerError(
                    code=ErrorCode.INVALID_REQUEST,
                    public_message=f"{name} exceeds the configured limit",
                    status_code=413,
                    request_id=request.request_id,
                )
        if not request.text.strip():
            raise WorkerError(
                code=ErrorCode.INVALID_REQUEST,
                public_message="text must contain non-whitespace characters",
                status_code=422,
                request_id=request.request_id,
            )

    def _ensure_model_locked(
        self,
        model_id: str,
        *,
        request_id: str,
    ) -> ModelSpec:
        spec = self._settings.model_by_id[model_id]
        if self._loaded_spec is not None and self._loaded_spec.model_id == model_id:
            return spec

        self._unload_model_locked()
        self._ensure_local_model_locked(spec, request_id=request_id)
        path, identity = self._verify_integrity(
            spec,
            request_id=request_id,
        )
        try:
            model = self._model_factory(
                model_path=str(path),
                n_gpu_layers=0,
                use_mmap=True,
                use_mlock=False,
                n_ctx=self._settings.n_ctx,
                n_batch=self._settings.n_batch,
                n_threads=self._settings.n_threads,
                n_threads_batch=self._settings.n_threads,
                flash_attn=False,
                offload_kqv=False,
                verbose=False,
            )
        except MemoryError:
            raise
        except Exception as exc:
            error = WorkerError(
                code=ErrorCode.MODEL_NOT_READY,
                public_message="Rewrite model could not be loaded",
                status_code=503,
                retryable=True,
                details={"model_id": spec.model_id},
                request_id=request_id,
            )
            self._record_readiness_failure(error, spec.model_id, identity)
            raise error from exc

        with self._state_lock:
            self._model = model
            self._loaded_spec = spec
            self._readiness_failure = None
            self._failure_model_id = None
            self._failure_identity = None
        logger.info(
            "Rewrite model loaded",
            extra={
                "model_id": spec.model_id,
                "revision": spec.revision,
                "device": "cpu",
            },
        )
        return spec

    def _run_inference_locked(self, request: RewriteRequest) -> Any:
        with self._state_lock:
            model = self._model
        if model is None:
            raise WorkerError(
                code=ErrorCode.MODEL_NOT_READY,
                public_message="Rewrite model is not loaded",
                status_code=503,
                retryable=True,
                request_id=request.request_id,
            )

        try:
            return model.create_chat_completion(
                messages=build_messages(request),
                response_format={
                    "type": "json_object",
                    "schema": rewrite_output_schema(),
                },
                temperature=(
                    request.temperature
                    if request.temperature is not None
                    else self._settings.temperature
                ),
                top_k=(
                    request.top_k
                    if request.top_k is not None
                    else self._settings.top_k
                ),
                top_p=(
                    request.top_p
                    if request.top_p is not None
                    else self._settings.top_p
                ),
                min_p=(
                    request.min_p
                    if request.min_p is not None
                    else self._settings.min_p
                ),
                repeat_penalty=(
                    request.repeat_penalty
                    if request.repeat_penalty is not None
                    else self._settings.repeat_penalty
                ),
                max_tokens=(
                    request.max_tokens
                    if request.max_tokens is not None
                    else self._settings.max_tokens
                ),
            )
        except MemoryError:
            raise
        except Exception as exc:
            raise WorkerError(
                code=ErrorCode.REWRITE_ERROR,
                public_message="Rewrite inference failed",
                status_code=500,
                retryable=True,
                request_id=request.request_id,
            ) from exc

    def _parse_output(
        self,
        completion: Any,
        *,
        request_id: str,
    ) -> RewriteModelOutput:
        try:
            content = completion["choices"][0]["message"]["content"]
            if not isinstance(content, str):
                raise TypeError("completion content is not text")
            payload = json.loads(content)
            parsed = RewriteModelOutput.model_validate(payload)
        except (KeyError, IndexError, TypeError, ValueError, ValidationError) as exc:
            raise WorkerError(
                code=ErrorCode.OUTPUT_INVALID,
                public_message="Rewrite model returned an invalid JSON result",
                status_code=502,
                retryable=True,
                request_id=request_id,
            ) from exc

        if len(parsed.rewritten_text) > self._settings.max_output_chars:
            raise WorkerError(
                code=ErrorCode.OUTPUT_INVALID,
                public_message="Rewritten text exceeds the configured limit",
                status_code=502,
                retryable=True,
                request_id=request_id,
            )
        if len(parsed.reason) > self._settings.max_reason_chars:
            raise WorkerError(
                code=ErrorCode.OUTPUT_INVALID,
                public_message="Rewrite reason exceeds the configured limit",
                status_code=502,
                retryable=True,
                request_id=request_id,
            )
        return parsed

    def _prepare_models_dir(self) -> None:
        try:
            root = self._settings.models_dir.expanduser().resolve(strict=False)
            root.mkdir(parents=True, exist_ok=True)
        except OSError as exc:
            raise WorkerError(
                code=ErrorCode.MODEL_NOT_READY,
                public_message="Rewrite model directory cannot be created",
                status_code=503,
                retryable=True,
                request_id="startup",
            ) from exc
        if not root.is_dir() or not os.access(root, os.W_OK | os.X_OK):
            raise WorkerError(
                code=ErrorCode.MODEL_NOT_READY,
                public_message="Rewrite model directory is not writable",
                status_code=503,
                retryable=True,
                request_id="startup",
            )

    def _models_root_ready(self) -> bool:
        try:
            root = self._settings.models_dir.expanduser().resolve(strict=False)
            return root.is_dir() and os.access(root, os.W_OK | os.X_OK)
        except OSError:
            return False

    def _ensure_local_model_locked(
        self,
        spec: ModelSpec,
        *,
        request_id: str,
    ) -> None:
        if self._safe_identity(spec) is not None:
            return
        try:
            target = spec.path_under(self._settings.models_dir)
        except ValueError as exc:
            error = WorkerError(
                code=ErrorCode.MODEL_INTEGRITY_ERROR,
                public_message="Allowlisted model path is unsafe",
                status_code=503,
                details={"model_id": spec.model_id},
                request_id=request_id,
            )
            self._record_readiness_failure(error, spec.model_id, None)
            raise error from exc

        if os.path.lexists(target):
            error = WorkerError(
                code=ErrorCode.MODEL_INTEGRITY_ERROR,
                public_message="Allowlisted model path is not a regular file",
                status_code=503,
                details={"model_id": spec.model_id},
                request_id=request_id,
            )
            self._record_readiness_failure(error, spec.model_id, None)
            raise error
        if not self._settings.auto_download:
            error = WorkerError(
                code=ErrorCode.MODEL_NOT_READY,
                public_message="Allowlisted model file is not available",
                status_code=503,
                retryable=True,
                details={"model_id": spec.model_id},
                request_id=request_id,
            )
            self._record_readiness_failure(error, spec.model_id, None)
            raise error

        self._download_model_locked(spec, target=target, request_id=request_id)

    def _download_model_locked(
        self,
        spec: ModelSpec,
        *,
        target: Path,
        request_id: str,
    ) -> None:
        temporary_path: Path | None = None
        try:
            target.parent.mkdir(parents=True, exist_ok=True)
            source = self._downloader.fetch(spec).expanduser().resolve(strict=True)
            source_info = source.stat()
            if not stat.S_ISREG(source_info.st_mode):
                raise OSError("downloaded model is not a regular file")

            descriptor, temporary_name = tempfile.mkstemp(
                prefix=f".{spec.filename}.",
                suffix=".part",
                dir=target.parent,
            )
            temporary_path = Path(temporary_name)
            with (
                os.fdopen(descriptor, "wb") as destination,
                source.open("rb") as input_file,
            ):
                shutil.copyfileobj(
                    input_file,
                    destination,
                    length=8 * 1024 * 1024,
                )
                destination.flush()
                os.fsync(destination.fileno())

            downloaded_identity = self._identity_for_path(temporary_path)
            if downloaded_identity is None or downloaded_identity.size != spec.size_bytes:
                raise WorkerError(
                    code=ErrorCode.MODEL_INTEGRITY_ERROR,
                    public_message="Downloaded model size does not match its pin",
                    status_code=503,
                    details={"model_id": spec.model_id},
                    request_id=request_id,
                )
            if self._sha256_file(temporary_path) != spec.sha256:
                raise WorkerError(
                    code=ErrorCode.MODEL_INTEGRITY_ERROR,
                    public_message="Downloaded model SHA256 does not match its pin",
                    status_code=503,
                    details={"model_id": spec.model_id},
                    request_id=request_id,
                )

            temporary_path.chmod(0o444)
            os.replace(temporary_path, target)
            temporary_path = None
            self._fsync_directory(target.parent)
            logger.info(
                "Pinned rewrite model downloaded",
                extra={
                    "model_id": spec.model_id,
                    "revision": spec.revision,
                    "size_bytes": spec.size_bytes,
                },
            )
        except WorkerError as error:
            self._record_readiness_failure(error, spec.model_id, None)
            raise
        except Exception as exc:
            error = WorkerError(
                code=ErrorCode.MODEL_DOWNLOAD_ERROR,
                public_message="Pinned rewrite model download failed",
                status_code=503,
                retryable=True,
                details={"model_id": spec.model_id},
                request_id=request_id,
            )
            self._record_readiness_failure(error, spec.model_id, None)
            raise error from exc
        finally:
            if temporary_path is not None:
                try:
                    temporary_path.unlink(missing_ok=True)
                except OSError:
                    logger.warning(
                        "Could not remove incomplete rewrite model",
                        extra={"model_id": spec.model_id},
                    )

    def _identity_for_path(self, path: Path) -> FileIdentity | None:
        try:
            info = path.stat(follow_symlinks=False)
        except OSError:
            return None
        if not stat.S_ISREG(info.st_mode):
            return None
        return FileIdentity(
            device=info.st_dev,
            inode=info.st_ino,
            size=info.st_size,
            modified_ns=info.st_mtime_ns,
        )

    def _sha256_file(self, path: Path) -> str:
        digest = hashlib.sha256()
        with path.open("rb") as model_file:
            for chunk in iter(lambda: model_file.read(8 * 1024 * 1024), b""):
                digest.update(chunk)
        return digest.hexdigest()

    def _fsync_directory(self, path: Path) -> None:
        try:
            descriptor = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
        except OSError:
            return
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)

    def _verify_integrity(
        self,
        spec: ModelSpec,
        *,
        request_id: str,
    ) -> tuple[Path, FileIdentity]:
        path = spec.path_under(self._settings.models_dir)
        identity = self._safe_identity(spec)
        if identity is None:
            error = WorkerError(
                code=ErrorCode.MODEL_NOT_READY,
                public_message="Allowlisted model file is not available",
                status_code=503,
                retryable=True,
                details={"model_id": spec.model_id},
                request_id=request_id,
            )
            self._record_readiness_failure(error, spec.model_id, None)
            raise error
        if identity.size != spec.size_bytes:
            error = WorkerError(
                code=ErrorCode.MODEL_INTEGRITY_ERROR,
                public_message="Allowlisted model size does not match its pin",
                status_code=503,
                details={"model_id": spec.model_id},
                request_id=request_id,
            )
            with self._state_lock:
                self._bad_identities[spec.model_id] = identity
            self._record_readiness_failure(error, spec.model_id, identity)
            raise error

        with self._state_lock:
            if self._verified_identities.get(spec.model_id) == identity:
                return path, identity

        try:
            digest = self._sha256_file(path)
        except OSError as exc:
            error = WorkerError(
                code=ErrorCode.MODEL_NOT_READY,
                public_message="Allowlisted model file cannot be read",
                status_code=503,
                retryable=True,
                details={"model_id": spec.model_id},
                request_id=request_id,
            )
            self._record_readiness_failure(error, spec.model_id, identity)
            raise error from exc

        identity_after = self._safe_identity(spec)
        if identity_after != identity or digest != spec.sha256:
            bad_identity = identity_after or identity
            error = WorkerError(
                code=ErrorCode.MODEL_INTEGRITY_ERROR,
                public_message="Allowlisted model SHA256 does not match its pin",
                status_code=503,
                details={"model_id": spec.model_id},
                request_id=request_id,
            )
            with self._state_lock:
                self._bad_identities[spec.model_id] = bad_identity
                self._verified_identities.pop(spec.model_id, None)
            self._record_readiness_failure(
                error,
                spec.model_id,
                bad_identity,
            )
            raise error

        with self._state_lock:
            self._verified_identities[spec.model_id] = identity
            self._bad_identities.pop(spec.model_id, None)
        return path, identity

    def _safe_identity(self, spec: ModelSpec) -> FileIdentity | None:
        try:
            path = spec.path_under(self._settings.models_dir)
            info = path.stat()
        except (OSError, ValueError):
            return None
        if not stat.S_ISREG(info.st_mode):
            return None
        return FileIdentity(
            device=info.st_dev,
            inode=info.st_ino,
            size=info.st_size,
            modified_ns=info.st_mtime_ns,
        )

    def _record_readiness_failure(
        self,
        error: WorkerError,
        model_id: str,
        identity: FileIdentity | None,
    ) -> None:
        with self._state_lock:
            self._readiness_failure = error
            self._failure_model_id = model_id
            self._failure_identity = identity

    def _clear_failure_if_model_changed_locked(self) -> None:
        if self._failure_model_id is None:
            return
        spec = self._settings.model_by_id.get(self._failure_model_id)
        if spec is None:
            return
        if self._safe_identity(spec) != self._failure_identity:
            self._readiness_failure = None
            self._failure_model_id = None
            self._failure_identity = None

    def _unload_model_locked(self) -> None:
        with self._state_lock:
            model = self._model
            loaded_model_id = (
                self._loaded_spec.model_id
                if self._loaded_spec is not None
                else None
            )
            self._model = None
            self._loaded_spec = None
        if model is None:
            return
        close = getattr(model, "close", None)
        if callable(close):
            try:
                close()
            except Exception:
                logger.exception(
                    "Rewrite model close failed",
                    extra={"model_id": loaded_model_id},
                )
        del model
        gc.collect()
        logger.info(
            "Rewrite model unloaded",
            extra={"model_id": loaded_model_id},
        )

    def _cancel_idle_timer_locked(self) -> None:
        with self._state_lock:
            self._idle_generation += 1
            timer = self._idle_timer
            self._idle_timer = None
        if timer is not None:
            timer.cancel()

    def _schedule_idle_unload_locked(self) -> None:
        delay = self._settings.idle_unload_seconds
        if delay <= 0:
            return
        with self._state_lock:
            self._idle_generation += 1
            generation = self._idle_generation
            old_timer = self._idle_timer
            timer = threading.Timer(
                delay,
                self._idle_unload,
                args=(generation,),
            )
            timer.daemon = True
            self._idle_timer = timer
        if old_timer is not None:
            old_timer.cancel()
        timer.start()

    def _idle_unload(self, generation: int) -> None:
        if not self._inference_lock.acquire(blocking=False):
            return
        try:
            with self._state_lock:
                if generation != self._idle_generation:
                    return
                self._idle_timer = None
            self._unload_model_locked()
        finally:
            self._inference_lock.release()
