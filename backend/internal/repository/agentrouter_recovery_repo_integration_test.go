//go:build integration

package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestRecoveryRepositoryGlobalLeaseAndDurableSlots(t *testing.T) {
	_ = testEntClient(t)
	ctx := context.Background()
	r := NewRecoveryRepository(integrationDB)
	lease, err := r.Acquire(ctx)
	require.NoError(t, err)
	_, err = r.Acquire(ctx)
	require.ErrorIs(t, err, service.ErrRecoveryBusy)
	lease.Close()
	lease, err = r.Acquire(ctx)
	require.NoError(t, err)
	defer lease.Close()
	slot := time.Now().UTC().Truncate(time.Microsecond)
	requestID := "recovery-test-" + slot.Format("20060102T150405.000000")
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM account_recovery_runs WHERE scheduled_for=$1 OR request_id=$2`, slot, requestID)
	})
	run, err := r.BeginRun(ctx, "schedule", &slot, "", "revision")
	require.NoError(t, err)
	run.Status = "success"
	require.NoError(t, r.FinishRun(ctx, run))
	_, err = r.BeginRun(ctx, "schedule", &slot, "", "revision")
	require.ErrorIs(t, err, service.ErrRecoveryConflict)
	manual, err := r.BeginRun(ctx, "manual", nil, requestID, "revision")
	require.NoError(t, err)
	manual.Status = "success"
	require.NoError(t, r.FinishRun(ctx, manual))
	_, err = r.BeginRun(ctx, "manual", nil, requestID, "revision")
	require.ErrorIs(t, err, service.ErrRecoveryConflict)
	runs, err := r.ListRuns(ctx, 100)
	require.NoError(t, err)
	var found int
	for _, value := range runs {
		if value.ID == run.ID || value.ID == manual.ID {
			found++
			require.Equal(t, "success", value.Status)
		}
	}
	require.Equal(t, 2, found)
}

func TestRecoveryRepositoryConfigurationCAS(t *testing.T) {
	_ = testEntClient(t)
	ctx := context.Background()
	r := NewRecoveryRepository(integrationDB)
	_, err := integrationDB.ExecContext(ctx, `DELETE FROM settings WHERE key=$1`, recoverySettingKey)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM settings WHERE key=$1`, recoverySettingKey)
	})
	require.ErrorIs(t, r.SaveConfig(ctx, "missing-previous", "invalid"), service.ErrRecoveryConflict)
	require.NoError(t, r.SaveConfig(ctx, "", `{"version":1}`))
	require.ErrorIs(t, r.SaveConfig(ctx, "", "stale"), service.ErrRecoveryConflict)
	require.NoError(t, r.SaveConfig(ctx, `{"version":1}`, `{"version":2}`))
	value, err := r.LoadConfig(ctx)
	require.NoError(t, err)
	require.True(t, strings.Contains(value, `"version":2`))
}
