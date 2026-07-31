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


class VoiceMode(StrEnum):
    VOICE_CLONE = "VOICE_CLONE"
    VOICE_DESIGN = "VOICE_DESIGN"
    AUTO_VOICE = "AUTO_VOICE"


class AudioEncoding(StrEnum):
    PCM_S16LE = "PCM_S16LE"
    WAV_PCM_S16LE = "WAV_PCM_S16LE"


class GenerateRequest(BaseModel):
    model_config = ConfigDict(extra="forbid", str_strip_whitespace=True)

    api_version: Literal["v1"]
    request_id: Identifier
    job_id: Identifier | None = None
    segment_id: Identifier | None = None
    attempt_id: Identifier | None = None
    attempt_number: int = Field(default=0, ge=0, le=1_000_000)
    text: str = Field(min_length=1)
    language: Language = "ru"
    mode: VoiceMode = VoiceMode.AUTO_VOICE
    voice_reference_text: str | None = Field(default=None, max_length=4_000)
    voice_reference_sha256: str | None = Field(
        default=None,
        pattern=r"^[a-fA-F0-9]{64}$",
    )
    instruction: str | None = Field(default=None, max_length=1_000)
    speed: float = Field(default=1.0, ge=0.5, le=2.0)
    guidance_scale: float = Field(default=2.0, ge=0.0, le=10.0)
    num_steps: int = Field(default=32, ge=1, le=100)
    normalize_text: bool = True
    denoise: bool = True
    t_shift: float = Field(default=0.1, ge=0.001, le=10.0)
    layer_penalty_factor: float = Field(default=5.0, ge=0.0, le=20.0)
    position_temperature: float = Field(default=5.0, ge=0.0, le=20.0)
    class_temperature: float = Field(default=0.0, ge=0.0, le=10.0)
    preprocess_prompt: bool = True
    postprocess_output: bool = True
    audio_chunk_duration: float = Field(default=15.0, ge=1.0, le=120.0)
    audio_chunk_threshold: float = Field(default=30.0, ge=1.0, le=600.0)
    pad_duration: float = Field(default=0.1, ge=0.0, le=5.0)
    fade_duration: float = Field(default=0.1, ge=0.0, le=5.0)
    seed: int | None = Field(default=None, ge=0, le=2**32 - 1)
    output_encoding: AudioEncoding = AudioEncoding.PCM_S16LE
    deadline_hint_ms: int | None = Field(default=None, ge=1, le=3_600_000)


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
    service_type: Literal["omnivoice"]
    model_id: str
    model_version: str
    device_type: str
    device_name: str
    total_vram_bytes: int
    available_vram_bytes: int
    max_concurrency: int
    supported_sample_rates: list[int]
    supported_languages: list[str]
    supported_modes: list[VoiceMode]
    ready: bool
    warnings: list[str]
