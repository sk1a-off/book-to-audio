#!/usr/bin/env python3
"""Select one allowlisted stress variant per homograph occurrence."""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import unicodedata
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any


COMBINING_ACUTE = "\u0301"
RUSSIAN_LETTER = "А-Яа-яЁё"


def strip_accents(value: str) -> str:
    return unicodedata.normalize("NFC", value.replace(COMBINING_ACUTE, ""))


def occurrence_span(source: str, surface: str, ordinal: int) -> tuple[int, int]:
    pattern = re.compile(
        rf"(?<![{RUSSIAN_LETTER}]){re.escape(surface)}(?![{RUSSIAN_LETTER}])"
    )
    matches = list(pattern.finditer(source))
    if ordinal < 0 or ordinal >= len(matches):
        raise ValueError(
            f"cannot find occurrence {ordinal} of {surface!r} in canonical text"
        )
    return matches[ordinal].span()


def validate_spec_case(case: dict[str, Any]) -> None:
    source = case["source_text"]
    occupied: list[tuple[int, int]] = []
    occurrence_ids: set[str] = set()
    candidate_ids: set[str] = set()
    for occurrence in case["occurrences"]:
        occurrence_id = occurrence["id"]
        if occurrence_id in occurrence_ids:
            raise ValueError(f"duplicate occurrence ID {occurrence_id!r}")
        occurrence_ids.add(occurrence_id)
        span = occurrence_span(source, occurrence["surface"], occurrence["ordinal"])
        if any(span[0] < end and start < span[1] for start, end in occupied):
            raise ValueError(f"overlapping occurrence {occurrence_id!r}")
        occupied.append(span)
        if len(occurrence["candidates"]) < 2:
            raise ValueError(f"occurrence {occurrence_id!r} needs at least two variants")
        for candidate in occurrence["candidates"]:
            if candidate["id"] in candidate_ids:
                raise ValueError(f"duplicate candidate ID {candidate['id']!r}")
            candidate_ids.add(candidate["id"])
            if strip_accents(candidate["text"]) != occurrence["surface"]:
                raise ValueError(
                    f"candidate {candidate['id']!r} changes the surface word"
                )
            if candidate["text"].count(COMBINING_ACUTE) != 1:
                raise ValueError(
                    f"candidate {candidate['id']!r} must contain one U+0301"
                )
    for override in case.get("fixed_overrides", []):
        occurrence_span(source, override["surface"], override["ordinal"])
        if strip_accents(override["text"]) != override["surface"]:
            raise ValueError("fixed override changes the surface word")
    if strip_accents(case["control_tts_text"]) != source:
        raise ValueError("control TTS text changes canonical text")


def build_prompt(case: dict[str, Any]) -> str:
    choices = []
    for occurrence in case["occurrences"]:
        start, _ = occurrence_span(
            case["source_text"], occurrence["surface"], occurrence["ordinal"]
        )
        choices.append(
            {
                "occurrence_id": occurrence["id"],
                "surface": occurrence["surface"],
                "character_offset": start,
                "allowed": occurrence["candidates"],
            }
        )
    schema = response_schema(case)
    return (
        "Определи правильное нормативное ударение каждого указанного русского "
        "омографа строго по смыслу полного предложения. Для каждого occurrence_id "
        "выбери ровно один candidate id из его allowed. Не переписывай текст, не "
        "создавай свои варианты и не объясняй ответ. Верни только JSON, строго "
        "соответствующий указанной схеме.\n\n"
        f"Текст:\n{case['source_text']}\n\n"
        "Варианты:\n"
        + json.dumps(choices, ensure_ascii=False, separators=(",", ":"))
        + "\n\nJSON Schema:\n"
        + json.dumps(schema, ensure_ascii=False, separators=(",", ":"))
    )


def response_schema(case: dict[str, Any]) -> dict[str, Any]:
    properties = {}
    required = []
    for occurrence in case["occurrences"]:
        occurrence_id = occurrence["id"]
        properties[occurrence_id] = {
            "type": "string",
            "enum": [candidate["id"] for candidate in occurrence["candidates"]],
        }
        required.append(occurrence_id)
    return {
        "type": "object",
        "properties": {
            "selections": {
                "type": "object",
                "properties": properties,
                "required": required,
            }
        },
        "required": ["selections"],
    }


def provider_error(exc: urllib.error.HTTPError) -> RuntimeError:
    status = ""
    message = ""
    try:
        envelope = json.loads(exc.read())
        error = envelope.get("error", {})
        status = str(error.get("status", ""))
        message = str(error.get("message", ""))
    except (json.JSONDecodeError, AttributeError, TypeError):
        pass
    details = ": ".join(item for item in (status, message) if item)
    return RuntimeError(
        f"Gemini HTTP error {exc.code}" + (f": {details}" if details else "")
    )


def request_native_selections(
    *, api_key: str, model: str, temperature: float, case: dict[str, Any]
) -> dict[str, str]:
    endpoint = (
        "https://generativelanguage.googleapis.com/v1beta/models/"
        f"{model}:generateContent"
    )
    payload = {
        "contents": [{"role": "user", "parts": [{"text": build_prompt(case)}]}],
        "generationConfig": {
            "temperature": temperature,
            "responseMimeType": "application/json",
            "responseSchema": response_schema(case),
        },
    }
    request = urllib.request.Request(
        endpoint,
        data=json.dumps(payload, ensure_ascii=False).encode("utf-8"),
        headers={"Content-Type": "application/json", "x-goog-api-key": api_key},
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=90) as response:
            envelope = json.load(response)
    except urllib.error.HTTPError as exc:
        raise provider_error(exc) from exc
    except urllib.error.URLError as exc:
        raise RuntimeError(f"Gemini connection failed: {exc.reason}") from exc
    try:
        text = envelope["candidates"][0]["content"]["parts"][0]["text"]
    except (KeyError, IndexError, TypeError) as exc:
        raise RuntimeError("Gemini returned an invalid structured response") from exc
    return validate_selections(case, text)


def gateway_endpoint(base_url: str) -> str:
    parsed = urllib.parse.urlsplit(base_url.strip())
    if parsed.scheme not in {"http", "https"} or not parsed.netloc:
        raise ValueError("gateway base URL must be an absolute HTTP(S) URL")
    if parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise ValueError("gateway base URL cannot contain credentials, query, or fragment")
    path = parsed.path.rstrip("/")
    if path.endswith("/chat/completions"):
        endpoint_path = path
    elif path.endswith("/v1"):
        endpoint_path = path + "/chat/completions"
    else:
        endpoint_path = path + "/v1/chat/completions"
    return urllib.parse.urlunsplit(
        (parsed.scheme, parsed.netloc, endpoint_path, "", "")
    )


def request_gateway_selections(
    *,
    api_key: str,
    base_url: str,
    model: str,
    temperature: float,
    reasoning_effort: str,
    timeout: float,
    case: dict[str, Any],
) -> dict[str, str]:
    payload = {
        "model": model,
        "messages": [{"role": "user", "content": build_prompt(case)}],
        "temperature": temperature,
        "response_format": {"type": "json_object"},
        "stream": False,
    }
    if reasoning_effort:
        payload["reasoning_effort"] = reasoning_effort
    request = urllib.request.Request(
        gateway_endpoint(base_url),
        data=json.dumps(payload, ensure_ascii=False).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {api_key}",
            "Content-Type": "application/json",
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            envelope = json.load(response)
    except urllib.error.HTTPError as exc:
        raise provider_error(exc) from exc
    except urllib.error.URLError as exc:
        raise RuntimeError(f"Gemini gateway connection failed: {exc.reason}") from exc
    except TimeoutError as exc:
        raise RuntimeError(
            f"Gemini gateway request timed out after {timeout:g}s"
        ) from exc
    try:
        text = envelope["choices"][0]["message"]["content"]
        if not isinstance(text, str):
            raise TypeError("content is not text")
    except (KeyError, IndexError, TypeError) as exc:
        raise RuntimeError("Gemini gateway returned an invalid response") from exc
    return validate_selections(case, text)


def validate_selections(case: dict[str, Any], text: str) -> dict[str, str]:
    try:
        selections = json.loads(text)["selections"]
    except (KeyError, TypeError, json.JSONDecodeError) as exc:
        raise RuntimeError("Gemini returned an invalid structured response") from exc
    if not isinstance(selections, dict):
        raise RuntimeError("Gemini selections must be an object")
    expected_ids = {item["id"] for item in case["occurrences"]}
    if set(selections) != expected_ids:
        raise RuntimeError("Gemini returned missing or unknown occurrence IDs")
    for occurrence in case["occurrences"]:
        allowed = {candidate["id"] for candidate in occurrence["candidates"]}
        if selections[occurrence["id"]] not in allowed:
            raise RuntimeError("Gemini returned a candidate outside the allowlist")
    return selections


def apply_replacements(
    source: str, replacements: list[tuple[int, int, str]]
) -> str:
    result = source
    for start, end, replacement in sorted(replacements, reverse=True):
        result = result[:start] + replacement + result[end:]
    if strip_accents(result) != source:
        raise RuntimeError("generated TTS text violated source-text conservation")
    return unicodedata.normalize("NFC", result)


def render_tts_text(case: dict[str, Any], selections: dict[str, str]) -> str:
    source = case["source_text"]
    replacements: list[tuple[int, int, str]] = []
    for occurrence in case["occurrences"]:
        start, end = occurrence_span(
            source, occurrence["surface"], occurrence["ordinal"]
        )
        selected_id = selections[occurrence["id"]]
        selected = next(
            candidate["text"]
            for candidate in occurrence["candidates"]
            if candidate["id"] == selected_id
        )
        replacements.append((start, end, selected))
    for override in case.get("fixed_overrides", []):
        start, end = occurrence_span(source, override["surface"], override["ordinal"])
        replacements.append((start, end, override["text"]))
    return apply_replacements(source, replacements)


def read_key(path: Path | None, env_name: str) -> str:
    if path is not None and path.exists():
        mode = path.stat().st_mode & 0o777
        if mode & 0o077:
            raise RuntimeError(f"secret file permissions are too broad: {mode:o}")
        value = path.read_text(encoding="utf-8").strip()
    else:
        value = os.getenv(env_name, "").strip()
    if not value:
        raise RuntimeError(f"Gemini API key is missing from {env_name}")
    return value


def write_checkpoint(
    *,
    path: Path,
    experiment_id: str,
    model: str,
    transport: str,
    reasoning_effort: str,
    temperature: float,
    ordered_case_ids: list[str],
    results_by_case: dict[str, dict[str, Any]],
    complete: bool,
) -> None:
    output = {
        "schema_version": "tts-quality-homograph-variant-gemini-result-v1",
        "experiment_id": experiment_id,
        "model": model,
        "transport": transport,
        "reasoning_effort": reasoning_effort or None,
        "temperature": temperature,
        "complete": complete,
        "results": [
            results_by_case[case_id]
            for case_id in ordered_case_ids
            if case_id in results_by_case
        ],
    }
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(
        json.dumps(output, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    temporary.replace(path)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--spec", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--model")
    parser.add_argument("--experiment-id")
    parser.add_argument(
        "--reasoning-effort",
        choices=("", "minimal", "low", "medium", "high"),
        default="",
    )
    parser.add_argument(
        "--gateway-base-url",
        default=os.getenv("GEMINI_GATEWAY_BASE_URL", ""),
    )
    parser.add_argument("--key-file", type=Path)
    parser.add_argument("--request-timeout", type=float, default=180.0)
    parser.add_argument("--resume", action="store_true")
    args = parser.parse_args()

    if args.request_timeout <= 0:
        raise ValueError("request timeout must be positive")

    spec: dict[str, Any] = json.loads(args.spec.read_text(encoding="utf-8"))
    gateway_mode = bool(args.gateway_base_url.strip())
    key_env = "GEMINI_GATEWAY_API_KEY" if gateway_mode else "GEMINI_API_KEY"
    key_path = args.key_file
    if key_path is None and not gateway_mode:
        key_path = Path("/tmp/tts-gemini-api-key")
    api_key = read_key(
        key_path if key_path is not None and key_path.exists() else None,
        key_env,
    )
    model = args.model or spec["model"]
    experiment_id = args.experiment_id or spec["experiment_id"]
    transport = "openai-compatible-gateway" if gateway_mode else "google-native"
    ordered_case_ids = [case["id"] for case in spec["cases"]]
    results_by_case: dict[str, dict[str, Any]] = {}
    if args.resume and args.output.exists():
        previous = json.loads(args.output.read_text(encoding="utf-8"))
        identity = (
            previous.get("experiment_id"),
            previous.get("model"),
            previous.get("transport"),
            previous.get("reasoning_effort"),
        )
        expected_identity = (
            experiment_id,
            model,
            transport,
            args.reasoning_effort or None,
        )
        if identity != expected_identity:
            raise ValueError("checkpoint identity does not match this experiment")
        for result in previous.get("results", []):
            case_id = result.get("case_id")
            if case_id not in ordered_case_ids or case_id in results_by_case:
                raise ValueError("checkpoint contains an unknown or duplicate case")
            results_by_case[case_id] = result
    for case in spec["cases"]:
        validate_spec_case(case)
        if case["id"] in results_by_case:
            continue
        if gateway_mode:
            selections = request_gateway_selections(
                api_key=api_key,
                base_url=args.gateway_base_url,
                model=model,
                temperature=spec["temperature"],
                reasoning_effort=args.reasoning_effort,
                timeout=args.request_timeout,
                case=case,
            )
        else:
            selections = request_native_selections(
                api_key=api_key,
                model=model,
                temperature=spec["temperature"],
                case=case,
            )
        candidate_tts_text = render_tts_text(case, selections)
        results_by_case[case["id"]] = {
            "case_id": case["id"],
            "source_text": case["source_text"],
            "control_tts_text": case["control_tts_text"],
            "candidate_tts_text": candidate_tts_text,
            "selections": selections,
            "matches_control": candidate_tts_text == case["control_tts_text"],
            "text_conserved": strip_accents(candidate_tts_text) == case["source_text"],
        }
        write_checkpoint(
            path=args.output,
            experiment_id=experiment_id,
            model=model,
            transport=transport,
            reasoning_effort=args.reasoning_effort,
            temperature=spec["temperature"],
            ordered_case_ids=ordered_case_ids,
            results_by_case=results_by_case,
            complete=False,
        )
    write_checkpoint(
        path=args.output,
        experiment_id=experiment_id,
        model=model,
        transport=transport,
        reasoning_effort=args.reasoning_effort,
        temperature=spec["temperature"],
        ordered_case_ids=ordered_case_ids,
        results_by_case=results_by_case,
        complete=True,
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (KeyError, TypeError, ValueError, RuntimeError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        raise SystemExit(1) from exc
