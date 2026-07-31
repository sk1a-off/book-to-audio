from __future__ import annotations

import hashlib
import io
import wave
from dataclasses import dataclass

import numpy as np

from .errors import ErrorCode, WorkerError
from .schemas import AudioEncoding, AudioSpec

WHISPER_SAMPLE_RATE = 16_000


@dataclass(frozen=True, slots=True)
class DecodedAudio:
    samples: np.ndarray
    source_sample_rate: int
    duration_ms: int
    sha256: str


def _pcm_to_wav(data: bytes, spec: AudioSpec) -> bytes:
    output = io.BytesIO()
    with wave.open(output, "wb") as wav_file:
        wav_file.setnchannels(spec.channels)
        wav_file.setsampwidth(spec.bits_per_sample // 8)
        wav_file.setframerate(spec.sample_rate_hz)
        wav_file.writeframes(data)
    return output.getvalue()


def _validate_wav_header(data: bytes, spec: AudioSpec, request_id: str) -> int:
    try:
        with wave.open(io.BytesIO(data), "rb") as wav_file:
            if wav_file.getcomptype() != "NONE":
                raise ValueError("compressed WAV is not supported")
            if wav_file.getnchannels() != spec.channels:
                raise ValueError("channel count does not match metadata")
            if wav_file.getsampwidth() * 8 != spec.bits_per_sample:
                raise ValueError("sample width does not match metadata")
            if wav_file.getframerate() != spec.sample_rate_hz:
                raise ValueError("sample rate does not match metadata")
            return wav_file.getnframes()
    except (EOFError, ValueError, wave.Error) as exc:
        raise WorkerError(
            code=ErrorCode.INVALID_AUDIO,
            public_message="WAV payload does not match its AudioSpec",
            status_code=422,
            request_id=request_id,
        ) from exc


def decode_audio(
    data: bytes,
    *,
    spec: AudioSpec,
    declared_sha256: str | None,
    max_seconds: float,
    min_rms: float,
    request_id: str,
) -> DecodedAudio:
    if spec.channels != 1 or spec.bits_per_sample != 16:
        raise WorkerError(
            code=ErrorCode.INVALID_AUDIO,
            public_message="Only mono 16-bit audio is supported",
            status_code=422,
            request_id=request_id,
        )
    if not data:
        raise WorkerError(
            code=ErrorCode.INVALID_AUDIO,
            public_message="Audio payload is empty",
            status_code=422,
            request_id=request_id,
        )

    payload_hash = hashlib.sha256(data).hexdigest()
    if declared_sha256 is not None and declared_sha256.lower() != payload_hash:
        raise WorkerError(
            code=ErrorCode.INVALID_AUDIO,
            public_message="Audio SHA-256 does not match",
            status_code=422,
            request_id=request_id,
        )

    if spec.encoding == AudioEncoding.PCM_S16LE:
        frame_bytes = spec.channels * (spec.bits_per_sample // 8)
        if len(data) % frame_bytes != 0:
            raise WorkerError(
                code=ErrorCode.INVALID_AUDIO,
                public_message="PCM byte count is not aligned to complete samples",
                status_code=422,
                request_id=request_id,
            )
        frames = len(data) // frame_bytes
        encoded_audio = _pcm_to_wav(data, spec)
    else:
        frames = _validate_wav_header(data, spec, request_id)
        encoded_audio = data

    duration_seconds = frames / spec.sample_rate_hz
    if duration_seconds > max_seconds:
        raise WorkerError(
            code=ErrorCode.INVALID_AUDIO,
            public_message="Audio duration exceeds the configured limit",
            status_code=413,
            request_id=request_id,
        )

    try:
        from faster_whisper.audio import decode_audio as decode_with_av

        samples = decode_with_av(io.BytesIO(encoded_audio), sampling_rate=WHISPER_SAMPLE_RATE)
    except Exception as exc:
        raise WorkerError(
            code=ErrorCode.INVALID_AUDIO,
            public_message="Audio payload could not be decoded",
            status_code=422,
            request_id=request_id,
        ) from exc

    prepared = np.nan_to_num(
        np.asarray(samples, dtype=np.float32),
        nan=0.0,
        posinf=0.0,
        neginf=0.0,
    )
    prepared = np.clip(prepared, -1.0, 1.0)
    if prepared.ndim != 1 or prepared.size == 0:
        raise WorkerError(
            code=ErrorCode.INVALID_AUDIO,
            public_message="Decoded audio is empty or not mono",
            status_code=422,
            request_id=request_id,
        )

    rms = float(np.sqrt(np.mean(np.square(prepared, dtype=np.float64))))
    if rms < min_rms:
        raise WorkerError(
            code=ErrorCode.INVALID_AUDIO,
            public_message="Audio contains no usable signal",
            status_code=422,
            request_id=request_id,
        )

    peak = float(np.max(np.abs(prepared)))
    if peak > 0:
        prepared = prepared / peak
    prepared = np.clip(prepared, -1.0, 1.0).astype(np.float32, copy=False)

    return DecodedAudio(
        samples=prepared,
        source_sample_rate=spec.sample_rate_hz,
        duration_ms=round(duration_seconds * 1000),
        sha256=payload_hash,
    )
