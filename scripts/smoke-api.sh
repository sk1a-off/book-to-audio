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
book_archive="${temporary_dir}/book.zip"
chapters_response="${temporary_dir}/chapters.json"
chapter_audio="${temporary_dir}/chapter.flac"

reference_text="$(<"${reference_text_file}")"

cat >"${book_file}" <<'FB2'
<?xml version="1.0" encoding="UTF-8"?>
<FictionBook xmlns="http://www.gribuser.ru/xml/fictionbook/2.0">
  <description><title-info><book-title>API smoke</book-title>
  <author><first-name>Codex</first-name></author></title-info></description>
  <body><section><title><p>Проверка</p></title>
  <p>Это простая проверка.</p></section></body>
</FictionBook>
FB2

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
  "${api_url}/v1/book" --output "${book_response}"

curl --fail-with-body --silent --show-error \
  -F "reference_audio=@${reference_audio};type=audio/flac" \
  --form-string "reference_text=${reference_text}" \
  -F "name=Smoke voice" \
  "${api_url}/v1/voice" --output "${voice_response}"

book_id="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["id"])' "${book_response}")"
voice_id="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["id"])' "${voice_response}")"

curl --fail-with-body --silent --show-error -X POST \
  "${api_url}/v1/generate/book/${book_id}/voice/${voice_id}" \
  --output "${job_response}"
job_id="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["job_id"])' "${job_response}")"

# The catalog must be readable immediately; this request is metadata-only and
# must not assemble a chapter or create an archive.
curl --fail-with-body --silent --show-error \
  "${api_url}/v1/job/${job_id}/chapters" --output "${chapters_response}"
python3 - "${chapters_response}" <<'PY'
import json
import sys

payload = json.load(open(sys.argv[1], encoding="utf-8"))
assert payload["chapters"], payload
assert payload["fragments"], payload
assert len(payload["fragments"]) == sum(
    chapter["fragments_count"] for chapter in payload["chapters"]
), payload
PY

for attempt in $(seq 1 900); do
  curl --fail-with-body --silent --show-error \
    "${api_url}/v1/job/${job_id}" --output "${status_response}"
  status="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["status"])' "${status_response}")"
  case "${status}" in
    completed) break ;;
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
  "${api_url}/v1/job/${job_id}/chapters" --output "${chapters_response}"
chapter_url="$(python3 -c '
import json,sys
chapters=json.load(open(sys.argv[1], encoding="utf-8"))["chapters"]
assert chapters, "chapter list is empty"
assert chapters[0]["ready"], chapters[0]
print(chapters[0]["audio_url"])
' "${chapters_response}")"

curl --fail-with-body --silent --show-error \
  "${api_url}${chapter_url}" --output "${chapter_audio}"

curl --fail-with-body --silent --show-error \
  "${api_url}/v1/job/${job_id}/audio.zip" --output "${book_archive}"

python3 - "${book_archive}" "${chapter_audio}" <<'PY'
import json
import sys
import zipfile
from pathlib import Path

book_archive = Path(sys.argv[1])
chapter_audio = Path(sys.argv[2])
assert chapter_audio.read_bytes().startswith(b"fLaC"), chapter_audio
with zipfile.ZipFile(book_archive) as bundle:
    names = bundle.namelist()
    assert "manifest.json" in names, names
    flac_names = [name for name in names if name.endswith(".flac")]
    assert flac_names, names
    assert all(bundle.read(name).startswith(b"fLaC") for name in flac_names)
    manifest = json.loads(bundle.read("manifest.json"))

print(json.dumps({
    "job_id": manifest["job"]["id"],
    "status": manifest["job"]["status"],
    "book_archive": str(book_archive),
    "chapter_flac": str(chapter_audio),
    "book_flac_files": flac_names,
}, ensure_ascii=False, indent=2))
PY
