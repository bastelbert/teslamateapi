-- TeslaMateApi: optional restricted database role for the charge cost endpoints (COST_DATABASE_DSN)
--
-- Run this after sql/charge_cost_audit.sql as the owner of the TeslaMate database, e.g.:
--   docker compose exec -T database psql -U teslamate -d teslamate < sql/charge_cost_role.sql
--
-- Afterwards set a password for the role (it is created without one, so no secret ends up in a file):
--   docker compose exec database psql -U teslamate -d teslamate -c '\password teslamateapi_cost'
--
-- and point COST_DATABASE_DSN to it, e.g.
--   COST_DATABASE_DSN=postgres://teslamateapi_cost:<password>@database:5432/teslamate?sslmode=disable
--
-- The role can only read id, car_id, end_date and cost of charging_processes, only update cost,
-- and only read and insert audit entries (no update or delete of the audit trail).
-- Creating a role needs the CREATEROLE privilege (the default TeslaMate user has it as superuser).

BEGIN;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'teslamateapi_cost') THEN
        CREATE ROLE teslamateapi_cost LOGIN;
    END IF;
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO teslamateapi_cost', current_database());
END
$$;

GRANT USAGE ON SCHEMA public TO teslamateapi_cost;
GRANT SELECT (id, car_id, end_date, cost) ON charging_processes TO teslamateapi_cost;
GRANT UPDATE (cost) ON charging_processes TO teslamateapi_cost;

GRANT USAGE ON SCHEMA teslamateapi_audit TO teslamateapi_cost;
GRANT SELECT, INSERT ON teslamateapi_audit.charge_cost_changes TO teslamateapi_cost;

COMMIT;
