from __future__ import annotations

import os
import re
import socket
from dataclasses import dataclass, field
from pathlib import Path

MODEL_ID_PATTERN = re.compile(r"^[a-z0-9][a-z0-9._-]{0,63}$")
REPOSITORY_PATTERN = re.compile(r"^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$")
COMMIT_PATTERN = re.compile(r"^[0-9a-f]{40}$")
SHA256_PATTERN = re.compile(r"^[0-9a-f]{64}$")
MIN_REQUEST_MAX_TOKENS = 16
MAX_REQUEST_MAX_TOKENS = 2_048

QWEN3_4B_REVISION = "bc640142c66e1fdd12af0bd68f40445458f3869b"
QWEN3_4B_SHA256 = "7485fe6f11af29433bc51cab58009521f205840f5b4ae3a32fa7f92e8534fdf5"
QWEN3_17B_REVISION = "90862c4b9d2787eaed51d12237eafdfe7c5f6077"
QWEN3_17B_SHA256 = "061b54daade076b5d3362dac252678d17da8c68f07560be70818cace6590cb1a"
DEFAULT_USER_PROMPT = (
    "Сделай только минимальную подготовку исходной фразы к русской озвучке: "
    "ничего не добавляй, не удаляй и не перефразируй; сохрани имена, смысловые "
    "единицы, буквенно-цифровое и символьное содержимое, их порядок и уже "
    "имеющиеся inline-теги буквально. Разрешены только пунктуация и пробелы, "
    "регистр, замена е↔ё и, при однозначном ударении, best-effort Unicode U+0301 "
    "в русском слове. Не раскрывай и не меняй цифры, даты, единицы измерения и "
    "аббревиатуры: за них отвечает OmniVoice normalize_text. В reason кратко "
    "перечисли фактически выполненные преобразования."
)


def _read_int(name: str, default: int) -> int:
    raw = os.getenv(name)
    return default if raw is None else int(raw)


def _read_float(name: str, default: float) -> float:
    raw = os.getenv(name)
    return default if raw is None else float(raw)


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


def _optional_env(name: str) -> str | None:
    value = os.getenv(name)
    if value is None:
        return None
    value = value.strip()
    return value or None


@dataclass(frozen=True, slots=True)
class ModelSpec:
    model_id: str
    display_name: str
    repo_id: str
    revision: str
    filename: str
    sha256: str
    size_bytes: int
    quantization: str
    recommended: bool = False

    def __post_init__(self) -> None:
        if MODEL_ID_PATTERN.fullmatch(self.model_id) is None:
            raise ValueError("model_id is not a safe allowlist identifier")
        if not self.display_name.strip():
            raise ValueError("model display name is required")
        if REPOSITORY_PATTERN.fullmatch(self.repo_id) is None:
            raise ValueError("repo_id must contain one safe namespace and name")
        if COMMIT_PATTERN.fullmatch(self.revision) is None:
            raise ValueError("model revision must be a 40-character lowercase commit")
        if (
            not self.filename.endswith(".gguf")
            or Path(self.filename).name != self.filename
        ):
            raise ValueError("model filename must be one GGUF basename")
        if SHA256_PATTERN.fullmatch(self.sha256) is None:
            raise ValueError("model SHA256 must contain 64 lowercase hex characters")
        if self.size_bytes <= 0:
            raise ValueError("model size must be positive")
        if not self.quantization:
            raise ValueError("model quantization is required")

    def path_under(self, models_dir: Path) -> Path:
        root = models_dir.expanduser().resolve(strict=False)
        candidate = (
            root / self.model_id / self.revision / self.filename
        ).resolve(strict=False)
        if not candidate.is_relative_to(root):
            raise ValueError("resolved model path escapes the configured model root")
        return candidate


def official_model_specs() -> tuple[ModelSpec, ...]:
    return (
        ModelSpec(
            model_id="qwen3-4b-q4-k-m",
            display_name="Qwen3 4B · Q4_K_M (рекомендуется)",
            repo_id="Qwen/Qwen3-4B-GGUF",
            revision=os.getenv(
                "REWRITER_QWEN3_4B_REVISION",
                QWEN3_4B_REVISION,
            ),
            filename="Qwen3-4B-Q4_K_M.gguf",
            sha256=os.getenv(
                "REWRITER_QWEN3_4B_SHA256",
                QWEN3_4B_SHA256,
            ),
            size_bytes=_read_int(
                "REWRITER_QWEN3_4B_SIZE_BYTES",
                2_497_280_256,
            ),
            quantization="Q4_K_M",
            recommended=True,
        ),
        ModelSpec(
            model_id="qwen3-1.7b-q8-0",
            display_name="Qwen3 1.7B · Q8_0 (быстрый)",
            repo_id="Qwen/Qwen3-1.7B-GGUF",
            revision=os.getenv(
                "REWRITER_QWEN3_17B_REVISION",
                QWEN3_17B_REVISION,
            ),
            filename="Qwen3-1.7B-Q8_0.gguf",
            sha256=os.getenv(
                "REWRITER_QWEN3_17B_SHA256",
                QWEN3_17B_SHA256,
            ),
            size_bytes=_read_int(
                "REWRITER_QWEN3_17B_SIZE_BYTES",
                1_834_426_016,
            ),
            quantization="Q8_0",
        ),
    )


@dataclass(frozen=True, slots=True)
class Settings:
    api_version: str = "v1"
    worker_id: str = field(default_factory=socket.gethostname)
    models_dir: Path = Path("/models/rewriter")
    models: tuple[ModelSpec, ...] = field(default_factory=official_model_specs)
    default_model_id: str = "qwen3-4b-q4-k-m"
    default_prompt: str = DEFAULT_USER_PROMPT
    auto_download: bool = True
    preload_model_id: str | None = None
    n_ctx: int = 8_192
    n_batch: int = 128
    n_threads: int = max(1, os.cpu_count() or 1)
    max_tokens: int = 1_024
    temperature: float = 0.1
    top_k: int = 20
    top_p: float = 0.8
    min_p: float = 0.0
    repeat_penalty: float = 1.05
    max_request_bytes: int = 32 * 1024
    max_text_chars: int = 8_000
    max_stt_text_chars: int = 8_000
    max_prompt_chars: int = 2_000
    max_output_chars: int = 12_000
    max_reason_chars: int = 1_000
    idle_unload_seconds: float = 300.0

    def __post_init__(self) -> None:
        if self.api_version != "v1":
            raise ValueError("only REWRITER_API_VERSION=v1 is supported")
        if not self.worker_id.strip():
            raise ValueError("worker_id cannot be empty")
        if not self.models:
            raise ValueError("at least one allowlisted model is required")
        identifiers = [model.model_id for model in self.models]
        if len(identifiers) != len(set(identifiers)):
            raise ValueError("allowlisted model IDs must be unique")
        if self.default_model_id not in identifiers:
            raise ValueError("default model is not allowlisted")
        if len(self.default_prompt) > self.max_prompt_chars:
            raise ValueError("default prompt exceeds max_prompt_chars")
        if (
            self.preload_model_id is not None
            and self.preload_model_id not in identifiers
        ):
            raise ValueError("preload model is not allowlisted")
        if not 1_024 <= self.n_ctx <= 131_072:
            raise ValueError("REWRITER_N_CTX must be between 1024 and 131072")
        if not 1 <= self.n_batch <= self.n_ctx:
            raise ValueError("REWRITER_N_BATCH must be between 1 and n_ctx")
        if self.n_threads <= 0:
            raise ValueError("REWRITER_N_THREADS must be positive")
        if not MIN_REQUEST_MAX_TOKENS <= self.max_tokens <= MAX_REQUEST_MAX_TOKENS:
            raise ValueError("REWRITER_MAX_TOKENS must be between 16 and 2048")
        if not 0.0 <= self.temperature <= 1.0:
            raise ValueError("REWRITER_TEMPERATURE must be between 0 and 1")
        if not 0 <= self.top_k <= 200:
            raise ValueError("REWRITER_TOP_K must be between 0 and 200")
        if not 0.0 <= self.top_p <= 1.0:
            raise ValueError("REWRITER_TOP_P must be between 0 and 1")
        if not 0.0 <= self.min_p <= 1.0:
            raise ValueError("REWRITER_MIN_P must be between 0 and 1")
        if not 0.5 <= self.repeat_penalty <= 2.0:
            raise ValueError(
                "REWRITER_REPEAT_PENALTY must be between 0.5 and 2"
            )
        if self.idle_unload_seconds < 0:
            raise ValueError("REWRITER_IDLE_UNLOAD_SECONDS cannot be negative")
        for field_name in (
            "max_request_bytes",
            "max_text_chars",
            "max_stt_text_chars",
            "max_prompt_chars",
            "max_output_chars",
            "max_reason_chars",
        ):
            if getattr(self, field_name) <= 0:
                raise ValueError(f"{field_name} must be positive")

    @property
    def model_by_id(self) -> dict[str, ModelSpec]:
        return {model.model_id: model for model in self.models}

    @classmethod
    def from_env(cls) -> Settings:
        return cls(
            api_version=os.getenv("REWRITER_API_VERSION", "v1"),
            worker_id=os.getenv("REWRITER_WORKER_ID", socket.gethostname()),
            models_dir=Path(
                os.getenv("REWRITER_MODELS_DIR", "/models/rewriter")
            ),
            models=official_model_specs(),
            default_model_id=os.getenv(
                "REWRITER_DEFAULT_MODEL_ID",
                "qwen3-4b-q4-k-m",
            ),
            default_prompt=os.getenv(
                "REWRITER_DEFAULT_PROMPT",
                DEFAULT_USER_PROMPT,
            ),
            auto_download=_read_bool("REWRITER_AUTO_DOWNLOAD", True),
            preload_model_id=_optional_env("REWRITER_PRELOAD_MODEL_ID"),
            n_ctx=_read_int("REWRITER_N_CTX", 8_192),
            n_batch=_read_int("REWRITER_N_BATCH", 128),
            n_threads=_read_int(
                "REWRITER_N_THREADS",
                max(1, os.cpu_count() or 1),
            ),
            max_tokens=_read_int("REWRITER_MAX_TOKENS", 1_024),
            temperature=_read_float("REWRITER_TEMPERATURE", 0.1),
            top_k=_read_int("REWRITER_TOP_K", 20),
            top_p=_read_float("REWRITER_TOP_P", 0.8),
            min_p=_read_float("REWRITER_MIN_P", 0.0),
            repeat_penalty=_read_float("REWRITER_REPEAT_PENALTY", 1.05),
            max_request_bytes=_read_int(
                "REWRITER_MAX_REQUEST_BYTES",
                32 * 1024,
            ),
            max_text_chars=_read_int("REWRITER_MAX_TEXT_CHARS", 8_000),
            max_stt_text_chars=_read_int(
                "REWRITER_MAX_STT_TEXT_CHARS",
                8_000,
            ),
            max_prompt_chars=_read_int(
                "REWRITER_MAX_PROMPT_CHARS",
                2_000,
            ),
            max_output_chars=_read_int(
                "REWRITER_MAX_OUTPUT_CHARS",
                12_000,
            ),
            max_reason_chars=_read_int(
                "REWRITER_MAX_REASON_CHARS",
                1_000,
            ),
            idle_unload_seconds=_read_float(
                "REWRITER_IDLE_UNLOAD_SECONDS",
                300.0,
            ),
        )
