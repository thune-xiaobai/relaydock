// Run with: node scripts/test-gateway.mjs /absolute/path/to/pi-coding-agent
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

if (!process.argv[2]) throw new Error('Pass the installed pi package root');
const { loadExtensions } = await import(pathToFileURL(path.join(process.argv[2], 'dist/core/extensions/loader.js')));
const spool = fs.mkdtempSync(path.join(os.tmpdir(), 'relaydock-gateway-'));
process.env.RELAYDOCK_SPOOL = spool;
process.env.RELAYDOCK_CHAT_REF = 'fixture-chat';
process.env.RELAYDOCK_SENDER_REF = 'fixture-sender';
try {
  const result = await loadExtensions([path.resolve('extensions/wecom-gateway.ts')], process.cwd());
  assert.deepEqual(result.errors, []);
  const tools = result.extensions[0].tools;
  assert.equal(tools.size, 4);
  const [prepare] = result.extensions[0].handlers.get('before_agent_start');
  const prepared = await prepare({systemPrompt:'EXISTING_COMPUTER_USE_RULES'}, {});
  assert.ok(prepared.systemPrompt.startsWith('EXISTING_COMPUTER_USE_RULES'));
  assert.ok(prepared.systemPrompt.includes('fixture-chat'));
  assert.ok(prepared.systemPrompt.includes('fixture-sender'));
  assert.ok(prepared.systemPrompt.includes('stateId、@r、@e'));
  const call = async (name, args) => tools.get(name).definition.execute('fixture-call', args, undefined, undefined, {});
  const input = {chat_ref:'fixture-chat', sender_ref:'fixture-sender', observation_id:'occurrence-1', text:'检查一下项目', source_context:'17:00，上一条消息为测试开始'};
  const first = await call('relaydock_receive', input);
  await call('relaydock_receive', input);
  assert.equal(fs.readdirSync(path.join(spool, 'inbox')).length, 1);
  await assert.rejects(call('relaydock_receive', {...input, sender_ref:'another'}));
  await assert.rejects(call('relaydock_receive', {...input, text:'different text'}));
  await call('relaydock_receive', {...input, observation_id:'occurrence-2'});
  assert.equal(fs.readdirSync(path.join(spool, 'inbox')).length, 2); // Same text, distinct user occurrence.
  const checkpoint = (await call('relaydock_outbox', {})).details.recent_inputs.find(m => m.observation_id === input.observation_id);
  assert.equal(checkpoint.source_context, input.source_context);
  assert.equal(checkpoint.text_preview, input.text);
  assert.equal(checkpoint.truncated, false);
  // The observation survives reloading the extension, not only model context.
  const reloaded = await loadExtensions([path.resolve('extensions/wecom-gateway.ts')], process.cwd());
  assert.deepEqual(reloaded.errors, []);
  const restored = await reloaded.extensions[0].tools.get('relaydock_outbox').definition.execute('fixture-reload', {}, undefined, undefined, {});
  assert.equal(restored.details.recent_inputs.length, 2);
  for (const [id, sequence] of [['out_later', 2], ['out_first', 1]]) {
    fs.writeFileSync(path.join(spool, 'outbox', `${id}.json`), JSON.stringify({id, sequence, text:'fixture output'}));
  }
  assert.equal((await call('relaydock_outbox', {})).details.messages[0].id, 'out_first');
  await call('relaydock_claim_output', {id:'out_first'});
  await assert.rejects(call('relaydock_claim_output', {id:'out_first'}));
  await call('relaydock_finish_output', {id:'out_first', status:'unknown'});
  await assert.rejects(call('relaydock_claim_output', {id:'out_first'}));
  await call('relaydock_finish_output', {id:'out_first', status:'sent'});
  await assert.rejects(call('relaydock_finish_output', {id:'out_first', status:'unknown'}));
  assert.equal((await call('relaydock_outbox', {})).details.messages.length, 1);
  assert.ok(first.details.id.startsWith('in_'));
  console.log('gateway tools: real pi loader; prompt composition, durable observation checkpoints, source binding, occurrence dedupe, output order and uncertain-send protection passed; no UI/send performed');
} finally {
  fs.rmSync(spool, {recursive:true, force:true});
}
