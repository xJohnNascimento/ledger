-- +goose Up

-- +goose StatementBegin
do $$
begin
    if not exists (select from pg_roles where rolname = 'ledger_owner') then
        create role ledger_owner nologin;
    end if;
    if not exists (select from pg_roles where rolname = 'ledger_app') then
        create role ledger_app login;
    end if;
    if not exists (select from pg_roles where rolname = 'ledger_audit') then
        create role ledger_audit login;
    end if;
end
$$;
-- +goose StatementEnd

grant ledger_owner to current_user;
create schema ledger authorization ledger_owner;
set local role ledger_owner;

alter default privileges for role ledger_owner revoke execute on functions from public;
alter default privileges for role ledger_owner in schema ledger grant select on tables to ledger_app, ledger_audit;
grant usage on schema ledger to ledger_app, ledger_audit;

create domain ledger.signed_amount as numeric
    check (
        scale(value) = 0
        and value >= -170141183460469231731687303715884105728
        and value <= 170141183460469231731687303715884105727
    );

create domain ledger.amount as numeric
    check (
        scale(value) = 0
        and value >= 0
        and value <= 170141183460469231731687303715884105727
    );

create table ledger.asset (
    code text primary key check (code ~ '^[A-Z]{3}$'),
    scale smallint not null check (scale between 0 and 18)
);

create table ledger.ledger (
    id text primary key check (id ~ '^[a-z][a-z0-9_-]{0,62}$'),
    legal_entity_key text not null check (legal_entity_key <> ''),
    name text not null check (name <> '')
);

create table ledger.purpose (
    code text primary key check (code ~ '^[a-z][a-z0-9_]{0,62}$'),
    owner_kind text not null check (owner_kind in ('CUSTOMER', 'PROVIDER', 'PAYMENT', 'HOUSE')),
    normal_side text not null check (normal_side in ('DEBIT', 'CREDIT')),
    enforce_available boolean not null,
    unique (code, owner_kind, normal_side, enforce_available)
);

create table ledger.account (
    id uuid primary key,
    ledger_id text not null references ledger.ledger,
    owner_kind text not null,
    owner_id text not null check (length(owner_id) between 1 and 128),
    purpose text not null,
    asset_code text not null references ledger.asset,
    account_key text not null check (length(account_key) <= 128),
    normal_side text not null,
    enforce_available boolean not null,
    status text not null default 'ACTIVE' check (status in ('ACTIVE', 'BLOCKED')),
    created_at timestamptz not null default now(),
    unique (ledger_id, owner_kind, owner_id, purpose, asset_code, account_key),
    unique (id, ledger_id),
    unique (id, ledger_id, asset_code),
    unique (id, enforce_available),
    foreign key (purpose, owner_kind, normal_side, enforce_available)
        references ledger.purpose (code, owner_kind, normal_side, enforce_available)
);

create table ledger.account_balance (
    account_id uuid primary key,
    enforce_available boolean not null,
    posted_balance ledger.signed_amount not null default 0,
    reserved_amount ledger.amount not null default 0,
    overdraft_limit ledger.amount not null default 0,
    version bigint not null default 0 check (version >= 0),
    last_posting_sequence bigint not null default 0 check (last_posting_sequence >= 0),
    foreign key (account_id, enforce_available) references ledger.account (id, enforce_available),
    constraint available_not_negative check (
        not enforce_available or posted_balance - reserved_amount + overdraft_limit >= 0
    )
);

create table ledger.command (
    id uuid primary key,
    ledger_id text not null references ledger.ledger,
    namespace text not null check (namespace ~ '^[a-z][a-z0-9_.-]{0,127}$'),
    reference text not null check (length(reference) between 1 and 256),
    operation text not null check (operation in (
        'POST_JOURNAL', 'RESERVE_FUNDS', 'COMMIT_RESERVATION', 'RELEASE_RESERVATION',
        'UPDATE_ACCOUNT_CONTROLS', 'POST_CORRECTION'
    )),
    request_hash bytea not null check (length(request_hash) = 32),
    hash_version smallint not null check (hash_version > 0),
    outcome text check (outcome in ('COMPLETED', 'REFUSED')),
    reason text check (reason ~ '^[A-Z][A-Z_]{0,63}$'),
    detail jsonb,
    entry_id uuid,
    reservation_id uuid,
    account_id uuid,
    committed_at timestamptz,
    created_xid xid8 not null,
    unique (ledger_id, namespace, reference),
    unique (id, ledger_id),
    check (case when outcome = 'REFUSED' then reason is not null else reason is null end),
    check ((outcome is null) = (committed_at is null)),
    check (outcome is not null or (detail is null and entry_id is null and reservation_id is null and account_id is null))
);

create table ledger.journal_entry (
    id uuid primary key,
    ledger_id text not null,
    command_id uuid not null unique,
    kind text not null check (kind ~ '^[a-z][a-z0-9_.-]{0,63}$'),
    product_reference text not null check (length(product_reference) between 1 and 256),
    recorded_at timestamptz not null default now(),
    accounting_date date not null default (now() at time zone 'UTC')::date,
    corrects_entry_id uuid,
    reason text check (length(reason) between 1 and 1024),
    created_xid xid8 not null,
    unique (id, ledger_id),
    foreign key (command_id, ledger_id) references ledger.command (id, ledger_id),
    foreign key (corrects_entry_id, ledger_id) references ledger.journal_entry (id, ledger_id),
    check ((corrects_entry_id is null) = (reason is null))
);

create index journal_entry_product_reference on ledger.journal_entry (product_reference);

create table ledger.posting (
    entry_id uuid not null,
    line_no smallint not null check (line_no between 1 and 50),
    ledger_id text not null,
    account_id uuid not null,
    asset_code text not null,
    side text not null check (side in ('DEBIT', 'CREDIT')),
    amount ledger.amount not null check (amount > 0),
    account_sequence bigint not null check (account_sequence > 0),
    primary key (entry_id, line_no),
    unique (account_id, account_sequence),
    foreign key (entry_id, ledger_id) references ledger.journal_entry (id, ledger_id),
    foreign key (account_id, ledger_id, asset_code) references ledger.account (id, ledger_id, asset_code)
);

create table ledger.reservation (
    id uuid primary key,
    ledger_id text not null,
    namespace text not null check (namespace ~ '^[a-z][a-z0-9_.-]{0,127}$'),
    reference text not null check (length(reference) between 1 and 256),
    fingerprint bytea not null check (length(fingerprint) = 32),
    created_by_command_id uuid not null unique,
    state text not null default 'ACTIVE' check (state in ('ACTIVE', 'COMMITTED', 'RELEASED')),
    final_command_id uuid unique,
    final_entry_id uuid,
    final_fingerprint bytea check (length(final_fingerprint) = 32),
    created_at timestamptz not null default now(),
    finalized_at timestamptz,
    unique (ledger_id, namespace, reference),
    unique (id, ledger_id),
    foreign key (created_by_command_id, ledger_id) references ledger.command (id, ledger_id),
    foreign key (final_command_id, ledger_id) references ledger.command (id, ledger_id),
    foreign key (final_entry_id, ledger_id) references ledger.journal_entry (id, ledger_id),
    check ((state = 'ACTIVE') = (final_command_id is null)),
    check ((state = 'ACTIVE') = (final_fingerprint is null)),
    check ((state = 'ACTIVE') = (finalized_at is null)),
    check (state <> 'ACTIVE' or final_entry_id is null)
);

create table ledger.reservation_scope (
    reservation_id uuid not null,
    account_id uuid not null,
    ledger_id text not null,
    primary key (reservation_id, account_id),
    foreign key (reservation_id, ledger_id) references ledger.reservation (id, ledger_id),
    foreign key (account_id, ledger_id) references ledger.account (id, ledger_id)
);

create table ledger.reservation_leg (
    reservation_id uuid not null,
    account_id uuid not null,
    max_reduction ledger.amount not null check (max_reduction > 0),
    primary key (reservation_id, account_id),
    foreign key (reservation_id, account_id) references ledger.reservation_scope (reservation_id, account_id)
);

create index reservation_leg_account on ledger.reservation_leg (account_id);

create table ledger.reservation_event (
    reservation_id uuid not null references ledger.reservation,
    version integer not null check (version > 0),
    from_state text check (from_state = 'ACTIVE'),
    to_state text not null check (to_state in ('ACTIVE', 'COMMITTED', 'RELEASED')),
    command_id uuid not null references ledger.command,
    entry_id uuid references ledger.journal_entry,
    occurred_at timestamptz not null default now(),
    primary key (reservation_id, version),
    check ((from_state is null) = (to_state = 'ACTIVE'))
);

alter table ledger.command
    add foreign key (entry_id) references ledger.journal_entry,
    add foreign key (reservation_id) references ledger.reservation,
    add foreign key (account_id) references ledger.account;

insert into ledger.asset (code, scale) values ('BRL', 2), ('CAD', 2), ('EUR', 2), ('USD', 2);

insert into ledger.ledger (id, legal_entity_key, name) values ('corridor', 'corridor', 'Corridor');

insert into ledger.purpose (code, owner_kind, normal_side, enforce_available) values
    ('funds', 'CUSTOMER', 'CREDIT', true),
    ('cash', 'PROVIDER', 'DEBIT', true),
    ('clearing', 'PAYMENT', 'CREDIT', true),
    ('fee_revenue', 'HOUSE', 'CREDIT', false),
    ('spread_revenue', 'HOUSE', 'CREDIT', false),
    ('funding_equity', 'HOUSE', 'CREDIT', false),
    ('fx_difference', 'HOUSE', 'DEBIT', false);

-- +goose StatementBegin
create function ledger.refuse_change() returns trigger
language plpgsql
set search_path = ledger, pg_temp
as $$
begin
    raise exception '% on ledger.% is not allowed', tg_op, tg_table_name
        using errcode = 'LD204';
end
$$;
-- +goose StatementEnd

-- +goose StatementBegin
create function ledger.stamp_transaction() returns trigger
language plpgsql
set search_path = ledger, pg_temp
as $$
begin
    new.created_xid := pg_current_xact_id();
    return new;
end
$$;
-- +goose StatementEnd

-- Only the listed columns may change, and only under the stated condition.
-- +goose StatementBegin
create function ledger.check_update() returns trigger
language plpgsql
set search_path = ledger, pg_temp
as $$
declare
    v_mutable text[];
    v_allowed boolean;
begin
    case tg_table_name
        when 'account' then
            v_mutable := array['status'];
            v_allowed := true;
        when 'command' then
            v_mutable := array['outcome', 'reason', 'detail', 'entry_id', 'reservation_id', 'account_id', 'committed_at'];
            v_allowed := old.outcome is null and old.created_xid = pg_current_xact_id();
        when 'reservation' then
            v_mutable := array['state', 'final_command_id', 'final_entry_id', 'final_fingerprint', 'finalized_at'];
            v_allowed := old.state = 'ACTIVE';
    end case;
    if not v_allowed or to_jsonb(new) - v_mutable is distinct from to_jsonb(old) - v_mutable then
        raise exception 'update of ledger.% % is not allowed', tg_table_name, old.id
            using errcode = 'LD204';
    end if;
    return new;
end
$$;
-- +goose StatementEnd

-- +goose StatementBegin
create function ledger.check_posting() returns trigger
language plpgsql
set search_path = ledger, pg_temp
as $$
begin
    perform from ledger.journal_entry
    where id = new.entry_id and created_xid = pg_current_xact_id();
    if not found then
        raise exception 'postings of entry % must be written by the transaction that created it', new.entry_id
            using errcode = 'LD203';
    end if;
    return new;
end
$$;
-- +goose StatementEnd

-- +goose StatementBegin
create function ledger.check_entry() returns trigger
language plpgsql
set search_path = ledger, pg_temp
as $$
declare
    v_legs integer;
    v_asset text;
begin
    select count(*) into v_legs from ledger.posting where entry_id = new.id;
    if v_legs not between 2 and 50 then
        raise exception 'entry % has % postings; 2 to 50 are required', new.id, v_legs
            using errcode = 'LD201';
    end if;

    select asset_code into v_asset
    from ledger.posting
    where entry_id = new.id
    group by asset_code
    having sum(case side when 'DEBIT' then amount else -amount end) <> 0
    limit 1;
    if found then
        raise exception 'entry % does not balance in %', new.id, v_asset
            using errcode = 'LD202';
    end if;
    return null;
end
$$;
-- +goose StatementEnd

-- +goose StatementBegin
create function ledger.check_command() returns trigger
language plpgsql
set search_path = ledger, pg_temp
as $$
begin
    perform from ledger.command where id = new.id and outcome is null;
    if found then
        raise exception 'command % would commit without an outcome', new.id
            using errcode = 'LD205';
    end if;
    return null;
end
$$;
-- +goose StatementEnd

create trigger stamp_transaction before insert on ledger.command
    for each row execute function ledger.stamp_transaction();
create trigger stamp_transaction before insert on ledger.journal_entry
    for each row execute function ledger.stamp_transaction();

create trigger check_posting before insert on ledger.posting
    for each row execute function ledger.check_posting();

create constraint trigger check_entry after insert on ledger.journal_entry
    deferrable initially deferred
    for each row execute function ledger.check_entry();

create constraint trigger check_command after insert on ledger.command
    deferrable initially deferred
    for each row execute function ledger.check_command();

create trigger check_update before update on ledger.account
    for each row execute function ledger.check_update();
create trigger check_update before update on ledger.command
    for each row execute function ledger.check_update();
create trigger check_update before update on ledger.reservation
    for each row execute function ledger.check_update();

-- +goose StatementBegin
do $$
declare
    v_table text;
begin
    foreach v_table in array array[
        'asset', 'ledger', 'purpose', 'journal_entry', 'posting',
        'reservation_scope', 'reservation_leg', 'reservation_event'
    ] loop
        execute format(
            'create trigger refuse_change before update or delete on ledger.%I for each row execute function ledger.refuse_change()',
            v_table
        );
    end loop;
    foreach v_table in array array['account', 'account_balance', 'command', 'reservation'] loop
        execute format(
            'create trigger refuse_change before delete on ledger.%I for each row execute function ledger.refuse_change()',
            v_table
        );
    end loop;
    for v_table in select tablename from pg_tables where schemaname = 'ledger' loop
        execute format(
            'create trigger refuse_truncate before truncate on ledger.%I for each statement execute function ledger.refuse_change()',
            v_table
        );
    end loop;
end
$$;
-- +goose StatementEnd

-- ON CONFLICT DO NOTHING waits for a concurrent insert of the same key; the read that follows is a
-- new statement, so its snapshot sees the winner.
-- +goose StatementBegin
create function ledger.ensure_account(
    p_new_id uuid,
    p_ledger_id text,
    p_owner_kind text,
    p_owner_id text,
    p_purpose text,
    p_asset_code text,
    p_account_key text,
    out account_id uuid,
    out created boolean
)
language plpgsql
security definer
set search_path = ledger, pg_temp
as $$
declare
    v_purpose ledger.purpose;
begin
    perform from ledger.ledger where id = p_ledger_id;
    if not found then
        raise exception 'ledger % not found', p_ledger_id using errcode = 'LD404';
    end if;
    perform from ledger.asset where code = p_asset_code;
    if not found then
        raise exception 'asset % not found', p_asset_code using errcode = 'LD404';
    end if;
    select * into v_purpose from ledger.purpose where code = p_purpose;
    if not found or v_purpose.owner_kind <> p_owner_kind then
        raise exception 'purpose % does not accept owner kind %', p_purpose, p_owner_kind
            using errcode = 'LD101';
    end if;

    insert into ledger.account as a (
        id, ledger_id, owner_kind, owner_id, purpose, asset_code, account_key, normal_side, enforce_available
    ) values (
        p_new_id, p_ledger_id, p_owner_kind, p_owner_id, p_purpose, p_asset_code, p_account_key,
        v_purpose.normal_side, v_purpose.enforce_available
    )
    on conflict (ledger_id, owner_kind, owner_id, purpose, asset_code, account_key) do nothing
    returning a.id into account_id;

    if found then
        insert into ledger.account_balance (account_id, enforce_available)
        values (ensure_account.account_id, v_purpose.enforce_available);
        created := true;
        return;
    end if;

    select a.id into strict account_id
    from ledger.account a
    where a.ledger_id = p_ledger_id
      and a.owner_kind = p_owner_kind
      and a.owner_id = p_owner_id
      and a.purpose = p_purpose
      and a.asset_code = p_asset_code
      and a.account_key = p_account_key;
    created := false;
end
$$;
-- +goose StatementEnd

grant execute on function ledger.ensure_account(uuid, text, text, text, text, text, text) to ledger_app;

reset role;
