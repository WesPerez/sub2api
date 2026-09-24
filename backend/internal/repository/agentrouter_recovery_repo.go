package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const recoveryLockID int64 = 73190220260922
const recoverySettingKey = "agentrouter_recovery_policy_v1"

type recoveryRepository struct{ db *sql.DB }

func NewRecoveryRepository(db *sql.DB) service.RecoveryRepository { return &recoveryRepository{db} }

func (r *recoveryRepository) LoadConfig(ctx context.Context) (string, error) {
	var value string
	err := r.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1`, recoverySettingKey).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func (r *recoveryRepository) SaveConfig(ctx context.Context, previous, next string) error {
	result, err := r.db.ExecContext(ctx, `INSERT INTO settings (key,value,updated_at) SELECT $1::varchar,$2::text,NOW() WHERE $3::text='' OR EXISTS (SELECT 1 FROM settings WHERE key=$1::varchar AND value=$3::text)
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value,updated_at=NOW() WHERE settings.value=$3::text`, recoverySettingKey, next, previous)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return service.ErrRecoveryConflict
	}
	return err
}

type recoveryLease struct{ conn *sql.Conn }

func (l *recoveryLease) Check(ctx context.Context) error { return l.conn.PingContext(ctx) }
func (l *recoveryLease) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := l.conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, recoveryLockID); err != nil {
		_ = l.conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	_ = l.conn.Close()
}
func (r *recoveryRepository) Acquire(ctx context.Context) (service.RecoveryLease, error) {
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	var locked bool
	if err = conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, recoveryLockID).Scan(&locked); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		_ = conn.Close()
		return nil, err
	}
	if !locked {
		_ = conn.Close()
		return nil, service.ErrRecoveryBusy
	}
	return &recoveryLease{conn}, nil
}

func (r *recoveryRepository) BeginRun(ctx context.Context, trigger string, slot *time.Time, requestID, revision string) (*service.RecoveryRun, error) {
	// The caller owns the global lease. Any older running record lost its executor.
	_, err := r.db.ExecContext(ctx, `UPDATE account_recovery_runs SET status='interrupted',finished_at=NOW(),error_message='执行进程已中断；下次计划重新核对账号' WHERE status='running'`)
	if err != nil {
		return nil, err
	}
	run := &service.RecoveryRun{Trigger: trigger, Status: "running", ScheduledFor: slot, ConfigRevision: revision}
	err = r.db.QueryRowContext(ctx, `INSERT INTO account_recovery_runs (trigger,scheduled_for,request_id,config_revision,status)
		VALUES ($1,$2,NULLIF($3,''),$4,'running') ON CONFLICT DO NOTHING RETURNING id,started_at`, trigger, slot, requestID, revision).Scan(&run.ID, &run.StartedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrRecoveryConflict
	}
	return run, err
}
func (r *recoveryRepository) FinishRun(ctx context.Context, run *service.RecoveryRun) error {
	data, err := json.Marshal(run.Outcome)
	if err != nil {
		return err
	}
	result, err := r.db.ExecContext(ctx, `UPDATE account_recovery_runs SET status=$2,finished_at=NOW(),outcome=$3,error_message=$4 WHERE id=$1 AND status='running'`, run.ID, run.Status, string(data), run.Error)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return service.ErrRecoveryConflict
	}
	return err
}
func (r *recoveryRepository) ListRuns(ctx context.Context, limit int) ([]service.RecoveryRun, error) {
	if limit < 1 || limit > 100 {
		limit = 30
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id,trigger,status,scheduled_for,started_at,finished_at,config_revision,outcome,error_message FROM account_recovery_runs ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	runs := []service.RecoveryRun{}
	for rows.Next() {
		var run service.RecoveryRun
		var outcome []byte
		if err = rows.Scan(&run.ID, &run.Trigger, &run.Status, &run.ScheduledFor, &run.StartedAt, &run.FinishedAt, &run.ConfigRevision, &outcome, &run.Error); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(outcome, &run.Outcome); err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}
func (r *recoveryRepository) PruneRuns(ctx context.Context, days, keep int) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM account_recovery_runs WHERE status<>'running' AND
		(started_at<NOW()-($1*INTERVAL '1 day') OR id IN (SELECT id FROM account_recovery_runs ORDER BY id DESC OFFSET $2))`, days, keep)
	return err
}
