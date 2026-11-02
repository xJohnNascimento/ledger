-- +goose Up

set local role ledger_owner;

-- A concurrent claim of the same reference makes the insert wait for that transaction. The read
-- that follows is a new statement, so its snapshot sees the winner's committed outcome.
-- +goose StatementBegin
create function ledger.claim_command(
    p_id uuid,
    p_ledger_id text,
    p_namespace text,
    p_reference text,
    p_operation text,
    p_request_hash bytea,
    p_hash_version smallint
) returns ledger.command
language plpgsql
set search_path = ledger, pg_temp
as $$
declare
    v_command ledger.command;
begin
    perform from ledger.ledger where id = p_ledger_id;
    if not found then
        raise exception 'ledger % not found', p_ledger_id using errcode = 'LD404';
    end if;

    insert into ledger.command (id, ledger_id, namespace, reference, operation, request_hash, hash_version)
    values (p_id, p_ledger_id, p_namespace, p_reference, p_operation, p_request_hash, p_hash_version)
    on conflict (ledger_id, namespace, reference) do nothing;
    if found then
        return null;
    end if;

    select * into strict v_command
    from ledger.command c
    where c.ledger_id = p_ledger_id and c.namespace = p_namespace and c.reference = p_reference;
    if v_command.operation <> p_operation
        or v_command.request_hash <> p_request_hash
        or v_command.hash_version <> p_hash_version then
        raise exception 'command %/% was already used with another request', p_namespace, p_reference
            using errcode = 'LD409', detail = v_command.id::text;
    end if;
    return v_command;
end
$$;
-- +goose StatementEnd

-- +goose StatementBegin
create function ledger.finish_command(
    p_id uuid,
    p_refusal jsonb,
    p_entry_id uuid,
    p_reservation_id uuid,
    p_account_id uuid
) returns ledger.command
language sql
set search_path = ledger, pg_temp
as $$
    update ledger.command
    set outcome = case when p_refusal is null then 'COMPLETED' else 'REFUSED' end,
        reason = p_refusal ->> 'reason',
        detail = p_refusal - 'reason',
        entry_id = case when p_refusal is null then p_entry_id end,
        reservation_id = case when p_refusal is null then p_reservation_id end,
        account_id = case when p_refusal is null then p_account_id end,
        committed_at = now()
    where id = p_id
    returning *;
$$;
-- +goose StatementEnd

-- Locks every touched balance in ascending account id, validates the journal against the stored
-- accounts, and either returns the refusal to store or writes the entry and returns null.
-- +goose StatementBegin
create function ledger.write_journal(
    p_ledger_id text,
    p_allowed_purposes text[],
    p_command_id uuid,
    p_entry_id uuid,
    p_kind text,
    p_product_reference text,
    p_accounting_date date,
    p_account_ids uuid[],
    p_sides text[],
    p_asset_codes text[],
    p_amounts numeric[]
) returns jsonb
language plpgsql
set search_path = ledger, pg_temp
as $$
declare
    v_line_count integer := cardinality(p_account_ids);
    v_account record;
    v_available numeric;
    v_refusal jsonb;
begin
    if v_line_count is null
        or cardinality(p_sides) is distinct from v_line_count
        or cardinality(p_asset_codes) is distinct from v_line_count
        or cardinality(p_amounts) is distinct from v_line_count then
        raise exception 'journal line arrays differ in length' using errcode = 'LD422';
    end if;

    perform from ledger.account_balance b
    where b.account_id = any(p_account_ids)
    order by b.account_id
    for update;

    for v_account in
        select l.account_id,
               a.id is not null and a.ledger_id = p_ledger_id as visible,
               coalesce(bool_and(l.asset_code = a.asset_code), false) as same_asset,
               coalesce(a.purpose = any(p_allowed_purposes), false) as allowed,
               a.purpose,
               a.status,
               a.enforce_available,
               b.posted_balance,
               b.reserved_amount,
               b.overdraft_limit,
               sum(case when l.side = a.normal_side then l.amount else -l.amount end) as delta
        from unnest(p_account_ids, p_sides, p_asset_codes, p_amounts) as l(account_id, side, asset_code, amount)
        left join ledger.account a on a.id = l.account_id
        left join ledger.account_balance b on b.account_id = a.id
        group by l.account_id, a.id, b.account_id
        order by l.account_id
    loop
        if not v_account.visible then
            raise exception 'account % not found', v_account.account_id using errcode = 'LD404';
        end if;
        if not v_account.same_asset then
            raise exception 'a posting on account % is not in its asset', v_account.account_id
                using errcode = 'LD422';
        end if;
        if not v_account.allowed then
            raise exception 'purpose % of account % is outside the caller''s scope', v_account.purpose, v_account.account_id
                using errcode = 'LD403';
        end if;
        if v_account.posted_balance + v_account.delta not between -170141183460469231731687303715884105728
                                                          and 170141183460469231731687303715884105727 then
            raise exception 'the balance of account % would leave the signed 128-bit range', v_account.account_id
                using errcode = 'LD400';
        end if;
        continue when v_refusal is not null or v_account.delta >= 0;

        if v_account.status = 'BLOCKED' then
            v_refusal := jsonb_build_object('reason', 'ACCOUNT_BLOCKED', 'account_id', v_account.account_id);
            continue;
        end if;
        continue when not v_account.enforce_available;
        v_available := v_account.posted_balance - v_account.reserved_amount + v_account.overdraft_limit;
        if v_available + v_account.delta < 0 then
            v_refusal := jsonb_build_object(
                'reason', 'INSUFFICIENT_FUNDS',
                'account_id', v_account.account_id,
                'available', v_available::text,
                'required', (-v_account.delta)::text
            );
        end if;
    end loop;

    if v_refusal is not null then
        return v_refusal;
    end if;

    insert into ledger.journal_entry (id, ledger_id, command_id, kind, product_reference, accounting_date)
    values (
        p_entry_id, p_ledger_id, p_command_id, p_kind, p_product_reference,
        coalesce(p_accounting_date, (now() at time zone 'UTC')::date)
    );

    insert into ledger.posting (entry_id, line_no, ledger_id, account_id, asset_code, side, amount, account_sequence)
    select p_entry_id, l.line_no, p_ledger_id, l.account_id, l.asset_code, l.side, l.amount,
           b.last_posting_sequence + row_number() over (partition by l.account_id order by l.line_no)
    from unnest(p_account_ids, p_sides, p_asset_codes, p_amounts)
        with ordinality as l(account_id, side, asset_code, amount, line_no)
    join ledger.account_balance b on b.account_id = l.account_id;

    update ledger.account_balance b
    set posted_balance = b.posted_balance + d.delta,
        last_posting_sequence = b.last_posting_sequence + d.postings,
        version = b.version + 1
    from (
        select l.account_id,
               sum(case when l.side = a.normal_side then l.amount else -l.amount end) as delta,
               count(*) as postings
        from unnest(p_account_ids, p_sides, p_amounts) as l(account_id, side, amount)
        join ledger.account a on a.id = l.account_id
        group by l.account_id
    ) d
    where b.account_id = d.account_id;

    return null;
end
$$;
-- +goose StatementEnd

-- +goose StatementBegin
create function ledger.post_journal(
    p_command_id uuid,
    p_ledger_id text,
    p_namespace text,
    p_reference text,
    p_request_hash bytea,
    p_hash_version smallint,
    p_allowed_purposes text[],
    p_entry_id uuid,
    p_kind text,
    p_product_reference text,
    p_accounting_date date,
    p_account_ids uuid[],
    p_sides text[],
    p_asset_codes text[],
    p_amounts numeric[]
) returns ledger.command
language plpgsql
security definer
set search_path = ledger, pg_temp
as $$
declare
    v_command ledger.command;
    v_refusal jsonb;
begin
    v_command := ledger.claim_command(
        p_command_id, p_ledger_id, p_namespace, p_reference, 'POST_JOURNAL', p_request_hash, p_hash_version
    );
    if v_command.id is not null then
        return v_command;
    end if;

    v_refusal := ledger.write_journal(
        p_ledger_id, p_allowed_purposes, p_command_id, p_entry_id, p_kind, p_product_reference,
        p_accounting_date, p_account_ids, p_sides, p_asset_codes, p_amounts
    );
    return ledger.finish_command(p_command_id, v_refusal, p_entry_id, null, null);
end
$$;
-- +goose StatementEnd

grant execute on function ledger.post_journal(
    uuid, text, text, text, bytea, smallint, text[], uuid, text, text, date, uuid[], text[], text[], numeric[]
) to ledger_app;

reset role;
