package main

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

const (
	testChargeCostToken     = "test-write-token-0123456789abcdefghijkl"
	testChargeCostRequestID = "4f9c1f7e-3b8a-4c55-9d2e-0a1b2c3d4e5f"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// newTestChargeCostEngine returns a router with only the charge cost routes, db may be nil for validation tests
func newTestChargeCostEngine(s *chargeCostService) *gin.Engine {
	r := gin.New()
	r.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{"code": "PAGE_NOT_FOUND", "message": "Page not found"})
	})
	registerChargeCostRoutes(r.Group("/api/v1"), s)
	return r
}

func newTestChargeCostService() *chargeCostService {
	return &chargeCostService{cfg: chargeCostConfig{
		tokenHash:    sha256.Sum256([]byte(testChargeCostToken)),
		currency:     "EUR",
		teslamateURL: "https://teslamate.example",
	}}
}

type testChargeCostRequest struct {
	method, path, body, token, contentType string
}

func doChargeCostRequest(t *testing.T, r *gin.Engine, req testChargeCostRequest) *httptest.ResponseRecorder {
	t.Helper()
	httpReq := httptest.NewRequest(req.method, req.path, strings.NewReader(req.body))
	token := req.token
	if token == "" {
		token = testChargeCostToken
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	contentType := req.contentType
	if contentType == "" {
		contentType = "application/json"
	}
	httpReq.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httpReq)
	return w
}

func decodeChargeCostResponse(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not json: %v (%s)", err, w.Body.String())
	}
	return body
}

func TestNormalizeChargeCostAmount(t *testing.T) {
	valid := map[string]string{
		"0":         "0.00",
		"0.0":       "0.00",
		"0.5":       "0.50",
		"12.34":     "12.34",
		"5":         "5.00",
		"999999.99": "999999.99",
		"100000":    "100000.00",
	}
	for in, want := range valid {
		got, ok := normalizeChargeCostAmount(in)
		if !ok || got != want {
			t.Errorf("normalizeChargeCostAmount(%q) = %q, %t; want %q", in, got, ok, want)
		}
	}

	invalid := []string{"", "-1", "01", "1.", ".5", "1.234", "1000000", "1e3", "1,5", "1 000", "1_000", " 1", "1 ", "+1", "NaN", "Infinity", "0x10", "١٢"}
	for _, in := range invalid {
		if got, ok := normalizeChargeCostAmount(in); ok {
			t.Errorf("normalizeChargeCostAmount(%q) = %q; want invalid", in, got)
		}
	}
}

func TestNormalizeChargeCostRequestID(t *testing.T) {
	if got, ok := normalizeChargeCostRequestID("4F9C1F7E-3B8A-4C55-9D2E-0A1B2C3D4E5F"); !ok || got != testChargeCostRequestID {
		t.Errorf("upper case uuid not normalized: %q %t", got, ok)
	}
	for _, in := range []string{"", "1", "4f9c1f7e3b8a4c559d2e0a1b2c3d4e5f", "{4f9c1f7e-3b8a-4c55-9d2e-0a1b2c3d4e5f}", "4f9c1f7e-3b8a-4c55-9d2e-0a1b2c3d4e5g"} {
		if _, ok := normalizeChargeCostRequestID(in); ok {
			t.Errorf("normalizeChargeCostRequestID(%q) accepted", in)
		}
	}
}

func TestChargeCostColumnSupported(t *testing.T) {
	// atttypmod of numeric(p,s) is ((p << 16) | s) + 4
	numeric := func(p, s int) int { return (p<<16 | s) + 4 }
	cases := []struct {
		name, typeName string
		typeMod        int
		want           bool
	}{
		{"id", "int4", -1, true},
		{"id", "int8", -1, true},
		{"id", "text", -1, false},
		{"car_id", "int2", -1, true},
		{"end_date", "timestamp", -1, true},
		{"end_date", "timestamptz", -1, true},
		{"end_date", "date", -1, false},
		{"cost", "numeric", numeric(6, 2), true},
		{"cost", "numeric", numeric(10, 4), true},
		{"cost", "numeric", -1, true},
		{"cost", "numeric", numeric(6, 1), false},
		{"cost", "numeric", numeric(6, 0), false},
		{"cost", "float8", -1, false},
		{"cost", "text", -1, false},
		{"other", "int4", -1, false},
	}
	for _, tc := range cases {
		if got := chargeCostColumnSupported(tc.name, tc.typeName, tc.typeMod); got != tc.want {
			t.Errorf("chargeCostColumnSupported(%s, %s, %d) = %t; want %t", tc.name, tc.typeName, tc.typeMod, got, tc.want)
		}
	}
}

func TestLoadChargeCostConfig(t *testing.T) {
	setValid := func(t *testing.T) {
		t.Setenv("ENABLE_COST_WRITE", "true")
		t.Setenv("COST_WRITE_TOKEN", testChargeCostToken)
		t.Setenv("COST_CURRENCY", "EUR")
		t.Setenv("COST_TESLAMATE_URL", "https://teslamate.example/")
		t.Setenv("API_TOKEN", "")
		t.Setenv("COST_DATABASE_DSN", "")
	}

	t.Run("disabled by default", func(t *testing.T) {
		t.Setenv("ENABLE_COST_WRITE", "")
		t.Setenv("COST_WRITE_TOKEN", testChargeCostToken)
		cfg, problems := loadChargeCostConfig()
		if cfg != nil || problems != nil {
			t.Fatalf("expected disabled, got %v %v", cfg, problems)
		}
	})

	t.Run("valid", func(t *testing.T) {
		setValid(t)
		cfg, problems := loadChargeCostConfig()
		if cfg == nil || len(problems) > 0 {
			t.Fatalf("expected valid config, got problems %v", problems)
		}
		if cfg.currency != "EUR" || cfg.teslamateURL != "https://teslamate.example" || cfg.tokenHash != sha256.Sum256([]byte(testChargeCostToken)) {
			t.Fatalf("unexpected config %+v", cfg)
		}
	})

	invalid := map[string][2]string{
		"no token":             {"COST_WRITE_TOKEN", ""},
		"short token":          {"COST_WRITE_TOKEN", "too-short-token"},
		"token with space":     {"COST_WRITE_TOKEN", "test write token 0123456789abcdefghijkl"},
		"token equals read":    {"API_TOKEN", testChargeCostToken},
		"no currency":          {"COST_CURRENCY", ""},
		"lower case currency":  {"COST_CURRENCY", "eur"},
		"currency too long":    {"COST_CURRENCY", "EURO"},
		"no teslamate url":     {"COST_TESLAMATE_URL", ""},
		"url without scheme":   {"COST_TESLAMATE_URL", "teslamate.example"},
		"url with credentials": {"COST_TESLAMATE_URL", "https://user:pass@teslamate.example"},
		"url with query":       {"COST_TESLAMATE_URL", "https://teslamate.example/?x=1"},
		"ftp url":              {"COST_TESLAMATE_URL", "ftp://teslamate.example"},
	}
	for name, env := range invalid {
		t.Run(name, func(t *testing.T) {
			setValid(t)
			t.Setenv(env[0], env[1])
			cfg, problems := loadChargeCostConfig()
			if cfg != nil || len(problems) == 0 {
				t.Fatalf("expected problems for %s=%q", env[0], env[1])
			}
			for _, p := range problems {
				if strings.Contains(p, testChargeCostToken) {
					t.Fatalf("problem message contains token: %s", p)
				}
			}
		})
	}
}

func TestChargeCostRoutesNotRegisteredWhenDisabled(t *testing.T) {
	r := newTestChargeCostEngine(nil)
	for _, req := range []testChargeCostRequest{
		{method: http.MethodGet, path: "/api/v1/cars/1/charges/1/cost"},
		{method: http.MethodPut, path: "/api/v1/cars/1/charges/1/cost", body: `{}`},
		{method: http.MethodPost, path: "/api/v1/changes/" + testChargeCostRequestID + "/undo", body: `{}`},
	} {
		w := doChargeCostRequest(t, r, req)
		if w.Code != http.StatusNotFound || decodeChargeCostResponse(t, w)["code"] != "PAGE_NOT_FOUND" {
			t.Errorf("%s %s: got %d %s; want default 404", req.method, req.path, w.Code, w.Body.String())
		}
	}
}

func TestChargeCostAuthorization(t *testing.T) {
	r := newTestChargeCostEngine(newTestChargeCostService())
	path := "/api/v1/cars/1/charges/1/cost"

	cases := []struct {
		name   string
		header string
		path   string
		want   int
	}{
		{"missing header", "", path, http.StatusUnauthorized},
		{"token in query only", "", path + "?token=" + testChargeCostToken, http.StatusUnauthorized},
		{"basic auth", "Basic " + testChargeCostToken, path, http.StatusUnauthorized},
		{"bearer without token", "Bearer ", path, http.StatusUnauthorized},
		{"wrong token", "Bearer " + strings.Repeat("x", 40), path, http.StatusForbidden},
		{"token prefix", "Bearer " + testChargeCostToken[:32], path, http.StatusForbidden},
	}
	for _, tc := range cases {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			req := httptest.NewRequest(method, tc.path, strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Errorf("%s %s: got %d; want %d", tc.name, method, w.Code, tc.want)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("%s %s: Cache-Control missing", tc.name, method)
			}
			if strings.Contains(w.Body.String(), testChargeCostToken) {
				t.Errorf("%s %s: response contains token", tc.name, method)
			}
		}
	}

	// valid token passes authorization, the request then fails validation before touching the database
	w := doChargeCostRequest(t, r, testChargeCostRequest{method: http.MethodPut, path: path, body: `{}`, token: testChargeCostToken})
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("valid token: got %d %s; want 422", w.Code, w.Body.String())
	}
	// scheme is case insensitive
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "bearer "+testChargeCostToken)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("lower case scheme: got %d %s; want 422", w.Code, w.Body.String())
	}
}

func TestChargeCostRequestValidation(t *testing.T) {
	r := newTestChargeCostEngine(newTestChargeCostService())
	put := "/api/v1/cars/1/charges/2/cost"
	undo := "/api/v1/changes/" + testChargeCostRequestID + "/undo"
	validPut := func(replace map[string]string) string {
		fields := map[string]string{"request_id": `"` + testChargeCostRequestID + `"`, "expected_cost": "null", "cost": `"12.34"`, "currency": `"EUR"`}
		for k, v := range replace {
			fields[k] = v
		}
		parts := []string{}
		for k, v := range fields {
			if v != "-" {
				parts = append(parts, `"`+k+`":`+v)
			}
		}
		return "{" + strings.Join(parts, ",") + "}"
	}

	cases := []struct {
		name        string
		method      string
		path        string
		body        string
		contentType string
		wantStatus  int
		wantCode    string
	}{
		{"wrong content type", http.MethodPut, put, validPut(nil), "text/plain", http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"form content type", http.MethodPut, put, "cost=1", "application/x-www-form-urlencoded", http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"body too large", http.MethodPut, put, `{"request_id":"` + strings.Repeat("a", chargeCostMaxBodyBytes) + `"}`, "", http.StatusRequestEntityTooLarge, "body_too_large"},
		{"unknown field", http.MethodPut, put, validPut(map[string]string{"note": `"x"`}), "", http.StatusBadRequest, "invalid_request"},
		{"trailing data", http.MethodPut, put, validPut(nil) + `{}`, "", http.StatusBadRequest, "invalid_request"},
		{"not json", http.MethodPut, put, `cost=1`, "", http.StatusBadRequest, "invalid_request"},
		{"json array", http.MethodPut, put, `[]`, "", http.StatusBadRequest, "invalid_request"},
		{"missing request_id", http.MethodPut, put, validPut(map[string]string{"request_id": "-"}), "", http.StatusUnprocessableEntity, "invalid_request_id"},
		{"invalid request_id", http.MethodPut, put, validPut(map[string]string{"request_id": `"abc"`}), "", http.StatusUnprocessableEntity, "invalid_request_id"},
		{"missing expected_cost", http.MethodPut, put, validPut(map[string]string{"expected_cost": "-"}), "", http.StatusUnprocessableEntity, "invalid_request"},
		{"invalid expected_cost", http.MethodPut, put, validPut(map[string]string{"expected_cost": `"1.234"`}), "", http.StatusUnprocessableEntity, "invalid_amount"},
		{"missing cost", http.MethodPut, put, validPut(map[string]string{"cost": "-"}), "", http.StatusUnprocessableEntity, "invalid_amount"},
		{"null cost", http.MethodPut, put, validPut(map[string]string{"cost": "null"}), "", http.StatusUnprocessableEntity, "invalid_amount"},
		{"float cost", http.MethodPut, put, validPut(map[string]string{"cost": "12.34"}), "", http.StatusUnprocessableEntity, "invalid_amount"},
		{"exponent cost", http.MethodPut, put, validPut(map[string]string{"cost": `"1e3"`}), "", http.StatusUnprocessableEntity, "invalid_amount"},
		{"negative cost", http.MethodPut, put, validPut(map[string]string{"cost": `"-1"`}), "", http.StatusUnprocessableEntity, "invalid_amount"},
		{"too many decimals", http.MethodPut, put, validPut(map[string]string{"cost": `"1.001"`}), "", http.StatusUnprocessableEntity, "invalid_amount"},
		{"too large cost", http.MethodPut, put, validPut(map[string]string{"cost": `"1000000"`}), "", http.StatusUnprocessableEntity, "invalid_amount"},
		{"thousands separator", http.MethodPut, put, validPut(map[string]string{"cost": `"1,000"`}), "", http.StatusUnprocessableEntity, "invalid_amount"},
		{"other currency", http.MethodPut, put, validPut(map[string]string{"currency": `"USD"`}), "", http.StatusUnprocessableEntity, "invalid_currency"},
		{"lower case currency", http.MethodPut, put, validPut(map[string]string{"currency": `"eur"`}), "", http.StatusUnprocessableEntity, "invalid_currency"},
		{"missing currency", http.MethodPut, put, validPut(map[string]string{"currency": "-"}), "", http.StatusUnprocessableEntity, "invalid_currency"},
		{"car id zero", http.MethodPut, "/api/v1/cars/0/charges/2/cost", validPut(nil), "", http.StatusNotFound, "charge_not_found"},
		{"car id text", http.MethodPut, "/api/v1/cars/abc/charges/2/cost", validPut(nil), "", http.StatusNotFound, "charge_not_found"},
		{"charge id leading zero", http.MethodPut, "/api/v1/cars/1/charges/02/cost", validPut(nil), "", http.StatusNotFound, "charge_not_found"},
		{"charge id negative", http.MethodGet, "/api/v1/cars/1/charges/-2/cost", "", "", http.StatusNotFound, "charge_not_found"},
		{"undo invalid change id", http.MethodPost, "/api/v1/changes/abc/undo", `{"request_id":"` + testChargeCostRequestID + `","expected_cost":null}`, "", http.StatusNotFound, "change_not_found"},
		{"undo unknown field", http.MethodPost, undo, `{"request_id":"` + testChargeCostRequestID + `","expected_cost":null,"cost":"1"}`, "", http.StatusBadRequest, "invalid_request"},
		{"undo missing expected_cost", http.MethodPost, undo, `{"request_id":"` + testChargeCostRequestID + `"}`, "", http.StatusUnprocessableEntity, "invalid_request"},
		{"undo invalid request_id", http.MethodPost, undo, `{"request_id":"x","expected_cost":null}`, "", http.StatusUnprocessableEntity, "invalid_request_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doChargeCostRequest(t, r, testChargeCostRequest{method: tc.method, path: tc.path, body: tc.body, contentType: tc.contentType})
			body := decodeChargeCostResponse(t, w)
			if w.Code != tc.wantStatus || body["code"] != tc.wantCode {
				t.Fatalf("got %d %v; want %d %s", w.Code, body, tc.wantStatus, tc.wantCode)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("Cache-Control missing")
			}
		})
	}
}

func TestChargeCostNoCORS(t *testing.T) {
	r := newTestChargeCostEngine(newTestChargeCostService())
	req := httptest.NewRequest(http.MethodOptions, "/api/v1/cars/1/charges/1/cost", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Access-Control-Request-Method", "PUT")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("CORS header set on preflight")
	}
}
