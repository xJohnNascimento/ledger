-- +goose Up

set local role ledger_owner;

-- write_journal gains the corrected entry and the reason, which a correction must write with its
-- entry because entries are append-only. The previous signature stays as a wrapper.
-- +goose StatementBegin
create function ledger.write_journal(
    p_ledger_id text,
    p_allowed_purposes text[],
    p_reservation_id uuid,
    p_command_id uuid,
    p_entry_id uuid,
    p_kind text,
    p_product_reference text,
    p_accounting_date date,
    p_account_ids uuid[],
    p_sides text[],
    p_asset_codes text[],
    p_amounts numeric[],
    p_corrects_entry_id uuid,
    p_reason text
) returns jsonb
language plpgsql
set search_path = ledger, pg_temp
as $$
declare
    v_line_count integer := cardinality(p_account_ids);
    v_settling boolean := p_reservation_id is not null;
    v_account record;
    v_available numeric;
    v_refusal jsonb;
begin
    if v_line_count is null
        or cardinality(p_sides) is distinct from v_line_count
        or cardinality(p_asset_codes) is distinct from v_line_count
        or cardinality(p_amounts) is distinct from v_line_count
        or (v_line_count = 0 and not v_settling) then
        raise exception 'journal lines are malformed' using errcode = 'LD422';
    end if;

    perform from ledger.account_balance b
    where b.account_id = any(p_account_ids)
       or b.account_id in (select g.account_id from ledger.reservation_leg g where g.reservation_id = p_reservation_id)
    order by b.account_id
    for update;

    for v_account in
        select l.account_id,
               a.id is not null and a.ledger_id = p_ledger_id as visible,
               coalesce(bool_and(l.asset_code = a.asset_code), false) as same_asset,
               coalesce(a.purpose = any(p_allowed_purposes), false) as allowed,
               s.account_id is not null as in_scope,
               g.max_reduction as ceiling,
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
        left join ledger.reservation_scope s on s.reservation_id = p_reservation_id and s.account_id = l.account_id
        left join ledger.reservation_leg g on g.reservation_id = p_reservation_id and g.account_id = l.account_id
        group by l.account_id, a.id, b.account_id, s.reservation_id, s.account_id, g.reservation_id, g.account_id
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
        if v_settling and not v_account.in_scope then
            raise exception 'account % is outside the reservation scope', v_account.account_id
                using errcode = 'LD423';
        end if;
        if v_settling and v_account.delta < 0 and v_account.enforce_available and v_account.ceiling is null then
            raise exception 'enforced account % is reduced but has no reservation leg', v_account.account_id
                using errcode = 'LD423';
        end if;
        if v_account.posted_balance + v_account.delta not between -170141183460469231731687303715884105728
                                                          and 170141183460469231731687303715884105727 then
            raise exception 'the balance of account % would leave the signed 128-bit range', v_account.account_id
                using errcode = 'LD400';
        end if;
        continue when v_refusal is not null or v_account.delta >= 0;

        if v_settling and -v_account.delta > coalesce(v_account.ceiling, -v_account.delta) then
            v_refusal := jsonb_build_object(
                'reason', 'CEILING_EXCEEDED',
                'account_id', v_account.account_id,
                'available', v_account.ceiling::text,
                'required', (-v_account.delta)::text
            );
            continue;
        end if;
        if not v_settling and v_account.status = 'BLOCKED' then
            v_refusal := jsonb_build_object('reason', 'ACCOUNT_BLOCKED', 'account_id', v_account.account_id);
            continue;
        end if;
        continue when not v_account.enforce_available;
        v_available := v_account.posted_balance - v_account.reserved_amount
                       + coalesce(v_account.ceiling, 0) + v_account.overdraft_limit;
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

    if v_line_count > 0 then
        insert into ledger.journal_entry (
            id, ledger_id, command_id, kind, product_reference, accounting_date, corrects_entry_id, reason
        ) values (
            p_entry_id, p_ledger_id, p_command_id, p_kind, p_product_reference,
            coalesce(p_accounting_date, (now() at time zone 'UTC')::date), p_corrects_entry_id, p_reason
        );

        insert into ledger.posting (entry_id, line_no, ledger_id, account_id, asset_code, side, amount, account_sequence)
        select p_entry_id, l.line_no, p_ledger_id, l.account_id, l.asset_code, l.side, l.amount,
               b.last_posting_sequence + row_number() over (partition by l.account_id order by l.line_no)
        from unnest(p_account_ids, p_sides, p_asset_codes, p_amounts)
            with ordinality as l(account_id, side, asset_code, amount, line_no)
        join ledger.account_balance b on b.account_id = l.account_id;
    end if;

    update ledger.account_balance b
    set posted_balance = b.posted_balance + coalesce(d.delta, 0),
        reserved_amount = b.reserved_amount - coalesce(d.released, 0),
        last_posting_sequence = b.last_posting_sequence + coalesce(d.postings, 0),
        version = b.version + 1
    from (
        select coalesce(j.account_id, g.account_id) as account_id, j.delta, j.postings, g.max_reduction as released
        from (
            select l.account_id,
                   sum(case when l.side = a.normal_side then l.amount else -l.amount end) as delta,
                   count(*) as postings
            from unnest(p_account_ids, p_sides, p_amounts) as l(account_id, side, amount)
            join ledger.account a on a.id = l.account_id
            group by l.account_id
        ) j
        full join (
            select account_id, max_reduction from ledger.reservation_leg where reservation_id = p_reservation_id
        ) g on g.account_id = j.account_id
    ) d
    where b.account_id = d.account_id;

    return null;
end
$$;
-- +goose StatementEnd

-- +goose StatementBegin
create or replace function ledger.write_journal(
    p_ledger_id text,
    p_allowed_purposes text[],
    p_reservation_id uuid,
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
language sql
set search_path = ledger, pg_temp
as $$
    select ledger.write_journal(
        p_ledger_id, p_allowed_purposes, p_reservation_id, p_command_id, p_entry_id, p_kind,
        p_product_reference, p_accounting_date, p_account_ids, p_sides, p_asset_codes, p_amounts, null, null
    );
$$;
-- +goose StatementEnd

-- +goose StatementBegin
create function ledger.post_correction(
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
    p_amounts numeric[],
    p_corrects_entry_id uuid,
    p_reason text
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
        p_command_id, p_ledger_id, p_namespace, p_reference, 'POST_CORRECTION', p_request_hash, p_hash_version
    );
    if v_command.id is not null then
        return v_command;
    end if;
    perform from ledger.journal_entry where id = p_corrects_entry_id and ledger_id = p_ledger_id;
    if not found then
        raise exception 'entry % not found', p_corrects_entry_id using errcode = 'LD404';
    end if;

    v_refusal := ledger.write_journal(
        p_ledger_id, p_allowed_purposes, null, p_command_id, p_entry_id, p_kind, p_product_reference,
        p_accounting_date, p_account_ids, p_sides, p_asset_codes, p_amounts, p_corrects_entry_id, p_reason
    );
    return ledger.finish_command(p_command_id, v_refusal, p_entry_id, null, null);
end
$$;
-- +goose StatementEnd

-- Controls take the same balance lock as money commands. Lowering an enforced account's overdraft
-- below what it already uses is a stored refusal, never a negative availability.
-- +goose StatementBegin
create function ledger.update_account_controls(
    p_command_id uuid,
    p_ledger_id text,
    p_namespace text,
    p_reference text,
    p_request_hash bytea,
    p_hash_version smallint,
    p_account_id uuid,
    p_status text,
    p_overdraft_limit numeric
) returns ledger.command
language plpgsql
security definer
set search_path = ledger, pg_temp
as $$
declare
    v_command ledger.command;
    v_balance record;
begin
    v_command := ledger.claim_command(
        p_command_id, p_ledger_id, p_namespace, p_reference, 'UPDATE_ACCOUNT_CONTROLS', p_request_hash, p_hash_version
    );
    if v_command.id is not null then
        return v_command;
    end if;

    select a.ledger_id, a.enforce_available, b.posted_balance, b.reserved_amount, b.overdraft_limit
    into v_balance
    from ledger.account_balance b
    join ledger.account a on a.id = b.account_id
    where b.account_id = p_account_id
    for update of b;
    if not found or v_balance.ledger_id <> p_ledger_id then
        raise exception 'account % not found', p_account_id using errcode = 'LD404';
    end if;

    if v_balance.enforce_available
        and v_balance.posted_balance - v_balance.reserved_amount + coalesce(p_overdraft_limit, v_balance.overdraft_limit) < 0 then
        return ledger.finish_command(p_command_id, jsonb_build_object(
            'reason', 'INSUFFICIENT_FUNDS',
            'account_id', p_account_id,
            'available', (v_balance.posted_balance - v_balance.reserved_amount + v_balance.overdraft_limit)::text,
            'required', (v_balance.overdraft_limit - p_overdraft_limit)::text
        ), null, null, null);
    end if;

    if p_status is not null then
        update ledger.account set status = p_status where id = p_account_id;
    end if;
    update ledger.account_balance
    set overdraft_limit = coalesce(p_overdraft_limit, overdraft_limit),
        version = version + 1
    where account_id = p_account_id;

    return ledger.finish_command(p_command_id, null, null, null, p_account_id);
end
$$;
-- +goose StatementEnd

grant execute on function ledger.post_correction(
    uuid, text, text, text, bytea, smallint, text[], uuid, text, text, date, uuid[], text[], text[], numeric[], uuid, text
) to ledger_app;
grant execute on function ledger.update_account_controls(
    uuid, text, text, text, bytea, smallint, uuid, text, numeric
) to ledger_app;

reset role;
