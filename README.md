# Call Worker Go

Production-oriented async Go worker for the call analysis MVP.

The service no longer exposes a long synchronous `/v1/process` endpoint. It works as a background worker:

1. Lovable uploads audio to Supabase Storage.
2. Lovable creates/updates `calls` and `call_files` in Supabase.
3. Lovable starts analysis via Supabase RPC `start_call_analysis(call_id)`.
4. The RPC creates a `processing_jobs` row with `status = queued`.
5. This Go service atomically claims queued jobs via Supabase RPC `claim_processing_job(worker_id)`.
6. The worker downloads audio from Supabase Storage.
7. The worker sends audio to SaluteSpeech / SmartSpeech.
8. The worker sends transcription to GigaChat for summary.
9. The worker writes results back to Supabase:
   - `call_transcriptions`
   - `call_analysis`
   - `call_files`
   - `call_reports`
   - `processing_logs`
10. The worker updates `calls.status` and `processing_jobs.status`.
11. Lovable displays progress by reading Supabase or via Realtime.

## HTTP API

Only service endpoints are exposed:

```bash
curl http://localhost:8080/healthz
curl http://localhost:8080/readyz
```

There is intentionally no public audio-processing endpoint. Processing is driven by Supabase `processing_jobs`.

## Required Supabase migration

Run:

```sql
migrations/001_async_worker_rpc.sql
```

It creates:

- unique index preventing duplicate active `analyze_call` jobs;
- `claim_processing_job(worker_id)` RPC for atomic worker locking;
- `start_call_analysis(call_id)` RPC for Lovable/frontend.

## Run locally

```bash
cp .env.example .env
# edit .env
make run
```

## Docker Compose

```bash
cp .env.example .env
# edit .env
make docker-up
```

## Environment

See `.env.example`.

Important secrets must stay only on the server:

- `SUPABASE_SERVICE_ROLE_KEY`
- `SALUTE_AUTH_KEY`
- `GIGACHAT_AUTH_KEY`

Never put these keys into Lovable/frontend.

## Worker statuses

The worker updates `calls.status`:

```text
queued
preparing_audio
recognizing
recognized
analyzing
generating_report
report_ready
error
```

On errors, it writes:

- `calls.error_code`
- `calls.error_message`
- `processing_jobs.error_code`
- `processing_jobs.error_message`
- `processing_jobs.error_details`
- `processing_logs`

## Reports currently generated

The worker generates:

- `transcription.txt`
- `summary.txt`
- `full-analysis.json`

PDF generation can be added as a separate report generator without changing the worker/job architecture.
# 2b-empty-call
