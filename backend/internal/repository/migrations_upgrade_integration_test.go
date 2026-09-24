//go:build integration

package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	dbmigrations "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// The original SQL bytes remain embedded in the application. This manifest
// pins the deployed 0.2.19 input without copying another full migration tree.
//
//go:embed testdata/schema-0.2.19.json
var schema019Manifest []byte

func TestMigrationsUpgrade019PreservesDataAndRecoversInterruptedCostIndex(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db := newMigrationUpgradeDatabase(t, ctx)
	oldFS := migration019FS(t)
	require.NoError(t, applyMigrationsFS(ctx, db, oldFS))
	requireMigrationCount(t, ctx, db, 299)

	var oldSchema bool
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT to_regclass('public.idx_usage_logs_api_key_created_cost') IS NULL
			AND to_regclass('public.mcp_oauth_grants') IS NULL
			AND NOT EXISTS (SELECT 1 FROM information_schema.columns
				WHERE table_schema = 'public' AND table_name = 'usage_logs'
					AND column_name = 'cache_creation_30m_tokens')
			AND NOT EXISTS (SELECT 1 FROM information_schema.columns
				WHERE table_schema = 'public' AND table_name = 'usage_group_rollup_state'
					AND column_name = 'expired_before')
			AND (SELECT relkind = 'r' FROM pg_class
				WHERE oid = 'public.usage_logs'::regclass)
	`).Scan(&oldSchema))
	require.True(t, oldSchema, "start with the deployed ordinary-table 0.2.19 schema")

	var userID, accountID, groupID, keyID int64
	require.NoError(t, db.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash, balance)
		VALUES ('migration@example.invalid', 'synthetic-hash', 123.45678901) RETURNING id
	`).Scan(&userID))
	require.NoError(t, db.QueryRowContext(ctx, `
		INSERT INTO accounts (name, platform, type, extra)
		VALUES ('migration-account', 'openai', 'apikey', '{"fixture":"019"}') RETURNING id
	`).Scan(&accountID))
	require.NoError(t, db.QueryRowContext(ctx, `
		INSERT INTO groups (name, platform, rate_multiplier, force_openai_fast)
		VALUES ('migration-group', 'openai', 1.25, TRUE) RETURNING id
	`).Scan(&groupID))
	require.NoError(t, db.QueryRowContext(ctx, `
		INSERT INTO api_keys (user_id, group_id, key, name)
		VALUES ($1, $2, 'sk-synthetic-migration', 'migration-key') RETURNING id
	`, userID, groupID).Scan(&keyID))
	_, err := db.ExecContext(ctx, `
		INSERT INTO usage_logs (
			user_id, api_key_id, account_id, group_id, request_id, model,
			input_tokens, output_tokens, cache_creation_5m_tokens,
			cache_creation_1h_tokens, total_cost, actual_cost, created_at)
		SELECT $1, $2, $3, $4, 'migration-' || n, 'fixture-model',
			n % 97, n % 53, n % 7, n % 11, n::numeric / 10000,
			n::numeric / 20000, TIMESTAMPTZ '2026-01-01 00:00:00+00'
				+ n * INTERVAL '1 minute'
		FROM generate_series(1, 4096) AS n
	`, userID, keyID, accountID, groupID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		UPDATE usage_group_rollup_state SET closed_before = DATE '2026-01-03',
			retained_from = TIMESTAMPTZ '2026-01-01 00:00:00+00', timezone_name = 'UTC'
		WHERE id = 1
	`)
	require.NoError(t, err)
	before := migrationUpgradeSnapshot(t, ctx, db)
	require.Equal(t, int64(4096), before.usageCount)

	// A real RowExclusiveLock makes CREATE INDEX CONCURRENTLY wait after it
	// commits its invalid catalog entry. Do not race a fast index build by size.
	writer, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Rollback() })
	var writerPID int
	require.NoError(t, writer.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&writerPID))
	_, err = writer.ExecContext(ctx, `
		INSERT INTO usage_logs (user_id, api_key_id, account_id, request_id, model)
		VALUES ($1, $2, $3, 'uncommitted-migration-writer', 'fixture-model')
	`, userID, keyID, accountID)
	require.NoError(t, err)

	migrationCtx, cancelMigration := context.WithTimeout(ctx, time.Minute)
	result := make(chan error, 1)
	go func() { result <- ApplyMigrations(migrationCtx, db) }()
	migrationDone := false
	t.Cleanup(func() {
		cancelMigration()
		if !migrationDone {
			select {
			case <-result:
			case <-time.After(10 * time.Second):
				t.Error("interrupted migration did not stop during cleanup")
			}
		}
	})

	waitCtx, cancelWait := context.WithTimeout(ctx, 30*time.Second)
	defer cancelWait()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-result:
			migrationDone = true
			t.Fatalf("migration returned before the real index wait: %v", err)
		default:
		}
		var waiting bool
		require.NoError(t, db.QueryRowContext(waitCtx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_progress_create_index p
				JOIN pg_class idx ON idx.oid = p.index_relid
				JOIN pg_namespace ns ON ns.oid = idx.relnamespace
				JOIN pg_index i ON i.indexrelid = idx.oid
				WHERE p.datid = (SELECT oid FROM pg_database WHERE datname = current_database())
					AND ns.nspname = 'public' AND idx.relname = $1 AND NOT i.indisvalid
					AND p.phase = 'waiting for writers before build'
					AND $2 = ANY(pg_blocking_pids(p.pid))
			)
		`, usageLogsAPIKeyCostIndex, writerPID).Scan(&waiting))
		if waiting {
			break
		}
		select {
		case <-ticker.C:
		case <-waitCtx.Done():
			t.Fatal("target concurrent index never reached its real writer wait")
		}
	}
	beforeCancelOID := migrationCostIndexOID(t, ctx, db)
	cancelMigration()
	select {
	case err := <-result:
		migrationDone = true
		require.Error(t, err)
		require.ErrorIs(t, migrationCtx.Err(), context.Canceled)
		require.Contains(t, err.Error(), usageLogsAPIKeyCostIndexMigration)
		var pgErr *pq.Error
		require.True(t, errors.Is(err, context.Canceled) ||
			(errors.As(err, &pgErr) && pgErr.Code == "57014"),
			"runner must return cancellation, not an unrelated migration failure: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("canceled migration did not return")
	}
	invalid, err := indexIsInvalid(ctx, db, usageLogsAPIKeyCostIndex)
	require.NoError(t, err)
	require.True(t, invalid, "the interrupted PostgreSQL index must actually remain invalid")
	requireMigrationCount(t, ctx, db, 299)
	var recorded bool
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE filename = $1)",
		usageLogsAPIKeyCostIndexMigration).Scan(&recorded))
	require.False(t, recorded, "interrupted nontransactional migration must not be recorded")
	require.NoError(t, writer.Rollback())
	require.Equal(t, before, migrationUpgradeSnapshot(t, ctx, db))

	require.NoError(t, ApplyMigrations(ctx, db))
	requireMigrationCount(t, ctx, db, 305)
	invalid, err = indexIsInvalid(ctx, db, usageLogsAPIKeyCostIndex)
	require.NoError(t, err)
	require.False(t, invalid)
	require.NotEqual(t, beforeCancelOID, migrationCostIndexOID(t, ctx, db),
		"recovery must drop and rebuild the invalid index, not skip IF NOT EXISTS")
	var indexDefinition string
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT pg_get_indexdef('public.idx_usage_logs_api_key_created_cost'::regclass)",
	).Scan(&indexDefinition))
	require.Contains(t, indexDefinition, "(api_key_id, created_at) INCLUDE (actual_cost)")
	require.Equal(t, before, migrationUpgradeSnapshot(t, ctx, db))

	var defaultsPreserved bool
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT NOT EXISTS (SELECT 1 FROM usage_logs
			WHERE cache_creation_30m_tokens <> 0 OR cache_creation_12h_tokens <> 0)
			AND NOT EXISTS (SELECT 1 FROM groups WHERE force_openai_ultrafast)
			AND EXISTS (SELECT 1 FROM usage_group_rollup_state WHERE id = 1
				AND expired_before = TIMESTAMPTZ '1970-01-01 00:00:00+00'
				AND building_until IS NULL AND build_dirty_from IS NULL)
			AND to_regclass('public.mcp_oauth_grants') IS NOT NULL
	`).Scan(&defaultsPreserved))
	require.True(t, defaultsPreserved)
	finalOID := migrationCostIndexOID(t, ctx, db)
	require.NoError(t, ApplyMigrations(ctx, db))
	requireMigrationCount(t, ctx, db, 305)
	require.Equal(t, finalOID, migrationCostIndexOID(t, ctx, db), "rerun must not rebuild a valid index")
	require.Equal(t, before, migrationUpgradeSnapshot(t, ctx, db))
}

func migration019FS(t *testing.T) fstest.MapFS {
	t.Helper()
	var manifest struct {
		SourceSHA      string `json:"source_sha"`
		MigrationCount int    `json:"migration_count"`
		Files          []struct {
			Filename string `json:"filename"`
			SHA256   string `json:"sha256"`
		} `json:"files"`
	}
	require.NoError(t, json.Unmarshal(schema019Manifest, &manifest))
	require.Equal(t, "7b4adfa0999d84ffe8daaaef063e795c761e485a", manifest.SourceSHA)
	require.Equal(t, 299, manifest.MigrationCount)
	require.Len(t, manifest.Files, manifest.MigrationCount)
	oldFS := make(fstest.MapFS, manifest.MigrationCount)
	for _, file := range manifest.Files {
		require.NotContains(t, oldFS, file.Filename)
		data, err := fs.ReadFile(dbmigrations.FS, file.Filename)
		require.NoError(t, err)
		sum := sha256.Sum256(data)
		require.Equal(t, file.SHA256, hex.EncodeToString(sum[:]), file.Filename)
		oldFS[file.Filename] = &fstest.MapFile{Data: data}
	}
	return oldFS
}

func newMigrationUpgradeDatabase(t *testing.T, ctx context.Context) *sql.DB {
	t.Helper()
	name := "migration_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err := integrationDB.ExecContext(ctx, "CREATE DATABASE "+pq.QuoteIdentifier(name))
	require.NoError(t, err)
	var db *sql.DB
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Only this test's uniquely named synthetic database is affected. Reap
		// a backend even if the cancellation assertion failed before teardown.
		_, err := integrationDB.ExecContext(cleanupCtx, `
			SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			WHERE datname = $1 AND pid <> pg_backend_pid()
		`, name)
		if err != nil {
			t.Errorf("stop synthetic migration database connections: %v", err)
		}
		if db != nil {
			closed := make(chan error, 1)
			go func() { closed <- db.Close() }()
			select {
			case err := <-closed:
				if err != nil {
					t.Errorf("close synthetic migration database: %v", err)
				}
			case <-cleanupCtx.Done():
				t.Error("synthetic migration database close exceeded cleanup deadline")
				return
			}
		}
		_, err = integrationDB.ExecContext(cleanupCtx, "DROP DATABASE "+pq.QuoteIdentifier(name))
		if err != nil {
			t.Errorf("clean synthetic migration database: %v", err)
		}
	})
	dsn, err := url.Parse(integrationDSN)
	require.NoError(t, err)
	dsn.Path = "/" + name
	db, err = sql.Open("postgres", dsn.String())
	require.NoError(t, err)
	db.SetMaxOpenConns(6)
	require.NoError(t, db.PingContext(ctx))
	return db
}

type migrationUpgradeDataSnapshot struct {
	usageCount                         int64
	usageDigest, totalCost, actualCost string
	identitiesDigest, rollupDigest     string
}

func migrationUpgradeSnapshot(t *testing.T, ctx context.Context, db *sql.DB) migrationUpgradeDataSnapshot {
	t.Helper()
	var snapshot migrationUpgradeDataSnapshot
	// Removing only the two newly added columns compares every original usage
	// field, not just a few selected totals, across interruption and upgrade.
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT COUNT(*), md5(string_agg((to_jsonb(u)
			- 'cache_creation_30m_tokens' - 'cache_creation_12h_tokens')::text, '' ORDER BY id)),
			SUM(total_cost)::text, SUM(actual_cost)::text FROM usage_logs u
	`).Scan(&snapshot.usageCount, &snapshot.usageDigest, &snapshot.totalCost, &snapshot.actualCost))
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT md5((SELECT jsonb_agg(to_jsonb(u) ORDER BY id)::text FROM users u)
			|| (SELECT jsonb_agg(to_jsonb(a) ORDER BY id)::text FROM accounts a)
			|| (SELECT jsonb_agg(to_jsonb(k) ORDER BY id)::text FROM api_keys k)
			|| (SELECT jsonb_agg(to_jsonb(g) - 'force_openai_ultrafast' ORDER BY id)::text FROM groups g))
	`).Scan(&snapshot.identitiesDigest))
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT md5((to_jsonb(s) - 'expired_before' - 'building_until' - 'build_dirty_from')::text)
		FROM usage_group_rollup_state s WHERE id = 1
	`).Scan(&snapshot.rollupDigest))
	return snapshot
}

func requireMigrationCount(t *testing.T, ctx context.Context, db *sql.DB, expected int) {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&count))
	require.Equal(t, expected, count)
}

func migrationCostIndexOID(t *testing.T, ctx context.Context, db *sql.DB) int64 {
	t.Helper()
	var oid int64
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT 'public.idx_usage_logs_api_key_created_cost'::regclass::oid::bigint").Scan(&oid))
	return oid
}
