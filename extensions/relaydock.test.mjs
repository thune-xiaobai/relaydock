// Real installed pi SDK, isolated credentials and a loopback model. No chat UI.
import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import os from "node:os";
import http from "node:http";
import { pathToFileURL, fileURLToPath } from "node:url";

function entry(anchor, name) {
  let dir = fs.statSync(anchor).isDirectory() ? anchor : path.dirname(anchor);
  for (;;) {
    for (const p of [dir, path.join(dir, "node_modules", name), path.join(dir, "lib", "node_modules", name), path.join(dir, "libexec", "lib", "node_modules", name)]) {
      try {
        const pkg = JSON.parse(fs.readFileSync(path.join(p, "package.json"), "utf8"));
        if (pkg.name === name) return path.join(p, pkg.main || "dist/index.js");
      } catch (e) { if (e.code !== "ENOENT" && e.code !== "ENOTDIR") throw e; }
    }
    if (path.dirname(dir) === dir) throw new Error(`Cannot find ${name}`);
    dir = path.dirname(dir);
  }
}
const sdkEntry = entry(fs.realpathSync(process.env.RELAYDOCK_PI_ANCHOR), "@earendil-works/pi-coding-agent");
const sdk = await import(pathToFileURL(sdkEntry));
const ai = await import(pathToFileURL(entry(sdkEntry, "@earendil-works/pi-ai")));
const extension = path.join(path.dirname(fileURLToPath(import.meta.url)), "relaydock.ts");
const sleep = (ms) => new Promise(resolve => setTimeout(resolve, ms));
async function until(fn, timeout = 7000) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) { const result = fn(); if (result) return result; await sleep(20); }
  assert.fail("condition did not become true before deadline");
}

async function fixture(t, extra) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "relaydock-bridge-test-"));
  const cwd = path.join(root, "project"), agentDir = path.join(root, "config"), bridge = path.join(root, "bridge");
  for (const dir of [cwd, agentDir, bridge]) fs.mkdirSync(dir, { recursive: true });
  process.env.RELAYDOCK_SESSION = "s_test"; process.env.RELAYDOCK_BRIDGE = bridge;
  const requests = [];
  const server = http.createServer((req, res) => {
    req.resume(); res.writeHead(200, { "Content-Type": "text/event-stream" }); res.flushHeaders();
    requests.push(res);
  });
  await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
  const runtime = await sdk.ModelRuntime.create({ credentials: new ai.InMemoryCredentialStore(), modelsPath: null,
    modelsStorePath: path.join(root, "models"), allowModelNetwork: false, refreshOnCreate: false });
  runtime.registerProvider("rd-test", { baseUrl: `http://127.0.0.1:${server.address().port}/v1`, api: "openai-completions",
    models: [{ id: "fixture", name: "fixture", reasoning: false, input: ["text"], contextWindow: 64000, maxTokens: 1024,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } }] });
  const loader = new sdk.DefaultResourceLoader({ cwd, agentDir, noExtensions: true, additionalExtensionPaths: [extension],
    extensionFactories: extra ? [extra] : [], noSkills: true, noPromptTemplates: true, noThemes: true, noContextFiles: true });
  await loader.reload();
  const { session } = await sdk.createAgentSession({ cwd, agentDir, modelRuntime: runtime, model: runtime.getModel("rd-test", "fixture"),
    resourceLoader: loader, sessionManager: sdk.SessionManager.inMemory(cwd), tools: [],
    settingsManager: sdk.SettingsManager.inMemory({ retry: { enabled: false }, compaction: { enabled: false } }) });
  const errors = [];
  t.after(async () => {
    await session.abort();
    await session.extensionRunner.emit({ type: "session_shutdown" }); session.dispose();
    server.closeAllConnections(); await new Promise(resolve => server.close(resolve));
    delete process.env.RELAYDOCK_SESSION; delete process.env.RELAYDOCK_BRIDGE;
    fs.rmSync(root, { recursive: true, force: true });
  });
  await session.bindExtensions({ onError: e => errors.push(e) });
  const read = name => { try { return JSON.parse(fs.readFileSync(path.join(bridge, name), "utf8")); } catch (e) { if (e.code === "ENOENT") return; throw e; } };
  const state = () => read("state.json");
  const receipt = id => read(`receipts/${id}.json`);
  function send(id, text, target = state()) {
    const r = { call_id: id, session_id: "s_test", instance: target.instance, native_session: target.native_session,
      action: "submit", run_id: `run_${id}`, text };
    const file = path.join(bridge, "requests", `${id}.json`);
    fs.writeFileSync(file + ".tmp", JSON.stringify(r)); fs.renameSync(file + ".tmp", file);
  }
  function finish(index) {
    const res = requests[index];
    for (const [delta, finish_reason] of [[{ role: "assistant", content: "done" }, null], [{}, "stop"]]) {
      res.write(`data: ${JSON.stringify({ id: "fixture", object: "chat.completion.chunk", model: "fixture", created: 1,
        choices: [{ index: 0, delta, finish_reason }] })}\n\n`);
    }
    res.end("data: [DONE]\n\n");
  }
  async function replace() {
    // The TUI owns replacement in this SDK. Exercise its extension lifecycle
    // here; TestRealPiRuntime additionally drives the actual /new command.
    await session.extensionRunner.emit({ type: "session_shutdown", reason: "new" });
    session.sessionManager.newSession();
    await session.extensionRunner.emit({ type: "session_start", reason: "new" });
  }
  return { state, receipt, send, session, runtime, requests, finish, errors, replace };
}

test("missing credentials rejects without poisoning the next run; long runs acknowledge on start, not completion", async t => {
  const f = await fixture(t);
  f.send("no_auth", "check project");
  await until(() => f.receipt("no_auth")?.error?.code === "rejected");
  assert.match(f.receipt("no_auth").error.message, /No API key/);
  assert.equal(f.state().status, "idle"); assert.equal(f.state().run_id, "");
  assert.equal(f.session.isStreaming, false); assert.equal(f.requests.length, 0);
  await f.runtime.setRuntimeApiKey("rd-test", "fixture");
  f.send("long", "long request");
  await until(() => f.receipt("long")?.data?.accepted);
  await until(() => f.requests.length === 1);
  assert.equal(f.session.isStreaming, true); assert.equal(f.state().status, "running");
  // Replaying an occurrence must recover the receipt without executing it.
  f.send("long", "long request"); await sleep(400);
  assert.equal(f.requests.length, 1);
  f.finish(0); await until(() => f.state().status === "idle");
  f.send("next", "next request");
  await until(() => f.receipt("next")?.data?.accepted);
  await until(() => f.requests.length === 2);
  f.finish(1); await until(() => f.state().status === "idle");
});

test("intercepted input becomes explicitly unknown, never accepted by an unrelated run", async t => {
  const f = await fixture(t, pi => pi.on("input", e => e.text === "INTERCEPT" ? { action: "handled" } : { action: "continue" }));
  await f.runtime.setRuntimeApiKey("rd-test", "fixture");
  const old = f.state();
  f.send("intercept", "INTERCEPT");
  await until(() => f.state().status === "unknown");
  assert.equal(f.receipt("intercept").error.code, "unknown"); assert.equal(f.requests.length, 0);
  f.send("blocked", "must not replay");
  await until(() => f.receipt("blocked")?.error?.code === "busy");
  const local = f.session.prompt("UNRELATED");
  await until(() => f.requests.length === 1);
  assert.match(f.state().run_id, /^local_/);
  assert.equal(f.receipt("intercept").error.code, "unknown");
  f.finish(0); await local;
  await until(() => f.state().status === "unknown");
  await f.replace();
  await until(() => f.state().instance !== old.instance);
  f.send("stale", "stale target", old);
  await until(() => f.receipt("stale")?.error?.code === "stale_target");
  f.send("fresh", "fresh request");
  await until(() => f.receipt("fresh")?.data?.accepted);
  await until(() => f.requests.length === 2);
  assert.equal(f.receipt("intercept").error.code, "unknown");
  f.finish(1); await until(() => f.state().status === "idle");
});

test("asynchronous SDK rejection after preflight never produces an acceptance receipt", async t => {
  const f = await fixture(t, pi => pi.on("session_start", (_e, ctx) => {
    // Simulate credentials disappearing after the bridge's preflight. SDK
    // validation still checks ModelRuntime, independently of this facade.
    ctx.modelRegistry.hasConfiguredAuth = () => true;
  }));
  f.send("race", "check project");
  await until(() => f.errors.some(e => e.event === "send_user_message"));
  await until(() => f.state().status === "unknown");
  assert.equal(f.receipt("race").error.code, "unknown");
  assert.equal(f.session.isStreaming, false); assert.equal(f.requests.length, 0);
});

test("late preflight resolution cannot inject into a replacement session", async t => {
  let resolveAuth, registry;
  const f = await fixture(t, pi => pi.on("session_start", (_e, ctx) => { registry = ctx.modelRegistry; }));
  registry.hasConfiguredAuth = () => false;
  registry.getProviderAuth = () => new Promise(resolve => { resolveAuth = resolve; });
  const old = f.state(); f.send("delayed", "must not move to another session");
  await until(() => resolveAuth);
  await f.replace();
  await until(() => f.state().instance !== old.instance);
  resolveAuth({ auth: { apiKey: "fixture" } }); await sleep(400);
  assert.equal(f.state().status, "idle"); assert.equal(f.state().run_id, "");
  assert.equal(f.requests.length, 0); assert.equal(f.receipt("delayed").error.code, "unknown");
});
