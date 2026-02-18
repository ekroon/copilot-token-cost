#!/usr/bin/env node

/**
 * copilot-token-cost.js
 *
 * Copilot CLI Token Cost Calculator — Node.js port
 *
 * Parses Copilot CLI process logs to extract per-model token usage
 * and calculates estimated API cost based on current pricing.
 */

'use strict';

const fs = require('fs');
const path = require('path');
const os = require('os');
const { spawnSync } = require('child_process');
const Database = require('better-sqlite3');

const DB_PATH = path.join(__dirname, '..', 'copilot-tokens.db');

// ─── Load pricing data from pricing.json ────────────────────────────────────
function loadPricing() {
  const scriptDir = path.dirname(__filename);
  const candidates = [
    path.join(scriptDir, 'pricing.json'),
    path.join(scriptDir, '..', 'pricing.json'),
    path.join(process.cwd(), 'pricing.json'),
  ];
  for (const p of candidates) {
    if (fs.existsSync(p)) {
      return JSON.parse(fs.readFileSync(p, 'utf-8'));
    }
  }
  process.stderr.write('Error: pricing.json not found\n');
  process.exit(1);
}

const _pricingData = loadPricing();
const PRICING_PERIODS = _pricingData.pricing_periods;


function getPeriod(timestamp) {
  if (timestamp == null) return PRICING_PERIODS[0];
  const dateStr = timestamp.substring(0, 10);
  for (const period of PRICING_PERIODS) {
    if (dateStr >= period.effective_from) return period;
  }
  return PRICING_PERIODS[PRICING_PERIODS.length - 1];
}


function getPremiumRequestCost(timestamp) {
  return getPeriod(timestamp).premium_request_cost;
}


// ─── SQLite schema & DB layer ───────────────────────────────────────────────

const SCHEMA_SQL = `
CREATE TABLE IF NOT EXISTS api_calls (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    model TEXT NOT NULL,
    model_normalized TEXT NOT NULL,
    prompt_tokens INTEGER NOT NULL,
    completion_tokens INTEGER NOT NULL,
    cache_creation_tokens INTEGER DEFAULT 0,
    cache_read_tokens INTEGER DEFAULT 0,
    is_user_turn INTEGER DEFAULT 0,
    timestamp TEXT,
    session_id TEXT,
    log_file TEXT,
    source TEXT DEFAULT 'local',
    UNIQUE(timestamp, model, prompt_tokens, completion_tokens, log_file, source)
);
CREATE TABLE IF NOT EXISTS parsed_logs (
    log_file TEXT NOT NULL,
    mtime REAL NOT NULL,
    source TEXT DEFAULT 'local',
    record_count INTEGER DEFAULT 0,
    parsed_at TEXT NOT NULL,
    PRIMARY KEY (log_file, source)
);
CREATE TABLE IF NOT EXISTS session_workspaces (
    session_id TEXT PRIMARY KEY,
    cwd TEXT NOT NULL,
    source TEXT DEFAULT 'local'
);
CREATE TABLE IF NOT EXISTS codespace_sync_state (
    codespace_name TEXT PRIMARY KEY,
    last_used_at TEXT,
    last_synced_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_api_calls_timestamp ON api_calls(timestamp);
CREATE INDEX IF NOT EXISTS idx_api_calls_model ON api_calls(model_normalized);
CREATE INDEX IF NOT EXISTS idx_api_calls_session ON api_calls(session_id);
`;


function initDB(dbPath) {
  const db = new Database(dbPath);
  db.pragma('journal_mode = WAL');
  db.pragma('foreign_keys = ON');
  db.exec(SCHEMA_SQL);
  return db;
}


function isLogParsed(db, logFile, mtime, source = 'local') {
  const row = db.prepare(
    'SELECT 1 FROM parsed_logs WHERE log_file = ? AND source = ? AND mtime = ?'
  ).get(logFile, source, mtime);
  return row !== undefined;
}


function markLogParsed(db, logFile, mtime, recordCount, source = 'local') {
  db.prepare(
    "INSERT OR REPLACE INTO parsed_logs (log_file, mtime, source, record_count, parsed_at) VALUES (?, ?, ?, ?, datetime('now'))"
  ).run(logFile, mtime, source, recordCount);
}


function insertRecords(db, records, source = 'local') {
  if (!records.length) return;
  const stmt = db.prepare(
    'INSERT OR IGNORE INTO api_calls ' +
    '(model, model_normalized, prompt_tokens, completion_tokens, ' +
    'cache_creation_tokens, cache_read_tokens, is_user_turn, ' +
    'timestamp, session_id, log_file, source) ' +
    'VALUES (@model, @model_normalized, @prompt_tokens, @completion_tokens, ' +
    '@cache_creation_tokens, @cache_read_tokens, @is_user_turn, ' +
    '@timestamp, @session_id, @log_file, @source)'
  );
  const insertMany = db.transaction((recs) => {
    for (const r of recs) {
      stmt.run({ ...r, source, is_user_turn: r.is_user_turn ? 1 : 0 });
    }
  });
  insertMany(records);
}


function upsertSessionWorkspace(db, sessionId, cwd, source = 'local') {
  db.prepare(
    'INSERT OR REPLACE INTO session_workspaces (session_id, cwd, source) VALUES (?, ?, ?)'
  ).run(sessionId, cwd, source);
}


function clearSource(db, source = 'local') {
  db.prepare('DELETE FROM api_calls WHERE source = ?').run(source);
  db.prepare('DELETE FROM parsed_logs WHERE source = ?').run(source);
  db.prepare('DELETE FROM session_workspaces WHERE source = ?').run(source);
}


function getCodespaceLastUsed(db, codespaceName) {
  const row = db.prepare(
    'SELECT last_used_at FROM codespace_sync_state WHERE codespace_name = ?'
  ).get(codespaceName);
  return row ? row.last_used_at : null;
}


function upsertCodespaceSyncState(db, codespaceName, lastUsedAt) {
  db.prepare(
    "INSERT OR REPLACE INTO codespace_sync_state (codespace_name, last_used_at, last_synced_at) VALUES (?, ?, datetime('now'))"
  ).run(codespaceName, lastUsedAt ?? null);
}


function _buildFilters(dateFrom, dateTo, source, tableAlias = 'a') {
  const clauses = [];
  const params = [];
  if (dateFrom != null) { clauses.push(`${tableAlias}.timestamp >= ?`); params.push(dateFrom); }
  if (dateTo != null) { clauses.push(`${tableAlias}.timestamp < ?`); params.push(dateTo); }
  if (source != null) { clauses.push(`${tableAlias}.source = ?`); params.push(source); }
  const where = clauses.length ? ' WHERE ' + clauses.join(' AND ') : '';
  return { where, params };
}


function queryModelStats(db, dateFrom, dateTo, source) {
  const { where, params } = _buildFilters(dateFrom, dateTo, source);
  const rows = db.prepare(
    'SELECT model_normalized, COUNT(*) AS api_calls, ' +
    'SUM(prompt_tokens) AS prompt_tokens, SUM(completion_tokens) AS completion_tokens, ' +
    'SUM(cache_creation_tokens) AS cache_creation_tokens, ' +
    'SUM(cache_read_tokens) AS cache_read_tokens, ' +
    'SUM(CASE WHEN is_user_turn = 1 THEN 1 ELSE 0 END) AS user_turns ' +
    `FROM api_calls a${where} GROUP BY model_normalized`
  ).all(...params);
  const result = {};
  for (const row of rows) {
    result[row.model_normalized] = {
      api_calls: row.api_calls, prompt_tokens: row.prompt_tokens,
      completion_tokens: row.completion_tokens,
      cache_creation_tokens: row.cache_creation_tokens,
      cache_read_tokens: row.cache_read_tokens, user_turns: row.user_turns,
    };
  }
  return result;
}


function queryDailyStats(db, dateFrom, dateTo, source) {
  const { where, params } = _buildFilters(dateFrom, dateTo, source);
  const rows = db.prepare(
    'SELECT substr(a.timestamp, 1, 10) AS day, model_normalized, ' +
    'COUNT(*) AS api_calls, SUM(prompt_tokens) AS prompt_tokens, ' +
    'SUM(completion_tokens) AS completion_tokens, ' +
    'SUM(cache_creation_tokens) AS cache_creation_tokens, ' +
    'SUM(cache_read_tokens) AS cache_read_tokens, ' +
    'SUM(CASE WHEN is_user_turn = 1 THEN 1 ELSE 0 END) AS user_turns ' +
    `FROM api_calls a${where} GROUP BY day, model_normalized`
  ).all(...params);
  const result = {};
  for (const row of rows) {
    const day = row.day || 'unknown';
    if (!result[day]) result[day] = {};
    result[day][row.model_normalized] = {
      api_calls: row.api_calls, prompt_tokens: row.prompt_tokens,
      completion_tokens: row.completion_tokens,
      cache_creation_tokens: row.cache_creation_tokens,
      cache_read_tokens: row.cache_read_tokens, user_turns: row.user_turns,
    };
  }
  return result;
}


function queryProjectStats(db, dateFrom, dateTo, source) {
  const { where, params } = _buildFilters(dateFrom, dateTo, source);
  const rows = db.prepare(
    "SELECT COALESCE(sw.cwd, '') AS cwd, COUNT(*) AS api_calls, " +
    'SUM(a.prompt_tokens) AS prompt_tokens, SUM(a.completion_tokens) AS completion_tokens, ' +
    'SUM(a.cache_creation_tokens) AS cache_creation_tokens, ' +
    'SUM(a.cache_read_tokens) AS cache_read_tokens, ' +
    'SUM(CASE WHEN a.is_user_turn = 1 THEN 1 ELSE 0 END) AS user_turns ' +
    `FROM api_calls a LEFT JOIN session_workspaces sw ON a.session_id = sw.session_id${where} ` +
    'GROUP BY cwd'
  ).all(...params);
  const result = {};
  for (const row of rows) {
    result[row.cwd] = {
      api_calls: row.api_calls, prompt_tokens: row.prompt_tokens,
      completion_tokens: row.completion_tokens,
      cache_creation_tokens: row.cache_creation_tokens,
      cache_read_tokens: row.cache_read_tokens, user_turns: row.user_turns,
    };
  }
  return result;
}


function queryRecords(db, dateFrom, dateTo, source) {
  const { where, params } = _buildFilters(dateFrom, dateTo, source);
  return db.prepare(
    'SELECT model, model_normalized, prompt_tokens, completion_tokens, ' +
    'cache_creation_tokens, cache_read_tokens, is_user_turn, ' +
    `timestamp, session_id, log_file, source FROM api_calls a${where}`
  ).all(...params);
}


function querySessionWorkspaces(db, source) {
  const rows = source != null
    ? db.prepare('SELECT session_id, cwd FROM session_workspaces WHERE source = ?').all(source)
    : db.prepare('SELECT session_id, cwd FROM session_workspaces').all();
  const result = {};
  for (const row of rows) result[row.session_id] = row.cwd;
  return result;
}


function exportJSONL(db, outputPath) {
  const fd = fs.openSync(outputPath, 'w');
  const cols = ['model', 'model_normalized', 'prompt_tokens', 'completion_tokens',
    'cache_creation_tokens', 'cache_read_tokens', 'is_user_turn',
    'timestamp', 'session_id', 'log_file', 'source'];
  const apiRows = db.prepare(
    'SELECT ' + cols.join(', ') + ' FROM api_calls'
  ).all();
  for (const row of apiRows) {
    fs.writeSync(fd, JSON.stringify({ type: 'api_call', ...row }) + '\n');
  }
  const swRows = db.prepare('SELECT session_id, cwd, source FROM session_workspaces').all();
  for (const row of swRows) {
    fs.writeSync(fd, JSON.stringify({ type: 'session_workspace', ...row }) + '\n');
  }
  fs.closeSync(fd);
}


function importJSONL(db, inputPath, sourceOverride) {
  const content = fs.readFileSync(inputPath, 'utf-8');
  let count = 0;
  for (const line of content.split('\n')) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    const obj = JSON.parse(trimmed);
    const rtype = obj.type;
    delete obj.type;
    if (rtype === 'api_call') {
      const src = sourceOverride || obj.source || 'local';
      obj.source = src;
      insertRecords(db, [obj], src);
    } else if (rtype === 'session_workspace') {
      const src = sourceOverride || obj.source || 'local';
      upsertSessionWorkspace(db, obj.session_id, obj.cwd, src);
    }
    count++;
  }
  return count;
}


function importSQLiteDB(db, otherDBPath, sourceOverride) {
  db.exec(`ATTACH DATABASE '${otherDBPath}' AS import_db`);
  try {
    if (sourceOverride) {
      db.prepare(
        'INSERT OR IGNORE INTO api_calls ' +
        '(model, model_normalized, prompt_tokens, completion_tokens, ' +
        'cache_creation_tokens, cache_read_tokens, is_user_turn, ' +
        'timestamp, session_id, log_file, source) ' +
        'SELECT model, model_normalized, prompt_tokens, completion_tokens, ' +
        'cache_creation_tokens, cache_read_tokens, is_user_turn, ' +
        'timestamp, session_id, log_file, ? FROM import_db.api_calls'
      ).run(sourceOverride);
      db.prepare(
        'INSERT OR REPLACE INTO session_workspaces (session_id, cwd, source) ' +
        'SELECT session_id, cwd, ? FROM import_db.session_workspaces'
      ).run(sourceOverride);
      db.prepare(
        'INSERT OR REPLACE INTO parsed_logs (log_file, mtime, source, record_count, parsed_at) ' +
        'SELECT log_file, mtime, ?, record_count, parsed_at FROM import_db.parsed_logs'
      ).run(sourceOverride);
    } else {
      db.exec(
        'INSERT OR IGNORE INTO api_calls ' +
        '(model, model_normalized, prompt_tokens, completion_tokens, ' +
        'cache_creation_tokens, cache_read_tokens, is_user_turn, ' +
        'timestamp, session_id, log_file, source) ' +
        'SELECT model, model_normalized, prompt_tokens, completion_tokens, ' +
        'cache_creation_tokens, cache_read_tokens, is_user_turn, ' +
        'timestamp, session_id, log_file, source FROM import_db.api_calls'
      );
      db.exec('INSERT OR REPLACE INTO session_workspaces SELECT * FROM import_db.session_workspaces');
      db.exec('INSERT OR REPLACE INTO parsed_logs SELECT * FROM import_db.parsed_logs');
    }
  } finally {
    db.exec('DETACH DATABASE import_db');
  }
}


function syncLogsToDB(db, logsDir, sessionDir, force = false, source = 'local') {
  const existing = db.prepare('SELECT COUNT(*) AS c FROM api_calls WHERE source = ?').get(source).c;
  const logFiles = fs.readdirSync(logsDir)
    .filter(f => f.startsWith('process-') && f.endsWith('.log'))
    .sort()
    .map(f => path.join(logsDir, f));

  // Clear parse tracker so all logs are re-parsed; keep existing api_calls (INSERT OR IGNORE handles dedup)
  if (force) {
    db.prepare('DELETE FROM parsed_logs WHERE source = ?').run(source);
    process.stderr.write(`  🔄 Force re-sync (${source}): re-parsing ${logFiles.length} log files (keeping ${existing.toLocaleString()} existing records)\n`);
  }
  let totalInserted = 0;
  let parsedCount = 0;

  for (const logPath of logFiles) {
    const filename = path.basename(logPath);
    const mtime = fs.statSync(logPath).mtimeMs / 1000;
    if (!force && isLogParsed(db, filename, mtime, source)) continue;
    const records = parseLogFile(logPath);
    records.forEach(r => { r.model_normalized = normalizeModel(r.model); });
    insertRecords(db, records, source);
    markLogParsed(db, filename, mtime, records.length, source);
    totalInserted += records.length;
    parsedCount++;
    if (force) {
      process.stderr.write(`  📄 [${parsedCount}/${logFiles.length}] ${filename} (${records.length} records)\n`);
    }
  }
  const workspaces = loadSessionWorkspaces(sessionDir);
  for (const [sid, cwd] of Object.entries(workspaces)) {
    upsertSessionWorkspace(db, sid, cwd, source);
  }
  if (parsedCount > 0) {
    const totalNow = db.prepare('SELECT COUNT(*) AS c FROM api_calls WHERE source = ?').get(source).c;
    const newRecords = totalNow - existing;
    process.stderr.write(`  ✅ Synced ${parsedCount} log files (${source}): ${newRecords.toLocaleString()} new records (${totalNow.toLocaleString()} total)\n`);
  }
  return totalInserted;
}


function listCodespaces(includeStopped = false) {
  const proc = spawnSync('gh', ['cs', 'list', '--json', 'name,state,lastUsedAt', '--limit', '1000'], { encoding: 'utf-8' });
  if (proc.status !== 0) {
    const err = (proc.stderr || '').trim();
    process.stderr.write(`  ⚠️ Codespaces sync skipped: ${err || 'failed to list codespaces'}\n`);
    return [];
  }
  let items = [];
  try {
    items = JSON.parse(proc.stdout || '[]');
  } catch {
    process.stderr.write('  ⚠️ Codespaces sync skipped: invalid JSON from gh cs list\n');
    return [];
  }
  const allowed = new Set(['Available']);
  if (includeStopped) allowed.add('Shutdown');
  return items.filter(cs => cs.name && allowed.has(cs.state));
}


function syncCodespacesToDB(db, includeStopped = false) {
  const codespaces = listCodespaces(includeStopped);
  if (!codespaces.length) return 0;
  let total = 0;
  for (const cs of codespaces) {
    const name = cs.name;
    const state = cs.state || '';
    const lastUsedAt = cs.lastUsedAt ?? null;
    if (lastUsedAt && getCodespaceLastUsed(db, name) === lastUsedAt) {
      process.stderr.write(`  ⏭️  Skipping ${name} (unchanged lastUsedAt)\n`);
      continue;
    }

    const shouldStop = state === 'Shutdown';
    const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'copilot-cs-'));
    let copied = false;
    try {
      const stage = path.join(tmpDir, name);
      fs.mkdirSync(stage, { recursive: true });
      const cp = spawnSync('gh', ['cs', 'cp', '-r', '-c', name, 'remote:~/.copilot', stage], { encoding: 'utf-8' });
      if (cp.status !== 0) {
        const err = (cp.stderr || '').trim();
        process.stderr.write(`  ⚠️ Failed to copy ${name}: ${err || 'gh cs cp failed'}\n`);
        continue;
      }

      const copilotDir = path.join(stage, '.copilot');
      const logsDir = path.join(copilotDir, 'logs');
      const sessionDir = path.join(copilotDir, 'session-state');
      if (!fs.existsSync(logsDir)) {
        process.stderr.write(`  ⚠️ Skipping ${name}: no .copilot/logs in copied data\n`);
        continue;
      }
      total += syncLogsToDB(db, logsDir, sessionDir, false, `codespace:${name}`);
      copied = true;
    } finally {
      if (shouldStop) {
        spawnSync('gh', ['cs', 'stop', '-c', name], { stdio: 'ignore' });
      }
      fs.rmSync(tmpDir, { recursive: true, force: true });
    }

    if (copied) upsertCodespaceSyncState(db, name, lastUsedAt);
  }
  return total;
}


function normalizeModel(modelName) {
  for (const prefix of ['sweagent-capi:', 'capi:']) {
    if (modelName.startsWith(prefix)) {
      modelName = modelName.slice(prefix.length);
    }
  }
  modelName = modelName.replace(/^capi-[a-z]+-ptuc-[a-z0-9]+(?:-ib)?-/, '');
  modelName = modelName.replace(/:defaultReasoningEffort=\w+/, '');
  modelName = modelName.replace(/-\d{4}-\d{2}-\d{2}$/, '');
  return modelName;
}


function getPricing(modelName, timestamp) {
  const normalized = normalizeModel(modelName);
  const mp = getPeriod(timestamp).model_pricing;
  if (mp[normalized]) return mp[normalized];
  for (const key of Object.keys(mp)) {
    if (normalized.startsWith(key) || key.startsWith(normalized)) {
      return mp[key];
    }
  }
  return null;
}


function getPremiumMultiplier(modelName, timestamp) {
  const normalized = normalizeModel(modelName);
  const mult = getPeriod(timestamp).premium_multiplier;
  if (normalized in mult) return mult[normalized];
  for (const key of Object.keys(mult)) {
    if (normalized.startsWith(key) || key.startsWith(normalized)) {
      return mult[key];
    }
  }
  return 1;
}


function parseLogFile(logPath) {
  const content = fs.readFileSync(logPath, { encoding: 'utf-8' });
  const records = [];
  const lines = content.split('\n');

  let lastModel = 'unknown';
  let lastTimestamp = null;
  let lastSession = null;
  let lastInitiator = 'agent';

  const tsRe = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})/;
  const sessionRe = /(?:Workspace initialized|Created ACP session|Flushed \d+ events to session)[: ]+([0-9a-f-]{36})/;
  const initiatorRe = /PremiumRequestProcessor: Setting X-Initiator to '(\w+)'/;
  const modelRe = /"model"\s*:\s*"([^"]+)"/;

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];

    const tsMatch = tsRe.exec(line);
    if (tsMatch) lastTimestamp = tsMatch[1];

    const sessionMatch = sessionRe.exec(line);
    if (sessionMatch) lastSession = sessionMatch[1];

    const initiatorMatch = initiatorRe.exec(line);
    if (initiatorMatch) lastInitiator = initiatorMatch[1];

    const modelMatch = modelRe.exec(line);
    if (modelMatch) {
      const candidate = modelMatch[1];
      if (!candidate.startsWith('{') && (candidate.includes('claude') || candidate.includes('gpt') || candidate.includes('gemini'))) {
        lastModel = candidate;
      }
    }

    if (line.includes('"completion_tokens"')) {
      const blockStart = Math.max(0, i - 10);
      const blockEnd = Math.min(lines.length, i + 15);
      const block = lines.slice(blockStart, blockEnd).join('\n');

      const promptMatch = /"prompt_tokens"\s*:\s*(\d+)/.exec(block);
      const completionMatch = /"completion_tokens"\s*:\s*(\d+)/.exec(block);
      const cacheCreationMatch = /"cache_creation_input_tokens"\s*:\s*(\d+)/.exec(block);
      const cacheReadMatch = /"cache_read_input_tokens"\s*:\s*(\d+)/.exec(block);
      const cachedTokensMatch = /"cached_tokens"\s*:\s*(\d+)/.exec(block);

      const blockModelMatch = /"model"\s*:\s*"([^"]+)"/.exec(block);
      if (blockModelMatch) {
        const candidate = blockModelMatch[1];
        if (candidate.includes('claude') || candidate.includes('gpt') || candidate.includes('gemini')) {
          lastModel = candidate;
        }
      }

      if (promptMatch && completionMatch) {
        let cacheRead = cacheReadMatch ? parseInt(cacheReadMatch[1], 10) : 0;
        if (!cacheRead && cachedTokensMatch) {
          cacheRead = parseInt(cachedTokensMatch[1], 10);
        }

        records.push({
          model: lastModel,
          prompt_tokens: parseInt(promptMatch[1], 10),
          completion_tokens: parseInt(completionMatch[1], 10),
          cache_creation_tokens: cacheCreationMatch ? parseInt(cacheCreationMatch[1], 10) : 0,
          cache_read_tokens: cacheRead,
          is_user_turn: lastInitiator === 'user',
          timestamp: lastTimestamp,
          session_id: lastSession,
          log_file: path.basename(logPath),
        });
        lastInitiator = 'agent';
      }
    }
  }

  return records;
}


function loadSessionWorkspaces(sessionDir) {
  const workspaces = {};
  if (!fs.existsSync(sessionDir)) return workspaces;
  let entries;
  try { entries = fs.readdirSync(sessionDir, { withFileTypes: true }); } catch { return workspaces; }
  for (const ent of entries) {
    if (!ent.isDirectory()) continue;
    const wsFile = path.join(sessionDir, ent.name, 'workspace.yaml');
    if (!fs.existsSync(wsFile)) continue;
    try {
      const text = fs.readFileSync(wsFile, 'utf-8');
      const m = /cwd:\s*(.+)/.exec(text);
      if (m) workspaces[ent.name] = m[1].trim();
    } catch { /* skip */ }
  }
  return workspaces;
}


function projectName(cwd) {
  const home = os.homedir();
  let p = cwd.replace(home, '~');
  p = p.replace(/~\/Library\/Mobile Documents\/iCloud~md~obsidian\/Documents\//, '📓 ');
  return p;
}


// ─── Cost helpers ────────────────────────────────────────────────────────────

function calcCost(model, stats, timestamp) {
  const pricing = getPricing(model, timestamp);
  if (!pricing) return 0.0;
  const netInput = Math.max(0, stats.prompt_tokens - stats.cache_read_tokens - stats.cache_creation_tokens);
  return (
    (netInput / 1e6) * pricing.input +
    (stats.completion_tokens / 1e6) * pricing.output +
    (stats.cache_read_tokens / 1e6) * pricing.cache_read +
    (stats.cache_creation_tokens / 1e6) * pricing.cache_write
  );
}


function calcCostNocache(model, stats, timestamp) {
  const pricing = getPricing(model, timestamp);
  if (!pricing) return 0.0;
  return (
    (stats.prompt_tokens / 1e6) * pricing.input +
    (stats.completion_tokens / 1e6) * pricing.output
  );
}


function sumDailyCost(model, dailyStats, costFn) {
  let total = 0;
  for (const day of Object.keys(dailyStats)) {
    if (dailyStats[day][model]) total += costFn(model, dailyStats[day][model], day);
  }
  return total;
}


function sumDailyPremCost(model, dailyStats) {
  let total = 0;
  for (const day of Object.keys(dailyStats)) {
    if (dailyStats[day][model]) total += dailyStats[day][model].premium_requests * getPremiumRequestCost(day);
  }
  return total;
}


function uncachedInput(stats) {
  return Math.max(0, stats.prompt_tokens - stats.cache_read_tokens - stats.cache_creation_tokens);
}


function cacheHitPct(promptTokens, cacheReadTokens) {
  if (promptTokens === 0) return '-';
  return `${Math.round(cacheReadTokens / promptTokens * 100)}%`;
}


function fmtTokens(n) {
  if (n >= 1e6) return `${(n / 1e6).toLocaleString('en-US', { minimumFractionDigits: 1, maximumFractionDigits: 1 })}M`;
  if (n >= 1e3) return `${(n / 1e3).toLocaleString('en-US', { minimumFractionDigits: 1, maximumFractionDigits: 1 })}K`;
  return String(n);
}


function fmtCost(cost) {
  if (cost >= 100) return `$${cost.toLocaleString('en-US', { minimumFractionDigits: 0, maximumFractionDigits: 0 })}`;
  if (cost >= 1) return `$${cost.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
  return `$${cost.toLocaleString('en-US', { minimumFractionDigits: 3, maximumFractionDigits: 3 })}`;
}


// ─── Unicode display width ──────────────────────────────────────────────────

function isEastAsianWide(cp) {
  // CJK Unified Ideographs
  if (cp >= 0x4E00 && cp <= 0x9FFF) return true;
  // CJK Unified Ideographs Extension A
  if (cp >= 0x3400 && cp <= 0x4DBF) return true;
  // CJK Unified Ideographs Extension B–F
  if (cp >= 0x20000 && cp <= 0x2FA1F) return true;
  // CJK Compatibility Ideographs
  if (cp >= 0xF900 && cp <= 0xFAFF) return true;
  // CJK Compatibility Forms
  if (cp >= 0xFE30 && cp <= 0xFE4F) return true;
  // Hangul Syllables
  if (cp >= 0xAC00 && cp <= 0xD7AF) return true;
  // Hangul Jamo
  if (cp >= 0x1100 && cp <= 0x115F) return true;
  if (cp >= 0x2329 && cp <= 0x232A) return true;
  // Hangul Jamo Extended
  if (cp >= 0xA960 && cp <= 0xA97F) return true;
  if (cp >= 0xD7B0 && cp <= 0xD7FF) return true;
  // Fullwidth Forms
  if (cp >= 0xFF01 && cp <= 0xFF60) return true;
  if (cp >= 0xFFE0 && cp <= 0xFFE6) return true;
  // CJK Radicals Supplement
  if (cp >= 0x2E80 && cp <= 0x2EFF) return true;
  // Kangxi Radicals
  if (cp >= 0x2F00 && cp <= 0x2FDF) return true;
  // CJK Symbols and Punctuation
  if (cp >= 0x3000 && cp <= 0x303E) return true;
  // Hiragana, Katakana
  if (cp >= 0x3040 && cp <= 0x30FF) return true;
  // Katakana Phonetic Extensions
  if (cp >= 0x31F0 && cp <= 0x31FF) return true;
  // Enclosed CJK Letters and Months
  if (cp >= 0x3200 && cp <= 0x32FF) return true;
  // CJK Compatibility
  if (cp >= 0x3300 && cp <= 0x33FF) return true;
  // Bopomofo
  if (cp >= 0x3100 && cp <= 0x312F) return true;
  if (cp >= 0x31A0 && cp <= 0x31BF) return true;
  // Yi Syllables, Yi Radicals
  if (cp >= 0xA000 && cp <= 0xA4CF) return true;
  return false;
}

function isCombiningMark(cp) {
  if (cp >= 0x0300 && cp <= 0x036F) return true;
  if (cp >= 0x1AB0 && cp <= 0x1AFF) return true;
  if (cp >= 0x1DC0 && cp <= 0x1DFF) return true;
  if (cp >= 0x20D0 && cp <= 0x20FF) return true;
  if (cp >= 0xFE20 && cp <= 0xFE2F) return true;
  return false;
}


function displayWidth(s) {
  const chars = Array.from(s);
  let w = 0;
  let i = 0;
  while (i < chars.length) {
    const ch = chars[i];
    const cp = ch.codePointAt(0);
    // VS16 (emoji presentation selector) makes the preceding char 2-wide
    if (i + 1 < chars.length && chars[i + 1] === '\uFE0F') {
      w += 2;
      i += 2;
      continue;
    }
    if (isCombiningMark(cp)) {
      i += 1;
      continue;
    }
    w += isEastAsianWide(cp) ? 2 : 1;
    i += 1;
  }
  return w;
}


function padCell(cell, width, alignRight = false) {
  const dw = displayWidth(cell);
  const padding = Math.max(0, width - dw);
  if (alignRight) return ' '.repeat(padding) + cell;
  return cell + ' '.repeat(padding);
}


// ─── Pretty table helpers ───────────────────────────────────────────────────

function printTable(title, headers, rows, footer = null, notes = null) {
  const allRows = [headers, ...rows];
  if (footer) allRows.push(footer);
  const colWidths = headers.map((_, i) =>
    Math.max(...allRows.map(row => displayWidth(String(row[i]))))
  );

  const innerWidth = colWidths.reduce((a, b) => a + b, 0) + 2 * (colWidths.length - 1) + 4;

  function fmtRow(cells) {
    const parts = cells.map((cell, i) => padCell(String(cell), colWidths[i], i > 0));
    const content = '  ' + parts.join('  ');
    const padding = Math.max(0, innerWidth - displayWidth(content));
    return '│' + content + ' '.repeat(padding) + '│';
  }

  function separator() {
    const content = '  ' + colWidths.map(w => '─'.repeat(w)).join('  ') + '  ';
    const padding = Math.max(0, innerWidth - content.length);
    return '│' + content + ' '.repeat(padding) + '│';
  }

  const bar = '─'.repeat(innerWidth);

  console.log(`┌─ ${title} ${'─'.repeat(innerWidth - title.length - 3)}┐`);
  console.log(`│${' '.repeat(innerWidth)}│`);
  console.log(fmtRow(headers));
  console.log(separator());
  for (const row of rows) {
    console.log(fmtRow(row));
  }
  if (footer) {
    console.log(separator());
    console.log(fmtRow(footer));
  }
  console.log(`│${' '.repeat(innerWidth)}│`);
  console.log(`└${bar}┘`);
  if (notes) {
    for (const note of notes) {
      console.log(`  ${note}`);
    }
  }
}


// ─── CLI argument parsing ───────────────────────────────────────────────────

function parseArgs(argv) {
  const args = {
    days: null,
    all: false,
    today: false,
    yesterday: false,
    from_days: null,
    to_days: null,
    logs_dir: null,
    json: false,
    help: false,
    sync: false,
    import_file: null,
    export_file: null,
    codespaces_sync: false,
    codespaces_include_stopped: false,
  };
  const positional = [];
  let i = 0;
  while (i < argv.length) {
    const a = argv[i];
    if (a === '--all') { args.all = true; }
    else if (a === '--today') { args.today = true; }
    else if (a === '--yesterday') { args.yesterday = true; }
    else if (a === '--json') { args.json = true; }
    else if (a === '--sync') { args.sync = true; }
    else if (a === '--codespaces-sync') { args.codespaces_sync = true; }
    else if (a === '--codespaces-include-stopped') { args.codespaces_include_stopped = true; }
    else if (a === '--help' || a === '-h') { args.help = true; }
    else if (a === '--from') { i++; args.from_days = parseInt(argv[i], 10); }
    else if (a === '--to') { i++; args.to_days = parseInt(argv[i], 10); }
    else if (a === '--logs-dir') { i++; args.logs_dir = argv[i]; }
    else if (a === '--import-file') { i++; args.import_file = argv[i]; }
    else if (a === '--export-file') { i++; args.export_file = argv[i]; }
    else if (/^\d+$/.test(a)) { positional.push(parseInt(a, 10)); }
    else {
      process.stderr.write(`Unknown option: ${a}\n`);
      process.exit(1);
    }
    i++;
  }
  if (positional.length > 0) args.days = positional[0];
  if (args.codespaces_include_stopped && !args.codespaces_sync) {
    process.stderr.write('--codespaces-include-stopped requires --codespaces-sync\n');
    process.exit(1);
  }
  return args;
}


function showHelp() {
  console.log(`usage: copilot-token-cost.js [days] [--all] [--today] [--yesterday]
                            [--from N] [--to N] [--logs-dir PATH] [--json]
                            [--sync] [--import-file FILE] [--export-file FILE]
                            [--codespaces-sync] [--codespaces-include-stopped]

Copilot CLI Token Cost Calculator

positional arguments:
  days                  Number of days to look back (default: 7)

options:
  -h, --help            show this help message and exit
  --all                 Process all available logs
  --today               Today only
  --yesterday           Yesterday only
  --from N              Start from N days ago (0=today, 1=yesterday)
  --to N                End at N days ago (0=today, 1=yesterday)
  --logs-dir PATH       Override logs directory
  --json                Output as JSON
  --sync                Force full re-sync of all log files
  --import-file FILE    Import data from JSONL or SQLite file
  --export-file FILE    Export data as JSONL
  --codespaces-sync     Sync Copilot data from running Codespaces via gh cs cp
  --codespaces-include-stopped
                        Include stopped Codespaces (will wake and sync them)

Examples:
  copilot-token-cost.js              # last 7 days
  copilot-token-cost.js 30           # last 30 days
  copilot-token-cost.js 1            # today
  copilot-token-cost.js --today      # today
  copilot-token-cost.js --yesterday  # yesterday only
  copilot-token-cost.js --from 3     # 3 days ago until now
  copilot-token-cost.js --from 3 --to 1  # 3 days ago to yesterday
  copilot-token-cost.js --from 1 --to 1  # yesterday only
  copilot-token-cost.js --all        # all logs`);
}


// ─── Date helpers ───────────────────────────────────────────────────────────

function todayMidnight() {
  const d = new Date();
  d.setHours(0, 0, 0, 0);
  return d;
}

function addDays(date, n) {
  const d = new Date(date);
  d.setDate(d.getDate() + n);
  return d;
}

function fmtDate(d) {
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, '0');
  const day = String(d.getDate()).padStart(2, '0');
  return `${y}-${m}-${day}`;
}

function parseISOTimestamp(ts) {
  // Parse "YYYY-MM-DDTHH:MM:SS" as local time
  const m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})/.exec(ts);
  if (!m) return null;
  return new Date(parseInt(m[1]), parseInt(m[2]) - 1, parseInt(m[3]),
                  parseInt(m[4]), parseInt(m[5]), parseInt(m[6]));
}


// ─── Aggregation helpers ────────────────────────────────────────────────────

function newStats() {
  return {
    api_calls: 0, prompt_tokens: 0, completion_tokens: 0,
    cache_creation_tokens: 0, cache_read_tokens: 0, premium_requests: 0,
  };
}

function addRecord(s, r, model) {
  s.api_calls += 1;
  s.prompt_tokens += r.prompt_tokens;
  s.completion_tokens += r.completion_tokens;
  s.cache_creation_tokens += r.cache_creation_tokens;
  s.cache_read_tokens += r.cache_read_tokens;
  if (r.is_user_turn) {
    s.premium_requests += getPremiumMultiplier(model, r.timestamp);
  }
}


// ─── Main ───────────────────────────────────────────────────────────────────

function main() {
  const args = parseArgs(process.argv.slice(2));

  if (args.help) {
    showHelp();
    process.exit(0);
  }

  const home = os.homedir();
  const logsDir = args.logs_dir || path.join(home, '.copilot', 'logs');
  const sessionDir = path.join(home, '.copilot', 'session-state');

  // ─── DB setup and sync ──────────────────────────────────────────────
  const db = initDB(DB_PATH);

  if (fs.existsSync(logsDir)) {
    syncLogsToDB(db, logsDir, sessionDir, args.sync);
  }

  if (args.codespaces_sync) {
    syncCodespacesToDB(db, args.codespaces_include_stopped);
  }

  if (args.import_file) {
    const importPath = args.import_file;
    if (importPath.endsWith('.db') || importPath.endsWith('.sqlite')) {
      importSQLiteDB(db, importPath);
    } else {
      importJSONL(db, importPath);
    }
  }

  if (args.export_file) {
    exportJSONL(db, args.export_file);
    db.close();
    return;
  }

  const midnight = todayMidnight();

  let cutoff, cutoffEnd, periodLabel;

  if (args.all) {
    cutoff = new Date(0);
    cutoffEnd = null;
    periodLabel = 'all time';
  } else if (args.today) {
    cutoff = midnight;
    cutoffEnd = null;
    periodLabel = 'today';
  } else if (args.yesterday) {
    cutoff = addDays(midnight, -1);
    cutoffEnd = midnight;
    periodLabel = 'yesterday';
  } else if (args.from_days !== null) {
    let fromDays = args.from_days;
    let toDays = args.to_days !== null ? args.to_days : 0;
    if (fromDays < toDays) { [fromDays, toDays] = [toDays, fromDays]; }
    cutoff = addDays(midnight, -fromDays);
    cutoffEnd = toDays > 0 ? addDays(addDays(midnight, -toDays), 1) : null;
    if (fromDays === toDays) {
      periodLabel = `${fmtDate(cutoff)} (1 day)`;
    } else {
      periodLabel = `${fromDays}d ago → ${toDays === 0 ? 'today' : `${toDays}d ago`}`;
    }
  } else {
    const days = args.days !== null ? args.days : 7;
    cutoff = addDays(midnight, -(days - 1));
    cutoffEnd = null;
    periodLabel = `last ${days} day${days !== 1 ? 's' : ''}`;
  }

  // Build date_from/date_to as ISO timestamps for DB queries (matching Python format)
  function fmtDateTime(d) {
    return `${fmtDate(d)}T00:00:00`;
  }
  const dateFrom = cutoff.getTime() > 0 ? fmtDateTime(cutoff) : null;
  const dateTo = cutoffEnd ? fmtDateTime(cutoffEnd) : null;
  const dateFromDisplay = cutoff.getTime() > 0 ? fmtDate(cutoff) : null;
  const dateToDisplay = cutoffEnd ? fmtDate(addDays(cutoffEnd, -1)) : fmtDate(new Date());
  const dateRange = dateFromDisplay ? `${dateFromDisplay} → ${dateToDisplay}` : null;

  // ─── Query aggregated stats from DB ──────────────────────────────────
  const dailyStats = queryDailyStats(db, dateFrom, dateTo);
  for (const day of Object.keys(dailyStats)) {
    for (const model of Object.keys(dailyStats[day])) {
      const s = dailyStats[day][model];
      s.premium_requests = s.user_turns * getPremiumMultiplier(model, day);
      delete s.user_turns;
    }
  }

  const modelStats = queryModelStats(db, dateFrom, dateTo);
  for (const model of Object.keys(modelStats)) {
    const s = modelStats[model];
    delete s.user_turns;
    // Compute model-level premium_requests from daily (multiplier varies by day)
    s.premium_requests = 0;
    for (const day of Object.keys(dailyStats)) {
      if (dailyStats[day][model]) s.premium_requests += dailyStats[day][model].premium_requests;
    }
  }

  const projectStatsRaw = queryProjectStats(db, dateFrom, dateTo);
  const projectStats = {};
  for (const [cwd, s] of Object.entries(projectStatsRaw)) {
    const proj = cwd ? projectName(cwd) : '(unknown)';
    s.premium_requests = s.user_turns; // already aggregated across models
    delete s.user_turns;
    if (projectStats[proj]) {
      for (const k of Object.keys(s)) {
        projectStats[proj][k] += s[k];
      }
    } else {
      projectStats[proj] = { ...s };
    }
  }

  const filteredRecords = queryRecords(db, dateFrom, dateTo);
  const sessionWorkspaces = querySessionWorkspaces(db);

  const totalRecords = Object.values(modelStats).reduce((a, s) => a + s.api_calls, 0);
  const logFileCountParams = [];
  let logFileCountSQL = 'SELECT COUNT(DISTINCT log_file) AS cnt FROM api_calls';
  if (dateFrom && dateTo) {
    logFileCountSQL += ' WHERE timestamp >= ? AND timestamp < ?';
    logFileCountParams.push(dateFrom, dateTo);
  } else if (dateFrom) {
    logFileCountSQL += ' WHERE timestamp >= ?';
    logFileCountParams.push(dateFrom);
  }
  const logFileCount = db.prepare(logFileCountSQL).get(...logFileCountParams).cnt;

  if (totalRecords === 0) {
    console.log(`No API calls found in ${periodLabel}.`);
    db.close();
    process.exit(0);
  }

  // ─── JSON output ─────────────────────────────────────────────────────
  if (args.json) {
    const output = {
      period: periodLabel, date_range: dateRange, log_files: logFileCount,
      api_calls: totalRecords, models: {}, daily: {}, projects: {},
      total_cost: 0.0, total_cost_without_cache: 0.0,
    };
    for (const model of Object.keys(modelStats).sort()) {
      const stats = modelStats[model];
      const cost = sumDailyCost(model, dailyStats, calcCost);
      const costNc = sumDailyCost(model, dailyStats, calcCostNocache);
      output.models[model] = {
        ...stats, input_uncached_tokens: uncachedInput(stats),
        premium_request_cost: Math.round(sumDailyPremCost(model, dailyStats) * 10000) / 10000,
        cost: Math.round(cost * 10000) / 10000,
        cost_without_cache: Math.round(costNc * 10000) / 10000,
      };
      output.total_cost += cost;
      output.total_cost_without_cache += costNc;
    }
    for (const day of Object.keys(dailyStats).sort()) {
      let dayTotal = 0, dayTotalNc = 0;
      output.daily[day] = {};
      for (const model of Object.keys(dailyStats[day])) {
        const stats = dailyStats[day][model];
        const cost = calcCost(model, stats, day);
        const costNc = calcCostNocache(model, stats, day);
        output.daily[day][model] = {
          ...stats, input_uncached_tokens: uncachedInput(stats),
          cost: Math.round(cost * 10000) / 10000,
          cost_without_cache: Math.round(costNc * 10000) / 10000,
        };
        dayTotal += cost;
        dayTotalNc += costNc;
      }
      output.daily[day]._total_cost = Math.round(dayTotal * 10000) / 10000;
      output.daily[day]._total_cost_without_cache = Math.round(dayTotalNc * 10000) / 10000;
    }
    // Per-project costs from records
    const projCosts = {};
    for (const r of filteredRecords) {
      const sid = r.session_id;
      const cwd = sid ? (sessionWorkspaces[sid] || '') : '';
      const proj = cwd ? projectName(cwd) : '(unknown)';
      const model = r.model_normalized;
      const rs = {
        prompt_tokens: r.prompt_tokens, completion_tokens: r.completion_tokens,
        cache_creation_tokens: r.cache_creation_tokens, cache_read_tokens: r.cache_read_tokens,
      };
      if (!projCosts[proj]) projCosts[proj] = { cost: 0, cost_without_cache: 0 };
      projCosts[proj].cost += calcCost(model, rs, r.timestamp);
      projCosts[proj].cost_without_cache += calcCostNocache(model, rs, r.timestamp);
    }
    for (const proj of Object.keys(projectStats)) {
      output.projects[proj] = {
        ...projectStats[proj], input_uncached_tokens: uncachedInput(projectStats[proj]),
        cost: Math.round((projCosts[proj] || { cost: 0 }).cost * 10000) / 10000,
        cost_without_cache: Math.round((projCosts[proj] || { cost_without_cache: 0 }).cost_without_cache * 10000) / 10000,
      };
    }
    output.total_cost = Math.round(output.total_cost * 10000) / 10000;
    output.total_cost_without_cache = Math.round(output.total_cost_without_cache * 10000) / 10000;
    output.total_premium_request_cost = Math.round(
      Object.keys(modelStats).reduce((a, m) => a + sumDailyPremCost(m, dailyStats), 0) * 10000
    ) / 10000;
    console.log(JSON.stringify(output, null, 2));
    db.close();
    return;
  }

  // ─── Pretty output ───────────────────────────────────────────────────
  console.log();
  const title = 'COPILOT CLI - TOKEN USAGE & COST REPORT';
  const titleWidth = title.length + 10;
  const titlePadL = Math.floor((titleWidth - title.length) / 2);
  const titlePadR = titleWidth - title.length - titlePadL;
  console.log(`╔${'═'.repeat(titleWidth)}╗`);
  console.log(`║${' '.repeat(titlePadL)}${title}${' '.repeat(titlePadR)}║`);
  console.log(`╚${'═'.repeat(titleWidth)}╝`);
  const totalPremium = Object.values(modelStats).reduce((a, s) => a + s.premium_requests, 0);
  const dateSuffix = dateRange ? ` (${dateRange})` : '';
  console.log(`  Period: ${periodLabel}${dateSuffix}  │  Log files: ${logFileCount}  │  API calls: ${totalRecords.toLocaleString('en-US')}  │  Premium requests: ${totalPremium.toLocaleString('en-US')}`);
  console.log();

  // ── Per-model table ──────────────────────────────────────────────────
  const modelHeaders = ['Model', 'Calls', 'Premium', 'Prem Cost', 'Input', 'Cached', 'Cache Write', 'Output', 'Hit%', 'Cost', 'No-Cache'];
  const modelRows = [];
  let tCost = 0, tNc = 0, tUnc = 0, tCached = 0, tCw = 0, tOut = 0, tCalls = 0, tPrompt = 0, tPremium = 0, tPremCost = 0;
  const sortedModels = Object.keys(modelStats).sort((a, b) =>
    sumDailyCost(b, dailyStats, calcCostNocache) - sumDailyCost(a, dailyStats, calcCostNocache)
  );
  for (const model of sortedModels) {
    const s = modelStats[model];
    const cost = sumDailyCost(model, dailyStats, calcCost);
    const costNc = sumDailyCost(model, dailyStats, calcCostNocache);
    const unc = uncachedInput(s);
    tCost += cost; tNc += costNc;
    tUnc += unc; tCached += s.cache_read_tokens; tCw += s.cache_creation_tokens;
    tOut += s.completion_tokens; tCalls += s.api_calls; tPrompt += s.prompt_tokens;
    tPremium += s.premium_requests;
    const p = getPricing(model);
    const mult = getPremiumMultiplier(model);
    const premiumStr = mult > 0 ? s.premium_requests.toLocaleString('en-US') : '-';
    const premCost = sumDailyPremCost(model, dailyStats);
    tPremCost += premCost;
    const premCostStr = mult === 0 ? '-' : fmtCost(premCost);
    modelRows.push([
      model, s.api_calls.toLocaleString('en-US'), premiumStr, premCostStr,
      fmtTokens(unc), fmtTokens(s.cache_read_tokens),
      fmtTokens(s.cache_creation_tokens), fmtTokens(s.completion_tokens),
      cacheHitPct(s.prompt_tokens, s.cache_read_tokens),
      p ? fmtCost(cost) : 'N/A', p ? fmtCost(costNc) : 'N/A',
    ]);
  }
  const modelFooter = [
    'TOTAL', tCalls.toLocaleString('en-US'), tPremium.toLocaleString('en-US'), fmtCost(tPremCost),
    fmtTokens(tUnc), fmtTokens(tCached),
    fmtTokens(tCw), fmtTokens(tOut),
    cacheHitPct(tPrompt, tCached),
    fmtCost(tCost), fmtCost(tNc),
  ];
  const savingsPct = tNc > 0 ? (1 - tCost / tNc) * 100 : 0;
  const modelNotes = tNc > 0 ? [`💰 Cache savings: ${fmtCost(tNc - tCost)} (${Math.round(savingsPct)}% reduction)`] : [];
  printTable('PER-MODEL SUMMARY', modelHeaders, modelRows, modelFooter, modelNotes);
  console.log();

  // ── Cost per premium request ─────────────────────────────────────────
  const premHeaders = ['Model', 'Multiplier', 'Premiums', 'API Cost', '$/Premium', 'Prem Cost', 'Discount'];
  const premRows = [];
  let premTotalCost = 0, premTotalReqs = 0;
  const sortedByPremium = Object.keys(modelStats).sort((a, b) =>
    modelStats[b].premium_requests - modelStats[a].premium_requests
  );
  for (const model of sortedByPremium) {
    const s = modelStats[model];
    const mult = getPremiumMultiplier(model);
    if (mult === 0) continue;
    const cost = sumDailyCost(model, dailyStats, calcCost);
    if (s.premium_requests > 0) {
      premTotalCost += cost;
      premTotalReqs += s.premium_requests;
      const costPer = cost / s.premium_requests;
      const premCost = sumDailyPremCost(model, dailyStats);
      const discount = cost > 0 ? ((1 - premCost / cost) * 100).toFixed(0) + '%' : '-';
      premRows.push([
        model, `${mult}×`, s.premium_requests.toLocaleString('en-US'),
        fmtCost(cost), fmtCost(costPer), fmtCost(premCost), discount,
      ]);
    } else {
      premRows.push([
        model, `${mult}×`, '-',
        fmtCost(cost), 'N/A', '-', '-',
      ]);
    }
  }
  if (premRows.length > 0) {
    const avgCost = premTotalReqs > 0 ? premTotalCost / premTotalReqs : 0;
    const totalPremCost = Object.keys(modelStats).reduce((a, m) => a + sumDailyPremCost(m, dailyStats), 0);
    const totalDiscount = premTotalCost > 0 ? ((1 - totalPremCost / premTotalCost) * 100).toFixed(0) + '%' : '-';
    const premFooter = ['TOTAL', '', premTotalReqs.toLocaleString('en-US'), fmtCost(premTotalCost), fmtCost(avgCost), fmtCost(totalPremCost), totalDiscount];
    const premNotes = ['ℹ️  Models with 0× multiplier (free tier) are excluded'];
    const missingCost = tCost - premTotalCost;
    if (missingCost > 0.001) {
      premNotes.push(`⚠  ${fmtCost(missingCost)} from models without premium data excluded from $/premium avg`);
    }
    printTable('COST PER PREMIUM REQUEST', premHeaders, premRows, premFooter, premNotes);
    console.log();
  }

  // ── Daily table ──────────────────────────────────────────────────────
  const dailyHeaders = ['Date', 'Calls', 'Premium', 'Input', 'Cached', 'Output', 'Hit%', 'Cost', 'No-Cache', 'Prem Cost', 'Discount'];
  const dailyRows = [];
  for (const day of Object.keys(dailyStats).sort()) {
    const dayModels = dailyStats[day];
    let dCalls = 0, dPremium = 0, dUnc = 0, dCached = 0, dOut = 0, dCost = 0, dNc = 0;
    for (const [m, s] of Object.entries(dayModels)) {
      dCalls += s.api_calls;
      dPremium += s.premium_requests;
      dUnc += uncachedInput(s);
      dCached += s.cache_read_tokens;
      dOut += s.completion_tokens;
      dCost += calcCost(m, s, day);
      dNc += calcCostNocache(m, s, day);
    }
    const dTotal = dUnc + dCached;
    const dPremCost = dPremium * getPremiumRequestCost(day);
    const dDiscount = dCost > 0 ? ((1 - dPremCost / dCost) * 100).toFixed(0) + '%' : '-';
    dailyRows.push([
      day, dCalls.toLocaleString('en-US'), dPremium.toLocaleString('en-US'),
      fmtTokens(dUnc), fmtTokens(dCached),
      fmtTokens(dOut), cacheHitPct(dTotal, dCached),
      fmtCost(dCost), fmtCost(dNc), fmtCost(dPremCost), dDiscount,
    ]);
  }
  printTable('DAILY BREAKDOWN', dailyHeaders, dailyRows);
  console.log();

  // ── Per-project table ────────────────────────────────────────────────
  const projCosts = {};
  for (const r of filteredRecords) {
    const sid = r.session_id;
    const cwd = sid ? (sessionWorkspaces[sid] || '') : '';
    const proj = cwd ? projectName(cwd) : '(unknown)';
    const model = r.model_normalized;
    const rs = {
      prompt_tokens: r.prompt_tokens, completion_tokens: r.completion_tokens,
      cache_creation_tokens: r.cache_creation_tokens, cache_read_tokens: r.cache_read_tokens,
    };
    if (!projCosts[proj]) projCosts[proj] = { cost: 0, cost_nc: 0 };
    projCosts[proj].cost += calcCost(model, rs, r.timestamp);
    projCosts[proj].cost_nc += calcCostNocache(model, rs, r.timestamp);
  }

  const projHeaders = ['Project', 'Calls', 'Premium', 'Input', 'Cached', 'Output', 'Hit%', 'Cost', 'No-Cache'];
  const projRows = [];
  const sortedProjects = Object.keys(projectStats).sort((a, b) =>
    (projCosts[b] || { cost_nc: 0 }).cost_nc - (projCosts[a] || { cost_nc: 0 }).cost_nc
  );
  for (const proj of sortedProjects) {
    const s = projectStats[proj];
    const pc = projCosts[proj] || { cost: 0, cost_nc: 0 };
    projRows.push([
      proj, s.api_calls.toLocaleString('en-US'), s.premium_requests.toLocaleString('en-US'),
      fmtTokens(uncachedInput(s)),
      fmtTokens(s.cache_read_tokens), fmtTokens(s.completion_tokens),
      cacheHitPct(s.prompt_tokens, s.cache_read_tokens),
      fmtCost(pc.cost), fmtCost(pc.cost_nc),
    ]);
  }
  printTable('PER-PROJECT BREAKDOWN', projHeaders, projRows);
  console.log();

  // ── Pricing reference ────────────────────────────────────────────────
  const priceHeaders = ['Model', 'Input/1M', 'Output/1M', 'Cache Read/1M', 'Cache Write/1M'];
  const priceRows = [];
  const usedModels = [...new Set(filteredRecords.map(r => r.model_normalized))].sort();
  for (const model of usedModels) {
    const p = getPricing(model);
    if (p) {
      priceRows.push([model, `$${p.input.toFixed(2)}`, `$${p.output.toFixed(2)}`, `$${p.cache_read.toFixed(3)}`, `$${p.cache_write.toFixed(2)}`]);
    } else {
      priceRows.push([model, 'N/A', 'N/A', 'N/A', 'N/A']);
    }
  }
  printTable('PRICING REFERENCE', priceHeaders, priceRows);
  console.log();
  console.log('  ⚠  Estimated API-equivalent costs. Copilot subscriptions include token usage.');
  console.log();
  db.close();
}

main();
