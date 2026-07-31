#!/usr/bin/env bash

set -Eeuo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
temporary_dir="$(mktemp -d)"
trap 'rm -rf -- "${temporary_dir}"' EXIT

api_url="${API_URL:-http://127.0.0.1:${API_HTTP_PORT:-8080}}"
data_dir="${DATA_DIR:-${project_root}/data}"
reference_audio="${data_dir}/voice_reference_omnivoice.flac"
reference_text_file="${data_dir}/transcription.txt"
book_file="${temporary_dir}/smoke.fb2"
book_response="${temporary_dir}/book.json"
voice_response="${temporary_dir}/voice.json"
job_response="${temporary_dir}/job.json"
status_response="${temporary_dir}/status.json"
archive_output="${temporary_dir}/book.zip"
chapters_response="${temporary_dir}/chapters.json"
chapter_archive_output="${temporary_dir}/chapter.zip"

reference_text="$(<"${reference_text_file}")"

printf '%s\n' \
  '<?xml version="1.0" encoding="UTF-8"?>' \
  '<FictionBook xmlns="http://www.gribuser.ru/xml/fictionbook/2.0">' \
  '  <description><title-info><book-title>API smoke</book-title>' \
  '  <author><first-name>Codex</first-name></author></title-info></description>' \
  '  <body><section><title><p>Проверка</p></title>' \
  '  <p>Это простая проверка.</p>' \
  '  </section></body>' \
  '</FictionBook>' >"${book_file}"

for attempt in $(seq 1 240); do
  if curl --fail --silent --output /dev/null "${api_url}/healthz"; then
    break
  fi
  if [[ "${attempt}" -eq 240 ]]; then
    printf 'Go API did not become ready in 240 seconds\n' >&2
    exit 1
  fi
  sleep 1
done

curl --fail-with-body --silent --show-error \
  -F "file=@${book_file};type=application/xml" \
  "${api_url}/v1/book" \
  --output "${book_response}"

curl --fail-with-body --silent --show-error \
  -F "reference_audio=@${reference_audio};type=audio/flac" \
  --form-string "reference_text=${reference_text}" \
  -F "name=Smoke voice" \
  "${api_url}/v1/voice" \
  --output "${voice_response}"

book_id="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["id"])' "${book_response}")"
voice_id="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["id"])' "${voice_response}")"

curl --fail-with-body --silent --show-error \
  -X POST \
  "${api_url}/v1/generate/book/${book_id}/voice/${voice_id}" \
  --output "${job_response}"

job_id="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["job_id"])' "${job_response}")"

for attempt in $(seq 1 900); do
  curl --fail-with-body --silent --show-error \
    "${api_url}/v1/job/${job_id}" \
    --output "${status_response}"
  status="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["status"])' "${status_response}")"
  case "${status}" in
    completed)
      break
      ;;
    completed_with_warnings|failed)
      curl --fail-with-body --silent --show-error \
        "${api_url}/v1/job/${job_id}/warnings" >&2
      printf '\nGeneration finished with status %s\n' "${status}" >&2
      exit 1
      ;;
  esac
  if [[ "${attempt}" -eq 900 ]]; then
    printf 'Generation job did not finish in 900 seconds\n' >&2
    exit 1
  fi
  sleep 1
done

curl --fail-with-body --silent --show-error \
  "${api_url}/v1/job/${job_id}/audio.zip" \
  --output "${archive_output}"

curl --fail-with-body --silent --show-error \
  "${api_url}/v1/job/${job_id}/chapters" \
  --output "${chapters_response}"

chapter_number="$(python3 -c '
import json
import sys

chapters = json.load(open(sys.argv[1], encoding="utf-8"))["chapters"]
assert chapters, "chapter list is empty"
print(chapters[0]["chapter_number"])
' "${chapters_response}")"

curl --fail-with-body --silent --show-error \
  "${api_url}/v1/job/${job_id}/chapters/${chapter_number}/audio.zip" \
  --output "${chapter_archive_output}"

python3 -c '
import json
import sys
import zipfile
from pathlib import Path

archive = Path(sys.argv[1])
chapter_archive = Path(sys.argv[2])
chapter_number = int(sys.argv[3])
with zipfile.ZipFile(archive) as bundle:
    names = bundle.namelist()
    assert "manifest.json" in names, names
    flac_names = [name for name in names if name.endswith(".flac")]
    assert flac_names, names
    assert not any(name.endswith(".wav") for name in names), names
    assert all(
        Path(name).name.startswith("character_")
        for name in flac_names
    ), flac_names
    assert all(
        bundle.read(name).startswith(b"fLaC")
        for name in flac_names
    ), flac_names
    manifest = json.loads(bundle.read("manifest.json"))

with zipfile.ZipFile(chapter_archive) as bundle:
    chapter_names = bundle.namelist()
    chapter_flac_names = [
        name for name in chapter_names if name.endswith(".flac")
    ]
    assert len(chapter_flac_names) == 1, chapter_names
    assert not any(name.endswith(".wav") for name in chapter_names), chapter_names
    expected_prefix = f"character_{chapter_number:04d}_"
    assert Path(chapter_flac_names[0]).name.startswith(expected_prefix), chapter_flac_names
    assert bundle.read(chapter_flac_names[0]).startswith(b"fLaC"), chapter_flac_names[0]
    chapter_manifest = json.loads(bundle.read("manifest.json"))
    assert chapter_manifest["scope"] == "chapter", chapter_manifest
    assert chapter_manifest["chapter"]["chapter_number"] == chapter_number, chapter_manifest

print(json.dumps({
    "job_id": manifest["job"]["id"],
    "status": manifest["job"]["status"],
    "archive": str(archive),
    "flac_files": flac_names,
    "chapter_archive": str(chapter_archive),
    "chapter_flac_files": chapter_flac_names,
}, ensure_ascii=False, indent=2))
' "${archive_output}" "${chapter_archive_output}" "${chapter_number}"
