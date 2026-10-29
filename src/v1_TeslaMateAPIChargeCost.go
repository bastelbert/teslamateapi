package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
)

// charge cost write support
// lets a client store the cost of a completed charging process in TeslaMate (charging_processes.cost)
// every change is written to an audit table in the same transaction, see docs/charge-cost-write.md

const (
	chargeCostMaxBodyBytes    = 4 << 10 // 4 KiB
	chargeCostMinTokenLength  = 32
	chargeCostRequestTimeout  = 10 * time.Second
	chargeCostAuditTable      = "teslamateapi_audit.charge_cost_changes"
	chargeCostAuditSetupHint  = "audit table " + chargeCostAuditTable + " is missing or not accessible, run sql/charge_cost_audit.sql"
	chargeCostHandlerLogLabel = "TeslaMateAPIChargeCostV1"
)

var (
	chargeCostAmountRegex   = regexp.MustCompile(`^(0|[1-9][0-9]{0,5})(\.[0-9]{1,2})?$`)
	chargeCostCurrencyRegex = regexp.MustCompile(`^[A-Z]{3}$`)
	chargeCostUUIDRegex     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	chargeCostTokenRegex    = regexp.MustCompile(`^[\x21-\x7e]+$`) // printable ascii without spaces

	errChargeCostAmountType = errors.New("amount has to be a string or null")
)

// chargeCostConfig holds the settings of the charge cost endpoints (ENABLE_COST_WRITE)
type chargeCostConfig struct {
	tokenHash    [sha256.Size]byte
	currency     string
	teslamateURL string
	dsn          string
}

// chargeCostService serves the charge cost endpoints
type chargeCostService struct {
	cfg chargeCostConfig
	db  *sql.DB
}

// chargeCostError is returned to the client with a real http status code
type chargeCostError struct {
	status      int
	code        string
	message     string
	withCurrent bool    // add current_cost to the response (cost conflicts)
	currentCost *string // current cost of the charging process
}

// optionalAmount keeps track if a json field was sent at all and if it was null
type optionalAmount struct {
	set   bool
	value *string
}

// UnmarshalJSON for optionalAmount, only strings and null are accepted (no floats)
func (a *optionalAmount) UnmarshalJSON(b []byte) error {
	a.set = true
	if string(b) == "null" {
		a.value = nil
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errChargeCostAmountType
	}
	a.value = &s
	return nil
}

// chargeCostSetRequest is the body of PUT /cars/:CarID/charges/:ChargeID/cost
type chargeCostSetRequest struct {
	RequestID    string         `json:"request_id"`
	ExpectedCost optionalAmount `json:"expected_cost"`
	Cost         optionalAmount `json:"cost"`
	Currency     string         `json:"currency"`
}

// chargeCostUndoRequest is the body of POST /changes/:RequestID/undo
type chargeCostUndoRequest struct {
	RequestID    string         `json:"request_id"`
	ExpectedCost optionalAmount `json:"expected_cost"`
}

// chargeCostStatus is the response of GET /cars/:CarID/charges/:ChargeID/cost
type chargeCostStatus struct {
	CarID        int64             `json:"car_id"`
	ChargeID     int64             `json:"charge_id"`
	Cost         *string           `json:"cost"`
	Currency     string            `json:"currency"`
	Completed    bool              `json:"completed"`
	Confirmed    *chargeCostChange `json:"confirmed"`
	TeslaMateURL string            `json:"teslamate_url"`
}

// chargeCostChange is one row of the audit table and the response of PUT and undo
type chargeCostChange struct {
	RequestID string  `json:"request_id"`
	Kind      string  `json:"kind"`
	CarID     int64   `json:"car_id"`
	ChargeID  int64   `json:"charge_id"`
	OldCost   *string `json:"old_cost"`
	NewCost   *string `json:"new_cost"`
	Currency  string  `json:"currency"`
	UndoOf    *string `json:"undo_of"`
	CreatedAt string  `json:"created_at"`
}

// initChargeCostWrite registers the charge cost endpoints if ENABLE_COST_WRITE is true and the configuration is valid
// in every other case no route is registered and the api behaves exactly as without this feature
func initChargeCostWrite(v1 *gin.RouterGroup) {
	cfg, problems := loadChargeCostConfig()
	if len(problems) > 0 {
		for _, problem := range problems {
			log.Println("[error] initChargeCostWrite - " + problem)
		}
		log.Println("[error] initChargeCostWrite - charge cost endpoints are NOT enabled.")
		return
	}
	if cfg == nil {
		return
	}

	// use the restricted database role if configured, otherwise the normal connection
	costDB := db
	if cfg.dsn != "" {
		var err error
		costDB, err = sql.Open("postgres", cfg.dsn)
		if err != nil {
			// not logging err since it could contain parts of the dsn
			log.Println("[error] initChargeCostWrite - COST_DATABASE_DSN is invalid.. charge cost endpoints are NOT enabled.")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), chargeCostRequestTimeout)
		defer cancel()
		if err := costDB.PingContext(ctx); err != nil {
			log.Println("[warning] initChargeCostWrite - COST_DATABASE_DSN connection test failed.. requests will return 503 until the database is reachable.")
		}
	}

	registerChargeCostRoutes(v1, &chargeCostService{cfg: *cfg, db: costDB})
	log.Printf("[info] initChargeCostWrite - charge cost endpoints enabled (currency %s, own database role: %t).", cfg.currency, cfg.dsn != "")
}

// loadChargeCostConfig reads the settings from the environment
// returns nil without problems if the feature is disabled
func loadChargeCostConfig() (*chargeCostConfig, []string) {
	if !getEnvAsBool("ENABLE_COST_WRITE", false) {
		return nil, nil
	}

	var problems []string

	token := getEnv("COST_WRITE_TOKEN", "")
	switch {
	case len(token) < chargeCostMinTokenLength:
		problems = append(problems, "COST_WRITE_TOKEN has to be set and at least 32 characters long.")
	case !chargeCostTokenRegex.MatchString(token):
		problems = append(problems, "COST_WRITE_TOKEN may only contain printable ascii characters without spaces.")
	case token == getEnv("API_TOKEN", ""):
		problems = append(problems, "COST_WRITE_TOKEN has to be different from API_TOKEN.")
	}

	currency := getEnv("COST_CURRENCY", "")
	if !chargeCostCurrencyRegex.MatchString(currency) {
		problems = append(problems, "COST_CURRENCY has to be set to an upper case ISO 4217 code (e.g. EUR).")
	}

	teslamateURL, ok := normalizeChargeCostTeslaMateURL(getEnv("COST_TESLAMATE_URL", ""))
	if !ok {
		problems = append(problems, "COST_TESLAMATE_URL has to be set to the http(s) base url of this TeslaMate instance.")
	}

	if len(problems) > 0 {
		return nil, problems
	}
	return &chargeCostConfig{
		tokenHash:    sha256.Sum256([]byte(token)),
		currency:     currency,
		teslamateURL: teslamateURL,
		dsn:          getEnv("COST_DATABASE_DSN", ""),
	}, nil
}

// normalizeChargeCostTeslaMateURL validates the base url and removes a trailing slash
func normalizeChargeCostTeslaMateURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	return strings.TrimRight(u.String(), "/"), true
}

// registerChargeCostRoutes adds the charge cost endpoints to /api/v1
func registerChargeCostRoutes(v1 *gin.RouterGroup, s *chargeCostService) {
	if s == nil {
		return
	}
	cost := v1.Group("", chargeCostNoStore, s.authorize)
	{
		cost.GET("/cars/:CarID/charges/:ChargeID/cost", s.TeslaMateAPICarsChargesCostGetV1)
		cost.PUT("/cars/:CarID/charges/:ChargeID/cost", s.TeslaMateAPICarsChargesCostPutV1)
		cost.POST("/changes/:RequestID/undo", s.TeslaMateAPIChangesUndoV1)
	}
}

// chargeCostNoStore makes sure no proxy or client caches cost responses
func chargeCostNoStore(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Next()
}

// authorize checks the Authorization: Bearer <COST_WRITE_TOKEN> header in constant time
// the token is never accepted from the url and never logged
func (s *chargeCostService) authorize(c *gin.Context) {
	scheme, token, found := strings.Cut(c.GetHeader("Authorization"), " ")
	token = strings.TrimSpace(token)
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		abortChargeCost(c, &chargeCostError{status: http.StatusUnauthorized, code: "unauthorized", message: "Authorization: Bearer <token> header required"})
		return
	}
	given := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(given[:], s.cfg.tokenHash[:]) != 1 {
		abortChargeCost(c, &chargeCostError{status: http.StatusForbidden, code: "forbidden", message: "token invalid"})
		return
	}
	c.Next()
}

// TeslaMateAPICarsChargesCostGetV1 returns the cost of a charging process
func (s *chargeCostService) TeslaMateAPICarsChargesCostGetV1(c *gin.Context) {
	carID, chargeID, ok := chargeCostPathIDs(c)
	if !ok {
		abortChargeCost(c, errChargeCostNotFound())
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), chargeCostRequestTimeout)
	defer cancel()

	status, cerr := s.loadStatus(ctx, carID, chargeID)
	if cerr != nil {
		abortChargeCost(c, cerr)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": status})
}

// TeslaMateAPICarsChargesCostPutV1 sets the cost of a completed charging process
func (s *chargeCostService) TeslaMateAPICarsChargesCostPutV1(c *gin.Context) {
	carID, chargeID, ok := chargeCostPathIDs(c)
	if !ok {
		abortChargeCost(c, errChargeCostNotFound())
		return
	}

	var req chargeCostSetRequest
	if cerr := decodeChargeCostBody(c, &req); cerr != nil {
		abortChargeCost(c, cerr)
		return
	}
	requestID, ok := normalizeChargeCostRequestID(req.RequestID)
	if !ok {
		abortChargeCost(c, errChargeCostInvalid("invalid_request_id", "request_id has to be a uuid"))
		return
	}
	expected, cerr := chargeCostExpected(req.ExpectedCost)
	if cerr != nil {
		abortChargeCost(c, cerr)
		return
	}
	if !req.Cost.set || req.Cost.value == nil {
		abortChargeCost(c, errChargeCostInvalid("invalid_amount", "cost is required and must not be null"))
		return
	}
	cost, ok := normalizeChargeCostAmount(*req.Cost.value)
	if !ok {
		abortChargeCost(c, errChargeCostInvalidAmount("cost"))
		return
	}
	if req.Currency != s.cfg.currency {
		abortChargeCost(c, errChargeCostInvalid("invalid_currency", "currency has to be "+s.cfg.currency))
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), chargeCostRequestTimeout)
	defer cancel()

	change, replayed, cerr := s.setCost(ctx, carID, chargeID, requestID, expected, cost)
	if cerr != nil {
		abortChargeCost(c, cerr)
		return
	}
	if !replayed {
		log.Printf("[info] %s - cost set: car_id=%d charge_id=%d request_id=%s", chargeCostHandlerLogLabel, carID, chargeID, requestID)
	}
	respondChargeCostChange(c, change, replayed)
}

// TeslaMateAPIChangesUndoV1 reverts the latest cost change of a charging process
func (s *chargeCostService) TeslaMateAPIChangesUndoV1(c *gin.Context) {
	targetID, ok := normalizeChargeCostRequestID(c.Param("RequestID"))
	if !ok {
		abortChargeCost(c, &chargeCostError{status: http.StatusNotFound, code: "change_not_found", message: "change not found"})
		return
	}

	var req chargeCostUndoRequest
	if cerr := decodeChargeCostBody(c, &req); cerr != nil {
		abortChargeCost(c, cerr)
		return
	}
	requestID, ok := normalizeChargeCostRequestID(req.RequestID)
	if !ok {
		abortChargeCost(c, errChargeCostInvalid("invalid_request_id", "request_id has to be a uuid"))
		return
	}
	expected, cerr := chargeCostExpected(req.ExpectedCost)
	if cerr != nil {
		abortChargeCost(c, cerr)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), chargeCostRequestTimeout)
	defer cancel()

	change, replayed, cerr := s.undoChange(ctx, targetID, requestID, expected)
	if cerr != nil {
		abortChargeCost(c, cerr)
		return
	}
	if !replayed {
		log.Printf("[info] %s - cost change undone: car_id=%d charge_id=%d request_id=%s undo_of=%s", chargeCostHandlerLogLabel, change.CarID, change.ChargeID, requestID, targetID)
	}
	respondChargeCostChange(c, change, replayed)
}

// respondChargeCostChange returns the audit entry, a repeated request gets the same body
func respondChargeCostChange(c *gin.Context, change *chargeCostChange, replayed bool) {
	if replayed {
		c.Header("Idempotent-Replayed", "true")
	}
	c.JSON(http.StatusOK, gin.H{"data": change})
}

// abortChargeCost writes the error response, never containing sql details or tokens
func abortChargeCost(c *gin.Context, e *chargeCostError) {
	if e.status >= http.StatusInternalServerError {
		log.Printf("[error] %s - (%s %s) %d %s", chargeCostHandlerLogLabel, c.Request.Method, c.FullPath(), e.status, e.code)
	} else {
		log.Printf("[warning] %s - (%s %s) %d %s", chargeCostHandlerLogLabel, c.Request.Method, c.FullPath(), e.status, e.code)
	}
	if e.status == http.StatusUnauthorized {
		c.Header("WWW-Authenticate", `Bearer realm="teslamateapi"`)
	}
	body := gin.H{"error": e.message, "code": e.code}
	if e.withCurrent {
		body["current_cost"] = e.currentCost
	}
	c.AbortWithStatusJSON(e.status, body)
}

// decodeChargeCostBody reads a json body of at most 4 KiB and rejects unknown fields
func decodeChargeCostBody(c *gin.Context, dst any) *chargeCostError {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return &chargeCostError{status: http.StatusUnsupportedMediaType, code: "unsupported_media_type", message: "Content-Type has to be application/json"}
	}

	dec := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, chargeCostMaxBodyBytes))
	dec.DisallowUnknownFields()
	err = dec.Decode(dst)
	if err == nil {
		// only one json value is allowed
		if err = dec.Decode(&struct{}{}); err == io.EOF {
			return nil
		}
		if err == nil {
			err = errors.New("trailing data")
		}
	}

	var maxBytesErr *http.MaxBytesError
	switch {
	case errors.As(err, &maxBytesErr):
		return &chargeCostError{status: http.StatusRequestEntityTooLarge, code: "body_too_large", message: "request body too large"}
	case errors.Is(err, errChargeCostAmountType):
		return errChargeCostInvalid("invalid_amount", "amounts have to be decimal strings like \"12.34\" or null")
	default:
		return &chargeCostError{status: http.StatusBadRequest, code: "invalid_request", message: "request body has to be a single json object without unknown fields"}
	}
}

// chargeCostExpected validates expected_cost, which has to be sent (null allowed)
func chargeCostExpected(a optionalAmount) (*string, *chargeCostError) {
	if !a.set {
		return nil, errChargeCostInvalid("invalid_request", "expected_cost is required (use null if no cost is set)")
	}
	if a.value == nil {
		return nil, nil
	}
	expected, ok := normalizeChargeCostAmount(*a.value)
	if !ok {
		return nil, errChargeCostInvalidAmount("expected_cost")
	}
	return &expected, nil
}

// normalizeChargeCostAmount validates the decimal text and returns it with two decimals ("5" -> "5.00")
func normalizeChargeCostAmount(s string) (string, bool) {
	if !chargeCostAmountRegex.MatchString(s) {
		return "", false
	}
	whole, fraction, _ := strings.Cut(s, ".")
	return whole + "." + (fraction + "00")[:2], true
}

// normalizeChargeCostRequestID validates a uuid and returns it in lower case
func normalizeChargeCostRequestID(s string) (string, bool) {
	if !chargeCostUUIDRegex.MatchString(s) {
		return "", false
	}
	return strings.ToLower(s), true
}

// chargeCostPathIDs reads CarID and ChargeID, only positive integers are valid
func chargeCostPathIDs(c *gin.Context) (int64, int64, bool) {
	carID, ok1 := parseChargeCostID(c.Param("CarID"))
	chargeID, ok2 := parseChargeCostID(c.Param("ChargeID"))
	return carID, chargeID, ok1 && ok2
}

func parseChargeCostID(s string) (int64, bool) {
	v, err := strconv.ParseInt(s, 10, 64)
	return v, err == nil && v > 0 && s == strconv.FormatInt(v, 10)
}

func errChargeCostNotFound() *chargeCostError {
	return &chargeCostError{status: http.StatusNotFound, code: "charge_not_found", message: "charge not found for this car"}
}

func errChargeCostInvalid(code, message string) *chargeCostError {
	return &chargeCostError{status: http.StatusUnprocessableEntity, code: code, message: message}
}

func errChargeCostInvalidAmount(field string) *chargeCostError {
	return errChargeCostInvalid("invalid_amount", field+" has to be a decimal string between 0 and 999999.99 with at most two decimals")
}
