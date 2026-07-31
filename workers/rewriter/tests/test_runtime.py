from __future__ import annotations

import hashlib
import json
import threading
import time
from pathlib import Path
from typing import Any

import pytest

from rewriter_worker.config import ModelSpec, Settings
from rewriter_worker.errors import ErrorCode, WorkerError
from rewriter_worker.runtime import LlamaCppRuntime
from rewriter_worker.schemas import RewriteRequest


def _write_model(
    root: Path,
    *,
    model_id: str,
    content: bytes,
    revision_digit: str = "1",
) -> ModelSpec:
    revision = revision_digit * 40
    spec = ModelSpec(
        model_id=model_id,
        display_name=model_id,
        repo_id=f"test/{model_id}",
        revision=revision,
        filename="model.gguf",
        sha256=hashlib.sha256(content).hexdigest(),
        size_bytes=len(content),
        quantization="TEST",
        recommended=model_id == "model-a",
    )
    path = spec.path_under(root)
    path.parent.mkdir(parents=True)
    path.write_bytes(content)
    return spec


def _missing_model(
    *,
    model_id: str,
    content: bytes,
    revision_digit: str = "1",
) -> ModelSpec:
    return ModelSpec(
        model_id=model_id,
        display_name=model_id,
        repo_id=f"test/{model_id}",
        revision=revision_digit * 40,
        filename="model.gguf",
        sha256=hashlib.sha256(content).hexdigest(),
        size_bytes=len(content),
        quantization="TEST",
        recommended=model_id == "model-a",
    )


def _settings(
    tmp_path: Path,
    models: tuple[ModelSpec, ...],
    **overrides: object,
) -> Settings:
    values: dict[str, object] = {
        "worker_id": "runtime-test",
        "models_dir": tmp_path,
        "models": models,
        "default_model_id": models[0].model_id,
        "default_prompt": "DEFAULT HINT",
        "n_ctx": 1_024,
        "n_batch": 32,
        "n_threads": 2,
        "max_tokens": 128,
        "temperature": 0.2,
        "idle_unload_seconds": 0,
    }
    values.update(overrides)
    return Settings(**values)


def _request(model_id: str, **overrides: object) -> RewriteRequest:
    values: dict[str, object] = {
        "api_version": "v1",
        "request_id": "request-1",
        "fragment_id": "fragment-1",
        "text": "Текст 42.",
        "stt_text": "",
        "warning_code": "",
        "model_id": model_id,
        "prompt": "",
    }
    values.update(overrides)
    return RewriteRequest.model_validate(values)


class FakeModel:
    def __init__(
        self,
        completion: dict[str, object] | None = None,
        *,
        wait: threading.Event | None = None,
        entered: threading.Event | None = None,
    ) -> None:
        self.completion = completion or {
            "choices": [
                {
                    "message": {
                        "content": json.dumps(
                            {
                                "rewritten_text": "Текст сорок два.",
                                "reason": "Число раскрыто словами.",
                            },
                            ensure_ascii=False,
                        )
                    }
                }
            ]
        }
        self.wait = wait
        self.entered = entered
        self.calls: list[dict[str, Any]] = []
        self.closed = 0

    def create_chat_completion(self, **kwargs: Any) -> dict[str, object]:
        self.calls.append(kwargs)
        if self.entered is not None:
            self.entered.set()
        if self.wait is not None:
            assert self.wait.wait(timeout=2)
        return self.completion

    def close(self) -> None:
        self.closed += 1


class FakeDownloader:
    def __init__(
        self,
        source: Path | None = None,
        *,
        error: Exception | None = None,
        wait: threading.Event | None = None,
        entered: threading.Event | None = None,
    ) -> None:
        self.source = source
        self.error = error
        self.wait = wait
        self.entered = entered
        self.specs: list[ModelSpec] = []

    def fetch(self, spec: ModelSpec) -> Path:
        self.specs.append(spec)
        if self.entered is not None:
            self.entered.set()
        if self.wait is not None:
            assert self.wait.wait(timeout=2)
        if self.error is not None:
            raise self.error
        assert self.source is not None
        return self.source


def test_missing_pinned_model_is_downloaded_once_and_atomically(
    tmp_path: Path,
) -> None:
    content = b"downloaded-model"
    spec = _missing_model(model_id="model-a", content=content)
    source = tmp_path / "hf-cache-source.gguf"
    source.write_bytes(content)
    downloader = FakeDownloader(source)
    model = FakeModel()
    runtime = LlamaCppRuntime(
        _settings(tmp_path, (spec,)),
        model_factory=lambda **_: model,
        downloader=downloader,
    )

    runtime.start()
    before = runtime.readiness()
    first = runtime.rewrite(_request(spec.model_id))
    runtime.unload()
    second = runtime.rewrite(_request(spec.model_id, request_id="request-2"))

    assert before.ready is True
    assert before.loaded_model_id is None
    assert before.available_models == 0
    assert first.model_revision == spec.revision
    assert second.model_revision == spec.revision
    assert downloader.specs == [spec]
    assert spec.path_under(tmp_path).read_bytes() == content
    assert list(spec.path_under(tmp_path).parent.glob("*.part")) == []


def test_bad_download_is_rejected_without_publishing_partial_file(
    tmp_path: Path,
) -> None:
    spec = _missing_model(model_id="model-a", content=b"good")
    source = tmp_path / "bad-cache-source.gguf"
    source.write_bytes(b"evil")
    runtime = LlamaCppRuntime(
        _settings(tmp_path, (spec,)),
        model_factory=lambda **_: FakeModel(),
        downloader=FakeDownloader(source),
    )
    runtime.start()

    with pytest.raises(WorkerError) as failure:
        runtime.rewrite(_request(spec.model_id))

    assert failure.value.code == ErrorCode.MODEL_INTEGRITY_ERROR
    assert spec.path_under(tmp_path).exists() is False
    assert list(spec.path_under(tmp_path).parent.glob("*.part")) == []
    assert runtime.readiness().ready is False


def test_download_transport_failure_has_explicit_retryable_error(
    tmp_path: Path,
) -> None:
    spec = _missing_model(model_id="model-a", content=b"good")
    runtime = LlamaCppRuntime(
        _settings(tmp_path, (spec,)),
        model_factory=lambda **_: FakeModel(),
        downloader=FakeDownloader(error=RuntimeError("network secret")),
    )
    runtime.start()

    with pytest.raises(WorkerError) as failure:
        runtime.rewrite(_request(spec.model_id))

    assert failure.value.code == ErrorCode.MODEL_DOWNLOAD_ERROR
    assert failure.value.retryable is True
    assert "network secret" not in str(failure.value)
    assert runtime.readiness().reason == "MODEL_DOWNLOAD_ERROR"


def test_download_and_inference_share_the_single_worker_slot(
    tmp_path: Path,
) -> None:
    content = b"downloaded-model"
    spec = _missing_model(model_id="model-a", content=content)
    source = tmp_path / "hf-cache-source.gguf"
    source.write_bytes(content)
    release = threading.Event()
    entered = threading.Event()
    runtime = LlamaCppRuntime(
        _settings(tmp_path, (spec,)),
        model_factory=lambda **_: FakeModel(),
        downloader=FakeDownloader(
            source,
            wait=release,
            entered=entered,
        ),
    )
    runtime.start()
    first_error: list[BaseException] = []

    def first_request() -> None:
        try:
            runtime.rewrite(_request(spec.model_id))
        except BaseException as exc:  # pragma: no cover - diagnostic capture
            first_error.append(exc)

    thread = threading.Thread(target=first_request)
    thread.start()
    assert entered.wait(timeout=2)

    with pytest.raises(WorkerError) as failure:
        runtime.rewrite(_request(spec.model_id, request_id="request-2"))
    assert failure.value.code == ErrorCode.RESOURCE_EXHAUSTED

    release.set()
    thread.join(timeout=2)
    assert thread.is_alive() is False
    assert first_error == []


def test_lazy_ready_loads_verified_cpu_model_with_constrained_json(
    tmp_path: Path,
) -> None:
    spec = _write_model(tmp_path, model_id="model-a", content=b"model-a")
    model = FakeModel()
    factory_calls: list[dict[str, Any]] = []

    def factory(**kwargs: Any) -> FakeModel:
        factory_calls.append(kwargs)
        return model

    runtime = LlamaCppRuntime(_settings(tmp_path, (spec,)), model_factory=factory)

    before = runtime.readiness()
    result = runtime.rewrite(
        _request(
            spec.model_id,
            temperature=0.45,
            top_k=11,
            top_p=0.7,
            min_p=0.04,
            repeat_penalty=1.1,
            max_tokens=222,
        )
    )
    after = runtime.readiness()

    assert before.ready is True
    assert before.loaded_model_id is None
    assert result.api_version == "v1"
    assert result.model_id == spec.model_id
    assert result.model_revision == spec.revision
    assert result.rewritten_text == "Текст сорок два."
    assert after.ready is True
    assert after.loaded_model_id == spec.model_id
    assert runtime.models()[0].integrity_verified is True
    assert factory_calls == [
        {
            "model_path": str(spec.path_under(tmp_path)),
            "n_gpu_layers": 0,
            "use_mmap": True,
            "use_mlock": False,
            "n_ctx": 1_024,
            "n_batch": 32,
            "n_threads": 2,
            "n_threads_batch": 2,
            "flash_attn": False,
            "offload_kqv": False,
            "verbose": False,
        }
    ]
    call = model.calls[0]
    assert call["temperature"] == 0.45
    assert call["top_k"] == 11
    assert call["top_p"] == 0.7
    assert call["min_p"] == 0.04
    assert call["repeat_penalty"] == 1.1
    assert call["max_tokens"] == 222
    assert call["response_format"]["type"] == "json_object"
    assert call["response_format"]["schema"]["additionalProperties"] is False
    assert "/no_think" in call["messages"][0]["content"]


def test_default_inference_values_and_prompt_are_applied(tmp_path: Path) -> None:
    spec = _write_model(tmp_path, model_id="model-a", content=b"model-a")
    model = FakeModel()
    runtime = LlamaCppRuntime(
        _settings(
            tmp_path,
            (spec,),
            temperature=0.3,
            top_k=15,
            top_p=0.75,
            min_p=0.03,
            repeat_penalty=1.08,
            max_tokens=321,
        ),
        model_factory=lambda **_: model,
    )

    runtime.rewrite(_request(spec.model_id))

    call = model.calls[0]
    assert call["temperature"] == 0.3
    assert call["top_k"] == 15
    assert call["top_p"] == 0.75
    assert call["min_p"] == 0.03
    assert call["repeat_penalty"] == 1.08
    assert call["max_tokens"] == 321
    assert "DEFAULT HINT" in call["messages"][1]["content"]


def test_model_switch_closes_previous_instance(tmp_path: Path) -> None:
    first = _write_model(tmp_path, model_id="model-a", content=b"model-a")
    second = _write_model(
        tmp_path,
        model_id="model-b",
        content=b"model-b",
        revision_digit="2",
    )
    instances = [FakeModel(), FakeModel()]

    def factory(**_: Any) -> FakeModel:
        return instances.pop(0)

    runtime = LlamaCppRuntime(
        _settings(tmp_path, (first, second)),
        model_factory=factory,
    )
    runtime.rewrite(_request(first.model_id))
    first_instance = runtime._model
    runtime.rewrite(_request(second.model_id))

    assert isinstance(first_instance, FakeModel)
    assert first_instance.closed == 1
    assert runtime.readiness().loaded_model_id == second.model_id


def test_checksum_failure_marks_not_ready_until_file_identity_changes(
    tmp_path: Path,
) -> None:
    good_content = b"good"
    spec = _write_model(tmp_path, model_id="model-a", content=good_content)
    path = spec.path_under(tmp_path)
    path.write_bytes(b"evil")
    runtime = LlamaCppRuntime(
        _settings(tmp_path, (spec,)),
        model_factory=lambda **_: FakeModel(),
    )

    assert runtime.readiness().ready is True
    with pytest.raises(WorkerError) as failure:
        runtime.rewrite(_request(spec.model_id))
    assert failure.value.code == ErrorCode.MODEL_INTEGRITY_ERROR
    assert runtime.readiness().ready is False
    assert runtime.models()[0].available is False

    time.sleep(0.002)
    path.write_bytes(good_content)

    assert runtime.readiness().ready is True
    assert runtime.readiness().loaded_model_id is None


def test_model_load_failure_marks_readiness_false(tmp_path: Path) -> None:
    spec = _write_model(tmp_path, model_id="model-a", content=b"model-a")

    def failing_factory(**_: Any) -> FakeModel:
        raise RuntimeError("private loader details")

    runtime = LlamaCppRuntime(
        _settings(tmp_path, (spec,)),
        model_factory=failing_factory,
    )

    with pytest.raises(WorkerError) as failure:
        runtime.rewrite(_request(spec.model_id))

    assert failure.value.code == ErrorCode.MODEL_NOT_READY
    readiness = runtime.readiness()
    assert readiness.ready is False
    assert readiness.reason == "MODEL_NOT_READY"


def test_non_allowlisted_model_is_rejected_before_any_load(tmp_path: Path) -> None:
    spec = _write_model(tmp_path, model_id="model-a", content=b"model-a")
    factory_called = False

    def factory(**_: Any) -> FakeModel:
        nonlocal factory_called
        factory_called = True
        return FakeModel()

    runtime = LlamaCppRuntime(
        _settings(tmp_path, (spec,)),
        model_factory=factory,
    )

    with pytest.raises(WorkerError) as failure:
        runtime.rewrite(_request("unknown"))

    assert failure.value.code == ErrorCode.MODEL_NOT_ALLOWED
    assert factory_called is False


def test_only_one_inference_can_run_at_a_time(tmp_path: Path) -> None:
    spec = _write_model(tmp_path, model_id="model-a", content=b"model-a")
    release = threading.Event()
    entered = threading.Event()
    runtime = LlamaCppRuntime(
        _settings(tmp_path, (spec,)),
        model_factory=lambda **_: FakeModel(wait=release, entered=entered),
    )
    first_error: list[BaseException] = []

    def first_request() -> None:
        try:
            runtime.rewrite(_request(spec.model_id))
        except BaseException as exc:  # pragma: no cover - diagnostic capture
            first_error.append(exc)

    thread = threading.Thread(target=first_request)
    thread.start()
    assert entered.wait(timeout=2)

    with pytest.raises(WorkerError) as failure:
        runtime.rewrite(
            _request(
                spec.model_id,
                request_id="request-2",
                fragment_id="fragment-2",
            )
        )
    assert failure.value.code == ErrorCode.RESOURCE_EXHAUSTED
    assert failure.value.status_code == 429

    release.set()
    thread.join(timeout=2)
    assert not thread.is_alive()
    assert first_error == []


def test_idle_timeout_unloads_but_keeps_worker_ready(tmp_path: Path) -> None:
    spec = _write_model(tmp_path, model_id="model-a", content=b"model-a")
    model = FakeModel()
    runtime = LlamaCppRuntime(
        _settings(tmp_path, (spec,), idle_unload_seconds=0.02),
        model_factory=lambda **_: model,
    )

    runtime.rewrite(_request(spec.model_id))
    deadline = time.monotonic() + 1
    while runtime.readiness().loaded_model_id is not None:
        assert time.monotonic() < deadline
        time.sleep(0.01)

    readiness = runtime.readiness()
    assert readiness.ready is True
    assert readiness.loaded_model_id is None
    assert model.closed == 1


def test_invalid_model_json_is_a_stable_gateway_error(tmp_path: Path) -> None:
    spec = _write_model(tmp_path, model_id="model-a", content=b"model-a")
    model = FakeModel(
        completion={"choices": [{"message": {"content": "not-json"}}]}
    )
    runtime = LlamaCppRuntime(
        _settings(tmp_path, (spec,)),
        model_factory=lambda **_: model,
    )

    with pytest.raises(WorkerError) as failure:
        runtime.rewrite(_request(spec.model_id))

    assert failure.value.code == ErrorCode.OUTPUT_INVALID
    assert failure.value.status_code == 502


@pytest.mark.parametrize(
    ("rewritten_text", "reason", "expected_message"),
    [
        ("text", "ok", "Rewritten text exceeds the configured limit"),
        ("ok", "reason", "Rewrite reason exceeds the configured limit"),
    ],
)
def test_configured_output_lengths_are_enforced_after_inference(
    tmp_path: Path,
    rewritten_text: str,
    reason: str,
    expected_message: str,
) -> None:
    spec = _write_model(tmp_path, model_id="model-a", content=b"model-a")
    model = FakeModel(
        completion={
            "choices": [
                {
                    "message": {
                        "content": json.dumps(
                            {
                                "rewritten_text": rewritten_text,
                                "reason": reason,
                            }
                        )
                    }
                }
            ]
        }
    )
    runtime = LlamaCppRuntime(
        _settings(
            tmp_path,
            (spec,),
            max_output_chars=3,
            max_reason_chars=3,
        ),
        model_factory=lambda **_: model,
    )

    with pytest.raises(WorkerError) as failure:
        runtime.rewrite(_request(spec.model_id))

    assert failure.value.code == ErrorCode.OUTPUT_INVALID
    assert failure.value.status_code == 502
    assert failure.value.retryable is True
    assert failure.value.public_message == expected_message
