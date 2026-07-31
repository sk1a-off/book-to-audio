from __future__ import annotations

import io
import wave
from dataclasses import dataclass

import numpy as np
import soundfile as sf

from .errors import ErrorCode, WorkerError
from .schemas import AudioEncoding


@dataclass(frozen=True, slots=True)
class ReferenceAudioInfo:
    sample_rate: int
    channels: int
    duration_seconds: float
    format: str


@dataclass(frozen=True, slots=True)
class PreparedPCM:
    data: bytes
    warnings: tuple[str, ...]


def inspect_reference_audio(
    data: bytes,
    *,
    max_seconds: float,
    request_id: str,
) -> ReferenceAudioInfo:
    try:
        with sf.SoundFile(io.BytesIO(data)) as audio_file:
            frames = len(audio_file)
            sample_rate = int(audio_file.samplerate)
            channels = int(audio_file.channels)
            format_name = str(audio_file.format)
    except (RuntimeError, TypeError, ValueError) as exc:
        raise WorkerError(
            code=ErrorCode.VOICE_REFERENCE_ERROR,
            public_message="Voice reference is not a supported audio file",
            status_code=422,
            request_id=request_id,
        ) from exc

    duration = frames / sample_rate if sample_rate else 0.0
    if frames <= 0 or sample_rate <= 0:
        raise WorkerError(
            code=ErrorCode.VOICE_REFERENCE_ERROR,
            public_message="Voice reference is empty",
            status_code=422,
            request_id=request_id,
        )
    if format_name.upper() not in {"FLAC", "WAV"}:
        raise WorkerError(
            code=ErrorCode.VOICE_REFERENCE_ERROR,
            public_message="Voice reference must be FLAC or PCM WAV",
            status_code=415,
            request_id=request_id,
        )
    if channels != 1:
        raise WorkerError(
            code=ErrorCode.VOICE_REFERENCE_ERROR,
            public_message="Voice reference must be mono",
            status_code=422,
            request_id=request_id,
        )
    if duration > max_seconds:
        raise WorkerError(
            code=ErrorCode.VOICE_REFERENCE_ERROR,
            public_message="Voice reference duration exceeds the configured limit",
            status_code=413,
            request_id=request_id,
        )

    return ReferenceAudioInfo(
        sample_rate=sample_rate,
        channels=channels,
        duration_seconds=duration,
        format=format_name,
    )


def float_audio_to_pcm_s16le(
    audio: np.ndarray,
    *,
    max_samples: int,
    min_rms: float,
    clipping_warning_ratio: float,
) -> PreparedPCM:
    mono = np.asarray(audio, dtype=np.float32)
    if mono.ndim > 1:
        mono = mono.mean(axis=0)
    if mono.ndim != 1 or mono.size == 0:
        raise ValueError("model returned empty or non-mono audio")
    if not np.isfinite(mono).all():
        raise ValueError("model returned non-finite audio")
    if mono.size > max_samples:
        raise ValueError("model returned audio longer than configured limit")

    rms = float(np.sqrt(np.mean(np.square(mono, dtype=np.float64))))
    if rms < min_rms:
        raise ValueError("model returned silent audio")

    clipping_ratio = float(np.mean(np.abs(mono) >= 1.0))
    warnings: list[str] = []
    if clipping_ratio > clipping_warning_ratio:
        warnings.append("OUTPUT_CLIPPING_RATIO_HIGH")

    clipped = np.clip(mono, -1.0, 1.0)
    pcm = np.rint(clipped * 32767.0).astype("<i2", copy=False).tobytes()
    return PreparedPCM(data=pcm, warnings=tuple(warnings))


def encode_audio(
    pcm_s16le: bytes,
    *,
    sample_rate: int,
    encoding: AudioEncoding,
) -> tuple[bytes, str, str]:
    if encoding == AudioEncoding.PCM_S16LE:
        return pcm_s16le, "application/octet-stream", "segment.pcm"

    output = io.BytesIO()
    with wave.open(output, "wb") as wav_file:
        wav_file.setnchannels(1)
        wav_file.setsampwidth(2)
        wav_file.setframerate(sample_rate)
        wav_file.writeframes(pcm_s16le)
    return output.getvalue(), "audio/wav", "segment.wav"
