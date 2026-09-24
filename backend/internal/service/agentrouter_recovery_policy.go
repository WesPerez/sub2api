package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/robfig/cron/v3"
	"github.com/shopspring/decimal"
)

var recoveryIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
var recoveryAliasPattern = regexp.MustCompile(`^[a-zA-Z0-9_]{1,32}$`)
var recoveryBalancePattern = regexp.MustCompile(`余额\s*[:：]?\s*\$?(-?\d+(?:\.\d+)?)`)

func invalidRecovery(format string, args ...any) error {
	return fmt.Errorf("%w：%s", ErrRecoveryInvalid, fmt.Sprintf(format, args...))
}

func DefaultRecoveryPolicy() RecoveryPolicy {
	return RecoveryPolicy{Version: 1, SiteHost: "agentrouter.org", Cron: "0 * * * *", Timezone: "Asia/Shanghai",
		MaxTargets: 200, BalanceMaxAgeHours: 6, RetentionDays: 31, MaxRuns: 1000, Groups: []RecoveryGroup{
			{ID: "gpt", Label: "GPT", Aliases: []string{"gpt"}, TopN: 3},
			{ID: "claude", Label: "Claude", Aliases: []string{"claude"}, TopN: 3},
			{ID: "glm", Label: "GLM", Aliases: []string{"glm"}, TopN: 3},
			{ID: "deepseek", Label: "DeepSeek", Aliases: []string{"deepseek"}, TopN: 3},
		}}
}

func ValidateRecoveryPolicy(p RecoveryPolicy) (RecoveryPolicy, error) {
	if p.BalanceMaxAgeHours == 0 {
		p.BalanceMaxAgeHours = 6
	}
	if p.BalanceMaxAgeHours < 1 || p.BalanceMaxAgeHours > 168 {
		return p, invalidRecovery("余额有效期应为 1 至 168 小时")
	}
	if p.Version != 1 || p.SiteHost != "agentrouter.org" {
		return p, invalidRecovery("目前只支持已核实的 AgentRouter 站点")
	}
	if p.MaxTargets < 1 || p.MaxTargets > 500 {
		return p, invalidRecovery("最大匹配账号数应为 1 至 500")
	}
	if p.RetentionDays < 1 || p.RetentionDays > 180 || p.MaxRuns < 20 || p.MaxRuns > 5000 {
		return p, invalidRecovery("记录保留应为 1 至 180 天、20 至 5000 条")
	}
	if len(p.Groups) < 1 || len(p.Groups) > 12 {
		return p, invalidRecovery("请配置 1 至 12 种账号类型")
	}
	if _, err := recoverySchedule(p); err != nil {
		return p, err
	}
	ids, aliases := map[string]bool{}, map[string]bool{}
	for i := range p.Groups {
		g := &p.Groups[i]
		g.Label = strings.TrimSpace(g.Label)
		if !recoveryIDPattern.MatchString(g.ID) || ids[g.ID] {
			return p, invalidRecovery("类型 ID 必须唯一，由小写字母、数字和下划线组成")
		}
		if len([]rune(g.Label)) < 1 || len([]rune(g.Label)) > 40 || strings.IndexFunc(g.Label, unicode.IsControl) >= 0 {
			return p, invalidRecovery("类型名称应为 1 至 40 个可见字符")
		}
		if g.TopN < 1 || g.TopN > 20 {
			return p, invalidRecovery("每类开放数量应为 1 至 20")
		}
		if len(g.Aliases) < 1 || len(g.Aliases) > 10 {
			return p, invalidRecovery("每类需配置 1 至 10 个后缀别名")
		}
		ids[g.ID] = true
		for j, a := range g.Aliases {
			a = strings.ToLower(a)
			if !recoveryAliasPattern.MatchString(a) || aliases[a] {
				return p, invalidRecovery("后缀别名不能重复，只接受字母、数字和下划线")
			}
			aliases[a], g.Aliases[j] = true, a
		}
	}
	return p, nil
}

func recoverySchedule(p RecoveryPolicy) (cron.Schedule, error) {
	if len(p.Cron) > 100 || len(strings.Fields(p.Cron)) != 5 {
		return nil, invalidRecovery("执行计划需为五段 Cron 表达式")
	}
	if _, err := time.LoadLocation(p.Timezone); err != nil {
		return nil, invalidRecovery("请选择有效时区")
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	s, err := parser.Parse("CRON_TZ=" + p.Timezone + " " + p.Cron)
	if err != nil {
		return nil, invalidRecovery("执行计划无效")
	}
	return s, nil
}

func recoveryDigest(value any) string {
	b, _ := json.Marshal(value)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func recoveryIdentity(a *Account) string {
	return recoveryDigest(struct {
		ID                   int64
		Name, Platform, Type string
		Credentials          map[string]any
		Groups               []int64
		Proxy                *int64
		Balance              any
	}{
		a.ID, a.Name, a.Platform, a.Type, a.Credentials, a.GroupIDs, a.ProxyID, a.Extra[IntegrationBalanceKey]})
}

func recoverySiteAccount(a *Account) bool {
	base, ok := a.Credentials["base_url"].(string)
	if !ok || a.ParentAccountID != nil || a.Type != AccountTypeAPIKey {
		return false
	}
	u, err := url.Parse(base)
	return err == nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), "agentrouter.org") && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Port() == "" || u.Port() == "443")
}

func parseRecoveryBalance(notes *string) *string {
	if notes == nil {
		return nil
	}
	m := recoveryBalancePattern.FindAllStringSubmatch(*notes, -1)
	if len(m) == 0 {
		return nil
	}
	d, err := decimal.NewFromString(m[len(m)-1][1])
	if err != nil {
		return nil
	}
	s := d.String()
	return &s
}

func recoveryAccountBalance(a *Account, maxAgeHours int) *string {
	value, state := AccountIntegrationBalance(a)
	if state == "legacy" {
		return parseRecoveryBalance(a.Notes)
	}
	if state != "valid" {
		return nil
	}
	if maxAgeHours == 0 {
		maxAgeHours = 6
	}
	at, _ := time.Parse(time.RFC3339Nano, value.ObservedAt)
	if time.Since(at) > time.Duration(maxAgeHours)*time.Hour {
		return nil
	}
	amount := value.Amount
	return &amount
}

// BuildRecoveryPlan is pure: it does not read credentials from other projects,
// make upstream requests or mutate account state.
func BuildRecoveryPlan(p RecoveryPolicy, accounts []Account) (RecoveryPlan, error) {
	plan := RecoveryPlan{Accounts: []RecoveryCandidate{}, Groups: []RecoveryGroupPlan{}, Warnings: []string{}}
	aliases := map[string]string{}
	for _, g := range p.Groups {
		for _, a := range g.Aliases {
			aliases[a] = g.ID
		}
	}
	seen := map[int64]bool{}
	for i := range accounts {
		a := &accounts[i]
		if !recoverySiteAccount(a) {
			continue
		}
		if a.ID <= 0 || seen[a.ID] {
			return plan, invalidRecovery("账号集合包含无效或重复 ID")
		}
		seen[a.ID] = true
		name := strings.ToLower(a.Name)
		suffix := name[strings.LastIndex(name, "-")+1:]
		c := RecoveryCandidate{ID: a.ID, Name: a.Name, Platform: a.Platform, Group: aliases[suffix], Balance: recoveryAccountBalance(a, p.BalanceMaxAgeHours), Schedulable: a.Schedulable, Action: "preserve", identity: recoveryIdentity(a)}
		if _, state := AccountIntegrationBalance(a); state != "legacy" && c.Balance == nil {
			c.Reason = "余额快照已失效或过期，保持原开关并等待刷新"
		}
		if c.Group == "" {
			c.Reason = "未匹配类型后缀，保持原开关"
		} else if a.Status != StatusActive && a.Status != StatusError {
			c.Reason = "账号已由管理员停用，保持原状态"
		}
		plan.Accounts = append(plan.Accounts, c)
	}
	plan.Matched = len(plan.Accounts)
	if plan.Matched > p.MaxTargets {
		return plan, invalidRecovery("匹配到 %d 个账号，超过配置的 %d 个安全上限", plan.Matched, p.MaxTargets)
	}
	sort.Slice(plan.Accounts, func(i, j int) bool { return plan.Accounts[i].ID < plan.Accounts[j].ID })
	for _, g := range p.Groups {
		gp := RecoveryGroupPlan{ID: g.ID, Label: g.Label, TopN: g.TopN, Selected: []int64{}}
		eligible := []int{}
		for i := range plan.Accounts {
			c := &plan.Accounts[i]
			if c.Group != g.ID {
				continue
			}
			gp.Matched++
			if c.Reason == "" && c.Balance != nil {
				eligible = append(eligible, i)
			}
		}
		gp.Eligible = len(eligible)
		if len(eligible) == 0 {
			plan.Warnings = append(plan.Warnings, g.Label+"：未匹配到可用余额，保留该类原开关")
		} else {
			sort.Slice(eligible, func(i, j int) bool {
				a, b := plan.Accounts[eligible[i]], plan.Accounts[eligible[j]]
				da, _ := decimal.NewFromString(*a.Balance)
				db, _ := decimal.NewFromString(*b.Balance)
				if da.Equal(db) {
					return a.ID < b.ID
				}
				return da.GreaterThan(db)
			})
			for i := range plan.Accounts {
				c := &plan.Accounts[i]
				if c.Group == g.ID && c.Reason == "" {
					c.Action = "disable"
					if c.Balance == nil {
						c.Reason = "余额未识别，本轮不入选"
					}
				}
			}
			for i, index := range eligible {
				if i >= g.TopN {
					break
				}
				plan.Accounts[index].Action = "enable"
				gp.Selected = append(gp.Selected, plan.Accounts[index].ID)
			}
		}
		plan.Selected += len(gp.Selected)
		plan.Groups = append(plan.Groups, gp)
	}
	identities := make([]string, len(plan.Accounts))
	for i, c := range plan.Accounts {
		identities[i] = c.identity
		if c.Group == "" {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("账号 %d 未归类，保持原开关", c.ID))
		}
	}
	plan.Revision = recoveryDigest(struct {
		Policy     RecoveryPolicy
		Plan       RecoveryPlan
		Identities []string
	}{p, plan, identities})
	return plan, nil
}
