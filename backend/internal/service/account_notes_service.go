package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

var (
	ErrAccountNotesConflict = errors.New("账号或备注已变化，请重新读取后再同步")
	ErrAccountNotesInvalid  = errors.New("备注格式无效或超过 20000 个字符")
)

// ConditionalAccountNotesRepository is a narrow write port. An integration
// cannot update credentials, account identity or scheduling through this API.
type ConditionalAccountNotesRepository interface {
	UpdateNotesIfUnchanged(context.Context, *Account, *string) (bool, error)
}

type ConditionalAccountBalanceRepository interface {
	UpdateNotesAndBalanceIfUnchanged(context.Context, *Account, *string, any) (bool, error)
}

type AccountNotesView struct {
	ID              int64                       `json:"id"`
	Name            string                      `json:"name"`
	Platform        string                      `json:"platform"`
	Type            string                      `json:"type"`
	BaseURL         string                      `json:"base_url"`
	Notes           *string                     `json:"notes"`
	Revision        string                      `json:"notes_revision"`
	BalanceSnapshot *IntegrationBalanceSnapshot `json:"balance_snapshot"`
	BalanceState    string                      `json:"balance_state"`
}

type AccountNotesService struct{ accounts AccountRepository }

func NewAccountNotesService(accounts AccountRepository) *AccountNotesService {
	return &AccountNotesService{accounts: accounts}
}

func accountNotesRevision(a *Account) string {
	// Only identity and note changes invalidate a sync. Transient rate limits and
	// last-used timestamps must not prevent a busy account's balance refresh.
	value, _ := json.Marshal(struct {
		ID                   int64
		Name, Platform, Type string
		Parent               *int64
		Credentials          map[string]any
		Notes                *string
		Balance              any
	}{a.ID, a.Name, a.Platform, a.Type, a.ParentAccountID, a.Credentials, a.Notes, a.Extra[IntegrationBalanceKey]})
	digest := sha256.Sum256(value)
	return `"` + hex.EncodeToString(digest[:]) + `"`
}

func accountNotesView(a *Account) AccountNotesView {
	baseURL, _ := a.Credentials["base_url"].(string)
	balance, state := AccountIntegrationBalance(a)
	return AccountNotesView{ID: a.ID, Name: a.Name, Platform: a.Platform, Type: a.Type,
		BaseURL: baseURL, Notes: a.Notes, Revision: accountNotesRevision(a), BalanceSnapshot: balance, BalanceState: state}
}

func (s *AccountNotesService) List(ctx context.Context, params pagination.PaginationParams) ([]AccountNotesView, *pagination.PaginationResult, error) {
	rows, page, err := s.accounts.ListWithFilters(ctx, params, "", AccountTypeAPIKey, "", "", 0, "")
	if err != nil {
		return nil, nil, err
	}
	views := make([]AccountNotesView, 0, len(rows))
	for i := range rows {
		views = append(views, accountNotesView(&rows[i]))
	}
	return views, page, nil
}

func (s *AccountNotesService) Get(ctx context.Context, id int64) (AccountNotesView, error) {
	a, err := s.accounts.GetByID(ctx, id)
	if err != nil {
		return AccountNotesView{}, err
	}
	if a == nil || a.Type != AccountTypeAPIKey {
		return AccountNotesView{}, ErrAccountNotFound
	}
	return accountNotesView(a), nil
}

func (s *AccountNotesService) Update(ctx context.Context, id int64, revision string, notes *string) (AccountNotesView, error) {
	return s.UpdateWithBalance(ctx, id, revision, notes, nil)
}

func (s *AccountNotesService) UpdateWithBalance(ctx context.Context, id int64, revision string, notes *string, input *IntegrationBalanceInput) (AccountNotesView, error) {
	if notes != nil && (!utf8.ValidString(*notes) || utf8.RuneCountInString(*notes) > 20000 || strings.ContainsRune(*notes, 0)) {
		return AccountNotesView{}, ErrAccountNotesInvalid
	}
	a, err := s.accounts.GetByID(ctx, id)
	if err != nil {
		return AccountNotesView{}, err
	}
	if a == nil || a.Type != AccountTypeAPIKey {
		return AccountNotesView{}, ErrAccountNotFound
	}
	if revision == "" || revision != accountNotesRevision(a) {
		return AccountNotesView{}, ErrAccountNotesConflict
	}
	var changed bool
	var balance any
	if input != nil {
		balance, err = prepareIntegrationBalance(a, input)
		if err != nil {
			return AccountNotesView{}, err
		}
		writer, ok := s.accounts.(ConditionalAccountBalanceRepository)
		if !ok {
			return AccountNotesView{}, errors.New("conditional balance repository unavailable")
		}
		changed, err = writer.UpdateNotesAndBalanceIfUnchanged(ctx, a, notes, balance)
	} else {
		writer, ok := s.accounts.(ConditionalAccountNotesRepository)
		if !ok {
			return AccountNotesView{}, errors.New("conditional account notes repository unavailable")
		}
		changed, err = writer.UpdateNotesIfUnchanged(ctx, a, notes)
	}
	if err != nil {
		return AccountNotesView{}, err
	}
	if !changed {
		return AccountNotesView{}, ErrAccountNotesConflict
	}
	// Describe this committed write, even if a subsequent editor has already
	// made another change. A later use of this revision will then conflict.
	a.Notes = notes
	if input != nil {
		extra := make(map[string]any, len(a.Extra)+1)
		for key, value := range a.Extra {
			extra[key] = value
		}
		extra[IntegrationBalanceKey] = balance
		a.Extra = extra
	}
	return accountNotesView(a), nil
}
