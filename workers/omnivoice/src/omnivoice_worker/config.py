from __future__ import annotations

import os
import re
import socket
from dataclasses import dataclass


def _read_bool(name: str, default: bool) -> bool:
    raw = os.getenv(name)
    if raw is None:
        return default

    normalized = raw.strip().lower()
    if normalized in {"1", "true", "yes", "on"}:
        return True
    if normalized in {"0", "false", "no", "off"}:
        return False
    raise ValueError(f"{name} must be a boolean")


def _read_int(name: str, default: int) -> int:
    raw = os.getenv(name)
    return default if raw is None else int(raw)


def _read_float(name: str, default: float) -> float:
    raw = os.getenv(name)
    return default if raw is None else float(raw)


@dataclass(frozen=True, slots=True)
class Settings:
    api_version: str = "v1"
    worker_id: str = socket.gethostname()
    model_id: str = "k2-fsa/OmniVoice"
    model_revision: str = "c5fdb5ccb189668d56333f77ba2629f4cd7535f4"
    local_files_only: bool = False
    device: str = "cuda:0"
    dtype: str = "float16"
    max_request_bytes: int = 8 * 1024 * 1024
    max_reference_bytes: int = 5 * 1024 * 1024
    max_reference_seconds: float = 15.0
    max_text_chars: int = 2_000
    max_text_words: int = 120
    max_output_seconds: float = 120.0
    min_output_rms: float = 1e-5
    clipping_warning_ratio: float = 0.01
    cache_max_entries: int = 4
    cache_max_reference_bytes: int = 16 * 1024 * 1024
    load_retry_seconds: float = 30.0
    background_load: bool = True
    warmup_enabled: bool = True
    warmup_steps: int = 2
    exit_on_oom: bool = True

    def __post_init__(self) -> None:
        if self.api_version != "v1":
            raise ValueError("only api_version=v1 is supported")
        if self.dtype not in {"float16", "float32", "bfloat16"}:
            raise ValueError("OMNIVOICE_DTYPE must be float16, float32, or bfloat16")
        if not self.device.startswith("cuda") and self.dtype == "float16":
            raise ValueError("float16 is only supported for CUDA in this worker")
        for field_name in (
            "max_request_bytes",
            "max_reference_bytes",
            "max_text_chars",
            "max_text_words",
            "cache_max_entries",
            "cache_max_reference_bytes",
            "warmup_steps",
        ):
            if getattr(self, field_name) <= 0:
                raise ValueError(f"{field_name} must be positive")
        if re.fullmatch(r"[0-9a-f]{40}", self.model_revision) is None:
            raise ValueError("OMNIVOICE_MODEL_REVISION must be a 40-character commit")
        if self.max_reference_seconds <= 0 or self.max_output_seconds <= 0:
            raise ValueError("duration settings are invalid")
        if not 0 <= self.min_output_rms <= 1:
            raise ValueError("OMNIVOICE_MIN_OUTPUT_RMS must be between 0 and 1")
        if not 0 <= self.clipping_warning_ratio <= 1:
            raise ValueError(
                "OMNIVOICE_CLIPPING_WARNING_RATIO must be between 0 and 1"
            )
        if self.load_retry_seconds < 0:
            raise ValueError("OMNIVOICE_LOAD_RETRY_SECONDS cannot be negative")

    @classmethod
    def from_env(cls) -> Settings:
        device = os.getenv("OMNIVOICE_DEVICE", "cuda:0")
        default_dtype = "float16" if device.startswith("cuda") else "float32"
        return cls(
            api_version=os.getenv("OMNIVOICE_API_VERSION", "v1"),
            worker_id=os.getenv("OMNIVOICE_WORKER_ID", socket.gethostname()),
            model_id=os.getenv("OMNIVOICE_MODEL_ID", "k2-fsa/OmniVoice"),
            model_revision=os.getenv(
                "OMNIVOICE_MODEL_REVISION",
                "c5fdb5ccb189668d56333f77ba2629f4cd7535f4",
            ),
            local_files_only=_read_bool("OMNIVOICE_LOCAL_FILES_ONLY", False),
            device=device,
            dtype=os.getenv("OMNIVOICE_DTYPE", default_dtype),
            max_request_bytes=_read_int(
                "OMNIVOICE_MAX_REQUEST_BYTES",
                8 * 1024 * 1024,
            ),
            max_reference_bytes=_read_int(
                "OMNIVOICE_MAX_REFERENCE_BYTES",
                5 * 1024 * 1024,
            ),
            max_reference_seconds=_read_float(
                "OMNIVOICE_MAX_REFERENCE_SECONDS",
                15.0,
            ),
            max_text_chars=_read_int("OMNIVOICE_MAX_TEXT_CHARS", 2_000),
            max_text_words=_read_int("OMNIVOICE_MAX_TEXT_WORDS", 120),
            max_output_seconds=_read_float("OMNIVOICE_MAX_OUTPUT_SECONDS", 120.0),
            min_output_rms=_read_float("OMNIVOICE_MIN_OUTPUT_RMS", 1e-5),
            clipping_warning_ratio=_read_float(
                "OMNIVOICE_CLIPPING_WARNING_RATIO",
                0.01,
            ),
            cache_max_entries=_read_int("OMNIVOICE_CACHE_MAX_ENTRIES", 4),
            cache_max_reference_bytes=_read_int(
                "OMNIVOICE_CACHE_MAX_REFERENCE_BYTES",
                16 * 1024 * 1024,
            ),
            load_retry_seconds=_read_float(
                "OMNIVOICE_LOAD_RETRY_SECONDS",
                30.0,
            ),
            background_load=_read_bool("OMNIVOICE_BACKGROUND_LOAD", True),
            warmup_enabled=_read_bool("OMNIVOICE_WARMUP_ENABLED", True),
            warmup_steps=_read_int("OMNIVOICE_WARMUP_STEPS", 2),
            exit_on_oom=_read_bool("OMNIVOICE_EXIT_ON_OOM", True),
        )
