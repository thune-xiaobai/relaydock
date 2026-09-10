/** RelayDock's local pi adapter. Load explicitly with pi -e /path/relaydock.ts. */
import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import * as fs from "node:fs";
import * as path from "node:path";
import { randomUUID } from "node:crypto";

const MAX_TEXT = 64 * 1024;
const MAX_LOG = 64 * 1024 * 1024;
const id = (prefix: string) => prefix + randomUUID().replaceAll("-", "");
const valid = (s: unknown): s is string => typeof s === "string" && /^[a-zA-Z0-9_-]{1,100}$/.test(s);
const clip = (s: string) => Buffer.from(s).subarray(0, MAX_TEXT).toString("utf8");

function write(file: string, value: unknown) {
	const tmp = file + "." + randomUUID() + ".tmp";
	const fd = fs.openSync(tmp, "wx", 0o600);
	try { fs.writeFileSync(fd, JSON.stringify(value)); fs.fsyncSync(fd); } finally { fs.closeSync(fd); }
	try { fs.renameSync(tmp, file); } finally { if (fs.existsSync(tmp)) fs.unlinkSync(tmp); }
}

export default function (pi: ExtensionAPI) {
	const directory = process.env.RELAYDOCK_BRIDGE;
	const session = process.env.RELAYDOCK_SESSION;
	if (!directory || !valid(session)) return;
	for (const d of [directory, path.join(directory, "requests"), path.join(directory, "receipts")]) fs.mkdirSync(d, { recursive: true, mode: 0o700 });
	const log = path.join(directory, "events.ndjson");
	let ctx: ExtensionContext;
	let instance = id("i_");
	let native = "";
	let seq = 0;
	let run = "";
	let status = "idle";
	let source = "local";
	let remotePending = false;
	let lastStop = "";
	let lastError = "";
	let timer: ReturnType<typeof setInterval> | undefined;
	let broken = false;

	const state = () => write(path.join(directory, "state.json"), {
		session_id: session, instance, native_session: native, status: broken ? "unknown" : status,
		run_id: run, updated_at: new Date().toISOString(), pid: process.pid,
	});
	function emit(kind: string, text = "", eventStatus = status) {
		const ev = { id: id("e_"), session_id: session, instance, native_session: native, run_id: run,
			seq: ++seq, kind, source, text: clip(text), status: eventStatus, time: new Date().toISOString() };
		const fd = fs.openSync(log, "a", 0o600);
		try { fs.writeFileSync(fd, JSON.stringify(ev) + "\n"); fs.fsyncSync(fd); } finally { fs.closeSync(fd); }
		state();
	}
	function guard(fn: () => void) {
		try { fn(); } catch (e) {
			broken = true;
			ctx?.ui.notify(`RelayDock bridge failed: ${String(e)}. Remote submission is disabled.`, "error");
		}
	}
	function receipt(call: string, data?: unknown, code?: string, message?: string) {
		write(path.join(directory, "receipts", call + ".json"), { call_id: call, ...(code ? { error: { code, message } } : { data }) });
	}
	function poll() {
		if (broken) return;
		state();
		for (const file of fs.readdirSync(path.join(directory, "requests")).sort()) {
			if (!file.endsWith(".json") || !valid(file.slice(0, -5))) continue;
			const requestPath = path.join(directory, "requests", file);
			const r = JSON.parse(fs.readFileSync(requestPath, "utf8"));
			if (r.call_id !== file.slice(0, -5)) throw new Error("bridge call ID mismatch");
			if (fs.existsSync(path.join(directory, "receipts", file))) { fs.unlinkSync(requestPath); continue; }
			// Commit uncertainty before injection. A crash between injection and
			// acceptance can never cause this request to execute a second time.
			receipt(r.call_id, undefined, "unknown", "delivery started; acceptance has not been confirmed");
			if (r.session_id !== session || r.instance !== instance || r.native_session !== native) {
				receipt(r.call_id, undefined, "stale_target", "pi instance or native session changed");
			} else if (r.action === "submit") {
				if (!ctx.isIdle() || status !== "idle" || remotePending) receipt(r.call_id, undefined, "busy", "pi is busy");
				else if (!valid(r.run_id) || typeof r.text !== "string" || !r.text.trim() || Buffer.byteLength(r.text) > MAX_TEXT || r.text.trimStart().startsWith("/")) {
					receipt(r.call_id, undefined, "invalid", "text must be a natural-language prompt, not a pi slash command");
				} else if (fs.existsSync(log) && fs.statSync(log).size >= MAX_LOG) {
					receipt(r.call_id, undefined, "capacity", "session event log reached 64 MiB; close and create another session");
				} else {
					run = r.run_id; source = "hub"; remotePending = true; status = "running"; lastStop = ""; lastError = "";
					emit("input", r.text);
					try {
						pi.sendUserMessage(r.text);
						receipt(r.call_id, { accepted: true, session_id: session, run_id: run, instance, native_session: native });
					} catch (e) {
						remotePending = false; status = "idle";
						receipt(r.call_id, undefined, "rejected", String(e)); emit("settled", String(e), "failed");
					}
				}
			} else if (r.action === "interrupt") {
				if (r.run_id !== run || status === "idle") receipt(r.call_id, undefined, "stale_target", "current run changed or already settled");
				else { ctx.abort(); receipt(r.call_id, { requested: true, run_id: run }); }
			} else receipt(r.call_id, undefined, "unsupported", "unknown action");
			fs.unlinkSync(requestPath);
		}
	}

	pi.on("session_start", (_event, context) => {
		ctx = context;
		if (timer) clearInterval(timer);
		instance = id("i_"); native = ctx.sessionManager.getSessionId(); seq = 0; run = ""; status = ctx.isIdle() ? "idle" : "running";
		remotePending = false; broken = false;
		guard(() => {
			// Recover an interrupted final append, retaining only complete events.
			if (fs.existsSync(log)) { const data = fs.readFileSync(log); const end = data.lastIndexOf(10); if (end !== data.length - 1) fs.truncateSync(log, end + 1); }
			emit("ready");
		});
		timer = setInterval(() => guard(poll), 200); timer.unref();
	});
	pi.on("input", (event) => {
		guard(() => {
			if (event.source === "extension" && remotePending) { remotePending = false; return; }
			if (status === "idle" || !run) { run = id("local_"); lastStop = ""; lastError = ""; }
			source = event.source === "interactive" ? "local" : event.source;
			emit("input", event.text);
		});
		return { action: "continue" as const };
	});
	pi.on("agent_start", () => guard(() => {
		if (!run) run = id("local_"); remotePending = false; status = "running"; emit("started");
	}));
	pi.on("message_end", (event) => guard(() => {
		const m = event.message;
		if (m.role !== "assistant") return;
		lastStop = m.stopReason; lastError = m.errorMessage || "";
		const text = m.content.filter((p) => p.type === "text").map((p) => p.text).join("\n");
		if (text || lastError) emit("output", text || lastError);
	}));
	pi.on("agent_settled", () => guard(() => {
		const outcome = lastStop === "aborted" ? "cancelled" : lastStop === "error" ? "failed" : lastStop === "stop" ? "completed" : "unknown";
		status = "idle"; remotePending = false; emit("settled", lastError, outcome);
		// Commands which bypass input (for example retry extensions) must not
		// resurrect a completed Run when their next agent_start arrives.
		run = ""; state();
	}));
	pi.on("ui_prompt_start", (e) => guard(() => { status = "waiting"; emit("waiting", e.title || e.kind); }));
	pi.on("ui_prompt_end", () => guard(() => { status = ctx.isIdle() ? "idle" : "running"; emit("status"); }));
	pi.on("session_shutdown", () => {
		if (timer) clearInterval(timer);
		guard(() => { status = "unknown"; emit("disconnected", "pi session bridge stopped"); });
	});
}
