#!/usr/bin/env node
import fs from 'node:fs';
import path from 'node:path';
import readline from 'node:readline';
import { fileURLToPath } from 'node:url';

export const SDK_VERSION = '1.0.31';
export const DEFAULT_MAX_FRAME_BYTES = 1024 * 1024;
export const MAX_PENDING_EXECUTIONS = 128;
export const MAX_PENDING_EXECUTIONS_PER_ATTEMPT = 8;
const COMMAND_TOOL = 'cursor_command';
const FILE_CHANGE_TOOL = 'cursor_file_change';
const CUSTOM_TOOL_SERVER = 'custom-user-tools';

class WorkerError extends Error {
  constructor(code) {
    super(code);
    this.code = code;
  }
}

function boundedString(value, maximum, allowEmpty = false) {
  return typeof value === 'string' && (allowEmpty || value.length > 0) && Buffer.byteLength(value, 'utf8') <= maximum && !value.includes('\0');
}

export function safeUsage(value) {
  if (!value || typeof value !== 'object') return undefined;
  const read = (name) => Number.isSafeInteger(value[name]) && value[name] >= 0 ? value[name] : 0;
  const usage = { inputTokens: read('inputTokens'), outputTokens: read('outputTokens'), totalTokens: read('totalTokens') };
  return Object.values(usage).some((entry) => entry !== 0) ? usage : undefined;
}

export function installedSDKVersion() {
  const sdkEntry = fileURLToPath(import.meta.resolve('@cursor/sdk'));
  let sdkDirectory = path.dirname(sdkEntry);
  for (let depth = 0; depth < 6; depth += 1) {
    const manifest = path.join(sdkDirectory, 'package.json');
    try {
      const parsed = JSON.parse(fs.readFileSync(manifest, 'utf8'));
      if (parsed?.name === '@cursor/sdk' && typeof parsed.version === 'string') return parsed.version;
    } catch {}
    sdkDirectory = path.dirname(sdkDirectory);
  }
  throw new WorkerError('sdk_version_unavailable');
}

export function agentOptions(config, store, workspace = config.stateDir, customTools = {}) {
  const exposesCustomTools = Object.keys(customTools).length > 0;
  return {
    apiKey: config.apiKey,
    model: { id: config.model },
    tools: exposesCustomTools ? ['mcp'] : [],
    disallowedTools: ['shell', 'task'],
    mcpServers: {},
    agents: {},
    local: {
      cwd: workspace,
      store,
      settingSources: [],
      customTools,
      enableAgentRetries: false,
    },
  };
}

// Cursor SDK 1.0.31 scopes custom-store lookups by the explicit local.cwd.
// Pre-tools agents were persisted with StateDir as cwd, while explicit tools
// run in a per-dialog workspace. Move only that exact legacy agent record;
// unrelated workspace records fail closed and runs/checkpoints stay untouched.
export async function migrateLegacyAgentWorkspace(store, agentId, legacyWorkspace, workspace) {
  if (!store?.agents || typeof store.agents.get !== 'function' || typeof store.agents.update !== 'function' ||
      !boundedString(agentId, 512) || !boundedString(legacyWorkspace, 4096) || !path.isAbsolute(legacyWorkspace) ||
      !boundedString(workspace, 4096) || !path.isAbsolute(workspace)) {
    throw new WorkerError('legacy_agent_migration_invalid');
  }
  if (workspace === legacyWorkspace) return false;
  const agent = await store.agents.get({ agentId });
  if (agent === null) return false;
  if (agent.cwd === workspace) return false;
  if (agent.agentId !== agentId || agent.cwd !== legacyWorkspace) {
    throw new WorkerError('legacy_agent_workspace_mismatch');
  }
  await store.agents.update({ agent: { ...agent, cwd: workspace } });
  const migrated = await store.agents.get({ agentId });
  if (migrated?.agentId !== agentId || migrated.cwd !== workspace) {
    throw new WorkerError('legacy_agent_migration_unconfirmed');
  }
  return true;
}

export function createCustomTools(attemptKey, executeTool) {
  const execute = (name) => async (args, context) => {
    const callId = context?.toolCallId;
    if (!boundedString(callId, 512) || !args || typeof args !== 'object' || Array.isArray(args)) {
      throw new WorkerError('tool_request_invalid');
    }
    return executeTool({ attemptKey, callId, name, args });
  };
  return {
    [COMMAND_TOOL]: {
      description: 'Run a command inside this dialog workspace. Read access cannot modify files; write access requires one explicit approval.',
      inputSchema: {
        type: 'object', additionalProperties: false, required: ['command', 'cwd', 'workspaceAccess'],
        properties: {
          command: { type: 'string', minLength: 1, maxLength: 32768 },
          cwd: { type: 'string', minLength: 1, maxLength: 4096 },
          workspaceAccess: { type: 'string', enum: ['read', 'write'] },
          timeoutMs: { type: 'integer', minimum: 1, maximum: 60000 },
        },
      },
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: false, openWorldHint: false },
      execute: execute(COMMAND_TOOL),
    },
    [FILE_CHANGE_TOOL]: {
      description: 'Apply an explicitly approved set of UTF-8 file writes or deletes inside this dialog workspace using exact content hashes.',
      inputSchema: {
        type: 'object', additionalProperties: false, required: ['changes'],
        properties: {
          changes: {
            type: 'array', minItems: 1, maxItems: 32,
            items: {
              type: 'object', additionalProperties: false,
              required: ['path', 'operation', 'expectedSha256', 'content'],
              properties: {
                path: { type: 'string', minLength: 1, maxLength: 4096 },
                operation: { type: 'string', enum: ['write', 'delete'] },
                expectedSha256: {
                  type: ['string', 'null'], pattern: '^[0-9a-f]{64}$',
                  description: 'SHA-256 of the current file, or null for a write without a precondition. Deletes require the current SHA-256.',
                },
                content: {
                  type: ['string', 'null'], maxLength: 1048576,
                  description: 'Complete UTF-8 content for write, or null for delete.',
                },
              },
            },
          },
        },
      },
      annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: false, openWorldHint: false },
      execute: execute(FILE_CHANGE_TOOL),
    },
  };
}

export function createRuntime(sdk, emit, installedVersion = SDK_VERSION, injectedExecutor) {
  const active = new Map();
  const toolAttempts = new Set();
  const toolFailures = new Set();
  const pendingExecutions = new Map();
  let config;
  let store;
  let nextExecutionId = 0;

  function send(frame) {
    const encoded = JSON.stringify(frame);
    if (Buffer.byteLength(encoded, 'utf8') > (config?.maxFrameBytes || DEFAULT_MAX_FRAME_BYTES)) throw new WorkerError('frame_too_large');
    emit(encoded + '\n');
  }

  function response(id, result = {}) {
    send({ type: 'response', id, ok: true, result });
  }

  function rejected(id, code = 'rejected') {
    send({ type: 'response', id, ok: false, code });
  }

  function executeTool(payload) {
    if (injectedExecutor) return injectedExecutor(payload);
    if (!toolAttempts.has(payload.attemptKey)) {
      return Promise.reject(new WorkerError('tool_attempt_closed'));
    }
    let attemptPending = 0;
    for (const pending of pendingExecutions.values()) {
      if (pending.attemptKey === payload.attemptKey) attemptPending += 1;
    }
    if (attemptPending >= MAX_PENDING_EXECUTIONS_PER_ATTEMPT || pendingExecutions.size >= MAX_PENDING_EXECUTIONS) {
      failToolAttempt(payload.attemptKey, 'tool_execution_capacity');
      return Promise.reject(new WorkerError('tool_execution_capacity'));
    }
    const id = `worker-${++nextExecutionId}`;
    return new Promise((resolve, reject) => {
      pendingExecutions.set(id, { attemptKey: payload.attemptKey, resolve, reject });
      try {
        send({ type: 'request', id, operation: 'execute_tool', payload });
      } catch (error) {
        pendingExecutions.delete(id);
        reject(error);
      }
    });
  }

  function rejectExecutions(predicate, code) {
    for (const [id, pending] of pendingExecutions) {
      if (!predicate(pending)) continue;
      pendingExecutions.delete(id);
      pending.reject(new WorkerError(code));
    }
  }

  function rejectAttemptExecutions(attemptKey, code) {
    rejectExecutions((pending) => pending.attemptKey === attemptKey, code);
  }

  function failToolAttempt(attemptKey, code) {
    toolAttempts.delete(attemptKey);
    toolFailures.add(attemptKey);
    rejectAttemptExecutions(attemptKey, code);
    const current = active.get(attemptKey);
    if (!current) return;
    try { void Promise.resolve(current.run.cancel()).catch(() => {}); } catch {}
  }

  async function consumeTools(run) {
    for await (const message of run.stream()) {
      if (message?.type !== 'tool_call') continue;
      const toolName = message?.args?.toolName;
      const synthetic = message.name === 'mcp' && message?.args?.providerIdentifier === CUSTOM_TOOL_SERVER &&
        (toolName === COMMAND_TOOL || toolName === FILE_CHANGE_TOOL);
      if (synthetic) continue;
      throw new WorkerError('unexpected_tool_event');
    }
  }

  async function pump(attemptKey, agent, run) {
    const tools = consumeTools(run);
    let result;
    try {
      [result] = await Promise.all([run.wait(), tools]);
      rejectAttemptExecutions(attemptKey, 'tool_attempt_closed');
      const status = toolFailures.has(attemptKey) ? 'unknown' :
        (['finished', 'error', 'cancelled'].includes(result?.status) ? result.status : 'unknown');
      const text = typeof result?.result === 'string' ? result.result : '';
      send({ type: 'event', attemptKey, event: 'terminal', status, text, usage: safeUsage(result?.usage) });
    } catch {
      toolAttempts.delete(attemptKey);
      toolFailures.delete(attemptKey);
      rejectAttemptExecutions(attemptKey, 'tool_attempt_closed');
      try { void Promise.resolve(run.cancel()).catch(() => {}); } catch {}
      send({ type: 'event', attemptKey, event: 'terminal', status: 'unknown' });
    } finally {
      toolAttempts.delete(attemptKey);
      toolFailures.delete(attemptKey);
      rejectAttemptExecutions(attemptKey, 'tool_attempt_closed');
      active.delete(attemptKey);
      try { agent.close(); } catch {}
    }
  }

  async function initialize(id, payload) {
    if (config || !payload || !boundedString(payload.apiKey, 4096) || !boundedString(payload.model, 200) ||
        !boundedString(payload.stateDir, 4096) || !Number.isInteger(payload.maxFrameBytes) ||
        payload.maxFrameBytes < 4096 || payload.maxFrameBytes > 8 * 1024 * 1024) {
      rejected(id);
      return;
    }
    fs.mkdirSync(payload.stateDir, { recursive: true, mode: 0o700 });
    config = { ...payload };
    store = new sdk.JsonlLocalAgentStore(path.join(config.stateDir, 'sdk-store'));
    response(id, { version: installedVersion });
  }

  async function dispatch(id, payload) {
    if (!config || !payload || !boundedString(payload.attemptKey, 4096) || !boundedString(payload.prompt, 64 * 1024) ||
        !boundedString(payload.policyContent, 64 * 1024) || !payload.policyContent.trim() ||
        !boundedString(payload.workspace, 4096) || !path.isAbsolute(payload.workspace) ||
        !['deny', 'explicit_once'].includes(payload.approvalMode) ||
        !(payload.resumeAgentId === '' || boundedString(payload.resumeAgentId, 512)) ||
        active.has(payload.attemptKey) || toolAttempts.has(payload.attemptKey)) {
      rejected(id);
      return;
    }
    toolAttempts.add(payload.attemptKey);
    toolFailures.delete(payload.attemptKey);
    let agent;
    try {
      const customTools = payload.approvalMode === 'explicit_once' ? createCustomTools(payload.attemptKey, executeTool) : {};
      const options = agentOptions(config, store, payload.workspace, customTools);
      if (payload.resumeAgentId) {
        await migrateLegacyAgentWorkspace(store, payload.resumeAgentId, config.stateDir, payload.workspace);
      }
      agent = payload.resumeAgentId ? await sdk.Agent.resume(payload.resumeAgentId, options) : await sdk.Agent.create(options);
      // Chat-alpha decision: retain Cursor's native system prompt. This account
      // cannot use the gated systemPrompt option. These are user-level guidance;
      // tools, inherited settings, and MCP servers remain the capability boundary.
      const prompt = `Chat guidance (user-level):\n${payload.policyContent}\n\nUser message:\n${payload.prompt}`;
      const run = await agent.send(prompt, { model: { id: config.model }, onStep: () => {}, onDelta: () => {} });
      if (!boundedString(agent?.agentId, 512) || !boundedString(run?.id, 512)) throw new WorkerError('native_identity_invalid');
      active.set(payload.attemptKey, { agent, run });
      response(id, { agentId: agent.agentId, runId: run.id });
      void pump(payload.attemptKey, agent, run);
    } catch (error) {
      toolAttempts.delete(payload.attemptKey);
      toolFailures.delete(payload.attemptKey);
      rejectAttemptExecutions(payload.attemptKey, 'tool_attempt_closed');
      active.delete(payload.attemptKey);
      try { agent?.close(); } catch {}
      throw error;
    }
  }

  async function steer(id, payload) {
    const current = active.get(payload?.attemptKey);
    if (!current || current.run.id !== payload.runId || !boundedString(payload.text, 64 * 1024) || typeof current.run.steer !== 'function') {
      rejected(id);
      return;
    }
    const status = await current.run.steer(payload.text);
    response(id, { status: typeof status === 'string' && status.length <= 64 ? status : 'unknown' });
  }

  async function cancel(id, payload) {
    const current = active.get(payload?.attemptKey);
    if (!current || current.run.id !== payload.runId || typeof current.run.cancel !== 'function') {
      rejected(id);
      return;
    }
    toolAttempts.delete(payload.attemptKey);
    rejectAttemptExecutions(payload.attemptKey, 'tool_attempt_cancelled');
    await current.run.cancel();
    response(id);
  }

  async function handle(frame) {
    if (frame?.type === 'response' && boundedString(frame.id, 128)) {
      const pending = pendingExecutions.get(frame.id);
      if (!pending) return;
      pendingExecutions.delete(frame.id);
      if (frame.ok) pending.resolve(frame.result);
      else pending.reject(new WorkerError(typeof frame.code === 'string' && /^[a-z0-9_]{1,64}$/.test(frame.code) ? frame.code : 'tool_execution_unknown'));
      return;
    }
    const id = frame?.id;
    if (frame?.type !== 'request' || !boundedString(id, 128) || !boundedString(frame.operation, 64)) return;
    try {
      if (frame.operation === 'init') await initialize(id, frame.payload);
      else if (frame.operation === 'dispatch') await dispatch(id, frame.payload);
      else if (frame.operation === 'steer') await steer(id, frame.payload);
      else if (frame.operation === 'cancel') await cancel(id, frame.payload);
      else rejected(id);
    } catch {
      rejected(id, 'unknown');
    }
  }

  function close() {
    toolAttempts.clear();
    toolFailures.clear();
    rejectExecutions(() => true, 'tool_worker_closed');
  }

  return { handle, close };
}

async function main() {
  const sdk = await import('@cursor/sdk');
  const runtime = createRuntime(sdk, (value) => process.stdout.write(value), installedSDKVersion());
  const lines = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
  try {
    for await (const line of lines) {
      if (Buffer.byteLength(line, 'utf8') > DEFAULT_MAX_FRAME_BYTES) process.exit(2);
      let frame;
      try { frame = JSON.parse(line); } catch { process.exit(2); }
      void runtime.handle(frame);
    }
  } finally {
    runtime.close();
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) await main();
