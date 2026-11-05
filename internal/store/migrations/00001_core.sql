-- +goose Up

-- A team owns keys and a budget. Limits of 0 mean no limit for that dimension.
CREATE TABLE teams (
    id                  text PRIMARY KEY,
    name                text NOT NULL UNIQUE,
    budget_limit_micros bigint NOT NULL CHECK (budget_limit_micros >= 0),
    requests_per_minute bigint NOT NULL DEFAULT 0 CHECK (requests_per_minute >= 0),
    tokens_per_minute   bigint NOT NULL DEFAULT 0 CHECK (tokens_per_minute >= 0),
    max_in_flight       bigint NOT NULL DEFAULT 0 CHECK (max_in_flight >= 0),
    created_at          timestamptz NOT NULL DEFAULT now()
);

-- Only a hash of a key's secret is stored, so a database leak reveals no usable key.
CREATE TABLE api_keys (
    id           text PRIMARY KEY,
    team_id      text NOT NULL REFERENCES teams (id),
    name         text NOT NULL,
    secret_hash  bytea NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    revoked_at   timestamptz
);
CREATE INDEX api_keys_team ON api_keys (team_id);

-- A price list is immutable once created, so that a charge can always be recomputed
-- from the version pinned on its attempt.
CREATE TABLE price_versions (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    label        text NOT NULL UNIQUE,
    content_hash text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- Prices are in pico-USD per token, so that fractions of a micro-USD are exact.
CREATE TABLE prices (
    price_version_id  bigint NOT NULL REFERENCES price_versions (id),
    provider          text NOT NULL,
    model             text NOT NULL,
    input_pico        bigint NOT NULL CHECK (input_pico >= 0),
    cached_input_pico bigint NOT NULL CHECK (cached_input_pico >= 0),
    cache_write_pico  bigint NOT NULL CHECK (cache_write_pico >= 0),
    output_pico       bigint NOT NULL CHECK (output_pico >= 0),
    PRIMARY KEY (price_version_id, provider, model)
);

-- This row is the authority for a team's spending in a period. Admission locks it,
-- which serializes admission per team across all gateway replicas.
CREATE TABLE budget_periods (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    team_id         text NOT NULL REFERENCES teams (id),
    starts_at       timestamptz NOT NULL,
    ends_at         timestamptz NOT NULL,
    limit_micros    bigint NOT NULL CHECK (limit_micros >= 0),
    settled_micros  bigint NOT NULL DEFAULT 0 CHECK (settled_micros >= 0),
    reserved_micros bigint NOT NULL DEFAULT 0 CHECK (reserved_micros >= 0),
    CHECK (ends_at > starts_at),
    UNIQUE (team_id, starts_at)
);

-- A logical client request. Its attempts are separate, because each can cost money.
CREATE TABLE requests (
    id         text PRIMARY KEY,
    team_id    text NOT NULL REFERENCES teams (id),
    key_id     text NOT NULL REFERENCES api_keys (id),
    alias      text NOT NULL,
    stream     boolean NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX requests_team_created ON requests (team_id, created_at);

-- One upstream attempt. See docs/adr/0002-postgres-is-the-budget-authority.md for
-- the state machine. A row in state 'reserved' proves nothing was sent, because
-- dispatch intent commits before sending. Every other open state may have cost money.
CREATE TABLE attempts (
    id               text PRIMARY KEY,
    request_id       text NOT NULL REFERENCES requests (id),
    number           integer NOT NULL CHECK (number >= 1),
    team_id          text NOT NULL REFERENCES teams (id),
    budget_period_id bigint NOT NULL REFERENCES budget_periods (id),
    price_version_id bigint NOT NULL REFERENCES price_versions (id),
    provider         text NOT NULL,
    model            text NOT NULL,

    -- What the reservation was computed from, so it can be reproduced.
    input_bound_tokens  bigint NOT NULL CHECK (input_bound_tokens >= 0),
    output_bound_tokens bigint NOT NULL CHECK (output_bound_tokens >= 0),
    reserved_micros     bigint NOT NULL CHECK (reserved_micros >= 0),

    state            text NOT NULL CHECK (state IN ('reserved', 'dispatched', 'uncertain', 'reconciled', 'released')),
    lease_expires_at timestamptz NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    dispatched_at    timestamptz,
    finished_at      timestamptz,
    settled_at       timestamptz,

    -- What happened, without any request or response content.
    outcome         text,
    error_kind      text,
    delivery        text,
    upstream_status integer,
    upstream_code   text,

    -- The provider's report: normalized counts, plus exactly what it said.
    usage_state        text,
    prompt_tokens      bigint,
    completion_tokens  bigint,
    cached_tokens      bigint,
    cache_write_tokens bigint,
    reasoning_tokens   bigint,
    raw_usage          jsonb,

    charge_micros    bigint CHECK (charge_micros >= 0),
    charge_kind      text CHECK (charge_kind IN ('reported', 'rejected', 'conservative')),
    over_reservation boolean NOT NULL DEFAULT false,

    UNIQUE (request_id, number)
);
CREATE INDEX attempts_open ON attempts (lease_expires_at) WHERE state IN ('reserved', 'dispatched', 'uncertain');
CREATE INDEX attempts_team_created ON attempts (team_id, created_at);
CREATE INDEX attempts_period ON attempts (budget_period_id);

-- Every change to settled spend. The unique key makes settlement idempotent: repeated
-- callbacks, retries, or reconcilers can't double-charge.
CREATE TABLE ledger_entries (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    attempt_id    text NOT NULL REFERENCES attempts (id),
    kind          text NOT NULL CHECK (kind IN ('charge', 'late_adjustment')),
    amount_micros bigint NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (attempt_id, kind)
);

-- +goose Down
DROP TABLE ledger_entries;
DROP TABLE attempts;
DROP TABLE requests;
DROP TABLE budget_periods;
DROP TABLE prices;
DROP TABLE price_versions;
DROP TABLE api_keys;
DROP TABLE teams;
