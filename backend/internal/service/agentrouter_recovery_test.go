package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func recoveryTestAccount(id int64, name, balance string) Account {
	note := "余额 $" + balance
	return Account{ID: id, Name: name, Platform: "openai", Type: "apikey", Status: StatusActive, Schedulable: true,
		Notes: &note, Credentials: map[string]any{"base_url": "https://agentrouter.org/v1", "api_key": "private-test-value"}}
}
func recoveryTestPolicy() RecoveryPolicy {
	p := DefaultRecoveryPolicy()
	p.Groups = p.Groups[:1]
	p.Groups[0].TopN = 1
	return p
}

func TestRecoveryPlanProtectsUnmatchedAndEmptyGroups(t *testing.T) {
	p := recoveryTestPolicy()
	accounts := []Account{recoveryTestAccount(1, "a-gpt", "9.99"), recoveryTestAccount(2, "b-gpt", "10.01"), recoveryTestAccount(3, "c-future", "99")}
	accounts[0].Credentials["base_url"] = "https://agentrouter.org.evil.invalid/v1"
	plan, err := BuildRecoveryPlan(p, accounts)
	require.NoError(t, err)
	require.Equal(t, []int64{2}, plan.Groups[0].Selected)
	require.Equal(t, "preserve", plan.Accounts[1].Action)
	data, err := json.Marshal(plan)
	require.NoError(t, err)
	require.NotContains(t, string(data), "private-test-value")
	accounts[1].Notes = nil
	plan, err = BuildRecoveryPlan(p, accounts)
	require.NoError(t, err)
	require.Empty(t, plan.Groups[0].Selected)
	require.NotEmpty(t, plan.Warnings)
	for _, a := range plan.Accounts {
		require.Equal(t, "preserve", a.Action)
	}
}
func TestRecoveryPlanUsesDecimalAndStableTieBreak(t *testing.T) {
	p := recoveryTestPolicy()
	p.Groups[0].TopN = 2
	accounts := []Account{recoveryTestAccount(3, "c-gpt", "9007199254740992.01"), recoveryTestAccount(2, "b-gpt", "9007199254740992.02"), recoveryTestAccount(1, "a-gpt", "9007199254740992.02")}
	plan, err := BuildRecoveryPlan(p, accounts)
	require.NoError(t, err)
	require.Equal(t, []int64{1, 2}, plan.Groups[0].Selected)
	p.MaxTargets = 2
	_, err = BuildRecoveryPlan(p, accounts)
	require.Error(t, err)
}
func TestRecoveryPolicyRejectsAmbiguousAliasesAndSchedules(t *testing.T) {
	p := DefaultRecoveryPolicy()
	p.Groups[1].Aliases = []string{"GPT"}
	_, err := ValidateRecoveryPolicy(p)
	require.Error(t, err)
	p = DefaultRecoveryPolicy()
	p.Cron = "@every 1s"
	_, err = ValidateRecoveryPolicy(p)
	require.Error(t, err)
	p = DefaultRecoveryPolicy()
	p.Timezone = "Bad/Timezone"
	_, err = ValidateRecoveryPolicy(p)
	require.Error(t, err)
}

type recoveryTestRepo struct {
	RecoveryRepository
	raw       string
	mu        sync.Mutex
	finished  *RecoveryRun
	writes    int
	finishErr error
}
type recoveryTestLease struct{ unlock func() }

func (l recoveryTestLease) Check(ctx context.Context) error            { return ctx.Err() }
func (l recoveryTestLease) Close()                                     { l.unlock() }
func (r *recoveryTestRepo) LoadConfig(context.Context) (string, error) { return r.raw, nil }
func (r *recoveryTestRepo) SaveConfig(_ context.Context, previous, next string) error {
	if r.raw != previous {
		return ErrRecoveryConflict
	}
	r.raw = next
	r.writes++
	return nil
}
func (r *recoveryTestRepo) Acquire(context.Context) (RecoveryLease, error) {
	if !r.mu.TryLock() {
		return nil, ErrRecoveryBusy
	}
	return recoveryTestLease{r.mu.Unlock}, nil
}
func (r *recoveryTestRepo) BeginRun(_ context.Context, trigger string, slot *time.Time, id, revision string) (*RecoveryRun, error) {
	return &RecoveryRun{ID: 1, Trigger: trigger, StartedAt: time.Now()}, nil
}
func (r *recoveryTestRepo) FinishRun(_ context.Context, run *RecoveryRun) error {
	r.finished = run
	return r.finishErr
}
func (r *recoveryTestRepo) PruneRuns(context.Context, int, int) error { return nil }

type recoveryTestAccounts struct {
	accounts []Account
	calls    []int64
	fail     int64
	change   int64
}

func (a *recoveryTestAccounts) ListAllWithFilters(context.Context, string, string, string, string, int64, string) ([]Account, error) {
	return a.accounts, nil
}
func (a *recoveryTestAccounts) GetByID(_ context.Context, id int64) (*Account, error) {
	for _, v := range a.accounts {
		if v.ID == id {
			if a.change == id {
				v.Name = "renamed-other"
			}
			return &v, nil
		}
	}
	return nil, errors.New("missing")
}
func (a *recoveryTestAccounts) ApplyScheduledRecovery(_ context.Context, account *Account, enabled bool) (bool, error) {
	a.calls = append(a.calls, account.ID)
	if account.ID == a.fail {
		return false, errors.New("failed")
	}
	return true, nil
}
func recoveryTestService(t *testing.T, p RecoveryPolicy, accounts []Account) (*AgentRouterRecoveryService, *recoveryTestRepo, *recoveryTestAccounts) {
	t.Helper()
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	r := &recoveryTestRepo{raw: string(raw)}
	a := &recoveryTestAccounts{accounts: accounts}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &AgentRouterRecoveryService{repo: r, accounts: a, states: a, ctx: ctx, cancel: cancel}, r, a
}
func TestRecoveryEnablesBeforeDisablingAndPreservesGroupOnFailure(t *testing.T) {
	for _, fail := range []int64{0, 2} {
		t.Run(string(rune('a'+fail)), func(t *testing.T) {
			p := recoveryTestPolicy()
			s, r, a := recoveryTestService(t, p, []Account{recoveryTestAccount(1, "a-gpt", "1"), recoveryTestAccount(2, "b-gpt", "2")})
			a.fail = fail
			plan, err := s.Preview(context.Background(), p)
			require.NoError(t, err)
			run, err := s.Run(context.Background(), nil, strings.Repeat("r", 20), plan.Revision)
			require.NoError(t, err)
			require.Same(t, run, r.finished)
			if fail == 0 {
				require.Equal(t, []int64{2, 1}, a.calls)
				require.Equal(t, "success", run.Status)
			} else {
				require.Equal(t, []int64{2}, a.calls)
				require.Equal(t, "partial", run.Status)
				require.Empty(t, run.Outcome.Disabled)
			}
		})
	}
}
func TestRecoverySkipsChangedIdentityAndRejectsOverlappingRun(t *testing.T) {
	p := recoveryTestPolicy()
	s, _, a := recoveryTestService(t, p, []Account{recoveryTestAccount(1, "a-gpt", "1")})
	plan, err := s.Preview(context.Background(), p)
	require.NoError(t, err)
	a.change = 1
	run, err := s.Run(context.Background(), nil, strings.Repeat("r", 20), plan.Revision)
	require.NoError(t, err)
	require.Equal(t, "partial", run.Status)
	require.Empty(t, a.calls)
	lease, err := s.repo.Acquire(context.Background())
	require.NoError(t, err)
	defer lease.Close()
	_, err = s.Run(context.Background(), nil, strings.Repeat("r", 20), plan.Revision)
	require.ErrorIs(t, err, ErrRecoveryBusy)
}
func TestRecoverySaveRejectsStaleConfigurationAndDoesNotRun(t *testing.T) {
	p := recoveryTestPolicy()
	s, r, a := recoveryTestService(t, p, []Account{recoveryTestAccount(1, "a-gpt", "1")})
	plan, err := s.Preview(context.Background(), p)
	require.NoError(t, err)
	_, err = s.Save(context.Background(), p, "stale", plan.Revision)
	require.ErrorIs(t, err, ErrRecoveryConflict)
	require.Zero(t, r.writes)
	view, err := s.Config(context.Background())
	require.NoError(t, err)
	_, err = s.Save(context.Background(), p, view.Revision, plan.Revision)
	require.NoError(t, err)
	require.Equal(t, 1, r.writes)
	require.Empty(t, a.calls)
	require.Nil(t, r.finished)
}

func TestRecoveryKeepsOutcomeVisibleWhenHistoryCannotBeSaved(t *testing.T) {
	p := recoveryTestPolicy()
	s, r, _ := recoveryTestService(t, p, []Account{recoveryTestAccount(1, "a-gpt", "1")})
	r.finishErr = errors.New("synthetic storage failure")
	plan, err := s.Preview(context.Background(), p)
	require.NoError(t, err)
	run, err := s.Run(context.Background(), nil, strings.Repeat("r", 20), plan.Revision)
	require.Error(t, err)
	require.NotNil(t, run)
	require.Equal(t, []int64{1}, run.Outcome.Enabled)
	require.Contains(t, run.Warning, "不要重复执行")
	require.NotContains(t, run.Warning, "synthetic")
}
