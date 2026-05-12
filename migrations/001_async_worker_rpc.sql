-- Atomic job claim for Go worker.
-- Run in Supabase SQL editor or via migrations.

create unique index if not exists unique_active_analyze_job_per_call
on public.processing_jobs(call_id)
where type = 'analyze_call'
and status in ('queued', 'running');

-- Replace is not allowed when the return type (or some other signature detail) changes.
drop function if exists public.claim_processing_job(text);

create or replace function public.claim_processing_job(p_worker_id text)
returns setof public.processing_jobs
language plpgsql
security definer
set search_path = public
as $$
declare
  v_job public.processing_jobs;
begin
  update public.processing_jobs
  set
    status = 'running',
    locked_at = now(),
    locked_by = p_worker_id,
    attempts = attempts + 1,
    started_at = coalesce(started_at, now()),
    updated_at = now()
  where id = (
    select id
    from public.processing_jobs
    where status = 'queued'
      and attempts < max_attempts
      and type in ('analyze_call', 'regenerate_report')
    order by created_at asc
    for update skip locked
    limit 1
  )
  returning * into v_job;

  if v_job.id is not null then
    return next v_job;
  end if;

  return;
end;
$$;

revoke all on function public.claim_processing_job(text) from public;

-- RPC for Lovable/frontend to start analysis safely.
-- This function should be callable by authenticated users.
drop function if exists public.start_call_analysis(uuid);

create or replace function public.start_call_analysis(p_call_id uuid)
returns public.processing_jobs
language plpgsql
security definer
set search_path = public
as $$
declare
  v_user_id uuid := auth.uid();
  v_call public.calls;
  v_job public.processing_jobs;
begin
  if v_user_id is null then
    raise exception 'not_authenticated';
  end if;

  select * into v_call
  from public.calls
  where id = p_call_id
    and deleted_at is null;

  if v_call.id is null then
    raise exception 'call_not_found';
  end if;

  if not exists (
    select 1
    from public.organization_users ou
    where ou.organization_id = v_call.organization_id
      and ou.user_id = v_user_id
  ) then
    raise exception 'forbidden';
  end if;

  if v_call.status not in ('uploaded', 'error') then
    raise exception 'invalid_call_status';
  end if;

  if exists (
    select 1
    from public.processing_jobs pj
    where pj.call_id = p_call_id
      and pj.type = 'analyze_call'
      and pj.status in ('queued', 'running')
  ) then
    raise exception 'analysis_already_running';
  end if;

  insert into public.processing_jobs (
    organization_id,
    call_id,
    type,
    status,
    attempts,
    max_attempts,
    created_at,
    updated_at
  ) values (
    v_call.organization_id,
    v_call.id,
    'analyze_call',
    'queued',
    0,
    3,
    now(),
    now()
  ) returning * into v_job;

  update public.calls
  set
    status = 'queued',
    error_code = null,
    error_message = null,
    updated_at = now()
  where id = p_call_id;

  return v_job;
end;
$$;

grant execute on function public.start_call_analysis(uuid) to authenticated;
