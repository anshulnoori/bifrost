# Build with linux/amd64 and the eStargz registry exporter; weights live in a Volume.
FROM python:3.11-slim-bookworm@sha256:4b4c524dc3dce996864e030c7bd9c6b0e517597189fee48f48e05b499442444b

ENV PIP_NO_CACHE_DIR=1 PYTHONUNBUFFERED=1
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates libgomp1 libstdc++6 \
    && rm -rf /var/lib/apt/lists/*
COPY requirements-gpu.lock /tmp/gpu.lock
RUN python -m pip install --require-hashes -r /tmp/gpu.lock
COPY requirements-state.lock /tmp/state.lock
RUN python -m pip install --require-hashes -r /tmp/state.lock
COPY optimize_loader.py /tmp/optimize_loader.py
RUN python /tmp/optimize_loader.py && rm /tmp/optimize_loader.py /tmp/gpu.lock /tmp/state.lock
