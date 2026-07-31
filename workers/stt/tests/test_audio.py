from __future__ import annotations

import io
import math
import wave

import numpy as np
import pytest

from stt_worker.audio import WHISPER_SAMPLE_RATE, decode_audio
from stt_worker.errors import ErrorCode, WorkerError
from stt_worker.schemas import AudioSpec


def _sine_pcm(sample_rate: int = 24_000, duration: float = 0.1) -> bytes:
    time = np.arange(round(sample_rate * duration), dtype=np.float32) / sample_rate
    samples = np.sin(2 * math.pi * 440 * time) * 0.25
    return np.rint(samples * 32767).astype("<i2").tobytes()


def test_pcm_is_validated_resampled_and_peak_normalized() -> None:
    data = _sine_pcm()
    decoded = decode_audio(
        data,
        spec=AudioSpec(),
        declared_sha256=None,
        max_seconds=1,
        min_rms=1e-5,
        request_id="request-1",
    )

    assert decoded.duration_ms == 100
    assert abs(decoded.samples.size - WHISPER_SAMPLE_RATE // 10) <= 1
    assert decoded.samples.dtype == np.float32
    assert np.max(np.abs(decoded.samples)) == pytest.approx(1.0)


def test_silence_is_rejected() -> None:
    with pytest.raises(WorkerError) as raised:
        decode_audio(
            b"\x00\x00" * 2400,
            spec=AudioSpec(),
            declared_sha256=None,
            max_seconds=1,
            min_rms=1e-5,
            request_id="request-1",
        )

    assert raised.value.code == ErrorCode.INVALID_AUDIO
    assert raised.value.public_message == "Audio contains no usable signal"


def test_wav_header_must_match_declared_spec() -> None:
    output = io.BytesIO()
    with wave.open(output, "wb") as wav_file:
        wav_file.setnchannels(1)
        wav_file.setsampwidth(2)
        wav_file.setframerate(16_000)
        wav_file.writeframes(_sine_pcm(sample_rate=16_000))

    with pytest.raises(WorkerError) as raised:
        decode_audio(
            output.getvalue(),
            spec=AudioSpec(encoding="WAV_PCM_S16LE", sample_rate_hz=24_000),
            declared_sha256=None,
            max_seconds=1,
            min_rms=1e-5,
            request_id="request-1",
        )

    assert raised.value.code == ErrorCode.INVALID_AUDIO
    assert raised.value.public_message == "WAV payload does not match its AudioSpec"
