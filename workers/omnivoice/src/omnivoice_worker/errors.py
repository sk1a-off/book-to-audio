from __future__ import annotations

from dataclasses import dataclass, field
from enum import StrEnum


class ErrorCode(StrEnum):
    INVALID_REQUEST = "INVALID_REQUEST"
    MODEL_NOT_READY = "MODEL_NOT_READY"
    VOICE_REFERENCE_ERROR = "VOICE_REFERENCE_ERROR"
    RESOURCE_EXHAUSTED = "RESOURCE_EXHAUSTED"
    OUT_OF_MEMORY = "OUT_OF_MEMORY"
    INFERENCE_ERROR = "INFERENCE_ERROR"
    TIMEOUT = "TIMEOUT"
    CANCELLED = "CANCELLED"
    INTERNAL_ERROR = "INTERNAL_ERROR"


@dataclass(slots=True)
class WorkerError(Exception):
    code: ErrorCode
    public_message: str
    status_code: int
    retryable: bool = False
    details: dict[str, str] = field(default_factory=dict)
    request_id: str | None = None

    def __str__(self) -> str:
        return self.public_message


def invalid_request(message: str, *, request_id: str | None = None) -> WorkerError:
    return WorkerError(
        code=ErrorCode.INVALID_REQUEST,
        public_message=message,
        status_code=422,
        request_id=request_id,
    )
