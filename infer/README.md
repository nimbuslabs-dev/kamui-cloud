# Infer

I'm Sneha, working on the infer folder for Kamui Cloud. This folder will handle AI model inference and serving.

## Status

Phase 1: `/healthz` and a dummy `/predict` endpoint. The real model comes in Phase 2.

## Setup

```powershell
cd infer
uv sync
```

## Run

Runs on port 8090.

```powershell
uv run uvicorn app:app --app-dir src --port 8090
```

Check it: http://127.0.0.1:8090/healthz

## Test

```powershell
uv run pytest
uv run ruff check .
```
