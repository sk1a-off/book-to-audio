from __future__ import annotations

import io

import numpy as np
import pytest
import soundfile as sf

from omnivoice_worker.audio import (
    float_audio_to_pcm_s16le,
    inspect_reference_audio,
)
from omnivoice_worker.errors import ErrorCode, WorkerError


def _encoded_audio(format_name: str, subtype: str | None = None) -> bytes:
    output = io.BytesIO()
    samples = np.sin(np.linspace(0, 20, 2400, dtype=np.float32)) * 0.25
    sf.write(
        output,
        samples,
        24_000,
        format=format_name,
        subtype=subtype,
    )
    return output.getvalue()


@pytest.mark.parametrize(
    ("format_name", "subtype"),
    [("FLAC", None), ("WAV", "PCM_16")],
)
def test_reference_allowlist_accepts_flac_and_pcm_wav(
    format_name: str,
    subtype: str | None,
) -> None:
    info = inspect_reference_audio(
        _encoded_audio(format_name, subtype),
        max_seconds=15,
        request_id="request-1",
    )

    assert info.format == format_name
    assert info.channels == 1
    assert info.sample_rate == 24_000


def test_reference_allowlist_rejects_an_actual_ogg_container() -> None:
    with pytest.raises(WorkerError) as raised:
        inspect_reference_audio(
            _encoded_audio("OGG", "VORBIS"),
            max_seconds=15,
            request_id="request-1",
        )

    assert raised.value.code == ErrorCode.VOICE_REFERENCE_ERROR
    assert raised.value.status_code == 415


def test_output_validation_rejects_silence_and_excessive_duration() -> None:
    with pytest.raises(ValueError, match="silent"):
        float_audio_to_pcm_s16le(
            np.zeros(100, dtype=np.float32),
            max_samples=100,
            min_rms=1e-5,
            clipping_warning_ratio=0.01,
        )

    with pytest.raises(ValueError, match="longer"):
        float_audio_to_pcm_s16le(
            np.ones(101, dtype=np.float32) * 0.1,
            max_samples=100,
            min_rms=1e-5,
            clipping_warning_ratio=0.01,
        )


def test_output_validation_reports_clipping() -> None:
    prepared = float_audio_to_pcm_s16le(
        np.array([0.2, 1.1, -1.2], dtype=np.float32),
        max_samples=10,
        min_rms=1e-5,
        clipping_warning_ratio=0.1,
    )

    assert prepared.data
    assert prepared.warnings == ("OUTPUT_CLIPPING_RATIO_HIGH",)
