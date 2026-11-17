// app.ts — the forge's HTTP server: /healthz (no auth), /api/v3/* (REST), /mcp (MCP),
// /api/graphql (GraphQL), and /{owner}/{repo}.git/* (git smart HTTP, v0 + v2).
// main.ts owns the lifecycle; this file owns the surface.
import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { Store } from './store.ts';
import { handleRest, sendJson } from './rest.ts';
import { handleMcp } from './mcp.ts';
import { handleGitHttp } from './git.ts';
import { handleGraphQLHttp } from './graphql.ts';

const TRIAL_NOW = process.env.MAKEFUN_TRIAL_NOW ?? '2026-01-01T00:00:00Z';
const TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? '';

const JSON_TYPE = 'application/json; charset=utf-8';

/** The path of a request URL, or '/' when it does not parse (never a throw from a request). */
function pathOf(req: IncomingMessage): string {
  try {
    return new URL(req.url ?? '/', 'http://entry').pathname;
  } catch {
    return '/';
  }
}

function queryOf(req: IncomingMessage): URLSearchParams {
  try {
    return new URL(req.url ?? '/', 'http://entry').searchParams;
  } catch {
    return new URLSearchParams();
  }
}

/** Build the server. It binds nothing until the caller listens; the entry listens on an ephemeral port. */
export function createApp(): Server {
  const store = new Store(TRIAL_NOW);
  let reqCounter = 0;
  const reqId = (): string => {
    reqCounter += 1;
    return `req-${reqCounter}`;
  };
  return createServer((req: IncomingMessage, res: ServerResponse) => {
    void (async () => {
      // One id per request, minted once and reused by every reply (including the 500 path).
      const id = reqId();
      const idOf = (): string => id;
      try {
        const rawPath = pathOf(req);
        const method = req.method ?? 'GET';
        // Health probe: no auth.
        if (method === 'GET' && rawPath === '/healthz') {
          sendJson(res, 200, { ok: true }, id);
          return;
        }
        // MCP endpoint.
        if (rawPath === '/mcp') {
          await handleMcp(req, res, { store, token: TOKEN, reqId: idOf });
          return;
        }
        // REST v3 surface.
        if (rawPath === '/api/v3' || rawPath.startsWith('/api/v3/')) {
          const sub = rawPath === '/api/v3' ? '' : rawPath.slice('/api/v3'.length);
          const path = sub.length > 1 && sub.endsWith('/') ? sub.slice(0, -1) : sub;
          await handleRest(req, res, path, queryOf(req), { store, token: TOKEN, reqId: idOf });
          return;
        }
        // GraphQL endpoint.
        if (rawPath === '/api/graphql' || rawPath === '/api/v3/graphql') {
          await handleGraphQLHttp(req, res, { store, token: TOKEN, reqId: idOf });
          return;
        }
        // Git smart HTTP: /{owner}/{repo}.git/info/refs, git-upload-pack, git-receive-pack.
        if (await handleGitHttp(req, res, rawPath, queryOf(req), { store, token: TOKEN, reqId: idOf })) {
          return;
        }
        // Everything else, every method: GitHub's 404 body.
        sendJson(res, 404, { message: 'Not Found' }, id);
      } catch (err) {
        try {
          const text = JSON.stringify({ message: 'Internal Server Error' });
          res.writeHead(500, { 'content-type': JSON_TYPE, 'content-length': Buffer.byteLength(text), 'x-github-request-id': id });
          res.end(text);
        } catch {
          // The socket is already gone; nothing left to do.
        }
        process.stderr.write(`[app] unhandled: ${err instanceof Error ? err.message : String(err)}\n`);
      }
    })();
  });
}
