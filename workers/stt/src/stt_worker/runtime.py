from __future__ import annotations

import importlib.metadata
import logging
import math
import os
import threading
import time
import unicodedata
from dataclasses import dataclass
from typing import Any, Protocol

import numpy as np

from .audio import DecodedAudio, decode_audio
from .config import Settings
from .errors import ErrorCode, WorkerError
from .schemas import (
    TranscribeRequest,
    TranscribeResponse,
    TranscriptSegment,
    Word,
    WorkerCapabilities,
)

logger = logging.getLogger(__name__)


@dataclass(frozen=True, slots=True)
class AudioPayload:
    data: bytes
    content_type: str


class STTRuntime(Protocol):
    def start(self) -> None: ...

    def capabilities(self) -> WorkerCapabilities: ...

    def transcribe(
        self,
        request: TranscribeRequest,
        payload: AudioPayload,
    ) -> TranscribeResponse: ...


def normalize_transcript(text: str) -> str:
    """Diagnostic normalization only; Go remains authoritative."""

    normalized = unicodedata.normalize("NFKC", text).casefold()
    return " ".join(normalized.split())


def _probability_from_log(value: float | None) -> float:
    if value is None or not math.isfinite(value):
        return 0.0
    return min(1.0, max(0.0, math.exp(value)))


class FasterWhisperRuntime:
    def __init__(self, settings: Settings) -> None:
        self._settings = settings
        self._model: Any | None = None
        self._model_version = "unknown"
        self._supported_languages: list[str] = []
        self._ready = False
        self._last_load_attempt = 0.0
        self._last_error_code: ErrorCode | None = None
        self._state_lock = threading.Lock()
        self._inference_slot = threading.BoundedSemaphore(value=1)

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
                from faster_whisper import WhisperModel

                model = WhisperModel(
                    self._settings.model_id,
                    device=self._settings.device,
                    compute_type=self._settings.compute_type,
                    cpu_threads=self._settings.cpu_threads,
                    num_workers=1,
                    download_root=self._settings.download_root,
                    local_files_only=self._settings.local_files_only,
                    revision=self._settings.model_revision,
                )
                if self._settings.warmup_enabled:
                    segments, _ = model.transcribe(
                        np.zeros(1600, dtype=np.float32),
                        word_timestamps=False,
                        language=None,
                        vad_filter=False,
                    )
                    list(segments)

                self._model = model
                package_version = importlib.metadata.version("faster-whisper")
                self._model_version = (
                    f"{self._settings.model_revision}+faster-whisper-{package_version}"
                )
                self._supported_languages = sorted(model.supported_languages)
                self._last_error_code = None
                self._ready = True
                logger.info(
                    "STT model is ready",
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
                logger.exception(
                    "STT model initialization failed",
                    extra={
                        "worker_id": self._settings.worker_id,
                        "model_id": self._settings.model_id,
                        "device": self._settings.device,
                    },
                )

    def capabilities(self) -> WorkerCapabilities:
        warnings: list[str] = []
        if self._last_error_code is not None:
            warnings.append(self._last_error_code.value)

        return WorkerCapabilities(
            api_version=self._settings.api_version,
            worker_id=self._settings.worker_id,
            service_type="stt",
            model_id=self._settings.model_id,
            model_version=self._model_version,
            device_type=self._settings.device,
            device_name=self._settings.device,
            total_vram_bytes=0,
            available_vram_bytes=0,
            max_concurrency=1,
            supported_sample_rates=[8_000, 16_000, 22_050, 24_000, 44_100, 48_000],
            supported_languages=self._supported_languages,
            supported_modes=["TRANSCRIBE"],
            ready=self._ready,
            warnings=warnings,
        )

    def transcribe(
        self,
        request: TranscribeRequest,
        payload: AudioPayload,
    ) -> TranscribeResponse:
        if (
            request.expected_text is not None
            and len(request.expected_text) > self._settings.max_expected_text_chars
        ):
            raise WorkerError(
                code=ErrorCode.INVALID_REQUEST,
                public_message="expected_text exceeds the configured limit",
                status_code=413,
                request_id=request.request_id,
            )
        if len(payload.data) > self._settings.max_audio_bytes:
            raise WorkerError(
                code=ErrorCode.INVALID_AUDIO,
                public_message="Audio payload exceeds the configured size limit",
                status_code=413,
                request_id=request.request_id,
            )

        if not self._ready:
            self.start()
        if not self._ready or self._model is None:
            raise WorkerError(
                code=ErrorCode.MODEL_NOT_READY,
                public_message="STT model is not ready",
                status_code=503,
                retryable=True,
                request_id=request.request_id,
            )
        if request.language and request.language not in self._supported_languages:
            raise WorkerError(
                code=ErrorCode.INVALID_REQUEST,
                public_message="Requested language is not supported by the STT model",
                status_code=422,
                request_id=request.request_id,
            )
        if not self._inference_slot.acquire(blocking=False):
            raise WorkerError(
                code=ErrorCode.RESOURCE_EXHAUSTED,
                public_message="STT worker is busy",
                status_code=429,
                retryable=True,
                request_id=request.request_id,
            )

        started_at = time.monotonic()
        try:
            decoded = decode_audio(
                payload.data,
                spec=request.audio,
                declared_sha256=request.audio_sha256,
                max_seconds=self._settings.max_audio_seconds,
                min_rms=self._settings.min_audio_rms,
                request_id=request.request_id,
            )
            try:
                segment_iterator, info = self._model.transcribe(
                    decoded.samples,
                    word_timestamps=request.word_timestamps_required,
                    language=request.language,
                    beam_size=request.beam_size,
                    patience=request.patience,
                    temperature=request.temperature,
                    vad_filter=request.vad_filter,
                )
                # Faster Whisper inference is lazy. The lock must cover this list().
                raw_segments = list(segment_iterator)
            except Exception as exc:
                self._raise_transcription_error(exc, request_id=request.request_id)

            response = self._build_response(
                request=request,
                decoded=decoded,
                raw_segments=raw_segments,
                info=info,
                processing_duration_ms=round((time.monotonic() - started_at) * 1000),
            )
            return response
        finally:
            self._inference_slot.release()

    def _build_response(
        self,
        *,
        request: TranscribeRequest,
        decoded: DecodedAudio,
        raw_segments: list[Any],
        info: Any,
        processing_duration_ms: int,
    ) -> TranscribeResponse:
        segments: list[TranscriptSegment] = []
        all_words: list[Word] = []
        text_parts: list[str] = []
        segment_confidences: list[float] = []

        for raw_segment in raw_segments:
            segment_text = str(raw_segment.text)
            text_parts.append(segment_text)
            segment_words: list[Word] = []
            for raw_word in raw_segment.words or []:
                word = Word(
                    text=str(raw_word.word),
                    normalized_text=normalize_transcript(str(raw_word.word)),
                    start_ms=max(0, round(float(raw_word.start) * 1000)),
                    end_ms=max(0, round(float(raw_word.end) * 1000)),
                    probability=min(1.0, max(0.0, float(raw_word.probability))),
                )
                segment_words.append(word)
                all_words.append(word)

            if segment_words:
                confidence = sum(word.probability for word in segment_words) / len(segment_words)
            else:
                confidence = _probability_from_log(
                    float(raw_segment.avg_logprob) if raw_segment.avg_logprob is not None else None
                )
            segment_confidences.append(confidence)
            segments.append(
                TranscriptSegment(
                    text=segment_text,
                    start_ms=max(0, round(float(raw_segment.start) * 1000)),
                    end_ms=max(0, round(float(raw_segment.end) * 1000)),
                    words=segment_words,
                    confidence=confidence,
                )
            )

        transcript = "".join(text_parts).strip()
        if all_words:
            confidence = sum(word.probability for word in all_words) / len(all_words)
        elif segment_confidences:
            confidence = sum(segment_confidences) / len(segment_confidences)
        else:
            confidence = 0.0

        detected_language = str(getattr(info, "language", "") or request.language or "")
        language_probability = min(
            1.0,
            max(0.0, float(getattr(info, "language_probability", 0.0) or 0.0)),
        )
        warnings: list[str] = []
        if request.language and detected_language and request.language != detected_language:
            warnings.append("DETECTED_LANGUAGE_DIFFERS_FROM_REQUEST")

        return TranscribeResponse(
            request_id=request.request_id,
            segment_id=request.segment_id or "",
            attempt_id=request.attempt_id or "",
            attempt_number=request.attempt_number,
            transcript=transcript,
            normalized_transcript=normalize_transcript(transcript),
            detected_language=detected_language,
            language_probability=language_probability,
            duration_ms=decoded.duration_ms,
            words=all_words,
            segments=segments,
            confidence=confidence,
            processing_duration_ms=processing_duration_ms,
            warnings=warnings,
        )

    def _raise_transcription_error(self, exc: Exception, *, request_id: str) -> None:
        message = str(exc).lower()
        if "out of memory" in message or "cuda_error_out_of_memory" in message:
            self._ready = False
            self._last_error_code = ErrorCode.OUT_OF_MEMORY
            if self._settings.exit_on_oom:
                exit_timer = threading.Timer(0.25, lambda: os._exit(1))
                exit_timer.daemon = True
                exit_timer.start()
            raise WorkerError(
                code=ErrorCode.OUT_OF_MEMORY,
                public_message="STT worker ran out of GPU memory",
                status_code=503,
                retryable=False,
                request_id=request_id,
            ) from exc
        raise WorkerError(
            code=ErrorCode.TRANSCRIPTION_ERROR,
            public_message="STT inference failed",
            status_code=500,
            retryable=True,
            request_id=request_id,
        ) from exc
