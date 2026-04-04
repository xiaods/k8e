/**
 * K8E Sandbox TypeScript SDK
 * Direct gRPC client — no MCP stdio overhead (~1-5ms vs ~500ms).
 *
 * Install:
 *   npm install @grpc/grpc-js @grpc/proto-loader
 *
 * Usage:
 *   import { SandboxClient } from "./sandbox_client";
 *   const client = new SandboxClient();
 *   const result = await client.run("print('hello')", "python");
 *   await client.close();
 */

import * as grpc from "@grpc/grpc-js";
import * as protoLoader from "@grpc/proto-loader";
import * as fs from "fs";
import * as path from "path";

const DEFAULT_ENDPOINT = "127.0.0.1:50051";
const PROTO_PATH = path.resolve(__dirname, "../../proto/sandbox/v1/sandbox.proto");

// ── types ──────────────────────────────────────────────────────────────────

export interface ExecResult {
  stdout: string;
  stderr: string;
  exitCode: number;
}

export interface FileEntry {
  path: string;
  modified: number;
}

export interface SessionOptions {
  runtimeClass?: string;
  allowedHosts?: string[];
  tenantId?: string;
}

// ── channel helpers ────────────────────────────────────────────────────────

function buildCredentials(): grpc.ChannelCredentials {
  const certPath = process.env.K8E_SANDBOX_CERT;
  if (certPath) {
    return grpc.credentials.createSsl(fs.readFileSync(certPath));
  }
  for (const p of [
    "/var/lib/k8e/server/tls/serving-kube-apiserver.crt",
    "/etc/k8e/tls/serving-kube-apiserver.crt",
  ]) {
    if (fs.existsSync(p)) {
      return grpc.credentials.createSsl(fs.readFileSync(p));
    }
  }
  return grpc.credentials.createSsl(); // system CA pool
}

function loadStub(endpoint: string): any {
  const pkg = protoLoader.loadSync(PROTO_PATH, {
    keepCase: false,
    longs: Number,
    defaults: true,
  }) as any;
  const proto = grpc.loadPackageDefinition(pkg) as any;
  return new proto.sandbox.v1.SandboxService(endpoint, buildCredentials());
}

// ── client ─────────────────────────────────────────────────────────────────

export class SandboxClient {
  private stub: any;
  private sessionId: string | null = null;
  private tenantId: string;

  constructor(endpoint?: string, tenantId = "") {
    const ep = endpoint ?? process.env.K8E_SANDBOX_ENDPOINT ?? DEFAULT_ENDPOINT;
    this.stub = loadStub(ep);
    this.tenantId = tenantId;
  }

  // ── lifecycle ────────────────────────────────────────────────────────────

  async close(): Promise<void> {
    const sid = this.sessionId;
    this.sessionId = null;
    if (sid && !this.tenantId) {
      await this.destroySession(sid).catch(() => {});
    }
    this.stub.close();
  }

  // ── high-level API ───────────────────────────────────────────────────────

  /** Run code in the default session (lazily created, reused across calls). */
  async run(code: string, language = "bash", timeout = 30): Promise<ExecResult> {
    const sid = await this.defaultSession();
    return this.exec(sid, buildCommand(code, language), timeout);
  }

  /** Run a command in an explicit session. */
  exec(sessionId: string, command: string, timeout = 30): Promise<ExecResult> {
    return call(this.stub, "exec", {
      session_id: sessionId,
      command,
      timeout,
      workdir: "/workspace",
    }).then((r: any) => ({ stdout: r.stdout, stderr: r.stderr, exitCode: r.exit_code }));
  }

  /** Run a command and receive output chunks via async iterator. */
  execStream(sessionId: string, command: string, timeout = 300): AsyncIterable<string> {
    const stream = this.stub.execStream({
      session_id: sessionId,
      command,
      timeout,
      workdir: "/workspace",
    });
    return (async function* () {
      for await (const chunk of stream) {
        yield (chunk as any).chunk as string;
      }
    })();
  }

  // ── session management ───────────────────────────────────────────────────

  async createSession(opts: SessionOptions = {}): Promise<string> {
    const r: any = await call(this.stub, "createSession", {
      runtime_class: opts.runtimeClass ?? "gvisor",
      allowed_hosts: opts.allowedHosts ?? [],
      tenant_id: opts.tenantId ?? "",
    });
    return r.session_id as string;
  }

  destroySession(sessionId: string): Promise<void> {
    return call(this.stub, "destroySession", { session_id: sessionId }).then(() => {});
  }

  // ── file operations ──────────────────────────────────────────────────────

  writeFile(sessionId: string, filePath: string, content: string): Promise<void> {
    return call(this.stub, "writeFile", {
      session_id: sessionId,
      path: filePath,
      content,
    }).then(() => {});
  }

  readFile(sessionId: string, filePath: string): Promise<string> {
    return call(this.stub, "readFile", {
      session_id: sessionId,
      path: filePath,
    }).then((r: any) => r.content as string);
  }

  listFiles(sessionId: string, since = 0): Promise<FileEntry[]> {
    return call(this.stub, "listFiles", {
      session_id: sessionId,
      since,
    }).then((r: any) =>
      (r.files as any[]).map((f) => ({ path: f.path, modified: f.modified }))
    );
  }

  // ── extras ───────────────────────────────────────────────────────────────

  async pipInstall(sessionId: string, packages: string[]): Promise<ExecResult> {
    const r: any = await call(this.stub, "pipInstall", {
      session_id: sessionId,
      packages,
    });
    return { stdout: r.output, stderr: "", exitCode: r.exit_code };
  }

  // ── internal ─────────────────────────────────────────────────────────────

  private async defaultSession(): Promise<string> {
    if (this.sessionId) return this.sessionId;
    this.sessionId = await this.createSession({ tenantId: this.tenantId });
    return this.sessionId;
  }
}

// ── helpers ────────────────────────────────────────────────────────────────

function buildCommand(code: string, language: string): string {
  const lang = language.toLowerCase();
  if (lang === "python" || lang === "python3") return `python3 -c ${JSON.stringify(code)}`;
  if (lang === "node" || lang === "nodejs") return `node -e ${JSON.stringify(code)}`;
  return code;
}

function call(stub: any, method: string, req: object): Promise<unknown> {
  return new Promise((resolve, reject) => {
    stub[method](req, (err: grpc.ServiceError | null, res: unknown) => {
      if (err) reject(err);
      else resolve(res);
    });
  });
}

// ── convenience factory ────────────────────────────────────────────────────

/**
 * Run a one-shot command and return the result. Opens and closes a client automatically.
 *
 * @example
 * const { stdout } = await sandboxRun("echo hello");
 */
export async function sandboxRun(
  code: string,
  language = "bash",
  endpoint?: string
): Promise<ExecResult> {
  const client = new SandboxClient(endpoint);
  try {
    return await client.run(code, language);
  } finally {
    await client.close();
  }
}
