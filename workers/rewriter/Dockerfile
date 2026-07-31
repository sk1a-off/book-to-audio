FROM python:3.12.13-slim-bookworm@sha256:72d3d75f2639ab82b34b29390ad3d6e0827c775befee94edda8e9976818f488d AS builder

ARG DEBIAN_FRONTEND=noninteractive

ENV CMAKE_ARGS="-DGGML_BLAS=ON -DGGML_BLAS_VENDOR=OpenBLAS -DGGML_NATIVE=OFF" \
    CMAKE_BUILD_PARALLEL_LEVEL="2" \
    FORCE_CMAKE="1" \
    PATH="/opt/venv/bin:${PATH}" \
    PIP_DISABLE_PIP_VERSION_CHECK="1"

RUN apt-get update \
    && apt-get install --yes --no-install-recommends \
        build-essential \
        cmake \
        libopenblas-dev \
        ninja-build \
        pkg-config \
    && rm -rf /var/lib/apt/lists/*

RUN python -m venv /opt/venv \
    && pip install --no-cache-dir --upgrade pip==26.1.2

WORKDIR /build

COPY requirements.lock /build/requirements.lock
RUN pip install --no-cache-dir --requirement /build/requirements.lock

FROM python:3.12.13-slim-bookworm@sha256:72d3d75f2639ab82b34b29390ad3d6e0827c775befee94edda8e9976818f488d

ARG DEBIAN_FRONTEND=noninteractive

ENV PATH="/opt/venv/bin:${PATH}" \
    PYTHONPATH="/app/src" \
    PYTHONUNBUFFERED="1" \
    PYTHONDONTWRITEBYTECODE="1" \
    HF_HOME="/models/huggingface" \
    REWRITER_MODELS_DIR="/models/rewriter"

RUN apt-get update \
    && apt-get install --yes --no-install-recommends \
        ca-certificates \
        libgomp1 \
        libopenblas0-pthread \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 10001 worker \
    && mkdir -p /app/src /models/huggingface /models/rewriter \
    && chown -R worker:worker /app /models

WORKDIR /app

COPY --from=builder /opt/venv /opt/venv
COPY --chown=worker:worker src /app/src

USER worker

EXPOSE 8000

HEALTHCHECK --interval=15s --timeout=5s --start-period=15s --retries=3 \
    CMD ["python", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8000/health/ready', timeout=3)"]

CMD ["uvicorn", "rewriter_worker.main:app", "--host", "0.0.0.0", "--port", "8000", "--workers", "1", "--no-access-log"]
