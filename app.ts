// app.ts — Node shim for the Go forge binary. main.ts owns the lifecycle;
// this file only spawns ./forge, waits for its first stdout line "PORT <n>",
// and exposes the four members main.ts uses: once('error'), listen(0, cb),
// address(), close(). The Go binary owns the whole HTTP surface.
import { EventEmitter } from 'node:events';
import { spawn, type ChildProcess } from 'node:child_process';
import { fileURLToPath } from 'node:url';

export class ForgeServer extends EventEmitter {
  private child: ChildProcess | null = null;
  private port: number | null = null;
  private portSeen = false;
  private stdoutBuf = '';

  listen(port: number, cb?: () => void): this {
    if (this.child !== null) throw new Error('forge server is already listening');
    const forgePath = fileURLToPath(new URL('./forge', import.meta.url));
    const child = spawn(forgePath, [], { stdio: ['ignore', 'pipe', 'inherit'] });
    this.child = child;

    const fail = (err: Error): void => {
      if (this.portSeen) return;
      this.emit('error', err);
    };
    child.once('error', (err) => fail(err instanceof Error ? err : new Error(String(err))));
    child.once('exit', (code, signal) => {
      if (!this.portSeen) {
        fail(new Error(`forge exited before reporting a port (code=${code} signal=${signal})`));
      }
    });
    child.stdout?.on('data', (chunk: Buffer) => {
      if (this.portSeen) return; // drain only; the port line is already known
      this.stdoutBuf += chunk.toString('utf8');
      const nl = this.stdoutBuf.indexOf('\n');
      if (nl < 0) return;
      const line = this.stdoutBuf.slice(0, nl).trim();
      const m = /^PORT (\d+)$/.exec(line);
      if (!m) {
        fail(new Error(`forge printed an unexpected first line: ${JSON.stringify(line)}`));
        return;
      }
      this.port = Number(m[1]);
      this.portSeen = true;
      if (cb) cb();
    });
    return this;
  }

  address(): { port: number } | null {
    return this.port === null ? null : { port: this.port };
  }

  close(): this {
    const child = this.child;
    this.child = null;
    if (child && child.exitCode === null && child.signalCode === null) {
      try {
        child.kill('SIGTERM');
      } catch {
        // Already gone; nothing left to do.
      }
      const t = setTimeout(() => {
        try {
          child.kill('SIGKILL');
        } catch {
          // Already gone.
        }
      }, 500);
      t.unref();
    }
    return this;
  }
}

/** Build the server. It binds nothing until the caller listens; the entry listens on an ephemeral port. */
export function createApp(): ForgeServer {
  return new ForgeServer();
}
