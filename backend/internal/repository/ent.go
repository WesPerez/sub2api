// Package repository 提供应用程序的基础设施层组件。
// 包括数据库连接初始化、ORM 客户端管理、Redis 连接、数据库迁移等核心功能。
package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/migrations"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/lib/pq"
)

const (
	maxDatabaseInitializationRetries = 8
	databaseInitializationRetryBase  = time.Second
	databaseInitializationRetryMax   = 30 * time.Second
)

// initializeDatabaseWithRetry retries only errors that indicate PostgreSQL is
// temporarily unavailable during startup. Permanent configuration, migration,
// and data errors are returned immediately so they remain visible to operators.
func initializeDatabaseWithRetry(ctx context.Context, initialize func(context.Context) error) error {
	return initializeDatabaseWithRetryWithWait(ctx, initialize, waitForDatabaseInitializationRetry)
}

func initializeDatabaseWithRetryWithWait(
	ctx context.Context,
	initialize func(context.Context) error,
	wait func(context.Context, time.Duration) error,
) error {
	for attempt := 1; ; attempt++ {
		if err := initialize(ctx); err == nil {
			return nil
		} else {
			if !isTransientDatabaseInitializationError(err) || attempt > maxDatabaseInitializationRetries {
				return err
			}

			delay := databaseInitializationRetryBase * time.Duration(1<<(attempt-1))
			if delay > databaseInitializationRetryMax {
				delay = databaseInitializationRetryMax
			}
			slog.Warn("database initialization temporarily unavailable; retrying",
				"retry", attempt,
				"max_retries", maxDatabaseInitializationRetries,
				"retry_in", delay,
				"error", err,
			)
			if err := wait(ctx, delay); err != nil {
				return err
			}
		}
	}
}

func waitForDatabaseInitializationRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isTransientDatabaseInitializationError(err error) bool {
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		return false
	}
	code := string(pqErr.Code)
	return code == "57P03" || strings.HasPrefix(code, "08")
}

// InitEnt 初始化 Ent ORM 客户端并返回客户端实例和底层的 *sql.DB。
//
// 该函数执行以下操作：
//  1. 初始化全局时区设置，确保时间处理一致性
//  2. 建立 PostgreSQL 数据库连接
//  3. 自动执行数据库迁移，确保 schema 与代码同步
//  4. 创建并返回 Ent 客户端实例
//
// 重要提示：调用者必须负责关闭返回的 ent.Client（关闭时会自动关闭底层的 driver/db）。
//
// 参数：
//   - cfg: 应用程序配置，包含数据库连接信息和时区设置
//
// 返回：
//   - *ent.Client: Ent ORM 客户端，用于执行数据库操作
//   - *sql.DB: 底层的 SQL 数据库连接，可用于直接执行原生 SQL
//   - error: 初始化过程中的错误
func InitEnt(cfg *config.Config) (*ent.Client, *sql.DB, error) {
	// 优先初始化时区设置，确保所有时间操作使用统一的时区。
	// 这对于跨时区部署和日志时间戳的一致性至关重要。
	if err := timezone.Init(cfg.Timezone); err != nil {
		return nil, nil, err
	}

	// 构建包含时区信息的数据库连接字符串 (DSN)。
	// 时区信息会传递给 PostgreSQL，确保数据库层面的时间处理正确。
	hostname := strings.TrimSpace(os.Getenv("HOSTNAME"))
	if hostname == "" {
		hostname = "unknown"
	}
	applicationName := migrationApplicationName(hostname)
	dsn := cfg.Database.DSNWithTimezone(cfg.Timezone) + " application_name='" + applicationName + "'"

	// 使用 Ent 的 SQL 驱动打开 PostgreSQL 连接。
	// dialect.Postgres 指定使用 PostgreSQL 方言进行 SQL 生成。
	var drv *entsql.Driver
	if cfg.Server.EnableServerTiming {
		connector, err := pq.NewConnector(dsn)
		if err != nil {
			return nil, nil, err
		}
		drv = entsql.OpenDB(dialect.Postgres, sql.OpenDB(newServerTimingConnector(connector)))
	} else {
		var err error
		drv, err = entsql.Open(dialect.Postgres, dsn)
		if err != nil {
			return nil, nil, err
		}
	}
	applyDBPoolSettings(drv.DB(), cfg)

	// 确保数据库 schema 已准备就绪。
	// SQL 迁移文件是 schema 的权威来源（source of truth）。
	// 这种方式比 Ent 的自动迁移更可控，支持复杂的迁移场景。
	migrationCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	migrationReporter, stopMigrationReporter := startMigrationProgressReporter(migrationCtx, applicationName)
	migrationReporter("started", "", 0, time.Time{})
	if err := initializeDatabaseWithRetry(migrationCtx, func(ctx context.Context) error {
		return applyMigrationsFSWithProgress(ctx, drv.DB(), migrations.FS, migrationReporter)
	}); err != nil {
		stopMigrationReporter()
		migrationReporter("failed", "", 0, time.Time{})
		slog.Error("database migration startup failed",
			"migration_application_name", applicationName,
			"error", err,
		)
		_ = drv.Close() // 迁移失败时关闭驱动，避免资源泄露
		return nil, nil, err
	}
	stopMigrationReporter()
	migrationReporter("completed", "", 0, time.Time{})

	// 创建 Ent 客户端，绑定到已配置的数据库驱动。
	client := ent.NewClient(ent.Driver(drv))

	// 启动阶段：从配置或数据库中确保系统密钥可用。
	if err := ensureBootstrapSecrets(migrationCtx, client, cfg); err != nil {
		_ = client.Close()
		return nil, nil, err
	}

	// 在密钥补齐后执行完整配置校验，避免空 jwt.secret 导致服务运行时失败。
	if err := cfg.Validate(); err != nil {
		_ = client.Close()
		return nil, nil, fmt.Errorf("validate config after secret bootstrap: %w", err)
	}

	seedCtx, seedCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer seedCancel()
	if err := ensureSimpleModeStartup(seedCtx, client, cfg); err != nil {
		_ = client.Close()
		return nil, nil, err
	}

	return client, drv.DB(), nil
}

func migrationApplicationName(hostname string) string {
	const prefix = "sub2api-"
	const maxPostgresIdentifierBytes = 63

	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		hostname = "unknown"
	}

	var safe strings.Builder
	for _, r := range hostname {
		if safe.Len() >= maxPostgresIdentifierBytes-len(prefix) {
			break
		}
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-' {
			_, _ = safe.WriteRune(r)
		} else {
			_ = safe.WriteByte('-')
		}
	}
	if safe.Len() == 0 {
		_, _ = safe.WriteString("unknown")
	}
	return prefix + safe.String()
}

func startMigrationProgressReporter(ctx context.Context, applicationName string) (migrationProgressFunc, func()) {
	started := time.Now().UTC()
	deadline, _ := ctx.Deadline()
	var mu sync.Mutex
	attempt, backendPID := 0, 0
	var backendStart time.Time
	currentMigration, phase := "", "started"
	emit := func(event string) {
		attrs := []any{
			"migration_event", event,
			"migration_application_name", applicationName,
			"migration_started_at", started.Format(time.RFC3339Nano),
			"migration_deadline_at", deadline.UTC().Format(time.RFC3339Nano),
			"migration_observed_at", time.Now().UTC().Format(time.RFC3339Nano),
			"migration_attempt", attempt,
			"migration_backend_pid", backendPID,
			"migration_backend_start", backendStart.UTC().Format(time.RFC3339Nano),
			"migration_phase", phase,
			"migration", currentMigration,
		}
		if event == "failed" {
			slog.Error("database migration startup failed", attrs...)
		} else {
			slog.Info("database migration progress", attrs...)
		}
	}
	reporterCtx, cancelReporter := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-reporterCtx.Done():
				return
			case <-ticker.C:
				mu.Lock()
				emit("heartbeat")
				mu.Unlock()
			}
		}
	}()
	report := func(event, migration string, pid int, backendStarted time.Time) {
		mu.Lock()
		defer mu.Unlock()
		if event == "waiting_for_connection" {
			attempt++
			backendPID, backendStart = 0, time.Time{}
		}
		if pid > 0 {
			backendPID, backendStart = pid, backendStarted
		}
		phase, currentMigration = event, migration
		emit(event)
	}
	return report, func() {
		cancelReporter()
		<-done
	}
}

// ensureSimpleModeStartup keeps admin concurrency setup independent of group seeding.
func ensureSimpleModeStartup(ctx context.Context, client *ent.Client, cfg *config.Config) error {
	if cfg.RunMode != config.RunModeSimple {
		return nil
	}
	if cfg.SimpleMode.AutoCreateDefaultGroups {
		if err := ensureSimpleModeDefaultGroups(ctx, client); err != nil {
			return err
		}
	}
	return ensureSimpleModeAdminConcurrency(ctx, client)
}
