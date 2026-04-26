// Unit tests for the 5 behavioural metric calculators.
//
// Each test inserts deterministic trade fixtures into a real Postgres,
// invokes the calculator, and asserts on the resulting metric tables.
// We use a real DB (not a mock) because the calculators rely on window
// functions, partial indexes, and Postgres ENUMs - mocking SQL strings
// would be brittle and prove nothing.
//
// To run:
//
//	TEST_DATABASE_URL='postgres://nevup:nevup@localhost:5432/nevup?sslmode=disable' \
//	  go test ./internal/metrics/...
//
// If TEST_DATABASE_URL is unset and the default DSN can't connect, the
// tests skip cleanly (so `go test ./...` stays green on dev machines
// without a Postgres running).
package metrics

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

const (
	defaultTestDSN = "postgres://nevup:nevup@localhost:5432/nevup?sslmode=disable"
)

var (
	testPoolOnce sync.Once
	testPool     *pgxpool.Pool
	testPoolErr  error
)

// setupTestDB returns a pool against the test database. If no DB is
// reachable, the test is skipped (not failed) so `go test ./...` stays
// usable on machines without a running Postgres.
func setupTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	testPoolOnce.Do(func() {
		dsn := os.Getenv("TEST_DATABASE_URL")
		if dsn == "" {
			dsn = defaultTestDSN
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			testPoolErr = err
			return
		}
		if err := pool.Ping(ctx); err != nil {
			pool.Close()
			testPoolErr = err
			return
		}
		// ensure migrations are applied (the api binary normally does this)
		if err := ensureSchema(ctx, pool); err != nil {
			pool.Close()
			testPoolErr = err
			return
		}
		testPool = pool
	})

	if testPoolErr != nil {
		t.Skipf("no test database available (set TEST_DATABASE_URL to run): %v", testPoolErr)
	}
	return testPool
}

// ensureSchema runs the migrations if the trades table doesn't exist.
// We don't import internal/db (would be a cycle for db tests if any).
func ensureSchema(ctx context.Context, pool *pgxpool.Pool) error {
	var exists bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name='trades')`).
		Scan(&exists)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	// find the migrations dir relative to the test package
	candidates := []string{"./migrations", "../../migrations", "../migrations"}
	for _, dir := range candidates {
		if files, err := filepath.Glob(filepath.Join(dir, "*.up.sql")); err == nil && len(files) > 0 {
			for _, f := range files {
				body, err := os.ReadFile(f)
				if err != nil {
					return err
				}
				if _, err := pool.Exec(ctx, string(body)); err != nil {
					return err
				}
			}
			return nil
		}
	}
	return nil // schema may already be there from a prior run
}

// resetTables truncates everything between tests so fixtures don't bleed.
func resetTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx, `
        TRUNCATE trades,
                 user_metrics,
                 user_emotion_metrics,
                 session_metrics,
                 overtrading_events
        RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
}

// insertTrade adds one trade row. fields are kept minimal; pass nil for
// optional values you don't care about in a given test.
type tradeFixture struct {
	tradeID       uuid.UUID
	userID        uuid.UUID
	sessionID     uuid.UUID
	asset         string
	direction     string // "long" / "short"
	entryPrice    float64
	exitPrice     *float64
	quantity      float64
	entryAt       time.Time
	exitAt        *time.Time
	status        string // "open" / "closed" / "cancelled"
	outcome       *string
	pnl           *float64
	planAdherence *int
	emotionalState *string
}

func insertTrade(t *testing.T, pool *pgxpool.Pool, f tradeFixture) {
	t.Helper()
	if f.tradeID == uuid.Nil {
		f.tradeID = uuid.New()
	}
	if f.asset == "" {
		f.asset = "AAPL"
	}
	if f.direction == "" {
		f.direction = "long"
	}
	if f.entryPrice == 0 {
		f.entryPrice = 100
	}
	if f.quantity == 0 {
		f.quantity = 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx, `
        INSERT INTO trades (
            trade_id, user_id, session_id,
            asset, asset_class, direction,
            entry_price, exit_price, quantity,
            entry_at, exit_at, status,
            outcome, pnl, plan_adherence, emotional_state
        ) VALUES ($1,$2,$3, $4,'equity',$5, $6,$7,$8, $9,$10,$11, $12,$13,$14,$15)`,
		f.tradeID, f.userID, f.sessionID,
		f.asset, f.direction,
		f.entryPrice, f.exitPrice, f.quantity,
		f.entryAt, f.exitAt, f.status,
		f.outcome, f.pnl, f.planAdherence, f.emotionalState,
	)
	require.NoError(t, err)
}

func ptr[T any](v T) *T { return &v }

// ---------------------------------------------------------------------------
// 1. PlanAdherence
// ---------------------------------------------------------------------------

func TestPlanAdherence_RollingTen(t *testing.T) {
	pool := setupTestDB(t)
	resetTables(t, pool)
	repo := NewRepo(pool)
	user := uuid.New()
	session := uuid.New()

	// 12 closed trades. plan_adherence is constrained to 1..5, so we cycle:
	// trade i (1..12) -> value ((i-1) % 5) + 1 -> [1,2,3,4,5,1,2,3,4,5,1,2]
	// The "last 10 by exit_at" are i=3..12 -> values [3,4,5,1,2,3,4,5,1,2]
	// Sum = 30, average = 3.0 over a window of 10.
	now := time.Now().UTC()
	for i := 1; i <= 12; i++ {
		exitAt := now.Add(time.Duration(i) * time.Minute)
		entryAt := exitAt.Add(-30 * time.Second)
		val := ((i - 1) % 5) + 1
		insertTrade(t, pool, tradeFixture{
			userID: user, sessionID: session, status: "closed",
			entryAt: entryAt, exitAt: &exitAt, exitPrice: ptr(110.0),
			outcome: ptr("win"), pnl: ptr(10.0),
			planAdherence: ptr(val),
		})
	}

	require.NoError(t, PlanAdherence(context.Background(), pool, repo, user))

	var score float64
	var window int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT plan_adherence_score, plan_adherence_window
           FROM user_metrics WHERE user_id = $1`, user).
		Scan(&score, &window))
	require.Equal(t, 10, window, "should aggregate the last 10 trades")
	require.InDelta(t, 3.0, score, 0.0001, "rolling-10 avg of plan_adherence values 3..12 cycled through 1..5")
}

func TestPlanAdherence_NoData(t *testing.T) {
	pool := setupTestDB(t)
	resetTables(t, pool)
	repo := NewRepo(pool)
	user := uuid.New()

	// no trades for this user; calculator should be a no-op
	require.NoError(t, PlanAdherence(context.Background(), pool, repo, user))

	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM user_metrics WHERE user_id = $1`, user).Scan(&n))
	require.Equal(t, 0, n, "no trades -> no user_metrics row written")
}

// ---------------------------------------------------------------------------
// 2. RevengeFlag
// ---------------------------------------------------------------------------

func TestRevengeFlag_Triggers(t *testing.T) {
	pool := setupTestDB(t)
	resetTables(t, pool)
	repo := NewRepo(pool)
	user := uuid.New()
	session := uuid.New()

	// previous: closed losing trade, exit at 12:00
	exit1 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	insertTrade(t, pool, tradeFixture{
		userID: user, sessionID: session, status: "closed",
		entryAt: exit1.Add(-time.Hour), exitAt: &exit1, exitPrice: ptr(90.0),
		outcome: ptr("loss"), pnl: ptr(-10.0),
		emotionalState: ptr("calm"),
	})

	// new: opened 60s after the loss, anxious -> qualifies
	newID := uuid.New()
	insertTrade(t, pool, tradeFixture{
		tradeID: newID,
		userID:  user, sessionID: session, status: "open",
		entryAt:        exit1.Add(60 * time.Second),
		emotionalState: ptr("anxious"),
	})

	// the worker would call SetRevengeFlag through trades.Repo; we simulate
	// that with a closure.
	setFlag := func(ctx context.Context, id uuid.UUID, flag bool) error {
		_, err := pool.Exec(ctx,
			`UPDATE trades SET revenge_flag = $1 WHERE trade_id = $2`, flag, id)
		return err
	}

	require.NoError(t, RevengeFlag(context.Background(), pool, repo, setFlag, newID, user))

	var flag bool
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT revenge_flag FROM trades WHERE trade_id = $1`, newID).Scan(&flag))
	require.True(t, flag, "anxious open within 90s of a loss must flag")

	var count int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT revenge_trades_count FROM user_metrics WHERE user_id = $1`,
		user).Scan(&count))
	require.Equal(t, 1, count)
}

func TestRevengeFlag_OutsideWindow(t *testing.T) {
	pool := setupTestDB(t)
	resetTables(t, pool)
	repo := NewRepo(pool)
	user := uuid.New()
	session := uuid.New()

	// loss closes at 12:00
	exit1 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	insertTrade(t, pool, tradeFixture{
		userID: user, sessionID: session, status: "closed",
		entryAt: exit1.Add(-time.Hour), exitAt: &exit1, exitPrice: ptr(90.0),
		outcome: ptr("loss"), pnl: ptr(-10.0),
		emotionalState: ptr("calm"),
	})

	// new: 91 seconds later (1s past the 90s window) with anxious emotion
	newID := uuid.New()
	insertTrade(t, pool, tradeFixture{
		tradeID: newID,
		userID:  user, sessionID: session, status: "open",
		entryAt:        exit1.Add(91 * time.Second),
		emotionalState: ptr("anxious"),
	})

	setFlag := func(ctx context.Context, id uuid.UUID, flag bool) error {
		_, err := pool.Exec(ctx,
			`UPDATE trades SET revenge_flag = $1 WHERE trade_id = $2`, flag, id)
		return err
	}
	require.NoError(t, RevengeFlag(context.Background(), pool, repo, setFlag, newID, user))

	var flag bool
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT revenge_flag FROM trades WHERE trade_id = $1`, newID).Scan(&flag))
	require.False(t, flag, "outside the 90s window: must NOT flag")
}

func TestRevengeFlag_WrongEmotion(t *testing.T) {
	pool := setupTestDB(t)
	resetTables(t, pool)
	repo := NewRepo(pool)
	user := uuid.New()
	session := uuid.New()

	exit1 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	insertTrade(t, pool, tradeFixture{
		userID: user, sessionID: session, status: "closed",
		entryAt: exit1.Add(-time.Hour), exitAt: &exit1, exitPrice: ptr(90.0),
		outcome: ptr("loss"), pnl: ptr(-10.0),
	})
	// new trade within 90s but with "calm" - must not flag
	newID := uuid.New()
	insertTrade(t, pool, tradeFixture{
		tradeID: newID,
		userID:  user, sessionID: session, status: "open",
		entryAt:        exit1.Add(45 * time.Second),
		emotionalState: ptr("calm"),
	})

	setFlag := func(ctx context.Context, id uuid.UUID, flag bool) error {
		_, err := pool.Exec(ctx,
			`UPDATE trades SET revenge_flag = $1 WHERE trade_id = $2`, flag, id)
		return err
	}
	require.NoError(t, RevengeFlag(context.Background(), pool, repo, setFlag, newID, user))

	var flag bool
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT revenge_flag FROM trades WHERE trade_id = $1`, newID).Scan(&flag))
	require.False(t, flag, "calm emotion must NOT trigger revenge flag")
}

// ---------------------------------------------------------------------------
// 3. SessionTilt
// ---------------------------------------------------------------------------

func TestSessionTilt_LossFollowingRatio(t *testing.T) {
	pool := setupTestDB(t)
	resetTables(t, pool)
	repo := NewRepo(pool)
	user := uuid.New()
	session := uuid.New()

	// session of 4 closed trades:
	//   t1 = win
	//   t2 = loss   (prev=win, not loss-following)
	//   t3 = loss   (prev=loss, IS loss-following)
	//   t4 = win    (prev=loss, IS loss-following)
	//
	// loss-following = 2 out of 4 -> tilt = 0.5
	base := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	outcomes := []string{"win", "loss", "loss", "win"}
	for i, oc := range outcomes {
		entry := base.Add(time.Duration(i*10) * time.Minute)
		exit := entry.Add(5 * time.Minute)
		insertTrade(t, pool, tradeFixture{
			userID: user, sessionID: session, status: "closed",
			entryAt: entry, exitAt: &exit, exitPrice: ptr(110.0),
			outcome: ptr(oc), pnl: ptr(10.0),
		})
	}

	require.NoError(t, SessionTilt(context.Background(), repo, session))

	var total, lossFollowing int
	var tilt float64
	require.NoError(t, pool.QueryRow(context.Background(), `
        SELECT total_trades, loss_following_trades, tilt_index::float8
          FROM session_metrics WHERE session_id = $1`, session).
		Scan(&total, &lossFollowing, &tilt))
	require.Equal(t, 4, total)
	require.Equal(t, 2, lossFollowing)
	require.InDelta(t, 0.5, tilt, 0.0001)
}

// ---------------------------------------------------------------------------
// 4. WinRateByEmotion
// ---------------------------------------------------------------------------

func TestWinRateByEmotion_IncrementsRightBucket(t *testing.T) {
	pool := setupTestDB(t)
	resetTables(t, pool)
	repo := NewRepo(pool)
	user := uuid.New()
	session := uuid.New()

	// closed winning trade tagged calm
	tradeID := uuid.New()
	exit := time.Now().UTC()
	entry := exit.Add(-time.Hour)
	insertTrade(t, pool, tradeFixture{
		tradeID: tradeID,
		userID:  user, sessionID: session, status: "closed",
		entryAt: entry, exitAt: &exit, exitPrice: ptr(110.0),
		outcome: ptr("win"), pnl: ptr(10.0),
		emotionalState: ptr("calm"),
	})

	require.NoError(t, WinRateByEmotion(context.Background(), pool, repo, tradeID, user))

	var wins, losses int
	require.NoError(t, pool.QueryRow(context.Background(), `
        SELECT wins, losses
          FROM user_emotion_metrics
         WHERE user_id = $1 AND emotional_state = 'calm'`, user).
		Scan(&wins, &losses))
	require.Equal(t, 1, wins)
	require.Equal(t, 0, losses)
}

func TestWinRateByEmotion_LossPath(t *testing.T) {
	pool := setupTestDB(t)
	resetTables(t, pool)
	repo := NewRepo(pool)
	user := uuid.New()
	session := uuid.New()

	tradeID := uuid.New()
	exit := time.Now().UTC()
	insertTrade(t, pool, tradeFixture{
		tradeID: tradeID,
		userID:  user, sessionID: session, status: "closed",
		entryAt: exit.Add(-time.Hour), exitAt: &exit, exitPrice: ptr(90.0),
		outcome: ptr("loss"), pnl: ptr(-10.0),
		emotionalState: ptr("anxious"),
	})

	require.NoError(t, WinRateByEmotion(context.Background(), pool, repo, tradeID, user))

	var wins, losses int
	require.NoError(t, pool.QueryRow(context.Background(), `
        SELECT wins, losses FROM user_emotion_metrics
         WHERE user_id = $1 AND emotional_state = 'anxious'`, user).
		Scan(&wins, &losses))
	require.Equal(t, 0, wins)
	require.Equal(t, 1, losses)
}

// ---------------------------------------------------------------------------
// 5. Overtrading
// ---------------------------------------------------------------------------

func TestOvertrading_TriggersAtElevenInWindow(t *testing.T) {
	pool := setupTestDB(t)
	resetTables(t, pool)
	repo := NewRepo(pool)
	user := uuid.New()
	session := uuid.New()

	// 11 trades opened within 30 minutes; the 11th must trigger an event.
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 11; i++ {
		insertTrade(t, pool, tradeFixture{
			userID: user, sessionID: session, status: "open",
			entryAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	// Run the calc anchored at the 11th trade
	require.NoError(t, Overtrading(context.Background(), pool, repo, nil,
		user, base.Add(10*time.Minute)))

	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM overtrading_events WHERE user_id = $1`, user).Scan(&n))
	require.Equal(t, 1, n, "exactly one event should be recorded")

	var count int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT overtrading_events_count FROM user_metrics WHERE user_id = $1`,
		user).Scan(&count))
	require.Equal(t, 1, count)
}

func TestOvertrading_TenInWindow_NoTrigger(t *testing.T) {
	pool := setupTestDB(t)
	resetTables(t, pool)
	repo := NewRepo(pool)
	user := uuid.New()
	session := uuid.New()

	// exactly 10 trades in window - threshold is "more than 10"
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		insertTrade(t, pool, tradeFixture{
			userID: user, sessionID: session, status: "open",
			entryAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	require.NoError(t, Overtrading(context.Background(), pool, repo, nil,
		user, base.Add(9*time.Minute)))

	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM overtrading_events WHERE user_id = $1`, user).Scan(&n))
	require.Equal(t, 0, n, "10 trades should NOT trigger (rule is > 10)")
}

func TestOvertrading_DedupesWithinSameWindow(t *testing.T) {
	pool := setupTestDB(t)
	resetTables(t, pool)
	repo := NewRepo(pool)
	user := uuid.New()
	session := uuid.New()

	// 12 trades in one 30-min window. Calling Overtrading at trade #11
	// should emit; calling again at trade #12 should NOT emit a 2nd event
	// (still inside the same incident).
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 12; i++ {
		insertTrade(t, pool, tradeFixture{
			userID: user, sessionID: session, status: "open",
			entryAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	require.NoError(t, Overtrading(context.Background(), pool, repo, nil,
		user, base.Add(10*time.Minute)))
	require.NoError(t, Overtrading(context.Background(), pool, repo, nil,
		user, base.Add(11*time.Minute)))

	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM overtrading_events WHERE user_id = $1`, user).Scan(&n))
	require.Equal(t, 1, n, "second call within the same window should be deduped")
}

// silence unused-import warnings if we ever trim a test
var _ = pgx.ErrNoRows
