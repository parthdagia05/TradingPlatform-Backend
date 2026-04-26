// Integration tests hit a live API + Postgres + Redis.
// They expect `docker compose up` to be running (or a manually-started stack)
// and read the API base URL from API_URL (default http://localhost:8080).
//
//   go test ./tests/...                  # runs them
//   go test -short ./tests/...           # skips them (use for unit-only runs)
//
// These cover the two automated proofs the spec demands:
//   - POST /trades is idempotent on tradeId (returns 200 + same record)
//   - cross-tenant data access returns 403 (NOT 404, NOT 200)
package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nevup/trade-journal/internal/auth"
)

const (
	jwtSecret = "97791d4db2aa5f689c3cc39356ce35762f0a73aa70923039d8ef72a2840a1b02"
	// Two seed users from the kickoff spec.
	userA = "f412f236-4edc-47a2-8f54-8763a6ed2ce8" // Alex Mercer
	userB = "fcd434aa-2201-4060-aeb2-f44c77aa0683" // Jordan Lee
)

func apiURL() string {
	if u := os.Getenv("API_URL"); u != "" {
		return u
	}
	return "http://localhost:8080"
}

func skipIfShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test (-short)")
	}
	// also skip cleanly when no API is reachable, so `go test ./...` on a
	// dev machine without `docker compose up` running stays green-or-skipped
	// rather than failing.
	cli := &http.Client{Timeout: 2 * time.Second}
	res, err := cli.Get(apiURL() + "/health")
	if err != nil {
		t.Skipf("skipping: API at %s is not reachable (%v) - run `docker compose up` or set API_URL", apiURL(), err)
	}
	res.Body.Close()
}

// tokenFor issues a 24h JWT for the user. Tests use the spec's exact secret.
func tokenFor(t *testing.T, sub string) string {
	t.Helper()
	tok, err := auth.IssueToken([]byte(jwtSecret), sub, "")
	require.NoError(t, err)
	return tok
}

// doJSON issues a request and decodes the JSON response into out (if non-nil).
// Returns the status code so callers can assert.
func doJSON(t *testing.T, method, path, token string, body any, out any) int {
	t.Helper()

	var rdr io.Reader
	if body != nil {
		buf := &bytes.Buffer{}
		require.NoError(t, json.NewEncoder(buf).Encode(body))
		rdr = buf
	}
	req, err := http.NewRequest(method, apiURL()+path, rdr)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	cli := &http.Client{Timeout: 10 * time.Second}
	res, err := cli.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()

	if out != nil && res.ContentLength != 0 {
		raw, _ := io.ReadAll(res.Body)
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, out)
		}
	}
	return res.StatusCode
}

// TEST: POST /trades is idempotent on tradeId
//
// Submit the same trade twice. Both calls must return 200, and the response
// body must be the same record (same tradeId, same createdAt).
func TestIdempotentTradeWrite(t *testing.T) {
	skipIfShort(t)
	tok := tokenFor(t, userA)
	tradeID := uuid.NewString()
	sessionID := uuid.NewString()
	body := map[string]any{
		"tradeId":     tradeID,
		"userId":      userA,
		"sessionId":   sessionID,
		"asset":       "AAPL",
		"assetClass":  "equity",
		"direction":   "long",
		"entryPrice":  178.45,
		"quantity":    10,
		"entryAt":     time.Now().UTC().Format(time.RFC3339),
		"status":      "open",
	}

	var first, second map[string]any

	require.Equal(t, 200, doJSON(t, "POST", "/trades", tok, body, &first))
	require.Equal(t, 200, doJSON(t, "POST", "/trades", tok, body, &second))

	require.Equal(t, first["tradeId"], second["tradeId"], "tradeId differs")
	require.Equal(t, first["createdAt"], second["createdAt"],
		"createdAt should be the same on idempotent re-submit (proves no new row)")
}

// TEST: cross-tenant requests return 403, never 404
//
// User A authenticates and tries to read User B's metrics. Must be 403.
func TestCrossTenantReturns403(t *testing.T) {
	skipIfShort(t)
	tokA := tokenFor(t, userA)

	url := fmt.Sprintf("/users/%s/metrics?from=%s&to=%s&granularity=daily",
		userB,
		time.Now().Add(-30*24*time.Hour).UTC().Format(time.RFC3339),
		time.Now().UTC().Format(time.RFC3339),
	)
	status := doJSON(t, "GET", url, tokA, nil, nil)
	require.Equal(t, http.StatusForbidden, status,
		"cross-tenant must be 403 (got %d)", status)
}

// TEST: missing Authorization header  401
func TestUnauthenticatedReturns401(t *testing.T) {
	skipIfShort(t)
	url := fmt.Sprintf("/users/%s/metrics?from=%s&to=%s&granularity=daily",
		userA, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		time.Now().UTC().Format(time.RFC3339))
	require.Equal(t, http.StatusUnauthorized, doJSON(t, "GET", url, "", nil, nil))
}

// TEST: GET /health returns 200 + reports DB+queue state
func TestHealthReturnsState(t *testing.T) {
	skipIfShort(t)
	var body map[string]any
	require.Equal(t, 200, doJSON(t, "GET", "/health", "", nil, &body))
	require.Equal(t, "ok", body["status"])
	require.Equal(t, "connected", body["dbConnection"])
	require.NotNil(t, body["queueLag"])
}

// TEST: posting a closed trade flows through the async pipeline
//
// End-to-end proof of the spec's "metrics computed asynchronously outside
// the write path" rule: read Alex's calm wins/losses, POST a brand-new
// closed winning trade with emotionalState=calm, poll the metrics endpoint
// until the calm.wins counter increments. If the worker isn't consuming or
// the producer/consumer wiring is broken, this test times out.
func TestMetricsUpdateAfterPost(t *testing.T) {
	skipIfShort(t)
	tok := tokenFor(t, userA)

	metricsURL := fmt.Sprintf("/users/%s/metrics?from=%s&to=%s&granularity=daily",
		userA,
		time.Now().Add(-365*24*time.Hour).UTC().Format(time.RFC3339),
		time.Now().Add(365*24*time.Hour).UTC().Format(time.RFC3339),
	)

	calmWins := func() float64 {
		var resp map[string]any
		require.Equal(t, 200, doJSON(t, "GET", metricsURL, tok, nil, &resp))
		stats, _ := resp["winRateByEmotionalState"].(map[string]any)
		calm, _ := stats["calm"].(map[string]any)
		if calm == nil {
			return 0
		}
		w, _ := calm["wins"].(float64)
		return w
	}

	before := calmWins()

	// post a calm/win trade we know hasn't been seen before
	body := map[string]any{
		"tradeId":        uuid.NewString(),
		"userId":         userA,
		"sessionId":      uuid.NewString(),
		"asset":          "AAPL",
		"assetClass":     "equity",
		"direction":      "long",
		"entryPrice":     100,
		"exitPrice":      110, // win
		"quantity":       1,
		"entryAt":        time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		"exitAt":         time.Now().UTC().Format(time.RFC3339),
		"status":         "closed",
		"planAdherence":  5,
		"emotionalState": "calm",
	}
	require.Equal(t, 200, doJSON(t, "POST", "/trades", tok, body, nil))

	// poll until the worker has applied the WinRateByEmotion calc
	deadline := time.Now().Add(10 * time.Second)
	var after float64
	for time.Now().Before(deadline) {
		after = calmWins()
		if after >= before+1 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	require.GreaterOrEqual(t, after, before+1,
		"calm wins should have incremented within 10s (before=%v after=%v) - worker may not be consuming",
		before, after)
}
