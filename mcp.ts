// mcp.ts — Model Context Protocol endpoint at POST /mcp (JSON-RPC 2.0, Streamable HTTP).
// Modern era first: server/discover (2026-07-28) with supportedVersions, serverInfo and a
// tools capability. Legacy initialize + notifications/initialized also answered. Every tool
// acts on the same in-memory store as REST, so objects created here read back over REST
// with the same ids. No auth -> HTTP 401. Bad JSON -> HTTP 400, JSON-RPC -32700.
// Unknown tool -> JSON-RPC -32601. Missing required argument -> JSON-RPC -32602.
import type { IncomingMessage, ServerResponse } from 'node:http';
import { ApiError, type Store } from './store.ts';
import { readBody, sendJson } from './rest.ts';

export interface McpContext {
  store: Store;
  token: string;
  reqId: () => string;
}

const SERVER_NAME = 'github-forge';
const SERVER_VERSION = '1.0.0';
const SUPPORTED_VERSIONS = ['2026-07-28', '2025-11-25', '2025-06-18'];

type Json = Record<string, unknown>;

interface ToolDef {
  name: string;
  description: string;
  inputSchema: Json;
}

const TOOLS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository owned by the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'Repository name' },
        description: { type: 'string', description: 'Repository description' },
        private: { type: 'boolean', description: 'Whether the repository is private' },
        owner: { type: 'string', description: 'Owner login' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Owner login' },
        repo: { type: 'string', description: 'Repository name' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Owner login' },
        repo: { type: 'string', description: 'Repository name' },
        title: { type: 'string', description: 'Issue title' },
        body: { type: 'string', description: 'Issue body' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List issues in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Owner login' },
        repo: { type: 'string', description: 'Repository name' },
        state: { type: 'string', description: 'Issue state: open, closed, or all' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Open a pull request from a head branch into a base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Owner login' },
        repo: { type: 'string', description: 'Repository name' },
        title: { type: 'string', description: 'Pull request title' },
        body: { type: 'string', description: 'Pull request body' },
        head: { type: 'string', description: 'Head branch name' },
        base: { type: 'string', description: 'Base branch name' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request by number.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Owner login' },
        repo: { type: 'string', description: 'Repository name' },
        pull_number: { type: 'integer', description: 'Pull request number' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

function rpcResult(id: unknown, result: unknown): Json {
  return { jsonrpc: '2.0', id: id ?? null, result };
}

function rpcError(id: unknown, code: number, message: string): Json {
  return { jsonrpc: '2.0', id: id ?? null, error: { code, message } };
}

function toolOk(result: unknown): Json {
  const text = JSON.stringify(result);
  return { content: [{ type: 'text', text }], structuredContent: result, isError: false };
}

function toolErr(message: string): Json {
  return { content: [{ type: 'text', text: message }], isError: true };
}

function str(v: unknown): string {
  return typeof v === 'string' ? v : '';
}

function asNumber(v: unknown): number | null {
  if (typeof v === 'number' && Number.isInteger(v)) return v;
  if (typeof v === 'string' && /^\d+$/.test(v)) return Number(v);
  return null;
}

/** Split "owner/repo" into parts, or null. */
function splitFullName(v: unknown): { owner: string; repo: string } | null {
  if (typeof v !== 'string') return null;
  const idx = v.indexOf('/');
  if (idx <= 0 || idx === v.length - 1) return null;
  return { owner: v.slice(0, idx), repo: v.slice(idx + 1) };
}

export async function handleMcp(req: IncomingMessage, res: ServerResponse, ctx: McpContext): Promise<void> {
  const reqId = ctx.reqId();
  if (req.method !== 'POST') {
    sendJson(res, 405, { message: 'Method Not Allowed' }, reqId);
    return;
  }
  // Raw body first: a body that is not JSON is HTTP 400 with JSON-RPC -32700.
  let text = '';
  try {
    const chunks: Buffer[] = [];
    for await (const chunk of req) chunks.push(chunk as Buffer);
    text = Buffer.concat(chunks).toString('utf8');
  } catch {
    sendJson(res, 400, rpcError(null, -32700, 'Parse error'), reqId);
    return;
  }
  let parsed: unknown = null;
  try {
    parsed = JSON.parse(text);
  } catch {
    sendJson(res, 400, rpcError(null, -32700, 'Parse error'), reqId);
    return;
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
    sendJson(res, 400, rpcError(null, -32700, 'Parse error'), reqId);
    return;
  }
  const msg = parsed as Json;
  const id: unknown = 'id' in msg ? msg.id : null;
  const method = typeof msg.method === 'string' ? msg.method : '';

  // Notifications carry no id and get an empty 202; they are answered before auth
  // because they are not requests and expect no response body.
  if (method.startsWith('notifications/')) {
    res.writeHead(202, { 'x-github-request-id': reqId });
    res.end();
    return;
  }

  // Auth (after the parse check, so a malformed body is always -32700).
  if (ctx.token !== '') {
    const auth = req.headers.authorization ?? '';
    if (auth !== `Bearer ${ctx.token}`) {
      sendJson(res, 401, { message: 'Unauthorized' }, reqId);
      return;
    }
  }

  const params: Json =
    typeof msg.params === 'object' && msg.params !== null && !Array.isArray(msg.params) ? (msg.params as Json) : {};

  try {
    if (method === 'server/discover') {
      const serverInfo = { name: SERVER_NAME, version: SERVER_VERSION };
      sendJson(
        res,
        200,
        rpcResult(id, {
          supportedVersions: SUPPORTED_VERSIONS,
          capabilities: { tools: {} },
          serverInfo,
          _meta: { 'io.modelcontextprotocol/serverInfo': serverInfo },
        }),
        reqId,
      );
      return;
    }
    if (method === 'initialize') {
      const offered = typeof params.protocolVersion === 'string' ? params.protocolVersion : '2025-11-25';
      const negotiated = SUPPORTED_VERSIONS.includes(offered) ? offered : '2025-11-25';
      sendJson(
        res,
        200,
        rpcResult(id, {
          protocolVersion: negotiated,
          capabilities: { tools: {} },
          serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
        }),
        reqId,
      );
      return;
    }
    if (method === 'ping') {
      sendJson(res, 200, rpcResult(id, {}), reqId);
      return;
    }
    if (method === 'tools/list') {
      // The same constant, serialized the same way every time: byte-identical lists.
      sendJson(res, 200, rpcResult(id, { tools: TOOLS }), reqId);
      return;
    }
    if (method === 'tools/call') {
      const result = await callTool(params, ctx);
      if (result.rpcError !== null) {
        sendJson(res, 200, rpcError(id, result.rpcError.code, result.rpcError.message), reqId);
        return;
      }
      sendJson(res, 200, rpcResult(id, result.toolResult), reqId);
      return;
    }
    sendJson(res, 200, rpcError(id, -32601, `Method not found: ${method}`), reqId);
  } catch (err) {
    if (err instanceof ApiError) {
      sendJson(res, 200, rpcError(id, -32602, err.message), reqId);
      return;
    }
    sendJson(res, 200, rpcError(id, -32603, 'Internal error'), reqId);
  }
}

function findTool(name: string): ToolDef | null {
  const norm = (s: string): string => s.toLowerCase().replace(/[^a-z0-9]/g, '');
  const wanted = norm(name);
  for (const tool of TOOLS) {
    if (norm(tool.name) === wanted) return tool;
    if (wanted.length > norm(tool.name).length && wanted.endsWith(norm(tool.name))) return tool;
  }
  return null;
}

/** Resolve owner/repo from tool arguments, accepting full_name as a fallback. */
function resolveRepoArgs(args: Json): { owner: string; repo: string } | null {
  let owner = str(args.owner);
  let repo = str(args.repo);
  if (owner === '' || repo === '') {
    const full = splitFullName(args.full_name ?? args.fullName);
    if (full !== null) {
      if (owner === '') owner = full.owner;
      if (repo === '') repo = full.repo;
    }
  }
  if (owner === '' || repo === '') return null;
  return { owner, repo };
}

async function callTool(
  params: Json,
  ctx: McpContext,
): Promise<{ rpcError: { code: number; message: string } | null; toolResult: Json | null }> {
  const store = ctx.store;
  const name = str(params.name);
  const args: Json =
    typeof params.arguments === 'object' && params.arguments !== null && !Array.isArray(params.arguments)
      ? (params.arguments as Json)
      : {};
  const tool = findTool(name);
  if (tool === null) return { rpcError: { code: -32601, message: `Unknown tool: ${name}` }, toolResult: null };

  // Required-argument check first: missing -> JSON-RPC -32602 (or isError, per the rules).
  const required = (tool.inputSchema.required as string[] | undefined) ?? [];
  const missing = required.filter((key) => args[key] === undefined || args[key] === null || args[key] === '');
  if (missing.length > 0) {
    return { rpcError: { code: -32602, message: `Missing required argument(s): ${missing.join(', ')}` }, toolResult: null };
  }

  try {
    switch (tool.name) {
      case 'create_repo': {
        const repoName = str(args.name);
        // The owner the caller names is the token's login; learn it when unknown.
        const ownerArg = str(args.owner);
        if (ownerArg !== '' && store.viewerLogin === null) store.viewerLogin = ownerArg;
        const repo = store.createRepo(repoName, typeof args.private === 'boolean' ? args.private : false, str(args.description));
        if (ownerArg !== '' && repo.owner === null) repo.owner = ownerArg;
        return { rpcError: null, toolResult: toolOk(store.repoJson(repo)) };
      }
      case 'get_repo': {
        const resolved = resolveRepoArgs(args);
        if (resolved === null) return { rpcError: { code: -32602, message: 'Missing required argument(s): owner, repo' }, toolResult: null };
        const repo = store.findRepo(resolved.owner, resolved.repo);
        if (repo === null) return { rpcError: null, toolResult: toolErr(`Repository ${resolved.owner}/${resolved.repo} not found`) };
        return { rpcError: null, toolResult: toolOk(store.repoJson(repo)) };
      }
      case 'create_issue': {
        const resolved = resolveRepoArgs(args);
        if (resolved === null) return { rpcError: { code: -32602, message: 'Missing required argument(s): owner, repo' }, toolResult: null };
        const repo = store.findRepo(resolved.owner, resolved.repo);
        if (repo === null) return { rpcError: null, toolResult: toolErr(`Repository ${resolved.owner}/${resolved.repo} not found`) };
        const title = str(args.title);
        if (title === '') return { rpcError: { code: -32602, message: 'Missing required argument(s): title' }, toolResult: null };
        const labels: string[] = Array.isArray(args.labels)
          ? args.labels.map((l) => (typeof l === 'string' ? l : typeof l === 'object' && l !== null ? str((l as Json).name) : '')).filter((s) => s !== '')
          : [];
        const issue = store.createIssue(repo, title, str(args.body), labels);
        return { rpcError: null, toolResult: toolOk(store.issueJson(repo, issue)) };
      }
      case 'list_issues': {
        const resolved = resolveRepoArgs(args);
        if (resolved === null) return { rpcError: { code: -32602, message: 'Missing required argument(s): owner, repo' }, toolResult: null };
        const repo = store.findRepo(resolved.owner, resolved.repo);
        if (repo === null) return { rpcError: null, toolResult: toolErr(`Repository ${resolved.owner}/${resolved.repo} not found`) };
        const state = typeof args.state === 'string' && args.state !== '' ? args.state : 'open';
        const list = store.listIssues(repo, state, null, null);
        return { rpcError: null, toolResult: toolOk(list.map((i) => store.issueJson(repo, i))) };
      }
      case 'create_pull_request': {
        const resolved = resolveRepoArgs(args);
        if (resolved === null) return { rpcError: { code: -32602, message: 'Missing required argument(s): owner, repo' }, toolResult: null };
        const repo = store.findRepo(resolved.owner, resolved.repo);
        if (repo === null) return { rpcError: null, toolResult: toolErr(`Repository ${resolved.owner}/${resolved.repo} not found`) };
        const pull = store.createPull(repo, str(args.title), str(args.body), str(args.head), str(args.base));
        return { rpcError: null, toolResult: toolOk(store.pullJson(repo, pull)) };
      }
      case 'merge_pull_request': {
        const resolved = resolveRepoArgs(args);
        if (resolved === null) return { rpcError: { code: -32602, message: 'Missing required argument(s): owner, repo' }, toolResult: null };
        const repo = store.findRepo(resolved.owner, resolved.repo);
        if (repo === null) return { rpcError: null, toolResult: toolErr(`Repository ${resolved.owner}/${resolved.repo} not found`) };
        const number = asNumber(args.pull_number ?? args.number);
        if (number === null) return { rpcError: { code: -32602, message: 'Missing required argument(s): pull_number' }, toolResult: null };
        const result = store.mergePull(repo, number);
        return { rpcError: null, toolResult: toolOk(result) };
      }
      default:
        return { rpcError: { code: -32601, message: `Unknown tool: ${name}` }, toolResult: null };
    }
  } catch (err) {
    if (err instanceof ApiError) return { rpcError: null, toolResult: toolErr(err.message) };
    throw err;
  }
}
