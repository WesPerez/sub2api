-- Reserve account IDs below 100000 for non-Grok accounts and allocate Grok
-- accounts from an independent sequence. Existing low-ID Grok rows must be
-- remapped before this migration is applied.

DO $$
DECLARE
    normal_sequence REGCLASS;
    normal_last_value BIGINT;
    normal_is_called BOOLEAN;
    normal_next_value BIGINT;
    max_normal_id BIGINT;
BEGIN
    IF EXISTS (SELECT 1 FROM accounts WHERE platform = 'grok' AND id < 100000) THEN
        RAISE EXCEPTION 'low-ID Grok accounts still exist; run the reviewed Grok ID remap first';
    END IF;

    IF EXISTS (SELECT 1 FROM accounts WHERE platform <> 'grok' AND id >= 100000) THEN
        RAISE EXCEPTION 'non-Grok accounts already occupy the reserved Grok ID range';
    END IF;

    normal_sequence := pg_get_serial_sequence('accounts', 'id')::REGCLASS;
    IF normal_sequence IS NULL THEN
        RAISE EXCEPTION 'accounts.id has no owned sequence';
    END IF;

    EXECUTE format('SELECT last_value, is_called FROM %s', normal_sequence)
        INTO normal_last_value, normal_is_called;
    normal_next_value := CASE
        WHEN normal_is_called THEN normal_last_value + 1
        ELSE normal_last_value
    END;
    IF normal_next_value >= 100000 THEN
        RAISE EXCEPTION 'normal account sequence has reached the Grok ID range: next value %', normal_next_value;
    END IF;

    SELECT COALESCE(MAX(id), 0)
      INTO max_normal_id
      FROM accounts
      WHERE platform <> 'grok';
    IF normal_next_value <= max_normal_id THEN
        RAISE EXCEPTION 'normal account sequence next value % is not above existing account ID %', normal_next_value, max_normal_id;
    END IF;
END $$;

CREATE SEQUENCE IF NOT EXISTS accounts_grok_id_seq
    AS BIGINT
    INCREMENT BY 1
    MINVALUE 100000
    NO MAXVALUE
    START WITH 100000
    CACHE 1
    NO CYCLE;

ALTER SEQUENCE accounts_grok_id_seq
    INCREMENT BY 1
    MINVALUE 100000
    NO MAXVALUE
    CACHE 1
    NO CYCLE;

-- Never move an existing Grok sequence backwards. Align it only when an
-- already-remapped account has a higher ID than the sequence has issued.
DO $$
DECLARE
    max_grok_id BIGINT;
    grok_last_value BIGINT;
    grok_is_called BOOLEAN;
BEGIN
    SELECT MAX(id) INTO max_grok_id FROM accounts WHERE platform = 'grok';
    SELECT last_value, is_called
      INTO grok_last_value, grok_is_called
      FROM accounts_grok_id_seq;

    IF max_grok_id IS NOT NULL
       AND (NOT grok_is_called OR grok_last_value < max_grok_id) THEN
        PERFORM setval('accounts_grok_id_seq', max_grok_id, true);
    END IF;
END $$;

CREATE OR REPLACE FUNCTION assign_account_id()
RETURNS TRIGGER
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $$
DECLARE
    account_sequence REGCLASS;
    sequence_last_value BIGINT;
    sequence_is_called BOOLEAN;
BEGIN
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

    IF NEW.id IS NULL THEN
        NEW.id := nextval(account_sequence);
    ELSE
        -- Explicit IDs are used by database restores and controlled imports.
        -- Keep the owning sequence at or above the supplied value.
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

DROP TRIGGER IF EXISTS trg_assign_account_id ON accounts;
CREATE TRIGGER trg_assign_account_id
    BEFORE INSERT ON accounts
    FOR EACH ROW
    EXECUTE FUNCTION assign_account_id();

-- The trigger now selects the correct sequence. Keeping the old column
-- default would consume a normal ID before the trigger sees a Grok row.
ALTER TABLE accounts ALTER COLUMN id DROP DEFAULT;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'accounts'::REGCLASS
          AND conname = 'chk_accounts_platform_id_range'
    ) THEN
        ALTER TABLE accounts
            ADD CONSTRAINT chk_accounts_platform_id_range
            CHECK (
                (platform = 'grok' AND id >= 100000)
                OR (platform <> 'grok' AND id < 100000)
            ) NOT VALID;
    END IF;
END $$;

ALTER TABLE accounts VALIDATE CONSTRAINT chk_accounts_platform_id_range;

DO $$
DECLARE
    normal_sequence REGCLASS;
BEGIN
    normal_sequence := pg_get_serial_sequence('accounts', 'id')::REGCLASS;
    EXECUTE format('ALTER SEQUENCE %s MAXVALUE 99999 NO CYCLE', normal_sequence);
END $$;

COMMENT ON SEQUENCE accounts_grok_id_seq IS
    'Independent account ID allocator for platform=grok; IDs start at 100000.';
COMMENT ON FUNCTION assign_account_id() IS
    'Assigns accounts.id from the Grok or normal sequence according to platform.';
