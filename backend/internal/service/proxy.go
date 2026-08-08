package service

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	FallbackModeNone   = "none"
	FallbackModeProxy  = "proxy"
	FallbackModeDirect = "direct"

	ProxyAccountIDTemplate = "{{account_id}}"
)

type Proxy struct {
	ID             int64
	Name           string
	Protocol       string
	Host           string
	Port           int
	Username       string
	Password       string
	Status         string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ExpiresAt      *time.Time
	FallbackMode   string
	BackupProxyID  *int64
	ExpiryWarnDays int
}

func (p *Proxy) IsActive() bool {
	return p.Status == StatusActive
}

// IsExpired 报告代理是否已过期（基于 expires_at，与 status 无关）。
func (p *Proxy) IsExpired(now time.Time) bool {
	return p.ExpiresAt != nil && !p.ExpiresAt.After(now)
}

func (p *Proxy) URL() string {
	return p.urlForUsername(p.usernameForAccount(0))
}

// ForAccount clones a shared proxy and binds its optional username template.
func (p *Proxy) ForAccount(accountID int64) *Proxy {
	if p == nil {
		return nil
	}
	bound := *p
	bound.Username = p.usernameForAccount(accountID)
	return &bound
}

func (p *Proxy) URLForAccount(accountID int64) string {
	bound := p.ForAccount(accountID)
	if bound == nil {
		return ""
	}
	return bound.URL()
}

func (p *Proxy) usernameForAccount(accountID int64) string {
	if !strings.Contains(p.Username, ProxyAccountIDTemplate) {
		return p.Username
	}
	replacement := "profile"
	if accountID > 0 {
		replacement = strconv.FormatInt(accountID, 10)
	} else if p.ID > 0 {
		replacement = "profile-" + strconv.FormatInt(p.ID, 10)
	}
	return strings.ReplaceAll(p.Username, ProxyAccountIDTemplate, replacement)
}

func (p *Proxy) urlForUsername(username string) string {
	u := &url.URL{
		Scheme: p.Protocol,
		Host:   net.JoinHostPort(p.Host, strconv.Itoa(p.Port)),
	}
	if username != "" && p.Password != "" {
		u.User = url.UserPassword(username, p.Password)
	}
	return u.String()
}

func ValidateProxyUsernameTemplate(username string) error {
	remaining := strings.ReplaceAll(username, ProxyAccountIDTemplate, "")
	if strings.ContainsAny(remaining, "{}") {
		return fmt.Errorf("unsupported proxy username template")
	}
	return nil
}

type ProxyWithAccountCount struct {
	Proxy
	AccountCount   int64
	LatencyMs      *int64
	LatencyStatus  string
	LatencyMessage string
	IPAddress      string
	Country        string
	CountryCode    string
	Region         string
	City           string
	QualityStatus  string
	QualityScore   *int
	QualityGrade   string
	QualitySummary string
	QualityChecked *int64
}

type ProxyAccountSummary struct {
	ID       int64
	Name     string
	Platform string
	Type     string
	Notes    *string
}
