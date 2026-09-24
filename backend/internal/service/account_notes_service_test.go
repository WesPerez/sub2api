package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

type notesTestRepository struct {
	AccountRepository
	account Account
	writes  int
	race    bool
}

func (r *notesTestRepository) GetByID(context.Context, int64) (*Account, error) {
	copy := r.account
	return &copy, nil
}
func (r *notesTestRepository) UpdateNotesIfUnchanged(_ context.Context, _ *Account, notes *string) (bool, error) {
	if r.race {
		return false, nil
	}
	r.writes++
	r.account.Notes = notes
	return true, nil
}

func TestAccountNotesConditionalUpdateAndMinimalProjection(t *testing.T) {
	ctx := context.Background()
	old, next := "original", "updated"
	repo := &notesTestRepository{account: Account{ID: 1, Name: "example", Type: AccountTypeAPIKey,
		Platform: PlatformOpenAI, Credentials: map[string]any{"base_url": "https://example.test", "api_key": "synthetic-secret"}, Notes: &old}}
	svc := NewAccountNotesService(repo)
	view, err := svc.Get(ctx, 1)
	require.NoError(t, err)
	data, err := json.Marshal(view)
	require.NoError(t, err)
	require.NotContains(t, string(data), "synthetic-secret")
	require.NotContains(t, string(data), "credentials")
	_, err = svc.Update(ctx, 1, "", &next)
	require.ErrorIs(t, err, ErrAccountNotesConflict)
	require.Zero(t, repo.writes)
	updated, err := svc.Update(ctx, 1, view.Revision, &next)
	require.NoError(t, err)
	require.Equal(t, &next, updated.Notes)
	require.NotEqual(t, view.Revision, updated.Revision)
	_, err = svc.Update(ctx, 1, view.Revision, &old)
	require.ErrorIs(t, err, ErrAccountNotesConflict)
	require.Equal(t, 1, repo.writes)
	repo.race = true
	_, err = svc.Update(ctx, 1, updated.Revision, &old)
	require.ErrorIs(t, err, ErrAccountNotesConflict)
	require.Equal(t, &next, repo.account.Notes)
}

func TestAccountNotesRevisionTracksIdentityButNotRuntime(t *testing.T) {
	a := Account{ID: 1, Name: "example", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "synthetic-original"}}
	before := accountNotesRevision(&a)
	a.Schedulable = true
	a.Status = StatusActive
	require.Equal(t, before, accountNotesRevision(&a))
	a.Credentials["api_key"] = "synthetic-replaced"
	require.NotEqual(t, before, accountNotesRevision(&a))
}
