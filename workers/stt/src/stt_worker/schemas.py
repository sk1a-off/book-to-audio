from __future__ import annotations

from enum import StrEnum
from typing import Annotated, Literal

from pydantic import BaseModel, ConfigDict, Field, StringConstraints

from .errors import ErrorCode

Identifier = Annotated[
    str,
    StringConstraints(
        strip_whitespace=True,
        min_length=1,
        max_length=128,
        pattern=r"^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$",
    ),
]
Language = Annotated[
    str,
    StringConstraints(
        strip_whitespace=True,
        min_length=2,
        max_length=16,
        pattern=r"^[A-Za-z][A-Za-z0-9-]*$",
    ),
]


class AudioEncoding(StrEnum):
    PCM_S16LE = "PCM_S16LE"
    WAV_PCM_S16LE = "WAV_PCM_S16LE"


class AudioSpec(BaseModel):
    model_config = ConfigDict(extra="forbid")

    encoding: AudioEncoding = AudioEncoding.PCM_S16LE
    sample_rate_hz: int = Field(default=24_000, ge=8_000, le=96_000)
    channels: int = Field(default=1, ge=1, le=2)
    bits_per_sample: int = Field(default=16)


class TranscribeRequest(BaseModel):
    model_config = ConfigDict(extra="forbid", str_strip_whitespace=True)

    api_version: Literal["v1"]
    request_id: Identifier
    job_id: Identifier | None = None
    segment_id: Identifier | None = None
    attempt_id: Identifier | None = None
    attempt_number: int = Field(default=0, ge=0, le=1_000_000)
    expected_text: str | None = Field(default=None)
    language: Language | None = None
    audio: AudioSpec = Field(default_factory=AudioSpec)
    audio_sha256: str | None = Field(
        default=None,
        pattern=r"^[a-fA-F0-9]{64}$",
    )
    beam_size: int = Field(default=5, ge=1, le=10)
    patience: float = Field(default=1.0, ge=0.1, le=2.0)
    temperature: float = Field(default=0.0, ge=0.0, le=1.0)
    vad_filter: bool = False
    word_timestamps_required: bool = True
    deadline_hint_ms: int | None = Field(default=None, ge=1, le=3_600_000)


class Word(BaseModel):
    text: str
    normalized_text: str
    start_ms: int = Field(ge=0)
    end_ms: int = Field(ge=0)
    probability: float = Field(ge=0.0, le=1.0)


class TranscriptSegment(BaseModel):
    text: str
    start_ms: int = Field(ge=0)
    end_ms: int = Field(ge=0)
    words: list[Word]
    confidence: float = Field(ge=0.0, le=1.0)


class TranscribeResponse(BaseModel):
    request_id: str
    segment_id: str
    attempt_id: str
    attempt_number: int
    transcript: str
    normalized_transcript: str
    detected_language: str
    language_probability: float = Field(ge=0.0, le=1.0)
    duration_ms: int = Field(ge=0)
    words: list[Word]
    segments: list[TranscriptSegment]
    confidence: float = Field(ge=0.0, le=1.0)
    processing_duration_ms: int = Field(ge=0)
    warnings: list[str]


class ErrorBody(BaseModel):
    code: ErrorCode
    message: str
    retryable: bool
    request_id: str
    details: dict[str, str] = Field(default_factory=dict)


class ErrorEnvelope(BaseModel):
    error: ErrorBody


class HealthResponse(BaseModel):
    status: Literal["alive", "ready", "not_ready"]
    ready: bool


class WorkerCapabilities(BaseModel):
    api_version: str
    worker_id: str
    service_type: Literal["stt"]
    model_id: str
    model_version: str
    device_type: str
    device_name: str
    total_vram_bytes: int
    available_vram_bytes: int
    max_concurrency: int
    supported_sample_rates: list[int]
    supported_languages: list[str]
    supported_modes: list[str]
    ready: bool
    warnings: list[str]
