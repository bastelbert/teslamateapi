# Charge cost write

TeslaMateApi can optionally write the cost of a **completed** charging process into TeslaMate (`charging_processes.cost`), for example after a Supercharger or wallbox invoice arrived.

The feature is **disabled by default**. Without `ENABLE_COST_WRITE=true` and a valid configuration the routes are not registered at all and the API behaves exactly as before (every route below answers with the normal `404 Page not found`). The read endpoints are not changed in any way, `GET /api/v1/cars/:CarID/charges` still returns `"cost": 0` for charges without cost.

- [How it works](#how-it-works)
- [Security information](#security-information)
- [Setup](#setup)
- [Environment variables](#environment-variables)
- [Endpoints](#endpoints)
- [Errors](#errors)
- [Compatibility with TeslaMate](#compatibility-with-teslamate)

## How it works

- Only `charging_processes.cost` is changed. No other column, no Tesla credentials, no vehicle commands.
- Amounts are decimal strings (`"12.34"`), never floats. `"0"` is a free charge, `null` means "not recorded". The API never sets `null`, except when an undo restores a previous `null`.
- The currency is fixed by the operator (`COST_CURRENCY`). Requests in another currency are rejected, nothing is converted.
- Optimistic concurrency: every write sends `expected_cost`, the value the client last saw. If the current value differs (compared numerically, `"5"` equals `5.00`), the request fails with `409` and the current value.
- Idempotency: every write has a client generated `request_id` (UUID). Repeating the identical request returns the identical response (with header `Idempotent-Replayed: true`) and writes nothing. The same `request_id` with other data fails with `409`.
- Atomic: one transaction locks the charging process (`SELECT … FOR UPDATE`), checks everything, updates the cost and inserts an audit entry. If the audit entry cannot be written, the cost change is rolled back.
- Audit trail: every change and every undo is stored in `teslamateapi_audit.charge_cost_changes`. The table is created by the operator with [sql/charge_cost_audit.sql](../sql/charge_cost_audit.sql), the API never creates or migrates anything.
- Schema guard: before every request the API checks that `charging_processes` still has `id`, `car_id` (integer), `end_date` (timestamp) and `cost` (numeric with at least two decimals). If TeslaMate changes its schema, the endpoints answer `503` instead of writing.

## Security information

- Run TeslaMateApi only in your home network or behind a reverse proxy with HTTPS. The write token is sent in every request.
- `COST_WRITE_TOKEN` is independent of `API_TOKEN`, has to be at least 32 characters long and must differ from `API_TOKEN`. Generate it randomly, e.g. `openssl rand -hex 32`.
- The token is only accepted in the `Authorization: Bearer <token>` header (never as `?token=`), compared in constant time and never logged.
- Responses of these routes have `Cache-Control: no-store`, no CORS headers are sent, bodies are limited to 4 KiB, must be `application/json` and must not contain unknown fields.
- **Make a backup of your TeslaMate database before the setup** (see the [TeslaMate backup docs](https://docs.teslamate.org/docs/maintenance/backup_restore)).
- Recommended: use the restricted database role from [sql/charge_cost_role.sql](../sql/charge_cost_role.sql) via `COST_DATABASE_DSN`. It can only read `id`, `car_id`, `end_date` and `cost`, only update `cost` and only read and insert audit entries.

## Setup

1. Back up the TeslaMate database.

2. Create the audit table (run as the TeslaMate database owner, it only creates the schema `teslamateapi_audit`):

   ```bash
   docker compose exec -T database psql -U teslamate -d teslamate < sql/charge_cost_audit.sql
   ```

3. Optional, recommended: create the restricted role and give it a password.

   ```bash
   docker compose exec -T database psql -U teslamate -d teslamate < sql/charge_cost_role.sql
   docker compose exec database psql -U teslamate -d teslamate -c '\password teslamateapi_cost'
   ```

4. Configure TeslaMateApi, e.g. in your `docker-compose.yml`:

   ```yaml
   services:
     teslamateapi:
       image: teslamateapi:cost-write # built from this repository, see below
       restart: always
       depends_on:
         - database
       environment:
         - ENCRYPTION_KEY=MySuperSecretEncryptionKey
         - DATABASE_USER=teslamate
         - DATABASE_PASS=secret
         - DATABASE_NAME=teslamate
         - DATABASE_HOST=database
         - MQTT_HOST=mosquitto
         - TZ=Europe/Berlin
         # charge cost write
         - ENABLE_COST_WRITE=true
         - COST_WRITE_TOKEN=${COST_WRITE_TOKEN} # from .env, at least 32 random characters
         - COST_CURRENCY=EUR
         - COST_TESLAMATE_URL=https://teslamate.home.example
         - COST_DATABASE_DSN=postgres://teslamateapi_cost:${COST_DB_PASSWORD}@database:5432/teslamate?sslmode=disable
       ports:
         - 8080:8080
   ```

   The upstream image `tobiasehlert/teslamateapi` does not contain this feature. Build the image from this repository:

   ```bash
   docker build -t teslamateapi:cost-write .
   ```

5. Check the log after the start. You should see `charge cost endpoints enabled (currency EUR, own database role: true)`. If the configuration is incomplete, every problem is logged as `[error] initChargeCostWrite - …` and the endpoints stay disabled while the rest of the API keeps working.

## Environment variables

| Variable              | Type    | Default | Description                                                                                          |
| --------------------- | ------- | ------- | ---------------------------------------------------------------------------------------------------- |
| **ENABLE_COST_WRITE** | boolean | _false_ | Registers the charge cost endpoints.                                                                 |
| **COST_WRITE_TOKEN**  | string  |         | Required. Bearer token for the endpoints, at least 32 printable characters, different from API_TOKEN. |
| **COST_CURRENCY**     | string  |         | Required. ISO 4217 code in upper case, e.g. `EUR`. No default.                                       |
| **COST_TESLAMATE_URL**| string  |         | Required. Base url of the TeslaMate instance this API writes to, returned as `teslamate_url`.        |
| **COST_DATABASE_DSN** | string  | _DATABASE\_\* settings_ | Optional. Separate PostgreSQL connection (restricted role) for these endpoints.        |

## Endpoints

All endpoints need the header `Authorization: Bearer <COST_WRITE_TOKEN>`. Successful responses are wrapped in `{"data": …}`, errors are `{"error": "<text>", "code": "<code>"}` with a real HTTP status code.

### GET `/api/v1/cars/:CarID/charges/:ChargeID/cost`

```bash
curl -H "Authorization: Bearer $COST_WRITE_TOKEN" http://teslamateapi:8080/api/v1/cars/1/charges/42/cost
```

```json
{
  "data": {
    "car_id": 1,
    "charge_id": 42,
    "cost": "12.34",
    "currency": "EUR",
    "completed": true,
    "confirmed": {
      "request_id": "0d6f3c52-9a71-4c1e-8b3f-2e5d7a9c4b10",
      "kind": "set",
      "car_id": 1,
      "charge_id": 42,
      "old_cost": null,
      "new_cost": "12.34",
      "currency": "EUR",
      "undo_of": null,
      "created_at": "2026-10-29T17:54:12Z"
    },
    "teslamate_url": "https://teslamate.home.example"
  }
}
```

- `cost`: decimal string or `null` (not recorded). `"0.00"` is a free charge.
- `completed`: the charging process has an `end_date`. Only completed charges can be changed.
- `confirmed`: the latest audit entry of this charge if its `new_cost` still equals the current cost, otherwise `null` (never changed through this API, or changed in TeslaMate afterwards).
- `teslamate_url`: `COST_TESLAMATE_URL`, so a client can verify it talks to the intended instance.

### PUT `/api/v1/cars/:CarID/charges/:ChargeID/cost`

```bash
curl -X PUT -H "Authorization: Bearer $COST_WRITE_TOKEN" -H "Content-Type: application/json" \
  -d '{"request_id":"0d6f3c52-9a71-4c1e-8b3f-2e5d7a9c4b10","expected_cost":null,"cost":"12.34","currency":"EUR"}' \
  http://teslamateapi:8080/api/v1/cars/1/charges/42/cost
```

| Field           | Type           | Description                                                                         |
| --------------- | -------------- | ----------------------------------------------------------------------------------- |
| `request_id`    | string (UUID)  | Required. New UUID per change, reuse it only to repeat the identical request.      |
| `expected_cost` | string or null | Required. The cost the client expects to be stored right now.                      |
| `cost`          | string         | Required. `^(0\|[1-9][0-9]{0,5})(\.[0-9]{1,2})?$`, i.e. 0 to 999999.99, no exponent, no separators. |
| `currency`      | string         | Required. Has to equal `COST_CURRENCY`.                                             |

TeslaMate stores the cost as `numeric(6,2)`, so values from 10000 upwards are rejected by the database with `422` even though the API format allows them.

Response (`200`), the stored audit entry:

```json
{
  "data": {
    "request_id": "0d6f3c52-9a71-4c1e-8b3f-2e5d7a9c4b10",
    "kind": "set",
    "car_id": 1,
    "charge_id": 42,
    "old_cost": null,
    "new_cost": "12.34",
    "currency": "EUR",
    "undo_of": null,
    "created_at": "2026-10-29T17:54:12Z"
  }
}
```

Amounts in responses always have two decimals (`"12.3"` is stored and returned as `"12.30"`). `created_at` is UTC in RFC 3339 without fractional seconds.

### POST `/api/v1/changes/:RequestID/undo`

Reverts the change `:RequestID` (the `request_id` of a previous set or undo). Only the **latest** change of a charging process can be undone, and only if the current cost still equals `expected_cost` and the value written by that change. Undoing an undo restores the undone value again.

```bash
curl -X POST -H "Authorization: Bearer $COST_WRITE_TOKEN" -H "Content-Type: application/json" \
  -d '{"request_id":"7b2e9d41-5c3a-4f8e-a6d1-93c0e8f2b754","expected_cost":"12.34"}' \
  http://teslamateapi:8080/api/v1/changes/0d6f3c52-9a71-4c1e-8b3f-2e5d7a9c4b10/undo
```

```json
{
  "data": {
    "request_id": "7b2e9d41-5c3a-4f8e-a6d1-93c0e8f2b754",
    "kind": "undo",
    "car_id": 1,
    "charge_id": 42,
    "old_cost": "12.34",
    "new_cost": null,
    "currency": "EUR",
    "undo_of": "0d6f3c52-9a71-4c1e-8b3f-2e5d7a9c4b10",
    "created_at": "2026-10-29T17:54:12Z"
  }
}
```

## Errors

| Status | `code`                                   | Meaning                                                                                  |
| ------ | ---------------------------------------- | ---------------------------------------------------------------------------------------- |
| 400    | `invalid_request`                        | Body is not a single JSON object or contains unknown fields.                            |
| 401    | `unauthorized`                           | `Authorization: Bearer` header missing or malformed.                                    |
| 403    | `forbidden`                              | Token invalid.                                                                           |
| 404    | `charge_not_found`, `change_not_found`   | Unknown charge, charge belongs to another car, invalid ID or unknown change.            |
| 409    | `cost_conflict`                          | `expected_cost` differs from the current cost, `current_cost` is part of the response.  |
| 409    | `request_id_reused`                      | `request_id` was already used with different data.                                      |
| 409    | `not_latest_change`                      | Undo of a change that is not the latest change of the charge.                           |
| 409    | `changed_externally`                     | Undo not possible, the cost was changed outside of this API (e.g. in TeslaMate).        |
| 413    | `body_too_large`                         | Body larger than 4 KiB.                                                                  |
| 415    | `unsupported_media_type`                 | `Content-Type` is not `application/json`.                                                |
| 422    | `invalid_amount`                         | Amount not a valid decimal string, `cost` is `null`, or too large for TeslaMate's column. |
| 422    | `invalid_currency`                       | Currency differs from `COST_CURRENCY`.                                                   |
| 422    | `invalid_request_id`                     | `request_id` is not a UUID.                                                              |
| 422    | `charge_not_completed`                   | Charging process has no `end_date` yet.                                                  |
| 503    | `audit_unavailable`                      | Audit table missing, not accessible or the audit entry could not be written.            |
| 503    | `schema_unsupported`                     | `charging_processes` does not have the expected columns and types.                     |
| 503    | `database_unavailable`                   | Other database problem or timeout.                                                       |

Example conflict:

```json
{ "code": "cost_conflict", "error": "expected_cost does not match the current cost", "current_cost": "12.34" }
```

Error responses never contain SQL details or tokens, details are in the TeslaMateApi log. Every `409`, `422` and `503` means nothing was changed.

## Compatibility with TeslaMate

TeslaMate does not guarantee a stable database schema. This feature only relies on `charging_processes.id`, `car_id`, `end_date` and `cost` and refuses to write if their types differ (see the schema guard above). It was tested against a synthetic schema that mirrors TeslaMate's `charging_processes` (`cost numeric(6,2)`, `end_date timestamp`), not against a real TeslaMate installation. TeslaMate itself sets the cost when a charge completes (if the geofence has a cost configured), which is why only completed charges can be changed. If the cost is edited later in TeslaMate's UI, `confirmed` becomes `null` and an undo of the previous API change fails with `changed_externally`.

## Tests

Unit tests run without a database. The integration tests need a **disposable** PostgreSQL database, they drop and recreate `charging_processes` and `teslamateapi_audit`:

```bash
docker run --rm -d --name teslamateapi-test -p 55432:5432 -e POSTGRES_USER=teslamate -e POSTGRES_PASSWORD=test postgres:17
TESLAMATEAPI_TEST_DATABASE_DSN='postgres://teslamate:test@localhost:55432/teslamate?sslmode=disable' go test -race ./src/
```
