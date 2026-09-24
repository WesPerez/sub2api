package service

import (
	"context"
	"errors"
	"time"
)

var (
	ErrRecoveryConflict = errors.New("配置或账号已变化，请重新预览")
	ErrRecoveryBusy     = errors.New("账号恢复正在执行，请等待本轮结束")
	ErrRecoveryInvalid  = errors.New("账号恢复配置无效")
)

// RecoveryPolicy is owned by the application. Saving it never runs a job.
type RecoveryPolicy struct {
	Version            int             `json:"version"`
	Enabled            bool            `json:"enabled"`
	SiteHost           string          `json:"site_host"`
	Cron               string          `json:"cron"`
	Timezone           string          `json:"timezone"`
	MaxTargets         int             `json:"max_targets"`
	BalanceMaxAgeHours int             `json:"balance_max_age_hours"`
	RetentionDays      int             `json:"retention_days"`
	MaxRuns            int             `json:"max_runs"`
	Groups             []RecoveryGroup `json:"groups"`
}

type RecoveryGroup struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Aliases []string `json:"aliases"`
	TopN    int      `json:"top_n"`
}

type RecoveryPolicyView struct {
	Policy    RecoveryPolicy `json:"policy"`
	Revision  string         `json:"revision"`
	NextRunAt *time.Time     `json:"next_run_at"`
}

// RecoveryCandidate deliberately excludes credentials, notes and proxy secrets.
type RecoveryCandidate struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Platform    string  `json:"platform"`
	Group       string  `json:"group"`
	Balance     *string `json:"balance"`
	Schedulable bool    `json:"schedulable"`
	Action      string  `json:"action"`
	Reason      string  `json:"reason,omitempty"`
	identity    string
}

type RecoveryGroupPlan struct {
	ID       string  `json:"id"`
	Label    string  `json:"label"`
	TopN     int     `json:"top_n"`
	Matched  int     `json:"matched"`
	Eligible int     `json:"eligible"`
	Selected []int64 `json:"selected"`
}

type RecoveryPlan struct {
	Revision string              `json:"revision"`
	Accounts []RecoveryCandidate `json:"accounts"`
	Groups   []RecoveryGroupPlan `json:"groups"`
	Warnings []string            `json:"warnings"`
	Matched  int                 `json:"matched"`
	Selected int                 `json:"selected"`
}

type RecoveryAccountError struct {
	ID      int64  `json:"id"`
	Stage   string `json:"stage"`
	Message string `json:"message"`
}

type RecoveryOutcome struct {
	Plan      RecoveryPlan           `json:"plan"`
	Recovered []int64                `json:"recovered"`
	Enabled   []int64                `json:"enabled"`
	Disabled  []int64                `json:"disabled"`
	Errors    []RecoveryAccountError `json:"errors"`
}

type RecoveryRun struct {
	ID             int64           `json:"id"`
	Trigger        string          `json:"trigger"`
	Status         string          `json:"status"`
	ScheduledFor   *time.Time      `json:"scheduled_for"`
	StartedAt      time.Time       `json:"started_at"`
	FinishedAt     *time.Time      `json:"finished_at"`
	ConfigRevision string          `json:"config_revision"`
	Outcome        RecoveryOutcome `json:"outcome"`
	Error          string          `json:"error,omitempty"`
	Warning        string          `json:"warning,omitempty"`
}

// The lease uses a dedicated database session; Check detects session loss before
// each side effect. Scheduled slots also have a permanent unique claim.
type RecoveryLease interface {
	Check(context.Context) error
	Close()
}

type RecoveryRepository interface {
	LoadConfig(context.Context) (string, error)
	SaveConfig(ctx context.Context, previous, next string) error
	Acquire(context.Context) (RecoveryLease, error)
	BeginRun(ctx context.Context, trigger string, slot *time.Time, requestID, revision string) (*RecoveryRun, error)
	FinishRun(context.Context, *RecoveryRun) error
	ListRuns(ctx context.Context, limit int) ([]RecoveryRun, error)
	PruneRuns(ctx context.Context, days, keep int) error
}

type recoveryAccounts interface {
	ListAllWithFilters(context.Context, string, string, string, string, int64, string) ([]Account, error)
	GetByID(context.Context, int64) (*Account, error)
}

type recoveryStateActions interface {
	ApplyScheduledRecovery(context.Context, *Account, bool) (bool, error)
}
