#!/usr/bin/env node

import { spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const USER_AGENT =
  'Mozilla/5.0 (Windows NT 10.0; Win64; x64) ' +
  'AppleWebKit/537.36 (KHTML, like Gecko) Chrome/132.0.0.0 Safari/537.36';
const MAX_RESPONSE_BYTES = 4 * 1024 * 1024;
const ACW_XOR_SEED = '3000176000856006061501533003690027800375';

function parseArgs(argv) {
  const options = {
    config: '/etc/sub2api-any-balance-sync.tsv',
    baseUrl: 'https://anyrouter.top',
    proxyUrl: '',
    proxyEnvFile: '',
  };

  for (let i = 0; i < argv.length; i += 1) {
    const key = argv[i];
    const value = argv[i + 1];
    if (key === '--config' && value) {
      options.config = value;
      i += 1;
    } else if (key === '--base-url' && value) {
      options.baseUrl = value;
      i += 1;
    } else if (key === '--proxy-url' && value !== undefined) {
      options.proxyUrl = value;
      i += 1;
    } else if (key === '--proxy-env-file' && value) {
      options.proxyEnvFile = value;
      i += 1;
    } else {
      throw new Error(`unsupported argument: ${key}`);
    }
  }

  const endpoint = new URL('/api/user/self', options.baseUrl);
  if (endpoint.protocol !== 'https:' || endpoint.hostname !== 'anyrouter.top') {
    throw new Error('refusing to send session cookies outside https://anyrouter.top');
  }
  if (options.proxyUrl && options.proxyEnvFile) {
    throw new Error('use either --proxy-url or --proxy-env-file, not both');
  }
  if (options.proxyEnvFile) {
    Object.assign(options, loadProxyConfig(options.proxyEnvFile));
  }
  options.endpoint = endpoint.toString();
  return options;
}

function parseEnvFile(path) {
  const values = {};
  for (const rawLine of readFileSync(path, 'utf8').split(/\r?\n/)) {
    const line = rawLine.trim();
    if (!line || line.startsWith('#')) continue;
    const separator = line.indexOf('=');
    if (separator <= 0) continue;
    const key = line.slice(0, separator).trim();
    let value = line.slice(separator + 1).trim();
    if ((value.startsWith('"') && value.endsWith('"')) ||
        (value.startsWith("'") && value.endsWith("'"))) {
      value = value.slice(1, -1);
    }
    values[key] = value;
  }
  return values;
}

function loadProxyConfig(path) {
  const values = parseEnvFile(path);
  const host = String(values.RESIN_SOCKS_HOST || '').trim();
  const port = Number.parseInt(String(values.RESIN_SOCKS_PORT || ''), 10);
  const proxyUser = String(values.RESIN_SOCKS_USER || '');
  const proxyPassword = String(values.RESIN_SOCKS_PASS || '');
  if (!/^[A-Za-z0-9.-]+$/.test(host) || !Number.isInteger(port) || port < 1 || port > 65535) {
    throw new Error('invalid Resin SOCKS proxy endpoint');
  }
  if (!proxyUser || !proxyPassword || /[\r\n]/.test(proxyUser + proxyPassword)) {
    throw new Error('invalid Resin SOCKS proxy credentials');
  }
  return {
    proxyUrl: `socks5h://${host}:${port}`,
    proxyUser,
    proxyPassword,
  };
}

function curlConfigValue(value) {
  return String(value).replace(/\\/g, '\\\\').replace(/"/g, '\\"');
}

function parseConfig(path) {
  const entries = [];
  const lines = readFileSync(path, 'utf8').split(/\r?\n/);
  for (const rawLine of lines) {
    const line = rawLine.trim();
    if (!line || line.startsWith('#')) continue;

    const fields = line.split(/\s+/);
    if (fields.length !== 2) {
      throw new Error(`invalid config line ${entries.length + 1}`);
    }
    const accountId = Number.parseInt(fields[0], 10);
    if (!Number.isSafeInteger(accountId) || accountId <= 0) {
      throw new Error(`invalid account id on config line ${entries.length + 1}`);
    }

    let cookie = fields[1].trim();
    if (cookie.startsWith('session=')) cookie = cookie.slice('session='.length);
    if (!cookie || !/^[A-Za-z0-9_+=-]+$/.test(cookie)) {
      throw new Error(`invalid session cookie on config line ${entries.length + 1}`);
    }

    entries.push({ ordinal: entries.length + 1, accountId, cookie });
  }

  if (entries.length === 0) throw new Error('cookie config is empty');
  if (new Set(entries.map((entry) => entry.accountId)).size !== entries.length) {
    throw new Error('cookie config contains duplicate account ids');
  }
  if (new Set(entries.map((entry) => entry.cookie)).size !== entries.length) {
    throw new Error('cookie config contains duplicate session cookies');
  }
  return entries;
}

function decodeBase64BufferLoose(value) {
  try {
    return Buffer.from(value, 'base64');
  } catch {
    const normalized = value.replace(/-/g, '+').replace(/_/g, '/');
    return Buffer.from(normalized, 'base64');
  }
}

function decodeGobSignedInt(encoded) {
  if (!encoded.length) return null;
  let unsigned = 0n;
  if (encoded[0] < 0x80) {
    unsigned = BigInt(encoded[0]);
  } else {
    const width = 0x100 - encoded[0];
    if (width <= 0 || encoded.length !== width + 1) return null;
    for (let i = 1; i < encoded.length; i += 1) {
      unsigned = (unsigned << 8n) | BigInt(encoded[i]);
    }
  }
  const signed = (unsigned & 1n) === 0n
    ? unsigned >> 1n
    : -((unsigned >> 1n) + 1n);
  if (signed <= 0n || signed > BigInt(Number.MAX_SAFE_INTEGER)) return null;
  return Number(signed);
}

function extractGobFieldInts(payload, fieldName) {
  const ids = [];
  const marker = Buffer.concat([
    Buffer.from(fieldName, 'utf8'),
    Buffer.from([0x03]),
    Buffer.from('int', 'utf8'),
    Buffer.from([0x04]),
  ]);
  let start = 0;
  while (start < payload.length) {
    const position = payload.indexOf(marker, start);
    if (position < 0) break;
    const encodedLength = payload[position + marker.length];
    const delimiter = payload[position + marker.length + 1];
    if (typeof encodedLength === 'number' && delimiter === 0x00) {
      const byteLength = encodedLength - 1;
      const valueStart = position + marker.length + 2;
      const valueEnd = valueStart + byteLength;
      if (byteLength > 0 && valueEnd <= payload.length) {
        const value = decodeGobSignedInt(payload.subarray(valueStart, valueEnd));
        if (value && !ids.includes(value)) ids.push(value);
      }
    }
    start = position + marker.length;
  }
  return ids;
}

function extractLikelyUserIds(cookie) {
  const ids = [];
  const push = (value) => {
    const parsed = Number.parseInt(String(value), 10);
    if (!Number.isSafeInteger(parsed) || parsed <= 0 || parsed > 10_000_000) return;
    if (!ids.includes(parsed)) ids.push(parsed);
  };

  const decodedBuffer = decodeBase64BufferLoose(cookie);
  const payloadBuffers = [decodedBuffer];
  const decoded = decodedBuffer.toString('utf8');
  const parts = decoded.split('|');
  if (parts.length >= 2) {
    const middle = decodeBase64BufferLoose(parts[1]);
    if (middle.length > 0) payloadBuffers.push(middle);
  }

  for (const payload of payloadBuffers) {
    const text = payload.toString('utf8');
    for (const match of text.matchAll(/_(\d{4,8})(?!\d)/g)) push(match[1]);
    for (const match of text.matchAll(/(?:user(?:name)?|uid|id)[^\d]{0,16}(\d{4,8})(?!\d)/gi)) {
      push(match[1]);
    }
    for (const value of extractGobFieldInts(payload, 'id')) push(value);
  }
  return ids;
}

function parseChallengeArg1(html) {
  return html.match(/var\s+arg1\s*=\s*['"]([0-9a-fA-F]+)['"]/)?.[1]?.toUpperCase() || null;
}

function parseChallengeMapping(html) {
  const match = html.match(/for\(var m=\[([^\]]+)\],p=L\(0x115\)/);
  if (!match?.[1]) return null;
  const values = match[1].split(',').map((raw) => {
    const value = raw.trim().toLowerCase();
    return value.startsWith('0x')
      ? Number.parseInt(value.slice(2), 16)
      : Number.parseInt(value, 10);
  });
  return values.some((value) => Number.isNaN(value)) ? null : values;
}

function solveAcwScV2(html) {
  const arg1 = parseChallengeArg1(html);
  const mapping = parseChallengeMapping(html);
  if (!arg1 || !mapping) return null;

  const reordered = [];
  for (let i = 0; i < arg1.length; i += 1) {
    for (let j = 0; j < mapping.length; j += 1) {
      if (mapping[j] === i + 1) reordered[j] = arg1[i];
    }
  }

  const source = reordered.join('');
  let output = '';
  for (let i = 0; i < source.length && i < ACW_XOR_SEED.length; i += 2) {
    const left = Number.parseInt(source.slice(i, i + 2), 16);
    const right = Number.parseInt(ACW_XOR_SEED.slice(i, i + 2), 16);
    if (Number.isNaN(left) || Number.isNaN(right)) return null;
    output += (left ^ right).toString(16).padStart(2, '0');
  }
  return output || null;
}

function parseJson(text) {
  try {
    return JSON.parse(text);
  } catch {
    return null;
  }
}

function curlRequest({ endpoint, curlConfig, userId, cookieJar, challengeCookie, bodyPath, headersPath }) {
  const args = [
    '--silent',
    '--show-error',
    '--compressed',
    '--connect-timeout', '8',
    '--max-time', '20',
    '--max-redirs', '0',
    '--user-agent', USER_AGENT,
    '--header', 'Accept: application/json',
    '--header', `New-Api-User: ${userId}`,
    '--cookie-jar', cookieJar,
    '--dump-header', headersPath,
    '--output', bodyPath,
    '--write-out', '%{http_code}',
    '--config', curlConfig,
  ];
  args.push('--cookie', cookieJar);
  if (challengeCookie) args.push('--cookie', `acw_sc__v2=${challengeCookie}`);
  args.push(endpoint);

  const result = spawnSync('curl', args, {
    encoding: 'utf8',
    maxBuffer: MAX_RESPONSE_BYTES,
  });
  if (result.error) throw result.error;
  if (result.status !== 0) {
    throw new Error(`curl failed: ${(result.stderr || '').trim() || `exit ${result.status}`}`);
  }
  const statusCode = Number.parseInt(result.stdout.trim(), 10);
  if (!Number.isInteger(statusCode)) throw new Error('curl returned an invalid HTTP status');
  return statusCode;
}

function fetchForUserId(options, entry, userId) {
  const workDir = mkdtempSync(join(tmpdir(), 'anyrouter-balance.'));
  const cookieJar = join(workDir, 'cookies.txt');
  const curlConfig = join(workDir, 'curl.conf');
  let challengeCookie = null;
  try {
    writeFileSync(
      cookieJar,
      [
        '# Netscape HTTP Cookie File',
        `anyrouter.top\tFALSE\t/\tTRUE\t0\tsession\t${entry.cookie}`,
        '',
      ].join('\n'),
      { mode: 0o600 },
    );
    const curlConfigLines = [];
    if (options.proxyUrl) {
      curlConfigLines.push(`proxy = "${curlConfigValue(options.proxyUrl)}"`);
    }
    if (options.proxyUser || options.proxyPassword) {
      curlConfigLines.push(
        `proxy-user = "${curlConfigValue(`${options.proxyUser}:${options.proxyPassword}`)}"`,
      );
    }
    writeFileSync(curlConfig, `${curlConfigLines.join('\n')}\n`, { mode: 0o600 });
    for (let attempt = 1; attempt <= 3; attempt += 1) {
      const bodyPath = join(workDir, `body-${attempt}`);
      const headersPath = join(workDir, `headers-${attempt}`);
      const statusCode = curlRequest({
        endpoint: options.endpoint,
        curlConfig,
        userId,
        cookieJar,
        challengeCookie,
        bodyPath,
        headersPath,
      });
      const body = readFileSync(bodyPath, 'utf8');
      const payload = parseJson(body);
      if (payload) {
        if (payload.success === true && payload.data) return payload.data;
        const message = typeof payload.message === 'string' ? payload.message.trim() : '';
        throw new Error(message || `AnyRouter returned HTTP ${statusCode}`);
      }

      challengeCookie = solveAcwScV2(body);
      if (!challengeCookie) {
        throw new Error(`AnyRouter returned non-JSON HTTP ${statusCode}`);
      }
    }
    throw new Error('AnyRouter challenge retry limit exceeded');
  } finally {
    rmSync(workDir, { recursive: true, force: true });
  }
}

function asFiniteNumber(value, field) {
  const parsed = Number(value);
  if (!Number.isFinite(parsed)) throw new Error(`invalid ${field} value`);
  return parsed;
}

function round6(value) {
  return Math.round(value * 1_000_000) / 1_000_000;
}

function fetchEntry(options, entry) {
  const userIds = extractLikelyUserIds(entry.cookie);
  if (userIds.length === 0) throw new Error('could not extract AnyRouter user id from session');

  let lastError = null;
  for (const userId of userIds) {
    try {
      const data = fetchForUserId(options, entry, userId);
      const returnedId = Number.parseInt(String(data.id), 10);
      if (returnedId !== userId) throw new Error('AnyRouter returned a different user id');
      const quotaRaw = asFiniteNumber(data.quota, 'quota');
      const usedRaw = asFiniteNumber(data.used_quota, 'used_quota');
      const balance = round6(quotaRaw / 500_000);
      const balanceUsed = round6(usedRaw / 500_000);
      return {
        ordinal: entry.ordinal,
        account_id: entry.accountId,
        any_user_id: returnedId,
        any_username: String(data.username || ''),
        balance,
        balance_used: balanceUsed,
        quota: round6(balance + balanceUsed),
        fetched_at: new Date().toISOString().replace(/\.\d{3}Z$/, 'Z'),
      };
    } catch (error) {
      lastError = error;
    }
  }
  throw lastError || new Error('AnyRouter balance request failed');
}

function main() {
  const options = parseArgs(process.argv.slice(2));
  const entries = parseConfig(options.config);
  const results = [];
  for (const entry of entries) {
    try {
      results.push(fetchEntry(options, entry));
    } catch (error) {
      throw new Error(`account ${entry.accountId}: ${error?.message || error}`);
    }
  }
  process.stdout.write(`${JSON.stringify(results)}\n`);
}

try {
  main();
} catch (error) {
  process.stderr.write(`anyrouter balance fetch failed: ${error?.message || error}\n`);
  process.exitCode = 1;
}
