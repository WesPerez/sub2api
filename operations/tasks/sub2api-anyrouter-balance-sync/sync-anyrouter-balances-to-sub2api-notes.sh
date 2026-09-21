#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
FETCHER="${ANYROUTER_FETCHER:-$SCRIPT_DIR/fetch-anyrouter-balances.mjs}"
COOKIE_CONFIG="${ANYROUTER_COOKIE_CONFIG:-/etc/sub2api-any-balance-sync.tsv}"
ANYROUTER_BASE_URL="${ANYROUTER_BASE_URL:-https://anyrouter.top}"
PROXY_ENV_FILE="${ANYROUTER_PROXY_ENV_FILE:-/etc/server-scheduled-tasks/apps-resin-anyrouter-balance.env}"

SUB2API_POSTGRES_CONTAINER="${SUB2API_POSTGRES_CONTAINER:-sub2api-prod-postgres}"
SUB2API_POSTGRES_USER="${POSTGRES_USER:-sub2api}"
SUB2API_POSTGRES_DB="${POSTGRES_DB:-sub2api}"
STATS_TIMEZONE="${STATS_TIMEZONE:-Asia/Shanghai}"
EXPECTED_ACCOUNT_IDS="1,2,5,4,7,2183"
EXPECTED_ANY_USER_IDS="181380,199848,200748,213103,201326,213232"
EXPECTED_ACCOUNT_COUNT=6
# Organizer prefixes are two base36 display characters.  Their values are
# intentionally ignored here.  The stable any-* key selects the primary note.
ANY_MANAGED_PREFIX_RE='^![0-9A-Za-z]{2}-'
ANY_NAME_PREFIX_RE="${ANY_MANAGED_PREFIX_RE}any-"
SUB2API_PRIMARY_ACCOUNT_KEY="${SUB2API_PRIMARY_ACCOUNT_KEY:-any-6945}"
LOCK_PATH="${LOCK_PATH:-/run/lock/sub2api-balance-notes.lock}"
APPLY="${APPLY:-1}"

for command in node jq docker flock stat curl; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "$command is required" >&2
    exit 1
  fi
done

if [[ ! -f "$FETCHER" ]]; then
  echo "AnyRouter fetcher not found: $FETCHER" >&2
  exit 1
fi
if [[ ! -f "$COOKIE_CONFIG" ]]; then
  echo "AnyRouter cookie config not found: $COOKIE_CONFIG" >&2
  exit 1
fi
if [[ ! -f "$PROXY_ENV_FILE" ]]; then
  echo "AnyRouter proxy config not found: $PROXY_ENV_FILE" >&2
  exit 1
fi
if [[ ! "$SUB2API_PRIMARY_ACCOUNT_KEY" =~ ^any-[0-9A-Za-z]+$ ]]; then
  echo "SUB2API_PRIMARY_ACCOUNT_KEY must match any-<stable-key>" >&2
  exit 1
fi

read -r config_owner config_mode < <(stat -c '%u %a' "$COOKIE_CONFIG")
if [[ "$config_owner" != "0" || ( "$config_mode" != "600" && "$config_mode" != "400" ) ]]; then
  echo "AnyRouter cookie config must be owned by root with mode 600 or 400" >&2
  exit 1
fi
read -r proxy_owner proxy_mode < <(stat -c '%u %a' "$PROXY_ENV_FILE")
if [[ "$proxy_owner" != "0" || ( "$proxy_mode" != "600" && "$proxy_mode" != "400" ) ]]; then
  echo "AnyRouter proxy config must be owned by root with mode 600 or 400" >&2
  exit 1
fi

case "$APPLY" in
  1|true|TRUE|yes|YES) psql_apply=true ;;
  0|false|FALSE|no|NO) psql_apply=false ;;
  *) echo "APPLY must be 0 or 1" >&2; exit 1 ;;
esac

exec 9>"$LOCK_PATH"
if ! flock -n 9; then
  echo "another AnyRouter balance sync is already running" >&2
  printf '%s\n' '{"status":"skipped","reason":"balance sync lock busy"}'
  exit 75
fi

umask 077
tmp_json="$(mktemp)"
tmp_csv="$(mktemp)"
trap 'rm -f "$tmp_json" "$tmp_csv"' EXIT

node "$FETCHER" \
  --config "$COOKIE_CONFIG" \
  --base-url "$ANYROUTER_BASE_URL" \
  --proxy-env-file "$PROXY_ENV_FILE" \
  > "$tmp_json"

jq -e --argjson expected "$EXPECTED_ACCOUNT_COUNT" '
  length == $expected and
  ([.[].ordinal] | unique | length) == $expected and
  ([.[].account_id] | unique | length) == $expected and
  all(.[];
    (.ordinal | type) == "number" and
    (.account_id | type) == "number" and
    (.any_user_id | type) == "number" and
    (.any_username | type) == "string" and (.any_username | length) > 0 and
    (.balance | type) == "number" and
    (.balance_used | type) == "number" and
    (.quota | type) == "number" and
    (.fetched_at | fromdateiso8601) > 0
  )
' "$tmp_json" >/dev/null

actual_account_ids="$(jq -r 'sort_by(.ordinal) | map(.account_id | tostring) | join(",")' "$tmp_json")"
if [[ "$actual_account_ids" != "$EXPECTED_ACCOUNT_IDS" ]]; then
  echo "cookie config account order does not match the approved target ids" >&2
  exit 1
fi

actual_any_user_ids="$(jq -r 'sort_by(.ordinal) | map(.any_user_id | tostring) | join(",")' "$tmp_json")"
if [[ "$actual_any_user_ids" != "$EXPECTED_ANY_USER_IDS" ]]; then
  echo "AnyRouter session identities do not match the approved account order" >&2
  exit 1
fi

jq -r '
  (["ordinal", "account_id", "any_user_id", "any_username", "balance", "balance_used", "quota", "fetched_at"] | @csv),
  (sort_by(.ordinal)[] | [
    .ordinal,
    .account_id,
    .any_user_id,
    .any_username,
    .balance,
    .balance_used,
    .quota,
    .fetched_at
  ] | @csv)
' "$tmp_json" > "$tmp_csv"

{
  cat <<'SQL'
BEGIN;

CREATE TEMP TABLE anyrouter_balance_sync (
  ordinal integer PRIMARY KEY,
  account_id bigint UNIQUE NOT NULL,
  any_user_id bigint NOT NULL,
  any_username text NOT NULL,
  balance numeric NOT NULL,
  balance_used numeric NOT NULL,
  quota numeric NOT NULL,
  fetched_at timestamptz NOT NULL
) ON COMMIT DROP;

\copy anyrouter_balance_sync(ordinal, account_id, any_user_id, any_username, balance, balance_used, quota, fetched_at) FROM STDIN WITH (FORMAT csv, HEADER true)
SQL
  cat "$tmp_csv"
  printf '\\.\n'
  cat <<'SQL'

CREATE TEMP TABLE anyrouter_balance_config (
  any_managed_prefix_re text NOT NULL,
  any_name_prefix_re text NOT NULL,
  primary_account_key text NOT NULL
) ON COMMIT DROP;

INSERT INTO anyrouter_balance_config(any_managed_prefix_re, any_name_prefix_re, primary_account_key)
VALUES (:'any_managed_prefix_re', :'any_name_prefix_re', :'primary_account_key');

DO $validation$
DECLARE
  invalid_targets text;
  configured_any_managed_prefix_re text;
  configured_any_name_prefix_re text;
  configured_primary_account_key text;
BEGIN
  SELECT any_managed_prefix_re, any_name_prefix_re, primary_account_key
  INTO configured_any_managed_prefix_re, configured_any_name_prefix_re, configured_primary_account_key
  FROM anyrouter_balance_config;

  IF (SELECT count(*) FROM anyrouter_balance_sync) <> 6 THEN
    RAISE EXCEPTION 'expected 6 AnyRouter balances';
  END IF;

  SELECT string_agg(format('ordinal=%s id=%s name=%s', d.ordinal, d.account_id, coalesce(a.name, '<missing>')), '; ' ORDER BY d.ordinal)
  INTO invalid_targets
  FROM anyrouter_balance_sync d
  LEFT JOIN accounts a ON a.id = d.account_id
  WHERE a.id IS NULL
     OR a.deleted_at IS NOT NULL
     OR a.platform <> 'openai'
     OR a.type <> 'apikey'
     OR a.name !~ configured_any_name_prefix_re;

  IF invalid_targets IS NOT NULL THEN
    RAISE EXCEPTION 'AnyRouter target validation failed: %', invalid_targets;
  END IF;
  IF (
    SELECT count(*)
    FROM anyrouter_balance_sync d
    JOIN accounts a ON a.id = d.account_id
    WHERE regexp_replace(a.name, configured_any_managed_prefix_re, '')
          LIKE configured_primary_account_key || '%'
  ) <> 1 THEN
    RAISE EXCEPTION 'primary AnyRouter note target must match exactly one %', configured_primary_account_key;
  END IF;
END
$validation$;

CREATE TEMP TABLE anyrouter_balance_rollup ON COMMIT DROP AS
WITH samples AS (
  SELECT
    d.*,
    a.notes,
    d.fetched_at AT TIME ZONE :'stats_timezone' AS sampled_at,
    regexp_replace(a.name, :'any_managed_prefix_re', '')
      LIKE :'primary_account_key' || '%' AS is_primary,
    regexp_match(
      split_part(coalesce(a.notes, ''), E'\n', 1),
      '，已用 (-?[0-9]+[.]?[0-9]*)，昨日耗 (-?[0-9]+[.]?[0-9]*)，上小时耗 (-?[0-9]+[.]?[0-9]*)，今日耗 (-?[0-9]+[.]?[0-9]*)(?:，本小时耗 (-?[0-9]+[.]?[0-9]*))?.*，统计 ([0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2})$'
    ) AS previous,
    regexp_match(
      split_part(coalesce(a.notes, ''), E'\n', 1),
      '^(([0-9]{4}-[0-9]{2}-[0-9]{2}) .+，)(?:METAPI余额|ANY余额) '
    ) AS checkin_prefix,
    NULLIF(split_part(coalesce(a.notes, ''), E'\n', 2), '') AS daily_checkin_summary
  FROM anyrouter_balance_sync d
  JOIN accounts a ON a.id = d.account_id
), usage_state AS (
  SELECT
    s.*,
    CASE WHEN s.previous IS NOT NULL THEN (s.previous)[1]::numeric END AS previous_used,
    CASE WHEN s.previous IS NOT NULL THEN (s.previous)[2]::numeric END AS previous_day,
    CASE WHEN s.previous IS NOT NULL THEN (s.previous)[3]::numeric END AS previous_hour,
    CASE WHEN s.previous IS NOT NULL THEN (s.previous)[4]::numeric END AS current_day,
    CASE WHEN s.previous IS NOT NULL THEN coalesce((s.previous)[5]::numeric, 0::numeric) END AS current_hour,
    CASE WHEN s.previous IS NOT NULL THEN (s.previous)[6]::timestamp END AS previous_sampled_at
  FROM samples s
), usage_delta AS (
  SELECT
    u.*,
    CASE
      WHEN u.previous_sampled_at IS NOT NULL
        AND u.sampled_at > u.previous_sampled_at
        AND u.balance_used >= u.previous_used
        THEN u.balance_used - u.previous_used
      ELSE 0::numeric
    END AS consumed
  FROM usage_state u
)
SELECT
  d.*,
  CASE
    WHEN d.previous_sampled_at IS NULL THEN 0::numeric
    WHEN d.sampled_at::date > d.previous_sampled_at::date THEN
      CASE
        WHEN date_trunc('hour', d.sampled_at) = date_trunc('hour', d.previous_sampled_at) + interval '1 hour'
          THEN d.current_day + d.consumed
        ELSE 0::numeric
      END
    ELSE d.previous_day
  END AS rolled_previous_day,
  CASE
    WHEN d.previous_sampled_at IS NULL THEN 0::numeric
    WHEN date_trunc('hour', d.sampled_at) > date_trunc('hour', d.previous_sampled_at) THEN
      CASE
        WHEN date_trunc('hour', d.sampled_at) = date_trunc('hour', d.previous_sampled_at) + interval '1 hour'
          THEN d.current_hour + d.consumed
        ELSE 0::numeric
      END
    ELSE d.previous_hour
  END AS rolled_previous_hour,
  CASE
    WHEN d.previous_sampled_at IS NULL THEN 0::numeric
    WHEN d.sampled_at::date > d.previous_sampled_at::date THEN 0::numeric
    WHEN d.sampled_at > d.previous_sampled_at THEN d.current_day + d.consumed
    ELSE d.current_day
  END AS rolled_current_day,
  CASE
    WHEN d.previous_sampled_at IS NULL THEN 0::numeric
    WHEN date_trunc('hour', d.sampled_at) > date_trunc('hour', d.previous_sampled_at) THEN 0::numeric
    WHEN d.sampled_at > d.previous_sampled_at THEN d.current_hour + d.consumed
    ELSE d.current_hour
  END AS rolled_current_hour
FROM usage_delta d;

\if :apply
CREATE TEMP TABLE anyrouter_updated_rows ON COMMIT DROP AS
WITH updated AS (
  UPDATE accounts a
  SET
    notes = concat(
      CASE
        WHEN r.checkin_prefix IS NOT NULL
          AND (r.checkin_prefix)[2] = to_char(r.sampled_at, 'YYYY-MM-DD')
          THEN (r.checkin_prefix)[1]
        ELSE to_char(r.sampled_at, 'YYYY-MM-DD') || ' 签到状态未知，'
      END,
      'ANY余额 ',
      trim(to_char(r.balance, 'FM999999999990.00')),
      ' / ',
      trim(to_char(r.quota, 'FM999999999990.00')),
      '，已用 ',
      trim(to_char(r.balance_used, 'FM999999999990.00')),
      '，昨日耗 ',
      trim(to_char(r.rolled_previous_day, 'FM999999999990.00')),
      '，上小时耗 ',
      trim(to_char(r.rolled_previous_hour, 'FM999999999990.00')),
      '，今日耗 ',
      trim(to_char(r.rolled_current_day, 'FM999999999990.00')),
      '，本小时耗 ',
      trim(to_char(r.rolled_current_hour, 'FM999999999990.00')),
      '，刷新 ',
      to_char(r.sampled_at, 'YYYY-MM-DD HH24:MI:SS'),
      '，统计 ',
      to_char(r.sampled_at, 'YYYY-MM-DD HH24:MI:SS'),
      CASE
        WHEN r.is_primary AND r.daily_checkin_summary IS NOT NULL
          THEN E'\n' || r.daily_checkin_summary
        ELSE ''
      END
    ),
    updated_at = now()
  FROM anyrouter_balance_rollup r
  WHERE a.id = r.account_id
  RETURNING a.id, a.name, a.notes
)
SELECT id, name, notes FROM updated;

DO $updated_validation$
BEGIN
  IF (SELECT count(*) FROM anyrouter_updated_rows) <> 6 THEN
    RAISE EXCEPTION 'expected to update 6 AnyRouter accounts';
  END IF;
END
$updated_validation$;

SELECT 'updated' AS result, id, name, notes FROM anyrouter_updated_rows ORDER BY name;
\else
SELECT
  'dry_run' AS result,
  a.id,
  a.name,
  concat(
    CASE
      WHEN r.checkin_prefix IS NOT NULL
        AND (r.checkin_prefix)[2] = to_char(r.sampled_at, 'YYYY-MM-DD')
        THEN (r.checkin_prefix)[1]
      ELSE to_char(r.sampled_at, 'YYYY-MM-DD') || ' 签到状态未知，'
    END,
    'ANY余额 ',
    trim(to_char(r.balance, 'FM999999999990.00')),
    ' / ',
    trim(to_char(r.quota, 'FM999999999990.00')),
    '，已用 ',
    trim(to_char(r.balance_used, 'FM999999999990.00')),
    '，昨日耗 ',
    trim(to_char(r.rolled_previous_day, 'FM999999999990.00')),
    '，上小时耗 ',
    trim(to_char(r.rolled_previous_hour, 'FM999999999990.00')),
    '，今日耗 ',
    trim(to_char(r.rolled_current_day, 'FM999999999990.00')),
    '，本小时耗 ',
    trim(to_char(r.rolled_current_hour, 'FM999999999990.00')),
    '，刷新 ',
    to_char(r.sampled_at, 'YYYY-MM-DD HH24:MI:SS'),
    '，统计 ',
    to_char(r.sampled_at, 'YYYY-MM-DD HH24:MI:SS'),
    CASE
      WHEN r.is_primary AND r.daily_checkin_summary IS NOT NULL
        THEN E'\n' || r.daily_checkin_summary
      ELSE ''
    END
  ) AS notes
FROM accounts a
JOIN anyrouter_balance_rollup r ON r.account_id = a.id
ORDER BY a.name;
\endif

COMMIT;
SQL
} | docker exec -i "$SUB2API_POSTGRES_CONTAINER" psql \
  -U "$SUB2API_POSTGRES_USER" \
  -d "$SUB2API_POSTGRES_DB" \
  -X \
  -v ON_ERROR_STOP=1 \
  -v "apply=$psql_apply" \
  -v "stats_timezone=$STATS_TIMEZONE" \
  -v "any_managed_prefix_re=$ANY_MANAGED_PREFIX_RE" \
  -v "primary_account_key=$SUB2API_PRIMARY_ACCOUNT_KEY" \
  -v "any_name_prefix_re=$ANY_NAME_PREFIX_RE"

# A small, non-secret terminal summary for the shared task console. Reaching
# this point means psql completed its validation and transaction successfully.
jq -cn --argjson apply "$psql_apply" --slurpfile accounts "$tmp_json" \
  '{status:(if $apply then "applied" else "preview" end),
    accounts:($accounts[0] | length),
    total_balance:($accounts[0] | map(.balance) | add)}'
