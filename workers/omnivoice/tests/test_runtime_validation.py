from __future__ import annotations

import pytest

from omnivoice_worker.config import Settings
from omnivoice_worker.errors import ErrorCode, WorkerError
from omnivoice_worker.runtime import RealOmniVoiceRuntime
from omnivoice_worker.schemas import GenerateRequest


def _request(**overrides: object) -> GenerateRequest:
    values: dict[str, object] = {
        "api_version": "v1",
        "request_id": "request-1",
        "text": "Тест.",
        "mode": "AUTO_VOICE",
    }
    values.update(overrides)
    return GenerateRequest.model_validate(values)


@pytest.mark.parametrize(
    ("generate_request", "message"),
    [
        (
            _request(mode="VOICE_CLONE", voice_reference_text="text"),
            "VOICE_CLONE requires voice_reference",
        ),
        (
            _request(mode="VOICE_DESIGN"),
            "VOICE_DESIGN requires instruction",
        ),
        (
            _request(mode="AUTO_VOICE", instruction="style"),
            "AUTO_VOICE does not accept instruction",
        ),
    ],
)
def test_mode_contract_is_validated_before_model_load(
    generate_request: GenerateRequest,
    message: str,
) -> None:
    runtime = RealOmniVoiceRuntime(Settings(warmup_enabled=False))

    with pytest.raises(WorkerError) as raised:
        runtime.generate(generate_request, None)

    assert raised.value.code == ErrorCode.INVALID_REQUEST
    assert raised.value.public_message == message


class _FakeCUDA:
    @staticmethod
    def is_available() -> bool:
        return False


class _FakeTorch:
    cuda = _FakeCUDA()

    class OutOfMemoryError(Exception):
        pass

    @staticmethod
    def manual_seed(_: int) -> None:
        return None


class _FakeGenerationConfig:
    def __init__(self, **values: object) -> None:
        self.values = values


class _FakeModel:
    sampling_rate = 24_000

    def __init__(self) -> None:
        self.last_kwargs: dict[str, object] | None = None

    def generate(self, **kwargs: object) -> list[object]:
        import numpy as np

        self.last_kwargs = kwargs
        return [np.sin(np.linspace(0, 20, 2_400, dtype=np.float32)) * 0.1]


def test_request_scoped_generation_settings_are_forwarded_to_model() -> None:
    runtime = RealOmniVoiceRuntime(
        Settings(device="cpu", dtype="float32", warmup_enabled=False)
    )
    model = _FakeModel()
    runtime._model = model
    runtime._torch = _FakeTorch()
    runtime._generation_config_type = _FakeGenerationConfig
    runtime._model_version = "test"
    runtime._supported_languages = ["ru"]
    runtime._ready = True

    result = runtime.generate(
        _request(
            normalize_text=False,
            denoise=False,
            t_shift=0.2,
            layer_penalty_factor=4.0,
            position_temperature=3.0,
            class_temperature=0.4,
            preprocess_prompt=False,
            postprocess_output=False,
            audio_chunk_duration=12.0,
            audio_chunk_threshold=24.0,
            pad_duration=0.0,
            fade_duration=0.05,
            seed=42,
        ),
        None,
    )

    assert result.seed_used == 42
    assert model.last_kwargs is not None
    assert model.last_kwargs["normalize_text"] is False
    config = model.last_kwargs["generation_config"]
    assert isinstance(config, _FakeGenerationConfig)
    assert config.values == {
        "num_step": 32,
        "guidance_scale": 2.0,
        "denoise": False,
        "t_shift": 0.2,
        "layer_penalty_factor": 4.0,
        "position_temperature": 3.0,
        "class_temperature": 0.4,
        "preprocess_prompt": False,
        "postprocess_output": False,
        "audio_chunk_duration": 12.0,
        "audio_chunk_threshold": 24.0,
        "pad_duration": 0.0,
        "fade_duration": 0.05,
    }
