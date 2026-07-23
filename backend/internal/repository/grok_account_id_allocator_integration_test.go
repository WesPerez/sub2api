//go:build integration

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var accountIDAllocatorFixtureCounter atomic.Uint64

type accountIDAllocatorFixture struct {
	schema string
}

func newAccountIDAllocatorFixture(t *testing.T) accountIDAllocatorFixture {
	t.Helper()

	schema := fmt.Sprintf("account_id_allocator_test_%d", accountIDAllocatorFixtureCounter.Add(1))
	_, err := integrationDB.ExecContext(context.Background(), fmt.Sprintf(`
CREATE SCHEMA %[1]s;
CREATE SEQUENCE %[1]s.accounts_id_seq
    AS BIGINT MINVALUE 1 MAXVALUE 99999 START WITH 1 CACHE 1 NO CYCLE;
CREATE SEQUENCE %[1]s.accounts_grok_id_seq
    AS BIGINT MINVALUE 100000 START WITH 100000 CACHE 1 NO CYCLE;
CREATE TABLE %[1]s.accounts (
    id BIGINT NOT NULL PRIMARY KEY,
    name TEXT NOT NULL,
    platform TEXT NOT NULL,
    type TEXT NOT NULL
);
ALTER SEQUENCE %[1]s.accounts_id_seq OWNED BY %[1]s.accounts.id;
CREATE TRIGGER trg_assign_account_id
    BEFORE INSERT ON %[1]s.accounts
    FOR EACH ROW
    EXECUTE FUNCTION public.assign_account_id();
`, schema))
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(
			context.Background(),
			fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", schema),
		)
	})

	return accountIDAllocatorFixture{schema: schema}
}

func (f accountIDAllocatorFixture) insertAutomatic(t *testing.T, platform string) int64 {
	t.Helper()

	var id int64
	err := integrationDB.QueryRowContext(
		context.Background(),
		fmt.Sprintf("INSERT INTO %s.accounts (name, platform, type) VALUES ($1, $2, 'oauth') RETURNING id", f.schema),
		fmt.Sprintf("automatic-%s", platform),
		platform,
	).Scan(&id)
	require.NoError(t, err)
	return id
}

func (f accountIDAllocatorFixture) insertExplicit(t *testing.T, platform string, id int64) int64 {
	t.Helper()

	var insertedID int64
	err := integrationDB.QueryRowContext(
		context.Background(),
		fmt.Sprintf("INSERT INTO %s.accounts (id, name, platform, type) VALUES ($1, $2, $3, 'oauth') RETURNING id", f.schema),
		id,
		fmt.Sprintf("explicit-%s-%d", platform, id),
		platform,
	).Scan(&insertedID)
	require.NoError(t, err)
	return insertedID
}

func (f accountIDAllocatorFixture) insertExplicitError(platform string, id int64) error {
	var insertedID int64
	return integrationDB.QueryRowContext(
		context.Background(),
		fmt.Sprintf("INSERT INTO %s.accounts (id, name, platform, type) VALUES ($1, $2, $3, 'oauth') RETURNING id", f.schema),
		id,
		fmt.Sprintf("invalid-%s-%d", platform, id),
		platform,
	).Scan(&insertedID)
}

func (f accountIDAllocatorFixture) sequenceState(t *testing.T, sequence string) (int64, bool) {
	t.Helper()

	var lastValue int64
	var isCalled bool
	err := integrationDB.QueryRowContext(
		context.Background(),
		fmt.Sprintf("SELECT last_value, is_called FROM %s.%s", f.schema, sequence),
	).Scan(&lastValue, &isCalled)
	require.NoError(t, err)
	return lastValue, isCalled
}

func TestGrokAccountIDAllocatorAutomaticAndExplicitIDs(t *testing.T) {
	f := newAccountIDAllocatorFixture(t)

	require.Equal(t, int64(1), f.insertAutomatic(t, "openai"))
	require.Equal(t, int64(100000), f.insertAutomatic(t, "grok"))

	require.Equal(t, int64(500), f.insertExplicit(t, "openai", 500))
	require.Equal(t, int64(100500), f.insertExplicit(t, "grok", 100500))
	require.Equal(t, int64(501), f.insertAutomatic(t, "anthropic"))
	require.Equal(t, int64(100501), f.insertAutomatic(t, "grok"))
}

func TestGrokAccountIDAllocatorRejectsCrossRangeIDsBeforeSequenceMutation(t *testing.T) {
	f := newAccountIDAllocatorFixture(t)

	err := f.insertExplicitError("openai", 100000)
	require.Error(t, err)
	require.Contains(t, err.Error(), "non-Grok account ID must be below 100000")
	err = f.insertExplicitError("grok", 99999)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Grok account ID must be at least 100000")

	normalLast, normalCalled := f.sequenceState(t, "accounts_id_seq")
	require.Equal(t, int64(1), normalLast)
	require.False(t, normalCalled)
	grokLast, grokCalled := f.sequenceState(t, "accounts_grok_id_seq")
	require.Equal(t, int64(100000), grokLast)
	require.False(t, grokCalled)
}

func TestGrokAccountIDAllocatorNeverMovesSequencesBackward(t *testing.T) {
	f := newAccountIDAllocatorFixture(t)

	f.insertExplicit(t, "openai", 500)
	f.insertExplicit(t, "openai", 400)
	f.insertExplicit(t, "grok", 100500)
	f.insertExplicit(t, "grok", 100400)

	require.Equal(t, int64(501), f.insertAutomatic(t, "openai"))
	require.Equal(t, int64(100501), f.insertAutomatic(t, "grok"))
}

func TestGrokAccountIDAllocatorSerializesConcurrentExplicitIDs(t *testing.T) {
	f := newAccountIDAllocatorFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	var highTx *sql.Tx
	var lowConn *sql.Conn
	var lowTx *sql.Tx
	t.Cleanup(func() {
		if highTx != nil {
			_ = highTx.Rollback()
		}
		cancel()
		if lowTx != nil {
			_ = lowTx.Rollback()
		}
		if lowConn != nil {
			_ = lowConn.Close()
		}
	})

	var err error
	highTx, err = integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)

	var highID int64
	err = highTx.QueryRowContext(
		ctx,
		fmt.Sprintf("INSERT INTO %s.accounts (id, name, platform, type) VALUES (5000, 'explicit-high', 'openai', 'oauth') RETURNING id", f.schema),
	).Scan(&highID)
	require.NoError(t, err)
	require.Equal(t, int64(5000), highID)

	lowConn, err = integrationDB.Conn(ctx)
	require.NoError(t, err)
	lowTx, err = lowConn.BeginTx(ctx, nil)
	require.NoError(t, err)

	var lowBackendPID int
	require.NoError(t, lowTx.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&lowBackendPID))
	type insertResult struct {
		id  int64
		err error
	}
	resultCh := make(chan insertResult, 1)
	go func() {
		var lowID int64
		err := lowTx.QueryRowContext(
			ctx,
			fmt.Sprintf("INSERT INTO %s.accounts (id, name, platform, type) VALUES (4000, 'explicit-low', 'openai', 'oauth') RETURNING id", f.schema),
		).Scan(&lowID)
		resultCh <- insertResult{id: lowID, err: err}
	}()

	waitForAdvisoryLockWait(t, ctx, lowBackendPID)
	require.NoError(t, highTx.Commit())

	result := <-resultCh
	require.NoError(t, result.err)
	require.Equal(t, int64(4000), result.id)
	require.NoError(t, lowTx.Commit())

	lastValue, isCalled := f.sequenceState(t, "accounts_id_seq")
	require.Equal(t, int64(5000), lastValue)
	require.True(t, isCalled)
	require.Equal(t, int64(5001), f.insertAutomatic(t, "openai"))
}

func TestGrokAccountIDAllocatorSerializesAutomaticIDBehindExplicitAlignment(t *testing.T) {
	f := newAccountIDAllocatorFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	var explicitTx *sql.Tx
	var automaticConn *sql.Conn
	var automaticTx *sql.Tx
	t.Cleanup(func() {
		if explicitTx != nil {
			_ = explicitTx.Rollback()
		}
		cancel()
		if automaticTx != nil {
			_ = automaticTx.Rollback()
		}
		if automaticConn != nil {
			_ = automaticConn.Close()
		}
	})

	var err error
	explicitTx, err = integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)

	var explicitID int64
	err = explicitTx.QueryRowContext(
		ctx,
		fmt.Sprintf("INSERT INTO %s.accounts (id, name, platform, type) VALUES (5000, 'explicit-high', 'openai', 'oauth') RETURNING id", f.schema),
	).Scan(&explicitID)
	require.NoError(t, err)
	require.Equal(t, int64(5000), explicitID)

	automaticConn, err = integrationDB.Conn(ctx)
	require.NoError(t, err)
	automaticTx, err = automaticConn.BeginTx(ctx, nil)
	require.NoError(t, err)

	var automaticBackendPID int
	require.NoError(t, automaticTx.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&automaticBackendPID))
	type insertResult struct {
		id  int64
		err error
	}
	resultCh := make(chan insertResult, 1)
	go func() {
		var automaticID int64
		err := automaticTx.QueryRowContext(
			ctx,
			fmt.Sprintf("INSERT INTO %s.accounts (name, platform, type) VALUES ('automatic-after-explicit', 'openai', 'oauth') RETURNING id", f.schema),
		).Scan(&automaticID)
		resultCh <- insertResult{id: automaticID, err: err}
	}()

	waitForAdvisoryLockWait(t, ctx, automaticBackendPID)
	require.NoError(t, explicitTx.Commit())

	result := <-resultCh
	require.NoError(t, result.err)
	require.Equal(t, int64(5001), result.id)
	require.NoError(t, automaticTx.Commit())

	lastValue, isCalled := f.sequenceState(t, "accounts_id_seq")
	require.Equal(t, int64(5001), lastValue)
	require.True(t, isCalled)
}

func TestGrokAccountIDAllocatorUsesOneGlobalLockAcrossSequences(t *testing.T) {
	f := newAccountIDAllocatorFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	var normalTx *sql.Tx
	var grokConn *sql.Conn
	var grokTx *sql.Tx
	t.Cleanup(func() {
		if normalTx != nil {
			_ = normalTx.Rollback()
		}
		cancel()
		if grokTx != nil {
			_ = grokTx.Rollback()
		}
		if grokConn != nil {
			_ = grokConn.Close()
		}
	})

	var err error
	normalTx, err = integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	var normalID int64
	err = normalTx.QueryRowContext(
		ctx,
		fmt.Sprintf("INSERT INTO %s.accounts (id, name, platform, type) VALUES (5000, 'normal-lock-holder', 'openai', 'oauth') RETURNING id", f.schema),
	).Scan(&normalID)
	require.NoError(t, err)
	require.Equal(t, int64(5000), normalID)

	grokConn, err = integrationDB.Conn(ctx)
	require.NoError(t, err)
	grokTx, err = grokConn.BeginTx(ctx, nil)
	require.NoError(t, err)
	var grokBackendPID int
	require.NoError(t, grokTx.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&grokBackendPID))

	type insertResult struct {
		id  int64
		err error
	}
	resultCh := make(chan insertResult, 1)
	go func() {
		var grokID int64
		err := grokTx.QueryRowContext(
			ctx,
			fmt.Sprintf("INSERT INTO %s.accounts (name, platform, type) VALUES ('grok-after-normal', 'grok', 'oauth') RETURNING id", f.schema),
		).Scan(&grokID)
		resultCh <- insertResult{id: grokID, err: err}
	}()

	waitForAdvisoryLockWait(t, ctx, grokBackendPID)
	require.NoError(t, normalTx.Commit())

	result := <-resultCh
	require.NoError(t, result.err)
	require.Equal(t, int64(100000), result.id)
	require.NoError(t, grokTx.Commit())

	normalLast, normalCalled := f.sequenceState(t, "accounts_id_seq")
	require.Equal(t, int64(5000), normalLast)
	require.True(t, normalCalled)
	grokLast, grokCalled := f.sequenceState(t, "accounts_grok_id_seq")
	require.Equal(t, int64(100000), grokLast)
	require.True(t, grokCalled)
}

func TestGrokAccountIDAllocatorNormalRangeUpperBoundary(t *testing.T) {
	f := newAccountIDAllocatorFixture(t)

	require.Equal(t, int64(99999), f.insertExplicit(t, "openai", 99999))
	lastValue, isCalled := f.sequenceState(t, "accounts_id_seq")
	require.Equal(t, int64(99999), lastValue)
	require.True(t, isCalled)

	var id int64
	err := integrationDB.QueryRowContext(
		context.Background(),
		fmt.Sprintf("INSERT INTO %s.accounts (name, platform, type) VALUES ('normal-overflow', 'openai', 'oauth') RETURNING id", f.schema),
	).Scan(&id)
	require.Error(t, err)
	require.Contains(t, err.Error(), "reached maximum value of sequence")
	require.Equal(t, int64(100000), f.insertAutomatic(t, "grok"))
}

func waitForAdvisoryLockWait(t *testing.T, ctx context.Context, backendPID int) {
	t.Helper()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		err := integrationDB.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM pg_locks
    WHERE pid = $1
      AND locktype = 'advisory'
      AND NOT granted
)`, backendPID).Scan(&waiting)
		require.NoError(t, err)
		if waiting {
			return
		}

		select {
		case <-ctx.Done():
			t.Fatalf("account insert never waited on the global allocator advisory lock: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}
