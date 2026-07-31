from __future__ import annotations

from typing import Annotated, Literal

from pydantic import BaseModel, ConfigDict, Field, StringConstraints

from .config import MAX_REQUEST_MAX_TOKENS, MIN_REQUEST_MAX_TOKENS
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
ModelIdentifier = Annotated[
    str,
    StringConstraints(
        strip_whitespace=True,
        min_length=1,
        max_length=64,
        pattern=r"^[a-z0-9][a-z0-9._-]{0,63}$",
    ),
]


class RewriteRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    api_version: Literal["v1"]
    request_id: Identifier
    fragment_id: Identifier
    text: str = Field(min_length=1, max_length=32_000)
    stt_text: str = Field(max_length=32_000)
    warning_code: str = Field(default="", max_length=128)
    model_id: ModelIdentifier
    prompt: str = Field(default="", max_length=8_000)
    temperature: float | None = Field(default=None, ge=0.0, le=1.0)
    top_k: int | None = Field(default=None, ge=0, le=200)
    top_p: float | None = Field(default=None, ge=0.0, le=1.0)
    min_p: float | None = Field(default=None, ge=0.0, le=1.0)
    repeat_penalty: float | None = Field(default=None, ge=0.5, le=2.0)
    max_tokens: int | None = Field(
        default=None,
        ge=MIN_REQUEST_MAX_TOKENS,
        le=MAX_REQUEST_MAX_TOKENS,
    )


class RewriteResponse(BaseModel):
    model_config = ConfigDict(extra="forbid")

    api_version: Literal["v1"]
    request_id: str
    fragment_id: str
    rewritten_text: str
    reason: str
    model_id: str
    model_revision: str
    duration_ms: int = Field(ge=0)


class RewriteModelOutput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    rewritten_text: str = Field(min_length=1, max_length=32_000)
    reason: str = Field(min_length=1, max_length=4_000)


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
    loaded_model_id: str | None = None
    available_models: int = Field(default=0, ge=0)
    reason: str = ""


class ModelResource(BaseModel):
    model_id: str
    display_name: str
    repo_id: str
    revision: str
    filename: str
    sha256: str
    size_bytes: int = Field(gt=0)
    quantization: str
    recommended: bool
    available: bool
    loaded: bool
    integrity_verified: bool
    device: Literal["cpu"]


class FloatSettingRange(BaseModel):
    default: float
    minimum: float
    maximum: float


class IntegerSettingRange(BaseModel):
    default: int
    minimum: int
    maximum: int


class RewriteSettingsResource(BaseModel):
    temperature: FloatSettingRange
    top_k: IntegerSettingRange
    top_p: FloatSettingRange
    min_p: FloatSettingRange
    repeat_penalty: FloatSettingRange
    max_tokens: IntegerSettingRange


class RewriteLimitsResource(BaseModel):
    max_request_bytes: int = Field(gt=0)
    max_text_chars: int = Field(gt=0)
    max_stt_text_chars: int = Field(gt=0)
    max_prompt_chars: int = Field(gt=0)
    max_output_chars: int = Field(gt=0)
    max_reason_chars: int = Field(gt=0)


class ModelsResponse(BaseModel):
    api_version: Literal["v1"]
    worker_id: str
    default_model_id: str
    default_prompt: str
    loaded_model_id: str | None
    settings: RewriteSettingsResource
    limits: RewriteLimitsResource
    models: list[ModelResource]
