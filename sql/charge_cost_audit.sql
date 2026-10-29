-- TeslaMateApi: audit table for the charge cost endpoints (ENABLE_COST_WRITE)
--
-- Run this once as the owner of the TeslaMate database (the user TeslaMate itself uses), e.g.:
--   docker compose exec -T database psql -U teslamate -d teslamate < sql/charge_cost_audit.sql
--
-- The script only creates objects in its own schema "teslamateapi_audit" and does not touch any TeslaMate table.
-- It can be run again safely. Make a backup of the TeslaMate database before.

BEGIN;

CREATE SCHEMA IF NOT EXISTS teslamateapi_audit;

CREATE TABLE IF NOT EXISTS teslamateapi_audit.charge_cost_changes (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    request_id uuid        NOT NULL UNIQUE,
    car_id     bigint      NOT NULL,
    charge_id  bigint      NOT NULL,
    old_cost   numeric,
    new_cost   numeric,
    currency   text        NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    kind       text        NOT NULL CHECK (kind IN ('set', 'undo')),
    undo_of    uuid        REFERENCES teslamateapi_audit.charge_cost_changes (request_id),
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((kind = 'undo') = (undo_of IS NOT NULL)),
    CHECK (kind = 'undo' OR new_cost IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS charge_cost_changes_charge_idx
    ON teslamateapi_audit.charge_cost_changes (charge_id, id DESC);

COMMENT ON TABLE teslamateapi_audit.charge_cost_changes IS
    'Changes of charging_processes.cost made through TeslaMateApi (PUT .../charges/:id/cost and POST /changes/:id/undo)';

COMMIT;
