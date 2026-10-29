package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
)

// integration tests against a disposable PostgreSQL database with a synthetic TeslaMate-like schema
// they only run if TESLAMATEAPI_TEST_DATABASE_DSN is set, e.g.
//   docker run --rm -d -p 55432:5432 -e POSTGRES_USER=teslamate -e POSTGRES_PASSWORD=test postgres:17
//   TESLAMATEAPI_TEST_DATABASE_DSN='postgres://teslamate:test@localhost:55432/teslamate?sslmode=disable' go test ./src/
// WARNING: the tests drop and recreate charging_processes and teslamateapi_audit, never point them to a real TeslaMate database

const chargeCostTestSchema = `
	DROP SCHEMA IF EXISTS teslamateapi_audit CASCADE;
	DROP TABLE IF EXISTS charging_processes;
	CREATE TABLE charging_processes (
		id serial PRIMARY KEY,
		start_date timestamp NOT NULL,
		end_date timestamp,
		charge_energy_added numeric(8,2),
		start_battery_level smallint,
		end_battery_level smallint,
		duration_min smallint,
		car_id smallint NOT NULL,
		cost numeric(6,2)
	);
	INSERT INTO charging_processes (id, start_date, end_date, charge_energy_added, car_id, cost) VALUES
		(1, '2026-01-01 10:00', '2026-01-01 11:00', 20.00, 1, NULL),
		(2, '2026-01-02 10:00', '2026-01-02 11:00', 10.00, 1, 0),
		(3, '2026-01-03 10:00', NULL,                5.00, 1, NULL),
		(4, '2026-01-04 10:00', '2026-01-04 11:00', 30.00, 2, 5.00);`

type chargeCostTestEnv struct {
	t   *testing.T
	db  *sql.DB
	dsn string
	r   *gin.Engine
}

func setupChargeCostIntegration(t *testing.T) *chargeCostTestEnv {
	t.Helper()
	dsn := os.Getenv("TESLAMATEAPI_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("TESLAMATEAPI_TEST_DATABASE_DSN not set")
	}
	testDB, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testDB.Close() })

	env := &chargeCostTestEnv{t: t, db: testDB, dsn: dsn}
	env.exec(chargeCostTestSchema)
	env.execFile("../sql/charge_cost_audit.sql")
	env.r = env.engine(testDB)
	return env
}

func (e *chargeCostTestEnv) engine(costDB *sql.DB) *gin.Engine {
	s := newTestChargeCostService()
	s.db = costDB
	return newTestChargeCostEngine(s)
}

func (e *chargeCostTestEnv) exec(query string, args ...any) {
	e.t.Helper()
	if _, err := e.db.Exec(query, args...); err != nil {
		e.t.Fatalf("exec failed: %v", err)
	}
}

func (e *chargeCostTestEnv) execFile(path string) {
	e.t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		e.t.Fatal(err)
	}
	e.exec(string(content))
}

// cost returns charging_processes.cost as text, nil for NULL
func (e *chargeCostTestEnv) cost(chargeID int) *string {
	e.t.Helper()
	var cost sql.NullString
	if err := e.db.QueryRow(`SELECT cost::text FROM charging_processes WHERE id = $1`, chargeID).Scan(&cost); err != nil {
		e.t.Fatal(err)
	}
	return chargeCostNullString(cost)
}

func (e *chargeCostTestEnv) auditCount() int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM teslamateapi_audit.charge_cost_changes`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

type chargeCostTestResponse struct {
	status   int
	header   http.Header
	raw      string
	data     map[string]any
	code     string
	current  any
	hasCurr  bool
	replayed bool
}

func (e *chargeCostTestEnv) do(r *gin.Engine, method, path, body string) chargeCostTestResponse {
	e.t.Helper()
	w := doChargeCostRequest(e.t, r, testChargeCostRequest{method: method, path: path, body: body})
	resp := chargeCostTestResponse{status: w.Code, header: w.Header(), raw: w.Body.String(), replayed: w.Header().Get("Idempotent-Replayed") == "true"}
	var parsed map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &parsed); err != nil {
		e.t.Fatalf("invalid json response %q: %v", w.Body.String(), err)
	}
	if data, ok := parsed["data"].(map[string]any); ok {
		resp.data = data
	}
	resp.code, _ = parsed["code"].(string)
	resp.current, resp.hasCurr = parsed["current_cost"]
	return resp
}

func (e *chargeCostTestEnv) get(carID, chargeID int) chargeCostTestResponse {
	e.t.Helper()
	return e.do(e.r, http.MethodGet, fmt.Sprintf("/api/v1/cars/%d/charges/%d/cost", carID, chargeID), "")
}

func (e *chargeCostTestEnv) put(carID, chargeID int, requestID string, expected, cost string) chargeCostTestResponse {
	e.t.Helper()
	return e.do(e.r, http.MethodPut, fmt.Sprintf("/api/v1/cars/%d/charges/%d/cost", carID, chargeID),
		fmt.Sprintf(`{"request_id":%q,"expected_cost":%s,"cost":%q,"currency":"EUR"}`, requestID, expected, cost))
}

func (e *chargeCostTestEnv) undo(changeID, requestID, expected string) chargeCostTestResponse {
	e.t.Helper()
	return e.do(e.r, http.MethodPost, "/api/v1/changes/"+changeID+"/undo",
		fmt.Sprintf(`{"request_id":%q,"expected_cost":%s}`, requestID, expected))
}

func newTestUUID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func expectStatus(t *testing.T, resp chargeCostTestResponse, status int, code string) {
	t.Helper()
	if resp.status != status || resp.code != code {
		t.Fatalf("got %d %q (%s); want %d %q", resp.status, resp.code, resp.raw, status, code)
	}
}

func expectCost(t *testing.T, got *string, want any) {
	t.Helper()
	switch w := want.(type) {
	case nil:
		if got != nil {
			t.Fatalf("cost = %q; want NULL", *got)
		}
	case string:
		if got == nil || *got != w {
			t.Fatalf("cost = %v; want %q", got, w)
		}
	}
}

func TestChargeCostIntegrationRead(t *testing.T) {
	env := setupChargeCostIntegration(t)

	resp := env.get(1, 1)
	expectStatus(t, resp, http.StatusOK, "")
	want := map[string]any{"car_id": 1.0, "charge_id": 1.0, "cost": nil, "currency": "EUR", "completed": true, "confirmed": nil, "teslamate_url": "https://teslamate.example"}
	if !reflect.DeepEqual(resp.data, want) {
		t.Fatalf("GET null cost = %v; want %v", resp.data, want)
	}
	if resp.header.Get("Cache-Control") != "no-store" {
		t.Fatal("Cache-Control missing")
	}

	// 0 is a free charge and has to stay distinguishable from null
	resp = env.get(1, 2)
	expectStatus(t, resp, http.StatusOK, "")
	if resp.data["cost"] != "0.00" {
		t.Fatalf("GET zero cost = %v; want \"0.00\"", resp.data["cost"])
	}

	resp = env.get(1, 3)
	expectStatus(t, resp, http.StatusOK, "")
	if resp.data["completed"] != false {
		t.Fatalf("ongoing charge completed = %v", resp.data["completed"])
	}

	expectStatus(t, env.get(2, 1), http.StatusNotFound, "charge_not_found")
	expectStatus(t, env.get(1, 4), http.StatusNotFound, "charge_not_found")
	expectStatus(t, env.get(1, 99), http.StatusNotFound, "charge_not_found")
}

func TestChargeCostIntegrationSetAndReplay(t *testing.T) {
	env := setupChargeCostIntegration(t)
	id := newTestUUID(t)

	first := env.put(1, 1, id, "null", "12.3")
	expectStatus(t, first, http.StatusOK, "")
	if first.replayed {
		t.Fatal("first request marked as replayed")
	}
	if first.data["request_id"] != id || first.data["kind"] != "set" || first.data["old_cost"] != nil || first.data["new_cost"] != "12.30" ||
		first.data["currency"] != "EUR" || first.data["undo_of"] != nil || first.data["car_id"] != 1.0 || first.data["charge_id"] != 1.0 {
		t.Fatalf("unexpected change %v", first.data)
	}
	expectCost(t, env.cost(1), "12.30")

	status := env.get(1, 1)
	if status.data["cost"] != "12.30" || !reflect.DeepEqual(status.data["confirmed"], first.data) {
		t.Fatalf("GET after set = %v; want confirmed %v", status.data, first.data)
	}

	// identical repetition: same answer, nothing written again
	again := env.put(1, 1, id, "null", "12.3")
	expectStatus(t, again, http.StatusOK, "")
	if !again.replayed || again.raw != first.raw {
		t.Fatalf("replay = %s (replayed %t); want %s", again.raw, again.replayed, first.raw)
	}
	// numerically equal amounts are the same request
	expectStatus(t, env.put(1, 1, id, "null", "12.30"), http.StatusOK, "")
	if n := env.auditCount(); n != 1 {
		t.Fatalf("audit entries = %d; want 1", n)
	}

	// same request_id with other data
	expectStatus(t, env.put(1, 1, id, "null", "13"), http.StatusConflict, "request_id_reused")
	expectStatus(t, env.put(1, 1, id, `"12.30"`, "12.3"), http.StatusConflict, "request_id_reused")
	expectStatus(t, env.put(1, 2, id, `"0"`, "12.3"), http.StatusConflict, "request_id_reused")
	expectCost(t, env.cost(1), "12.30")
	expectCost(t, env.cost(2), "0.00")

	// expected_cost is compared numerically: "0" matches 0.00
	resp := env.put(1, 2, newTestUUID(t), `"0"`, "0.5")
	expectStatus(t, resp, http.StatusOK, "")
	if resp.data["old_cost"] != "0.00" || resp.data["new_cost"] != "0.50" {
		t.Fatalf("unexpected change %v", resp.data)
	}
	// setting 0 is allowed (free charge)
	expectStatus(t, env.put(1, 1, newTestUUID(t), `"12.30"`, "0"), http.StatusOK, "")
	expectCost(t, env.cost(1), "0.00")
}

func TestChargeCostIntegrationRejections(t *testing.T) {
	env := setupChargeCostIntegration(t)

	expectStatus(t, env.put(1, 3, newTestUUID(t), "null", "1"), http.StatusUnprocessableEntity, "charge_not_completed")
	expectCost(t, env.cost(3), nil)

	expectStatus(t, env.put(2, 1, newTestUUID(t), "null", "1"), http.StatusNotFound, "charge_not_found")
	expectStatus(t, env.put(1, 99, newTestUUID(t), "null", "1"), http.StatusNotFound, "charge_not_found")
	expectCost(t, env.cost(1), nil)

	conflict := env.put(1, 2, newTestUUID(t), "null", "1")
	expectStatus(t, conflict, http.StatusConflict, "cost_conflict")
	if !conflict.hasCurr || conflict.current != "0.00" {
		t.Fatalf("conflict current_cost = %v", conflict.current)
	}
	conflict = env.put(1, 1, newTestUUID(t), `"0"`, "1")
	expectStatus(t, conflict, http.StatusConflict, "cost_conflict")
	if !conflict.hasCurr || conflict.current != nil {
		t.Fatalf("conflict current_cost = %v; want null", conflict.current)
	}

	// TeslaMate's column is numeric(6,2), the api accepts up to 999999.99
	expectStatus(t, env.put(1, 1, newTestUUID(t), "null", "100000"), http.StatusUnprocessableEntity, "invalid_amount")
	expectCost(t, env.cost(1), nil)

	if n := env.auditCount(); n != 0 {
		t.Fatalf("audit entries = %d; want 0", n)
	}
}

func TestChargeCostIntegrationUndo(t *testing.T) {
	env := setupChargeCostIntegration(t)

	a, b := newTestUUID(t), newTestUUID(t)
	expectStatus(t, env.put(1, 1, a, "null", "10"), http.StatusOK, "")
	expectStatus(t, env.put(1, 1, b, `"10"`, "20"), http.StatusOK, "")

	// only the latest change can be undone
	expectStatus(t, env.undo(a, newTestUUID(t), `"20"`), http.StatusConflict, "not_latest_change")
	// expected_cost has to match
	conflict := env.undo(b, newTestUUID(t), `"10"`)
	expectStatus(t, conflict, http.StatusConflict, "cost_conflict")
	if conflict.current != "20.00" {
		t.Fatalf("conflict current_cost = %v", conflict.current)
	}
	expectStatus(t, env.undo(newTestUUID(t), newTestUUID(t), `"20"`), http.StatusNotFound, "change_not_found")

	c := newTestUUID(t)
	undone := env.undo(b, c, `"20"`)
	expectStatus(t, undone, http.StatusOK, "")
	if undone.data["kind"] != "undo" || undone.data["undo_of"] != b || undone.data["old_cost"] != "20.00" || undone.data["new_cost"] != "10.00" {
		t.Fatalf("unexpected undo %v", undone.data)
	}
	expectCost(t, env.cost(1), "10.00")

	// identical repetition of the undo
	again := env.undo(b, c, `"20"`)
	expectStatus(t, again, http.StatusOK, "")
	if !again.replayed || again.raw != undone.raw {
		t.Fatalf("undo replay = %s; want %s", again.raw, undone.raw)
	}
	expectStatus(t, env.undo(b, c, `"10"`), http.StatusConflict, "request_id_reused")
	expectStatus(t, env.undo(a, c, `"20"`), http.StatusConflict, "request_id_reused")
	// the undone change is not the latest any more
	expectStatus(t, env.undo(b, newTestUUID(t), `"10"`), http.StatusConflict, "not_latest_change")

	// undo of an undo restores the undone value again
	expectStatus(t, env.undo(c, newTestUUID(t), `"10"`), http.StatusOK, "")
	expectCost(t, env.cost(1), "20.00")
	if n := env.auditCount(); n != 4 {
		t.Fatalf("audit entries = %d; want 4", n)
	}
}

func TestChargeCostIntegrationUndoToNullAndExternalChange(t *testing.T) {
	env := setupChargeCostIntegration(t)

	a := newTestUUID(t)
	expectStatus(t, env.put(1, 1, a, "null", "10"), http.StatusOK, "")
	u := newTestUUID(t)
	undone := env.undo(a, u, `"10"`)
	expectStatus(t, undone, http.StatusOK, "")
	if undone.data["new_cost"] != nil {
		t.Fatalf("undo new_cost = %v; want null", undone.data["new_cost"])
	}
	expectCost(t, env.cost(1), nil)
	status := env.get(1, 1)
	if status.data["cost"] != nil {
		t.Fatalf("GET after undo cost = %v", status.data["cost"])
	}
	if confirmed, _ := status.data["confirmed"].(map[string]any); confirmed["request_id"] != u {
		t.Fatalf("GET after undo confirmed = %v; want undo %s", status.data["confirmed"], u)
	}

	// cost changed in TeslaMate after the api change
	f := newTestUUID(t)
	expectStatus(t, env.put(2, 4, f, `"5"`, "7"), http.StatusOK, "")
	env.exec(`UPDATE charging_processes SET cost = 8 WHERE id = 4`)
	status = env.get(2, 4)
	if status.data["cost"] != "8.00" || status.data["confirmed"] != nil {
		t.Fatalf("GET after external change = %v; want unconfirmed 8.00", status.data)
	}
	resp := env.undo(f, newTestUUID(t), `"8"`)
	expectStatus(t, resp, http.StatusConflict, "changed_externally")
	expectCost(t, env.cost(4), "8.00")
}

func TestChargeCostIntegrationParallelWriters(t *testing.T) {
	env := setupChargeCostIntegration(t)

	// different requests with the same expected_cost: exactly one wins
	const writers = 10
	results := make([]chargeCostTestResponse, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = env.put(1, 1, newTestUUID(t), "null", fmt.Sprintf("%d.00", i+1))
		}(i)
	}
	wg.Wait()
	ok, conflicts := 0, 0
	var winner string
	for _, r := range results {
		switch r.status {
		case http.StatusOK:
			ok++
			winner, _ = r.data["new_cost"].(string)
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("unexpected response %d %s", r.status, r.raw)
		}
	}
	if ok != 1 || conflicts != writers-1 {
		t.Fatalf("ok=%d conflicts=%d; want 1 and %d", ok, conflicts, writers-1)
	}
	expectCost(t, env.cost(1), winner)

	// the same request sent in parallel is executed once
	id := newTestUUID(t)
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = env.put(1, 2, id, `"0"`, "4.20")
		}(i)
	}
	wg.Wait()
	replays := 0
	for _, r := range results {
		if r.status != http.StatusOK {
			t.Fatalf("parallel identical request: %d %s", r.status, r.raw)
		}
		if r.replayed {
			replays++
		}
	}
	if replays != writers-1 {
		t.Fatalf("replays = %d; want %d", replays, writers-1)
	}
	if n := env.auditCount(); n != 2 {
		t.Fatalf("audit entries = %d; want 2", n)
	}
}

func TestChargeCostIntegrationAuditFailureRollsBack(t *testing.T) {
	env := setupChargeCostIntegration(t)
	env.exec(`
		CREATE FUNCTION teslamateapi_audit.fail_insert() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'synthetic audit failure'; END $$;
		CREATE TRIGGER fail_insert BEFORE INSERT ON teslamateapi_audit.charge_cost_changes
			FOR EACH ROW EXECUTE FUNCTION teslamateapi_audit.fail_insert();`)

	resp := env.put(1, 1, newTestUUID(t), "null", "10")
	expectStatus(t, resp, http.StatusServiceUnavailable, "audit_unavailable")
	if containsAny(resp.raw, "synthetic", "INSERT", "charge_cost_changes (") {
		t.Fatalf("response leaks sql details: %s", resp.raw)
	}
	expectCost(t, env.cost(1), nil)
}

func TestChargeCostIntegrationMissingAuditTable(t *testing.T) {
	env := setupChargeCostIntegration(t)
	env.exec(`DROP SCHEMA teslamateapi_audit CASCADE`)

	expectStatus(t, env.put(1, 1, newTestUUID(t), "null", "10"), http.StatusServiceUnavailable, "audit_unavailable")
	expectStatus(t, env.get(1, 1), http.StatusServiceUnavailable, "audit_unavailable")
	expectStatus(t, env.undo(newTestUUID(t), newTestUUID(t), "null"), http.StatusServiceUnavailable, "audit_unavailable")
	expectCost(t, env.cost(1), nil)

	var exists bool
	if err := env.db.QueryRow(`SELECT to_regclass('teslamateapi_audit.charge_cost_changes') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("api created the audit table on its own (exists=%t, err=%v)", exists, err)
	}
}

func TestChargeCostIntegrationUnsupportedSchema(t *testing.T) {
	changes := map[string]string{
		"cost as text":         `ALTER TABLE charging_processes ALTER COLUMN cost TYPE text`,
		"cost with one digit":  `ALTER TABLE charging_processes ALTER COLUMN cost TYPE numeric(6,1)`,
		"cost as float":        `ALTER TABLE charging_processes ALTER COLUMN cost TYPE double precision`,
		"end_date renamed":     `ALTER TABLE charging_processes RENAME COLUMN end_date TO ended_at`,
		"car_id dropped":       `ALTER TABLE charging_processes DROP COLUMN car_id`,
		"table missing":        `DROP TABLE charging_processes`,
		"end_date as date":     `ALTER TABLE charging_processes ALTER COLUMN end_date TYPE date`,
		"car_id as text":       `ALTER TABLE charging_processes ALTER COLUMN car_id TYPE text`,
		"cost column missing":  `ALTER TABLE charging_processes DROP COLUMN cost`,
		"id renamed":           `ALTER TABLE charging_processes RENAME COLUMN id TO charge_id`,
		"cost wider precision": ``,
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			env := setupChargeCostIntegration(t)
			if change == "" {
				// supported variant: more precision and timestamptz
				env.exec(`ALTER TABLE charging_processes ALTER COLUMN cost TYPE numeric(10,4), ALTER COLUMN end_date TYPE timestamptz`)
				expectStatus(t, env.put(1, 1, newTestUUID(t), "null", "1.5"), http.StatusOK, "")
				return
			}
			env.exec(change)
			expectStatus(t, env.put(1, 1, newTestUUID(t), "null", "10"), http.StatusServiceUnavailable, "schema_unsupported")
			expectStatus(t, env.get(1, 1), http.StatusServiceUnavailable, "schema_unsupported")
			if n := env.auditCount(); n != 0 {
				t.Fatalf("audit entries = %d; want 0", n)
			}
		})
	}
}

func TestChargeCostIntegrationRestrictedRole(t *testing.T) {
	env := setupChargeCostIntegration(t)
	base, err := url.Parse(env.dsn)
	if err != nil || base.Scheme != "postgres" && base.Scheme != "postgresql" {
		t.Skip("TESLAMATEAPI_TEST_DATABASE_DSN has to be an url for this test")
	}
	var canCreateRole bool
	if err := env.db.QueryRow(`SELECT rolsuper OR rolcreaterole FROM pg_roles WHERE rolname = current_user`).Scan(&canCreateRole); err != nil || !canCreateRole {
		t.Skip("test database user cannot create roles")
	}

	env.execFile("../sql/charge_cost_role.sql")
	t.Cleanup(func() {
		_, _ = env.db.Exec(`DROP OWNED BY teslamateapi_cost; DROP ROLE IF EXISTS teslamateapi_cost`)
	})
	password := newTestUUID(t)
	env.exec(`ALTER ROLE teslamateapi_cost PASSWORD '` + password + `'`)

	roleURL := *base
	roleURL.User = url.UserPassword("teslamateapi_cost", password)
	roleDB, err := sql.Open("postgres", roleURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = roleDB.Close() }()

	env.r = env.engine(roleDB)
	a := newTestUUID(t)
	expectStatus(t, env.get(1, 1), http.StatusOK, "")
	expectStatus(t, env.put(1, 1, a, "null", "10"), http.StatusOK, "")
	expectStatus(t, env.put(1, 1, newTestUUID(t), "null", "11"), http.StatusConflict, "cost_conflict")
	expectStatus(t, env.undo(a, newTestUUID(t), `"10"`), http.StatusOK, "")
	expectCost(t, env.cost(1), nil)

	// the role cannot change anything else
	for _, stmt := range []string{
		`UPDATE charging_processes SET car_id = 2 WHERE id = 1`,
		`UPDATE charging_processes SET end_date = NULL WHERE id = 1`,
		`SELECT charge_energy_added FROM charging_processes`,
		`DELETE FROM charging_processes`,
		`INSERT INTO charging_processes (start_date, car_id) VALUES (now(), 1)`,
		`UPDATE teslamateapi_audit.charge_cost_changes SET new_cost = 1`,
		`DELETE FROM teslamateapi_audit.charge_cost_changes`,
		`CREATE TABLE teslamateapi_audit.x (id int)`,
	} {
		if _, err := roleDB.Exec(stmt); err == nil {
			t.Errorf("restricted role was allowed to run: %s", stmt)
		}
	}
}

func containsAny(s string, parts ...string) bool {
	for _, p := range parts {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}
