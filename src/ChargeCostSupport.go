package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/lib/pq"
)

// database part of the charge cost endpoints
// all writes run in one transaction: lock charging process, checks, update cost, insert audit entry, commit

// chargeCostQuerier is implemented by *sql.DB and *sql.Tx
type chargeCostQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

const chargeCostChangeColumns = `request_id::text, kind, car_id, charge_id, old_cost::text, new_cost::text, currency, undo_of::text, created_at`

// loadStatus returns the current cost of a charging process and the latest audit entry if it still matches
func (s *chargeCostService) loadStatus(ctx context.Context, carID, chargeID int64) (*chargeCostStatus, *chargeCostError) {
	if cerr := checkChargeCostSchema(ctx, s.db); cerr != nil {
		return nil, cerr
	}

	status := chargeCostStatus{ChargeID: chargeID, Currency: s.cfg.currency, TeslaMateURL: s.cfg.teslamateURL}
	var cost sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT car_id, end_date IS NOT NULL, cost::text
		FROM charging_processes
		WHERE id = $1`, chargeID).Scan(&status.CarID, &status.Completed, &cost)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && status.CarID != carID) {
		return nil, errChargeCostNotFound()
	}
	if err != nil {
		return nil, chargeCostDBError(err, "load charge")
	}
	status.Cost = chargeCostNullString(cost)

	latest, matchesCurrent, cerr := latestChargeCostChange(ctx, s.db, carID, chargeID)
	if cerr != nil {
		return nil, cerr
	}
	if latest != nil && matchesCurrent {
		status.Confirmed = latest
	}
	return &status, nil
}

// setCost writes a new cost, returns replayed=true if the same request was already executed
func (s *chargeCostService) setCost(ctx context.Context, carID, chargeID int64, requestID string, expected *string, cost string) (*chargeCostChange, bool, *chargeCostError) {
	var (
		change   *chargeCostChange
		replayed bool
	)
	cerr := s.inTx(ctx, func(tx *sql.Tx) *chargeCostError {
		if cerr := checkChargeCostSchema(ctx, tx); cerr != nil {
			return cerr
		}

		charge, cerr := lockChargeCostCharge(ctx, tx, chargeID, expected)
		if cerr != nil {
			return cerr
		}
		if charge.carID != carID {
			return errChargeCostNotFound()
		}

		// idempotency: same request_id with the same data returns the stored result
		existing, same, cerr := findChargeCostChange(ctx, tx, requestID,
			`kind = 'set' AND car_id = $2 AND charge_id = $3 AND new_cost = $4::numeric AND old_cost IS NOT DISTINCT FROM $5::numeric AND currency = $6`,
			carID, chargeID, cost, expected, s.cfg.currency)
		if cerr != nil {
			return cerr
		}
		if existing != nil {
			if !same {
				return errChargeCostIdempotency()
			}
			change, replayed = existing, true
			return nil
		}

		if !charge.completed {
			return errChargeCostInvalid("charge_not_completed", "charge is not completed yet")
		}
		if !charge.matchesExpected {
			return errChargeCostConflict(charge.cost)
		}

		if _, err := tx.ExecContext(ctx, `UPDATE charging_processes SET cost = $2::numeric WHERE id = $1`, chargeID, cost); err != nil {
			return chargeCostDBError(err, "update cost")
		}
		change, cerr = insertChargeCostChange(ctx, tx, requestID, "set", carID, chargeID, charge.cost, &cost, s.cfg.currency, nil)
		return cerr
	})
	return change, replayed, cerr
}

// undoChange reverts the change targetID if it is still the latest change of its charging process
func (s *chargeCostService) undoChange(ctx context.Context, targetID, requestID string, expected *string) (*chargeCostChange, bool, *chargeCostError) {
	var (
		change   *chargeCostChange
		replayed bool
	)
	cerr := s.inTx(ctx, func(tx *sql.Tx) *chargeCostError {
		if cerr := checkChargeCostSchema(ctx, tx); cerr != nil {
			return cerr
		}

		target, _, cerr := findChargeCostChange(ctx, tx, targetID, "true")
		if cerr != nil {
			return cerr
		}
		if target == nil {
			return &chargeCostError{status: http.StatusNotFound, code: "change_not_found", message: "change not found"}
		}

		charge, cerr := lockChargeCostCharge(ctx, tx, target.ChargeID, expected)
		if cerr != nil {
			return cerr
		}
		if charge.carID != target.CarID {
			return errChargeCostNotFound()
		}

		// idempotency: same request_id with the same data returns the stored result
		existing, same, cerr := findChargeCostChange(ctx, tx, requestID,
			`kind = 'undo' AND undo_of = $2::uuid AND old_cost IS NOT DISTINCT FROM $3::numeric`,
			targetID, expected)
		if cerr != nil {
			return cerr
		}
		if existing != nil {
			if !same {
				return errChargeCostIdempotency()
			}
			change, replayed = existing, true
			return nil
		}

		latest, latestMatchesCurrent, cerr := latestChargeCostChange(ctx, tx, target.CarID, target.ChargeID)
		if cerr != nil {
			return cerr
		}
		if latest == nil || latest.RequestID != target.RequestID {
			return &chargeCostError{status: http.StatusConflict, code: "not_latest_change", message: "only the latest change of a charge can be undone"}
		}
		if target.Currency != s.cfg.currency {
			return errChargeCostInvalid("invalid_currency", "change was made in "+target.Currency+", configured currency is "+s.cfg.currency)
		}
		if !charge.completed {
			return errChargeCostInvalid("charge_not_completed", "charge is not completed yet")
		}
		if !charge.matchesExpected {
			return errChargeCostConflict(charge.cost)
		}
		if !latestMatchesCurrent {
			// cost was changed outside of this api (e.g. in TeslaMate) after the change
			return &chargeCostError{status: http.StatusConflict, code: "changed_externally", message: "cost was changed outside of this api, undo not possible", withCurrent: true, currentCost: charge.cost}
		}

		if _, err := tx.ExecContext(ctx, `UPDATE charging_processes SET cost = $2::numeric WHERE id = $1`, target.ChargeID, target.OldCost); err != nil {
			return chargeCostDBError(err, "undo cost")
		}
		change, cerr = insertChargeCostChange(ctx, tx, requestID, "undo", target.CarID, target.ChargeID, charge.cost, target.OldCost, s.cfg.currency, &targetID)
		return cerr
	})
	return change, replayed, cerr
}

// inTx runs fn in a transaction and commits only if fn returns no error
func (s *chargeCostService) inTx(ctx context.Context, fn func(tx *sql.Tx) *chargeCostError) *chargeCostError {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return chargeCostDBError(err, "begin transaction")
	}
	defer func() { _ = tx.Rollback() }()

	if cerr := fn(tx); cerr != nil {
		return cerr
	}
	if err := tx.Commit(); err != nil {
		return chargeCostDBError(err, "commit")
	}
	return nil
}

// chargeCostCharge is the locked charging process
type chargeCostCharge struct {
	carID           int64
	completed       bool
	cost            *string
	matchesExpected bool
}

// lockChargeCostCharge locks the charging process row until the transaction ends
func lockChargeCostCharge(ctx context.Context, tx *sql.Tx, chargeID int64, expected *string) (*chargeCostCharge, *chargeCostError) {
	var (
		charge chargeCostCharge
		cost   sql.NullString
	)
	err := tx.QueryRowContext(ctx, `
		SELECT car_id, end_date IS NOT NULL, cost::text, cost IS NOT DISTINCT FROM $2::numeric
		FROM charging_processes
		WHERE id = $1
		FOR UPDATE`, chargeID, expected).Scan(&charge.carID, &charge.completed, &cost, &charge.matchesExpected)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errChargeCostNotFound()
	}
	if err != nil {
		return nil, chargeCostDBError(err, "lock charge")
	}
	charge.cost = chargeCostNullString(cost)
	return &charge, nil
}

// findChargeCostChange loads an audit entry by request_id
// match is an sql condition on the entry using $2.. as args, the result is returned as second value
func findChargeCostChange(ctx context.Context, q chargeCostQuerier, requestID, match string, args ...any) (*chargeCostChange, bool, *chargeCostError) {
	var matches bool
	row := q.QueryRowContext(ctx, `SELECT `+chargeCostChangeColumns+`, COALESCE((`+match+`), false)
		FROM `+chargeCostAuditTable+`
		WHERE request_id = $1::uuid`, append([]any{requestID}, args...)...)
	change, err := scanChargeCostChange(row.Scan, &matches)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, chargeCostDBError(err, "load audit entry")
	}
	return change, matches, nil
}

// latestChargeCostChange loads the latest audit entry of a charging process
// the second value tells if its new_cost still equals the current cost
func latestChargeCostChange(ctx context.Context, q chargeCostQuerier, carID, chargeID int64) (*chargeCostChange, bool, *chargeCostError) {
	var matchesCurrent bool
	row := q.QueryRowContext(ctx, `SELECT `+chargeCostChangeColumns+`,
			new_cost IS NOT DISTINCT FROM (SELECT cost FROM charging_processes WHERE id = $2)
		FROM `+chargeCostAuditTable+`
		WHERE car_id = $1 AND charge_id = $2
		ORDER BY id DESC
		LIMIT 1`, carID, chargeID)
	change, err := scanChargeCostChange(row.Scan, &matchesCurrent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, chargeCostDBError(err, "load latest audit entry")
	}
	return change, matchesCurrent, nil
}

// insertChargeCostChange writes the audit entry, an error rolls back the cost change
func insertChargeCostChange(ctx context.Context, tx *sql.Tx, requestID, kind string, carID, chargeID int64, oldCost, newCost *string, currency string, undoOf *string) (*chargeCostChange, *chargeCostError) {
	row := tx.QueryRowContext(ctx, `
		INSERT INTO `+chargeCostAuditTable+` (request_id, kind, car_id, charge_id, old_cost, new_cost, currency, undo_of)
		VALUES ($1::uuid, $2, $3, $4, $5::numeric, $6::numeric, $7, $8::uuid)
		RETURNING `+chargeCostChangeColumns, requestID, kind, carID, chargeID, oldCost, newCost, currency, undoOf)
	change, err := scanChargeCostChange(row.Scan)
	if err != nil {
		return nil, chargeCostDBError(err, "insert audit entry")
	}
	return change, nil
}

// scanChargeCostChange scans the chargeCostChangeColumns and optional extra columns
func scanChargeCostChange(scan func(dest ...any) error, extra ...any) (*chargeCostChange, error) {
	var (
		change                   chargeCostChange
		oldCost, newCost, undoOf sql.NullString
		createdAt                time.Time
	)
	dest := append([]any{&change.RequestID, &change.Kind, &change.CarID, &change.ChargeID, &oldCost, &newCost, &change.Currency, &undoOf, &createdAt}, extra...)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	change.OldCost = chargeCostNullString(oldCost)
	change.NewCost = chargeCostNullString(newCost)
	change.UndoOf = chargeCostNullString(undoOf)
	change.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	return &change, nil
}

// checkChargeCostSchema refuses to work if TeslaMate's table differs from what this code expects
// or if the audit table has not been created by the operator
func checkChargeCostSchema(ctx context.Context, q chargeCostQuerier) *chargeCostError {
	rows, err := q.QueryContext(ctx, `
		SELECT a.attname, t.typname, a.atttypmod
		FROM pg_catalog.pg_attribute a
		JOIN pg_catalog.pg_type t ON t.oid = a.atttypid
		WHERE a.attrelid = to_regclass('charging_processes')
			AND a.attnum > 0
			AND NOT a.attisdropped
			AND a.attname IN ('id', 'car_id', 'cost', 'end_date')`)
	if err != nil {
		return chargeCostDBError(err, "check schema")
	}
	defer func() { _ = rows.Close() }()

	supported := 0
	for rows.Next() {
		var (
			name, typeName string
			typeMod        int
		)
		if err := rows.Scan(&name, &typeName, &typeMod); err != nil {
			return chargeCostDBError(err, "check schema")
		}
		if chargeCostColumnSupported(name, typeName, typeMod) {
			supported++
		}
	}
	if err := rows.Err(); err != nil {
		return chargeCostDBError(err, "check schema")
	}
	if supported != 4 {
		return &chargeCostError{status: http.StatusServiceUnavailable, code: "schema_unsupported", message: "TeslaMate database schema is not supported by this version of the api, nothing was changed"}
	}

	var auditExists bool
	if err := q.QueryRowContext(ctx, `SELECT to_regclass('`+chargeCostAuditTable+`') IS NOT NULL`).Scan(&auditExists); err != nil {
		return chargeCostDBError(err, "check audit table")
	}
	if !auditExists {
		return &chargeCostError{status: http.StatusServiceUnavailable, code: "audit_unavailable", message: chargeCostAuditSetupHint}
	}
	return nil
}

// chargeCostColumnSupported checks the type of the charging_processes columns used here
func chargeCostColumnSupported(name, typeName string, typeMod int) bool {
	switch name {
	case "id", "car_id":
		return typeName == "int2" || typeName == "int4" || typeName == "int8"
	case "end_date":
		return typeName == "timestamp" || typeName == "timestamptz"
	case "cost":
		// numeric without precision or with a scale of at least 2 (e.g. numeric(6,2), TeslaMate 4.4.0: numeric(14,2))
		return typeName == "numeric" && (typeMod == -1 || (typeMod-4)&0xffff >= 2)
	}
	return false
}

// chargeCostDBError maps database errors to responses without sql details, the details are logged
func chargeCostDBError(err error, stage string) *chargeCostError {
	log.Printf("[error] %s - database error (%s): %v", chargeCostHandlerLogLabel, stage, err)

	if errors.Is(err, context.DeadlineExceeded) {
		return &chargeCostError{status: http.StatusServiceUnavailable, code: "database_unavailable", message: "database timeout, nothing was changed"}
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		switch {
		case pqErr.Code == "22003": // numeric_value_out_of_range
			return errChargeCostInvalid("invalid_amount", "cost is too large for TeslaMate's cost column")
		case pqErr.Code == "23505": // unique_violation on request_id
			return errChargeCostIdempotency()
		case pqErr.Code == "42P01" || pqErr.Code == "3F000" || pqErr.Code == "42501": // undefined_table, invalid_schema_name, insufficient_privilege
			return &chargeCostError{status: http.StatusServiceUnavailable, code: "audit_unavailable", message: chargeCostAuditSetupHint + " (or the database role lacks privileges), nothing was changed"}
		case stage == "insert audit entry":
			return &chargeCostError{status: http.StatusServiceUnavailable, code: "audit_unavailable", message: "writing the audit entry failed, nothing was changed"}
		}
	}
	return &chargeCostError{status: http.StatusServiceUnavailable, code: "database_unavailable", message: "database error, nothing was changed"}
}

func errChargeCostIdempotency() *chargeCostError {
	return &chargeCostError{status: http.StatusConflict, code: "request_id_reused", message: "request_id was already used with different data"}
}

func errChargeCostConflict(current *string) *chargeCostError {
	return &chargeCostError{status: http.StatusConflict, code: "cost_conflict", message: "expected_cost does not match the current cost", withCurrent: true, currentCost: current}
}

func chargeCostNullString(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}
