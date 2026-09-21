#!/usr/bin/env bash
set -euo pipefail

root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
container="sub2api-anyrouter-note-test-$$"

cleanup() {
  local status=$?
  if (( status != 0 )); then
    docker logs --tail 30 "$container" >&2 || true
  fi
  docker rm -f "$container" >/dev/null 2>&1 || true
}
trap cleanup EXIT

bash -n "$root/sync-anyrouter-balances-to-sub2api-notes.sh"
docker run --rm -d --name "$container" \
  -e POSTGRES_PASSWORD=test \
  postgres:18-alpine >/dev/null

for _ in $(seq 1 30); do
  # The image's initialization server accepts Unix sockets before restarting.
  # TCP becomes ready only when the final server has completed initialization.
  if docker exec "$container" pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
docker exec "$container" pg_isready -h 127.0.0.1 -U postgres >/dev/null

docker exec -i "$container" psql -v ON_ERROR_STOP=1 -U postgres >/dev/null <<'SQL'
DO $test$
DECLARE
  source_note text :=
    '2026-07-31 已签到，METAPI余额 120.00 / 200.00，已用 80.00，昨日耗 5.00，上小时耗 2.00，今日耗 3.00，本小时耗 1.00，刷新 2026-07-31 10:03:00，统计 2026-07-31 10:03:00'
    || E'\n' ||
    '签到（07-31 10:02）：简直了6个账号签到成功';
  previous text[];
  checkin_prefix text[];
  daily_summary text;
  rendered text;
BEGIN
  previous := regexp_match(
    split_part(source_note, E'\n', 1),
    '，已用 (-?[0-9]+[.]?[0-9]*)，昨日耗 (-?[0-9]+[.]?[0-9]*)，上小时耗 (-?[0-9]+[.]?[0-9]*)，今日耗 (-?[0-9]+[.]?[0-9]*)(?:，本小时耗 (-?[0-9]+[.]?[0-9]*))?.*，统计 ([0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2})$'
  );
  checkin_prefix := regexp_match(
    split_part(source_note, E'\n', 1),
    '^(([0-9]{4}-[0-9]{2}-[0-9]{2}) .+，)(?:METAPI余额|ANY余额) '
  );
  daily_summary := NULLIF(split_part(source_note, E'\n', 2), '');

  IF previous IS NULL OR previous[1] <> '80.00' OR previous[6] <> '2026-07-31 10:03:00' THEN
    RAISE EXCEPTION 'usage parsing failed: %', previous;
  END IF;
  IF checkin_prefix IS NULL OR checkin_prefix[2] <> '2026-07-31' THEN
    RAISE EXCEPTION 'check-in prefix parsing failed: %', checkin_prefix;
  END IF;

  rendered := concat(
    '2026-07-31 已签到，ANY余额 120.00 / 200.00，统计 2026-07-31 10:33:00',
    CASE WHEN daily_summary IS NOT NULL THEN E'\n' || daily_summary ELSE '' END
  );
  IF split_part(rendered, E'\n', 2) <> daily_summary THEN
    RAISE EXCEPTION 'daily summary was not preserved: %', rendered;
  END IF;

  IF (
    SELECT count(*)
    FROM (VALUES
      ('!F3-any-694590262'),
      ('!07-any-1223143211')
    ) AS accounts(name)
    WHERE regexp_replace(name, '^![0-9A-Za-z]{2}-', '') LIKE 'any-6945%'
  ) <> 1 THEN
    RAISE EXCEPTION 'stable primary key must ignore the complete display prefix';
  END IF;
END
$test$;
SQL

printf '%s\n' 'AnyRouter multiline note tests passed'
