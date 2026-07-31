from __future__ import annotations

import gc
import hashlib
import importlib.metadata
import logging
import os
import random
import tempfile
import threading
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Protocol

import numpy as np

from .audio import float_audio_to_pcm_s16le, inspect_reference_audio
from .cache import VoicePromptCache
from .config import Settings
from .errors import ErrorCode, WorkerError
from .schemas import GenerateRequest, VoiceMode, WorkerCapabilities

logger = logging.getLogger(__name__)


@dataclass(frozen=True, slots=True)
class VoiceReference:
    audio: bytes
    content_type: str


@dataclass(frozen=True, slots=True)
class GenerationResult:
    pcm_s16le: bytes
    sample_rate: int
    seed_used: int
    voice_cache_hit: bool
    generation_duration_ms: int
    model_id: str
    model_version: str
    device: str
    warnings: tuple[str, ...] = ()

    @property
    def total_samples(self) -> int:
        return len(self.pcm_s16le) // 2

    @property
    def duration_ms(self) -> int:
        return round(self.total_samples * 1000 / self.sample_rate)


class OmniVoiceRuntime(Protocol):
    def start(self) -> None: ...

    def capabilities(self) -> WorkerCapabilities: ...

    def generate(
        self,
        request: GenerateRequest,
        reference: VoiceReference | None,
    ) -> GenerationResult: ...


class RealOmniVoiceRuntime:
    def __init__(self, settings: Settings) -> None:
        self._settings = settings
        self._model: Any | None = None
        self._torch: Any | None = None
        self._generation_config_type: Any | None = None
        self._model_version = "unknown"
        self._supported_languages: list[str] = []
        self._device_name = settings.device
        self._total_vram_bytes = 0
        self._available_vram_bytes = 0
        self._ready = False
        self._last_load_attempt = 0.0
        self._last_error_code: ErrorCode | None = None
        self._state_lock = threading.Lock()
        self._inference_slot = threading.BoundedSemaphore(value=1)
        self._prompt_cache = VoicePromptCache(
            max_entries=settings.cache_max_entries,
            max_weight=settings.cache_max_reference_bytes,
        )

    def start(self) -> None:
        if self._ready:
            return

        with self._state_lock:
            if self._ready:
                return

            now = time.monotonic()
            if (
                self._last_load_attempt
                and now - self._last_load_attempt < self._settings.load_retry_seconds
            ):
                return
            self._last_load_attempt = now

            started_at = time.monotonic()
            try:
                import torch
                from huggingface_hub import snapshot_download
                from omnivoice import OmniVoice, OmniVoiceGenerationConfig
                from omnivoice.utils.lang_map import LANG_IDS

                dtype = getattr(torch, self._settings.dtype)
                resolved_model_path = snapshot_download(
                    repo_id=self._settings.model_id,
                    revision=self._settings.model_revision,
                    local_files_only=self._settings.local_files_only,
                )
                model = OmniVoice.from_pretrained(
                    resolved_model_path,
                    device_map=self._settings.device,
                    dtype=dtype,
                    load_asr=False,
                )

                if self._settings.warmup_enabled:
                    self._warm_up(
                        model=model,
                        torch_module=torch,
                        config_type=OmniVoiceGenerationConfig,
                    )

                self._model = model
                self._torch = torch
                self._generation_config_type = OmniVoiceGenerationConfig
                package_version = importlib.metadata.version("omnivoice")
                self._model_version = (
                    f"{self._settings.model_revision}+omnivoice-{package_version}"
                )
                self._supported_languages = sorted(LANG_IDS)
                self._capture_device_info(torch)
                self._last_error_code = None
                self._ready = True
                logger.info(
                    "OmniVoice model is ready",
                    extra={
                        "worker_id": self._settings.worker_id,
                        "model_id": self._settings.model_id,
                        "device": self._settings.device,
                        "duration_ms": round((time.monotonic() - started_at) * 1000),
                    },
                )
            except Exception:
                self._ready = False
                self._last_error_code = ErrorCode.MODEL_NOT_READY
                gc.collect()
                try:
                    if "torch" in locals() and torch.cuda.is_available():
                        torch.cuda.empty_cache()
                except Exception:
                    logger.debug("CUDA cleanup after load failure failed", exc_info=True)
                logger.exception(
                    "OmniVoice model initialization failed",
                    extra={
                        "worker_id": self._settings.worker_id,
                        "model_id": self._settings.model_id,
                        "device": self._settings.device,
                    },
                )

    def _warm_up(self, *, model: Any, torch_module: Any, config_type: Any) -> None:
        self._set_seed(torch_module, 0)
        result = model.generate(
            text="Тест.",
            language="ru",
            speed=1.0,
            generation_config=config_type(
                num_step=self._settings.warmup_steps,
                guidance_scale=1.0,
                audio_chunk_threshold=999.0,
            ),
        )
        if not result or np.asarray(result[0]).size == 0:
            raise RuntimeError("warm-up returned empty audio")

    def _capture_device_info(self, torch_module: Any) -> None:
        if not self._settings.device.startswith("cuda") or not torch_module.cuda.is_available():
            self._device_name = self._settings.device
            return

        index = torch_module.device(self._settings.device).index or 0
        properties = torch_module.cuda.get_device_properties(index)
        available, total = torch_module.cuda.mem_get_info(index)
        self._device_name = str(properties.name)
        self._total_vram_bytes = int(total)
        self._available_vram_bytes = int(available)

    def capabilities(self) -> WorkerCapabilities:
        warnings: list[str] = []
        if self._last_error_code is not None:
            warnings.append(self._last_error_code.value)

        return WorkerCapabilities(
            api_version=self._settings.api_version,
            worker_id=self._settings.worker_id,
            service_type="omnivoice",
            model_id=self._settings.model_id,
            model_version=self._model_version,
            device_type=self._settings.device.split(":", maxsplit=1)[0],
            device_name=self._device_name,
            total_vram_bytes=self._total_vram_bytes,
            available_vram_bytes=self._available_vram_bytes,
            max_concurrency=1,
            supported_sample_rates=[24_000],
            supported_languages=self._supported_languages,
            supported_modes=list(VoiceMode),
            ready=self._ready,
            warnings=warnings,
        )

    def generate(
        self,
        request: GenerateRequest,
        reference: VoiceReference | None,
    ) -> GenerationResult:
        self._validate_request(request, reference)
        if not self._ready:
            self.start()
        if not self._ready or self._model is None:
            raise WorkerError(
                code=ErrorCode.MODEL_NOT_READY,
                public_message="OmniVoice model is not ready",
                status_code=503,
                retryable=True,
                request_id=request.request_id,
            )
        if self._supported_languages and request.language not in self._supported_languages:
            raise WorkerError(
                code=ErrorCode.INVALID_REQUEST,
                public_message="Requested language is not supported by OmniVoice",
                status_code=422,
                request_id=request.request_id,
            )
        if not self._inference_slot.acquire(blocking=False):
            raise WorkerError(
                code=ErrorCode.RESOURCE_EXHAUSTED,
                public_message="OmniVoice worker is busy",
                status_code=429,
                retryable=True,
                request_id=request.request_id,
            )

        started_at = time.monotonic()
        try:
            seed = (
                request.seed if request.seed is not None else random.SystemRandom().randrange(2**32)
            )
            self._set_seed(self._torch, seed)
            generation_config = self._generation_config_type(
                num_step=request.num_steps,
                guidance_scale=request.guidance_scale,
                denoise=request.denoise,
                t_shift=request.t_shift,
                layer_penalty_factor=request.layer_penalty_factor,
                position_temperature=request.position_temperature,
                class_temperature=request.class_temperature,
                preprocess_prompt=request.preprocess_prompt,
                postprocess_output=request.postprocess_output,
                audio_chunk_duration=request.audio_chunk_duration,
                audio_chunk_threshold=request.audio_chunk_threshold,
                pad_duration=request.pad_duration,
                fade_duration=request.fade_duration,
            )
            kwargs: dict[str, Any] = {
                "text": request.text,
                "language": request.language,
                "speed": request.speed,
                "normalize_text": request.normalize_text,
                "generation_config": generation_config,
            }

            cache_hit = False
            if request.mode == VoiceMode.VOICE_CLONE:
                assert reference is not None
                prompt, cache_hit = self._get_voice_prompt(request, reference)
                kwargs["voice_clone_prompt"] = prompt
                if request.instruction:
                    kwargs["instruct"] = request.instruction
            elif request.mode == VoiceMode.VOICE_DESIGN:
                kwargs["instruct"] = request.instruction

            try:
                generated = self._model.generate(**kwargs)
            except Exception as exc:
                self._raise_inference_error(exc, request_id=request.request_id)

            if not generated:
                raise WorkerError(
                    code=ErrorCode.INFERENCE_ERROR,
                    public_message="OmniVoice returned no audio",
                    status_code=500,
                    retryable=True,
                    request_id=request.request_id,
                )
            sample_rate = int(self._model.sampling_rate)
            if sample_rate != 24_000:
                raise WorkerError(
                    code=ErrorCode.INFERENCE_ERROR,
                    public_message="OmniVoice returned an unsupported sample rate",
                    status_code=500,
                    retryable=False,
                    request_id=request.request_id,
                )
            try:
                prepared_pcm = float_audio_to_pcm_s16le(
                    generated[0],
                    max_samples=round(
                        sample_rate * self._settings.max_output_seconds
                    ),
                    min_rms=self._settings.min_output_rms,
                    clipping_warning_ratio=self._settings.clipping_warning_ratio,
                )
            except (TypeError, ValueError) as exc:
                raise WorkerError(
                    code=ErrorCode.INFERENCE_ERROR,
                    public_message="OmniVoice returned invalid audio",
                    status_code=500,
                    retryable=True,
                    request_id=request.request_id,
                ) from exc

            return GenerationResult(
                pcm_s16le=prepared_pcm.data,
                sample_rate=sample_rate,
                seed_used=seed,
                voice_cache_hit=cache_hit,
                generation_duration_ms=round((time.monotonic() - started_at) * 1000),
                model_id=self._settings.model_id,
                model_version=self._model_version,
                device=self._settings.device,
                warnings=prepared_pcm.warnings,
            )
        finally:
            self._inference_slot.release()

    def _validate_request(
        self,
        request: GenerateRequest,
        reference: VoiceReference | None,
    ) -> None:
        text = request.text.strip()
        if len(text) > self._settings.max_text_chars:
            raise WorkerError(
                code=ErrorCode.INVALID_REQUEST,
                public_message="Text exceeds the configured character limit",
                status_code=413,
                request_id=request.request_id,
            )
        if len(text.split()) > self._settings.max_text_words:
            raise WorkerError(
                code=ErrorCode.INVALID_REQUEST,
                public_message="Text exceeds the configured word limit",
                status_code=413,
                request_id=request.request_id,
            )

        if request.mode == VoiceMode.VOICE_CLONE:
            if reference is None:
                raise WorkerError(
                    code=ErrorCode.INVALID_REQUEST,
                    public_message="VOICE_CLONE requires voice_reference",
                    status_code=422,
                    request_id=request.request_id,
                )
            if not (request.voice_reference_text or "").strip():
                raise WorkerError(
                    code=ErrorCode.INVALID_REQUEST,
                    public_message="VOICE_CLONE requires voice_reference_text",
                    status_code=422,
                    request_id=request.request_id,
                )
        elif reference is not None or request.voice_reference_text:
            raise WorkerError(
                code=ErrorCode.INVALID_REQUEST,
                public_message="Voice reference is only valid for VOICE_CLONE",
                status_code=422,
                request_id=request.request_id,
            )

        if request.mode == VoiceMode.VOICE_DESIGN and not (request.instruction or "").strip():
            raise WorkerError(
                code=ErrorCode.INVALID_REQUEST,
                public_message="VOICE_DESIGN requires instruction",
                status_code=422,
                request_id=request.request_id,
            )
        if request.mode == VoiceMode.AUTO_VOICE and request.instruction:
            raise WorkerError(
                code=ErrorCode.INVALID_REQUEST,
                public_message="AUTO_VOICE does not accept instruction",
                status_code=422,
                request_id=request.request_id,
            )

    def _get_voice_prompt(
        self,
        request: GenerateRequest,
        reference: VoiceReference,
    ) -> tuple[Any, bool]:
        if len(reference.audio) > self._settings.max_reference_bytes:
            raise WorkerError(
                code=ErrorCode.VOICE_REFERENCE_ERROR,
                public_message="Voice reference exceeds the configured size limit",
                status_code=413,
                request_id=request.request_id,
            )

        audio_hash = hashlib.sha256(reference.audio).hexdigest()
        if (
            request.voice_reference_sha256 is not None
            and request.voice_reference_sha256.lower() != audio_hash
        ):
            raise WorkerError(
                code=ErrorCode.VOICE_REFERENCE_ERROR,
                public_message="Voice reference SHA-256 does not match",
                status_code=422,
                request_id=request.request_id,
            )

        info = inspect_reference_audio(
            reference.audio,
            max_seconds=self._settings.max_reference_seconds,
            request_id=request.request_id,
        )
        expected_content_types = {
            "FLAC": {"audio/flac", "audio/x-flac"},
            "WAV": {"audio/wav", "audio/x-wav", "audio/vnd.wave"},
        }
        if (
            reference.content_type != "application/octet-stream"
            and reference.content_type not in expected_content_types[info.format.upper()]
        ):
            raise WorkerError(
                code=ErrorCode.VOICE_REFERENCE_ERROR,
                public_message="Voice reference media type does not match its container",
                status_code=415,
                request_id=request.request_id,
            )
        normalized_text = " ".join((request.voice_reference_text or "").split())
        cache_key = self._voice_cache_key(audio_hash, normalized_text)
        cached, hit = self._prompt_cache.get(cache_key)
        if hit:
            return cached, True

        suffix = ".flac" if info.format.upper() == "FLAC" else ".wav"
        temporary_path: Path | None = None
        try:
            with tempfile.NamedTemporaryFile(
                prefix="omnivoice-reference-",
                suffix=suffix,
                delete=False,
            ) as temporary:
                temporary.write(reference.audio)
                temporary_path = Path(temporary.name)

            try:
                prompt = self._model.create_voice_clone_prompt(
                    ref_audio=str(temporary_path),
                    ref_text=normalized_text,
                )
            except Exception as exc:
                if self._is_oom(exc):
                    self._recover_from_oom()
                    raise WorkerError(
                        code=ErrorCode.OUT_OF_MEMORY,
                        public_message="OmniVoice ran out of GPU memory",
                        status_code=503,
                        retryable=False,
                        request_id=request.request_id,
                    ) from exc
                raise WorkerError(
                    code=ErrorCode.VOICE_REFERENCE_ERROR,
                    public_message="Voice reference could not be prepared",
                    status_code=422,
                    request_id=request.request_id,
                ) from exc
        finally:
            if temporary_path is not None:
                temporary_path.unlink(missing_ok=True)

        self._prompt_cache.put(cache_key, prompt, weight=len(reference.audio))
        return prompt, False

    def _voice_cache_key(self, audio_hash: str, normalized_text: str) -> str:
        payload = "\0".join(
            (
                "omnivoice-clone-cache-v1",
                self._settings.model_id,
                self._model_version,
                audio_hash,
                normalized_text,
            )
        )
        return hashlib.sha256(payload.encode("utf-8")).hexdigest()

    @staticmethod
    def _set_seed(torch_module: Any, seed: int) -> None:
        torch_module.manual_seed(seed)
        if torch_module.cuda.is_available():
            torch_module.cuda.manual_seed(seed)
            torch_module.cuda.manual_seed_all(seed)
        random.seed(seed)
        np.random.seed(seed)

    def _raise_inference_error(self, exc: Exception, *, request_id: str) -> None:
        if self._is_oom(exc):
            self._recover_from_oom()
            raise WorkerError(
                code=ErrorCode.OUT_OF_MEMORY,
                public_message="OmniVoice ran out of GPU memory",
                status_code=503,
                retryable=False,
                request_id=request_id,
            ) from exc
        raise WorkerError(
            code=ErrorCode.INFERENCE_ERROR,
            public_message="OmniVoice inference failed",
            status_code=500,
            retryable=True,
            request_id=request_id,
        ) from exc

    def _is_oom(self, exc: Exception) -> bool:
        if self._torch is not None and isinstance(exc, self._torch.OutOfMemoryError):
            return True
        return "out of memory" in str(exc).lower()

    def _recover_from_oom(self) -> None:
        self._ready = False
        self._last_error_code = ErrorCode.OUT_OF_MEMORY
        self._prompt_cache.clear()
        gc.collect()
        if self._torch is not None and self._torch.cuda.is_available():
            self._torch.cuda.empty_cache()
        if self._settings.exit_on_oom:
            exit_timer = threading.Timer(0.25, lambda: os._exit(1))
            exit_timer.daemon = True
            exit_timer.start()
