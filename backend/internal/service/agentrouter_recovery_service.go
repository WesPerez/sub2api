package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/robfig/cron/v3"
)

// AgentRouterRecoveryService owns orchestration only. Account state transitions
// remain in the existing account and rate-limit services.
type AgentRouterRecoveryService struct {
	repo     RecoveryRepository
	accounts recoveryAccounts
	states   recoveryStateActions
	ctx      context.Context
	cancel   context.CancelFunc
	cron     *cron.Cron
	mu       sync.Mutex
	stopped  bool
	wg       sync.WaitGroup
}

func NewAgentRouterRecoveryService(repo RecoveryRepository, accounts AccountRepository, states *RateLimitService) *AgentRouterRecoveryService {
	ctx, cancel := context.WithCancel(context.Background())
	return &AgentRouterRecoveryService{repo: repo, accounts: accounts, states: states, ctx: ctx, cancel: cancel}
}

func (s *AgentRouterRecoveryService) Config(ctx context.Context) (RecoveryPolicyView, error) {
	raw, err := s.repo.LoadConfig(ctx)
	if err != nil {
		return RecoveryPolicyView{}, err
	}
	p := DefaultRecoveryPolicy()
	if raw != "" {
		if err = json.Unmarshal([]byte(raw), &p); err != nil {
			return RecoveryPolicyView{}, fmt.Errorf("账号恢复配置无法读取")
		}
	}
	p, err = ValidateRecoveryPolicy(p)
	view := RecoveryPolicyView{Policy: p, Revision: recoveryDigest(raw)}
	if err != nil {
		return view, err
	}
	if p.Enabled {
		schedule, _ := recoverySchedule(p)
		next := schedule.Next(time.Now())
		view.NextRunAt = &next
	}
	return view, nil
}

func (s *AgentRouterRecoveryService) Preview(ctx context.Context, p RecoveryPolicy) (RecoveryPlan, error) {
	p, err := ValidateRecoveryPolicy(p)
	if err != nil {
		return RecoveryPlan{}, err
	}
	accounts, err := s.accounts.ListAllWithFilters(ctx, "", "", "", "", 0, "")
	if err != nil {
		return RecoveryPlan{}, fmt.Errorf("读取账号失败，请稍后重试")
	}
	return BuildRecoveryPlan(p, accounts)
}

func (s *AgentRouterRecoveryService) Save(ctx context.Context, p RecoveryPolicy, revision, previewRevision string) (RecoveryPolicyView, error) {
	p, err := ValidateRecoveryPolicy(p)
	if err != nil {
		return RecoveryPolicyView{}, err
	}
	lease, err := s.repo.Acquire(ctx)
	if err != nil {
		return RecoveryPolicyView{}, err
	}
	defer lease.Close()
	raw, err := s.repo.LoadConfig(ctx)
	if err != nil {
		return RecoveryPolicyView{}, err
	}
	if recoveryDigest(raw) != revision {
		return RecoveryPolicyView{}, ErrRecoveryConflict
	}
	plan, err := s.Preview(ctx, p)
	if err != nil {
		return RecoveryPolicyView{}, err
	}
	if plan.Revision != previewRevision {
		return RecoveryPolicyView{}, ErrRecoveryConflict
	}
	if p.Enabled {
		for _, g := range plan.Groups {
			if g.Eligible == 0 {
				return RecoveryPolicyView{}, invalidRecovery("%s 没有可用余额候选，请先核对后缀和备注", g.Label)
			}
		}
	}
	data, err := json.Marshal(p)
	if err != nil {
		return RecoveryPolicyView{}, err
	}
	if err = lease.Check(ctx); err != nil {
		return RecoveryPolicyView{}, err
	}
	if err = s.repo.SaveConfig(ctx, raw, string(data)); err != nil {
		return RecoveryPolicyView{}, err
	}
	return s.Config(ctx)
}

func (s *AgentRouterRecoveryService) History(ctx context.Context) ([]RecoveryRun, error) {
	return s.repo.ListRuns(ctx, 30)
}

var recoveryRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,100}$`)

func (s *AgentRouterRecoveryService) Run(ctx context.Context, slot *time.Time, requestID, expectedPlan string) (*RecoveryRun, error) {
	if slot == nil && (!recoveryRequestIDPattern.MatchString(requestID) || expectedPlan == "") {
		return nil, invalidRecovery("请先预览，并提供有效的执行请求标识")
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil, fmt.Errorf("应用正在停止")
	}
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	lease, err := s.repo.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer lease.Close()
	view, err := s.Config(ctx)
	if err != nil {
		return nil, err
	}
	if slot != nil && !view.Policy.Enabled {
		return nil, nil
	}
	plan, err := s.Preview(ctx, view.Policy)
	if err != nil {
		return nil, err
	}
	if slot == nil && plan.Revision != expectedPlan {
		return nil, ErrRecoveryConflict
	}
	trigger := "manual"
	if slot != nil {
		trigger = "schedule"
	}
	run, err := s.repo.BeginRun(ctx, trigger, slot, requestID, view.Revision)
	if err != nil {
		return nil, err
	}
	run.Outcome = RecoveryOutcome{Plan: plan, Recovered: []int64{}, Enabled: []int64{}, Disabled: []int64{}, Errors: []RecoveryAccountError{}}
	addError := func(id int64, stage, message string) {
		run.Outcome.Errors = append(run.Outcome.Errors, RecoveryAccountError{ID: id, Stage: stage, Message: message})
	}
	ordered := append([]RecoveryCandidate(nil), plan.Accounts...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Action == "enable" && ordered[j].Action != "enable" })
	opened := map[string]int{}
	selected := map[string]int{}
	for _, group := range plan.Groups {
		selected[group.ID] = len(group.Selected)
	}
	for _, candidate := range ordered {
		if candidate.Action == "preserve" {
			continue
		}
		if candidate.Action == "disable" && opened[candidate.Group] < selected[candidate.Group] {
			addError(candidate.ID, "schedule", "该类入选账号未全部开放，保留原开关")
			continue
		}
		if err = ctx.Err(); err != nil {
			break
		}
		if err = lease.Check(ctx); err != nil {
			break
		}
		current, readErr := s.accounts.GetByID(ctx, candidate.ID)
		if readErr != nil || current == nil {
			addError(candidate.ID, "identity", "无法核对账号，已跳过")
			continue
		}
		if recoveryIdentity(current) != candidate.identity || parseBalanceIdentity(current, view.Policy.BalanceMaxAgeHours) != candidate.BalanceValue() {
			addError(candidate.ID, "identity", "账号身份或余额已变化，已跳过")
			continue
		}
		// Never clear a manually disabled account's state after the preview.
		if current.Status != StatusActive && current.Status != StatusError {
			addError(candidate.ID, "identity", "账号已被停用，已跳过")
			continue
		}
		if err = lease.Check(ctx); err != nil {
			break
		}
		changed, applyErr := s.states.ApplyScheduledRecovery(ctx, current, candidate.Action == "enable")
		if applyErr != nil {
			addError(candidate.ID, "schedule", "恢复状态或更新调度失败")
			continue
		}
		if !changed {
			addError(candidate.ID, "identity", "账号在操作前发生变化，已跳过")
			continue
		}
		if candidate.Action == "enable" {
			run.Outcome.Recovered = append(run.Outcome.Recovered, candidate.ID)
		}
		if candidate.Action == "enable" {
			opened[candidate.Group]++
			run.Outcome.Enabled = append(run.Outcome.Enabled, candidate.ID)
		} else {
			run.Outcome.Disabled = append(run.Outcome.Disabled, candidate.ID)
		}
	}
	run.Status = "success"
	if len(run.Outcome.Errors) > 0 || len(plan.Warnings) > 0 {
		run.Status = "partial"
	}
	if err != nil {
		run.Status = "interrupted"
		run.Error = "执行已中断，保留已完成的变更；下次执行重新核对"
	}
	finished := time.Now()
	run.FinishedAt = &finished
	finishCtx, finishCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer finishCancel()
	if finishErr := s.repo.FinishRun(finishCtx, run); finishErr != nil {
		run.Warning = "账号操作已结束，但执行记录保存失败；请先核对账号状态，不要重复执行"
		return run, fmt.Errorf("账号操作结束，但执行记录保存失败")
	}
	if pruneErr := s.repo.PruneRuns(finishCtx, view.Policy.RetentionDays, view.Policy.MaxRuns); pruneErr != nil {
		logger.LegacyPrintf("service.agentrouter_recovery", "history retention failed")
	}
	return run, err
}

func parseBalanceIdentity(a *Account, maxAgeHours int) string {
	value := recoveryAccountBalance(a, maxAgeHours)
	if value == nil {
		return ""
	}
	return *value
}
func (c RecoveryCandidate) BalanceValue() string {
	if c.Balance == nil {
		return ""
	}
	return *c.Balance
}

func (s *AgentRouterRecoveryService) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cron != nil || s.stopped {
		return
	}
	s.cron = cron.New()
	_, _ = s.cron.AddFunc("* * * * *", func() {
		now := time.Now().Truncate(time.Minute)
		view, err := s.Config(s.ctx)
		if err != nil {
			logger.LegacyPrintf("service.agentrouter_recovery", "configuration unavailable")
			return
		}
		if !view.Policy.Enabled {
			return
		}
		schedule, _ := recoverySchedule(view.Policy)
		if !schedule.Next(now.Add(-time.Minute)).Equal(now) {
			return
		}
		if _, err = s.Run(s.ctx, &now, "", ""); err != nil && !errors.Is(err, ErrRecoveryBusy) && !errors.Is(err, ErrRecoveryConflict) {
			logger.LegacyPrintf("service.agentrouter_recovery", "scheduled execution failed; inspect account recovery history")
		}
	})
	s.cron.Start()
}
func (s *AgentRouterRecoveryService) Stop() {
	s.mu.Lock()
	s.stopped = true
	s.cancel()
	c := s.cron
	s.mu.Unlock()
	if c != nil {
		<-c.Stop().Done()
	}
	s.wg.Wait()
}
