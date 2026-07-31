FROM nvidia/cuda:12.8.1-cudnn-runtime-ubuntu24.04@sha256:ac55d124da4882b497f732d8dfd9a702d5447a5f29d08d56da6f64f0a1eb34bc

ARG DEBIAN_FRONTEND=noninteractive

ENV PATH="/opt/venv/bin:${PATH}" \
    PYTHONPATH="/app/src" \
    PYTHONUNBUFFERED="1" \
    PYTHONDONTWRITEBYTECODE="1" \
    HF_HOME="/models/huggingface"

RUN apt-get update \
    && apt-get install --yes --no-install-recommends \
        ca-certificates \
        python3 \
        python3-pip \
        python3-venv \
    && rm -rf /var/lib/apt/lists/*

RUN python3 -m venv /opt/venv \
    && pip install --no-cache-dir --upgrade pip==26.1.2 \
    && useradd --create-home --uid 10001 worker \
    && mkdir -p /app/src /models \
    && chown -R worker:worker /app /models

WORKDIR /app

COPY requirements.lock /app/requirements.lock
RUN pip install --no-cache-dir --requirement /app/requirements.lock

COPY --chown=worker:worker src /app/src

USER worker

EXPOSE 8000

HEALTHCHECK --interval=15s --timeout=5s --start-period=180s --retries=3 \
    CMD ["python", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8000/health/ready', timeout=3)"]

CMD ["uvicorn", "stt_worker.main:app", "--host", "0.0.0.0", "--port", "8000", "--workers", "1", "--no-access-log"]
