package migrations

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration177HardensExplicitAccountIDSequenceAlignment(t *testing.T) {
	entries, err := FS.ReadDir(".")
	require.NoError(t, err)

	allocatorIndex := -1
	hardeningIndex := -1
	for i, entry := range entries {
		switch entry.Name() {
		case "175_grok_account_id_allocator.sql":
			allocatorIndex = i
		case "177_grok_account_id_allocator_hardening.sql":
			hardeningIndex = i
		}
	}
	require.NotEqual(t, -1, allocatorIndex)
	require.NotEqual(t, -1, hardeningIndex)
	require.Less(t, allocatorIndex, hardeningIndex)

	content, err := FS.ReadFile("177_grok_account_id_allocator_hardening.sql")
	require.NoError(t, err)

	sql := string(content)
	validationIndex := strings.Index(sql, "IF NEW.id IS NOT NULL THEN")
	lockStatement := "hashtextextended('sub2api.accounts.id_allocator', 0)"
	lockIndex := strings.Index(sql, lockStatement)
	automaticBranchIndex := strings.Index(sql, "IF NEW.id IS NULL THEN")
	nextvalIndex := strings.Index(sql, "NEW.id := nextval(account_sequence)")
	readIndex := strings.Index(sql, "SELECT last_value, is_called FROM %s")
	setvalIndex := strings.Index(sql, "PERFORM setval(account_sequence, NEW.id, true)")
	require.GreaterOrEqual(t, validationIndex, 0)
	require.GreaterOrEqual(t, lockIndex, 0)
	require.GreaterOrEqual(t, automaticBranchIndex, 0)
	require.GreaterOrEqual(t, nextvalIndex, 0)
	require.GreaterOrEqual(t, readIndex, 0)
	require.GreaterOrEqual(t, setvalIndex, 0)
	require.Less(t, validationIndex, lockIndex, "explicit ID range validation must precede sequence locking or mutation")
	require.Less(t, lockIndex, automaticBranchIndex, "automatic and explicit allocation must share the global allocator lock")
	require.Less(t, automaticBranchIndex, nextvalIndex)
	require.Less(t, lockIndex, readIndex, "sequence state must be read after acquiring the advisory lock")
	require.Less(t, readIndex, setvalIndex)
	require.Equal(t, 1, strings.Count(sql, lockStatement))

	require.Contains(t, sql, "CREATE OR REPLACE FUNCTION public.assign_account_id()")
	require.Contains(t, sql, "PERFORM pg_advisory_xact_lock(")
	require.NotContains(t, sql, "account_sequence::OID")
	require.Contains(t, sql, "IF NEW.id > sequence_last_value")
	require.Contains(t, sql, "OR (NEW.id = sequence_last_value AND NOT sequence_is_called)")
	require.Contains(t, sql, "IF NEW.id IS NULL THEN\n        NEW.id := nextval(account_sequence);")
	require.NotContains(t, sql, "DROP TRIGGER")
	require.NotContains(t, sql, "ALTER TABLE")
}

func TestMigration177ChecksumUsesRunnerTrimmedContent(t *testing.T) {
	content, err := FS.ReadFile("177_grok_account_id_allocator_hardening.sql")
	require.NoError(t, err)

	trimmed := strings.TrimSpace(string(content))
	require.NotEmpty(t, trimmed)
	sum := sha256.Sum256([]byte(trimmed))

	// Keep this value in sync until the migration is first deployed. Once it is
	// applied, both this assertion and schema_migrations enforce immutability.
	require.Equal(t, "723f1216263f87b75f88425bc6ce0892fcc1254551fa028364b3a507f8e725af", hex.EncodeToString(sum[:]))
}
