-- Serialize every account ID allocation so explicit imports cannot realign a
-- sequence behind a concurrent automatic nextval. A single lock also avoids
-- cross-sequence lock-order deadlocks in transactions that insert both kinds.

CREATE OR REPLACE FUNCTION public.assign_account_id()
RETURNS TRIGGER
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $$
DECLARE
    account_sequence REGCLASS;
    sequence_last_value BIGINT;
    sequence_is_called BOOLEAN;
BEGIN
    -- Reject an invalid explicit ID before touching either sequence. Automatic
    -- IDs are checked after nextval below, as in the original allocator.
    IF NEW.id IS NOT NULL THEN
        IF NEW.platform = 'grok' AND NEW.id < 100000 THEN
            RAISE EXCEPTION 'Grok account ID must be at least 100000';
        END IF;
        IF NEW.platform <> 'grok' AND NEW.id >= 100000 THEN
            RAISE EXCEPTION 'non-Grok account ID must be below 100000';
        END IF;
    END IF;

    IF NEW.platform = 'grok' THEN
        account_sequence := format('%I.%I', TG_TABLE_SCHEMA, 'accounts_grok_id_seq')::REGCLASS;
    ELSE
        account_sequence := pg_get_serial_sequence(
            format('%I.%I', TG_TABLE_SCHEMA, TG_TABLE_NAME),
            'id'
        )::REGCLASS;
    END IF;
    IF account_sequence IS NULL THEN
        RAISE EXCEPTION 'no account ID sequence is configured for platform %', NEW.platform;
    END IF;

    -- Account creation is a low-frequency management operation. One global
    -- transaction-level lock trades negligible parallelism for deterministic
    -- ordering across both sequences and every trigger-driven writer.
    PERFORM pg_advisory_xact_lock(
        hashtextextended('sub2api.accounts.id_allocator', 0)
    );

    IF NEW.id IS NULL THEN
        NEW.id := nextval(account_sequence);
    ELSE
        -- Re-read only after acquiring the lock. setval is non-transactional,
        -- so never call it with a value below the latest observed state.
        EXECUTE format('SELECT last_value, is_called FROM %s', account_sequence)
            INTO sequence_last_value, sequence_is_called;
        IF NEW.id > sequence_last_value
           OR (NEW.id = sequence_last_value AND NOT sequence_is_called) THEN
            PERFORM setval(account_sequence, NEW.id, true);
        END IF;
    END IF;

    IF NEW.platform = 'grok' AND NEW.id < 100000 THEN
        RAISE EXCEPTION 'Grok account ID must be at least 100000';
    END IF;
    IF NEW.platform <> 'grok' AND NEW.id >= 100000 THEN
        RAISE EXCEPTION 'non-Grok account ID must be below 100000';
    END IF;

    RETURN NEW;
END $$;

COMMENT ON FUNCTION public.assign_account_id() IS
    'Globally serializes automatic account ID allocation and explicit-ID sequence alignment.';
