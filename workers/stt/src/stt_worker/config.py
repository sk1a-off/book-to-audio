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
    model_id: str = "small"
    model_revision: str = "536b0662742c02347bc0e980a01041f333bce120"
    device: str = "cuda"
    compute_type: str = "float16"
    cpu_threads: int = 0
    download_root: str = "/models/faster-whisper"
    local_files_only: bool = False
    max_request_bytes: int = 36 * 1024 * 1024
    max_audio_bytes: int = 32 * 1024 * 1024
    max_audio_seconds: float = 600.0
    max_expected_text_chars: int = 4_000
    min_audio_rms: float = 1e-5
    load_retry_seconds: float = 30.0
    background_load: bool = True
    warmup_enabled: bool = True
    exit_on_oom: bool = True

    def __post_init__(self) -> None:
        if self.api_version != "v1":
            raise ValueError("only api_version=v1 is supported")
        if re.fullmatch(r"[0-9a-f]{40}", self.model_revision) is None:
            raise ValueError("STT_MODEL_REVISION must be a 40-character commit")
        if self.device not in {"cuda", "cpu", "auto"}:
            raise ValueError("STT_DEVICE must be cuda, cpu, or auto")
        if self.cpu_threads < 0:
            raise ValueError("STT_CPU_THREADS cannot be negative")
        for field_name in (
            "max_request_bytes",
            "max_audio_bytes",
            "max_expected_text_chars",
        ):
            if getattr(self, field_name) <= 0:
                raise ValueError(f"{field_name} must be positive")
        if self.max_audio_seconds <= 0 or self.min_audio_rms < 0:
            raise ValueError("audio limits are invalid")
        if self.load_retry_seconds < 0:
            raise ValueError("STT_LOAD_RETRY_SECONDS cannot be negative")

    @classmethod
    def from_env(cls) -> Settings:
        device = os.getenv("STT_DEVICE", "cuda")
        default_compute_type = "float16" if device in {"cuda", "auto"} else "int8"
        return cls(
            api_version=os.getenv("STT_API_VERSION", "v1"),
            worker_id=os.getenv("STT_WORKER_ID", socket.gethostname()),
            model_id=os.getenv("STT_MODEL_ID", "small"),
            model_revision=os.getenv(
                "STT_MODEL_REVISION",
                "536b0662742c02347bc0e980a01041f333bce120",
            ),
            device=device,
            compute_type=os.getenv("STT_COMPUTE_TYPE", default_compute_type),
            cpu_threads=_read_int("STT_CPU_THREADS", 0),
            download_root=os.getenv(
                "STT_DOWNLOAD_ROOT",
                "/models/faster-whisper",
            ),
            local_files_only=_read_bool("STT_LOCAL_FILES_ONLY", False),
            max_request_bytes=_read_int(
                "STT_MAX_REQUEST_BYTES",
                36 * 1024 * 1024,
            ),
            max_audio_bytes=_read_int("STT_MAX_AUDIO_BYTES", 32 * 1024 * 1024),
            max_audio_seconds=_read_float("STT_MAX_AUDIO_SECONDS", 600.0),
            max_expected_text_chars=_read_int(
                "STT_MAX_EXPECTED_TEXT_CHARS",
                4_000,
            ),
            min_audio_rms=_read_float("STT_MIN_AUDIO_RMS", 1e-5),
            load_retry_seconds=_read_float("STT_LOAD_RETRY_SECONDS", 30.0),
            background_load=_read_bool("STT_BACKGROUND_LOAD", True),
            warmup_enabled=_read_bool("STT_WARMUP_ENABLED", True),
            exit_on_oom=_read_bool("STT_EXIT_ON_OOM", True),
        )
