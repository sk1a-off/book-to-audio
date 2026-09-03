#!/usr/bin/env python3
"""Run a fail-closed Gemini selector over immutable russtress candidates."""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import unicodedata
import urllib.error
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Any


VOWELS = frozenset("аеёиоуыэюяАЕЁИОУЫЭЮЯ")
WORD_RE = re.compile(r"[А-Яа-яЁё]+(?:-[А-Яа-яЁё]+)*")
COMBINING_ACUTE = "\u0301"


@dataclass(frozen=True, slots=True)
class StressCandidate:
    candidate_id: str
    word: str
    stressed_word: str
    accent_after: int
    source_start: int


def parse_russtress_candidates(source: str, accented: str) -> list[StressCandidate]:
    if accented.replace("'", "") != source:
        raise ValueError("russtress output changed canonical text")

    candidates: list[StressCandidate] = []
    plain_offset = 0
    candidate_number = 0
    for match in WORD_RE.finditer(accented):
        token = match.group(0)
        # Apostrophes split the regex token, so inspect the complete non-space span.
        span_end = match.end()
        while span_end < len(accented) and accented[span_end] == "'":
            span_end += 1
            while span_end < len(accented) and accented[span_end].isalpha():
                span_end += 1
        dense_word = accented[match.start():span_end]
        if "'" not in dense_word:
            continue
        if dense_word.count("'") != 1:
            raise ValueError(f"multiple stress marks in {dense_word!r}")
        mark_index = dense_word.index("'")
        plain_word = dense_word.replace("'", "")
        if mark_index == 0 or dense_word[mark_index - 1] not in VOWELS:
            raise ValueError(f"stress mark does not follow a Russian vowel: {dense_word!r}")

        source_start = source.find(plain_word, plain_offset)
        if source_start < 0:
            raise ValueError(f"cannot align russtress word {plain_word!r}")
        plain_offset = source_start + len(plain_word)
        candidate_number += 1
        candidates.append(
            StressCandidate(
                candidate_id=f"c{candidate_number:03d}",
                word=plain_word,
                stressed_word=dense_word.replace("'", COMBINING_ACUTE),
                accent_after=mark_index,
                source_start=source_start,
            )
        )
    return candidates


def build_prompt(source: str, candidates: list[StressCandidate]) -> str:
    rows = [
        {
            "id": item.candidate_id,
            "word": item.word,
            "proposed": item.stressed_word,
            "offset": item.source_start,
        }
        for item in candidates
    ]
    return (
        "Ты выбираешь только действительно необходимые ударения для русского "
        "художественного TTS OmniVoice. Сохраняй метку, если без неё контекстный "
        "омограф, имя или редкое слово с высокой вероятностью будет произнесено "
        "неверно. Удаляй ударения с обычных однозначных слов: плотная разметка "
        "разрушает интонацию. Если предложенная позиция ударения неверна, не "
        "выбирай этот ID. Не исправляй и не создавай варианты: верни только ID "
        "из списка.\n\n"
        f"Исходный текст:\n{source}\n\n"
        "Кандидаты:\n"
        + json.dumps(rows, ensure_ascii=False, separators=(",", ":"))
    )


def request_selection(
    *, api_key: str, model: str, prompt: str, allowed_ids: list[str]
) -> list[str]:
    endpoint = (
        "https://generativelanguage.googleapis.com/v1beta/models/"
        f"{model}:generateContent"
    )
    payload = {
        "contents": [{"role": "user", "parts": [{"text": prompt}]}],
        "generationConfig": {
            "temperature": 0,
            "responseMimeType": "application/json",
            "responseSchema": {
                "type": "object",
                "properties": {
                    "keep_ids": {
                        "type": "array",
                        "items": {"type": "string", "enum": allowed_ids},
                        "maxItems": len(allowed_ids),
                    }
                },
                "required": ["keep_ids"],
            },
        },
    }
    request = urllib.request.Request(
        endpoint,
        data=json.dumps(payload, ensure_ascii=False).encode("utf-8"),
        headers={
            "Content-Type": "application/json",
            "x-goog-api-key": api_key,
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=90) as response:
            envelope = json.load(response)
    except urllib.error.HTTPError as exc:
        # Extract only the documented public error fields. Never include request
        # headers, the complete envelope, or provider metadata: those can contain
        # credential-adjacent information.
        provider_status = ""
        provider_message = ""
        try:
            error_envelope = json.loads(exc.read())
            provider_error = error_envelope.get("error", {})
            provider_status = str(provider_error.get("status", ""))
            provider_message = str(provider_error.get("message", ""))
        except (json.JSONDecodeError, AttributeError, TypeError):
            pass
        details = ": ".join(
            value for value in (provider_status, provider_message) if value
        )
        suffix = f": {details}" if details else ""
        raise RuntimeError(f"Gemini HTTP error {exc.code}{suffix}") from exc
    except urllib.error.URLError as exc:
        raise RuntimeError(f"Gemini connection failed: {exc.reason}") from exc

    try:
        text = envelope["candidates"][0]["content"]["parts"][0]["text"]
        decoded = json.loads(text)
        selected = decoded["keep_ids"]
    except (KeyError, IndexError, TypeError, json.JSONDecodeError) as exc:
        raise RuntimeError("Gemini returned an invalid structured response") from exc
    if not isinstance(selected, list) or any(not isinstance(x, str) for x in selected):
        raise RuntimeError("keep_ids must be a string array")
    if len(selected) != len(set(selected)):
        raise RuntimeError("Gemini returned duplicate candidate IDs")
    if not set(selected).issubset(allowed_ids):
        raise RuntimeError("Gemini returned an unknown candidate ID")
    return selected


def apply_selection(
    source: str, candidates: list[StressCandidate], selected_ids: list[str]
) -> str:
    selected = {item.candidate_id: item for item in candidates if item.candidate_id in selected_ids}
    output = source
    insertions = sorted(
        (item.source_start + item.accent_after, item.candidate_id)
        for item in selected.values()
    )
    for position, _ in reversed(insertions):
        output = output[:position] + COMBINING_ACUTE + output[position:]
    if strip_accents(output) != source:
        raise RuntimeError("selector output violated source-text conservation")
    return unicodedata.normalize("NFC", output)


def strip_accents(value: str) -> str:
    return unicodedata.normalize("NFC", value.replace(COMBINING_ACUTE, ""))


def read_key(path: Path) -> str:
    if path.exists():
        mode = path.stat().st_mode & 0o777
        if mode & 0o077:
            raise RuntimeError(f"secret file permissions are too broad: {mode:o}")
        value = path.read_text(encoding="utf-8").strip()
    else:
        value = os.getenv("GEMINI_API_KEY", "").strip()
    if not value:
        raise RuntimeError("Gemini API key is missing")
    return value


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--spec", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--key-file", type=Path, default=Path("/tmp/tts-gemini-api-key"))
    args = parser.parse_args()

    spec: dict[str, Any] = json.loads(args.spec.read_text(encoding="utf-8"))
    api_key = read_key(args.key_file)
    results: list[dict[str, Any]] = []
    for case in spec["cases"]:
        candidates = parse_russtress_candidates(case["source_text"], case["russtress_text"])
        allowed_ids = [item.candidate_id for item in candidates]
        selected = request_selection(
            api_key=api_key,
            model=spec["model"],
            prompt=build_prompt(case["source_text"], candidates),
            allowed_ids=allowed_ids,
        )
        filtered = apply_selection(case["source_text"], candidates, selected)
        results.append(
            {
                "case_id": case["id"],
                "source_text": case["source_text"],
                "control_tts_text": case["control_tts_text"],
                "candidate_tts_text": filtered,
                "selected_ids": selected,
                "candidate_count": len(candidates),
                "selected_count": len(selected),
                "text_conserved": strip_accents(filtered) == case["source_text"],
            }
        )

    output = {
        "schema_version": "tts-quality-russtress-gemini-filter-result-v1",
        "experiment_id": spec["experiment_id"],
        "model": spec["model"],
        "temperature": spec["temperature"],
        "results": results,
    }
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(
        json.dumps(output, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (KeyError, TypeError, ValueError, RuntimeError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        raise SystemExit(1) from exc
