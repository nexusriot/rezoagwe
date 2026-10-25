'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { Runtime } = require('../src/core/runtime');
const { RestorePlans } = require('../src/core/restore');

function temp(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'rezoagwe-runtime-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return dir;
}

function file(t) { return path.join(temp(t), 'state.json'); }

test('switching cluster profiles retains source identity and requires explicit data import',async t=>{
 const runtime=new Runtime(temp(t));t.after(()=>runtime.shutdown());
 const source=runtime.settings.cluster, id=runtime.engine.nodeId;
 runtime.engine.store.write('source-only','secret');
 const preview=runtime.previewCluster('other');assert.equal(preview.currentEntries,1);assert.equal(preview.destinationEntries,0);
 await runtime.applySettings({cluster:'other'});assert.notEqual(runtime.engine.nodeId,id);assert.equal(runtime.engine.store.get('source-only'),undefined);
 runtime.engine.store.write('other-only','second');
 await runtime.applySettings({cluster:source});assert.equal(runtime.engine.nodeId,id);assert.equal(runtime.engine.store.get('source-only'),'secret');assert.equal(runtime.engine.store.get('other-only'),undefined);
});

test('a profile switch does not discard unsaved state after a disk failure',async t=>{
 const runtime=new Runtime(temp(t));t.after(()=>runtime.shutdown());
 await runtime.engine.persister.flush();
 const original=runtime.settings.cluster,engine=runtime.engine;
 engine.persister.writeAtomic=async()=>{throw new Error('disk full');};
 engine.store.write('unsaved','keep me');
 await assert.rejects(runtime.applySettings({cluster:'other'}),/disk full/);
 assert.equal(runtime.engine,engine);assert.equal(runtime.settings.cluster,original);assert.equal(engine.store.get('unsaved'),'keep me');
});

test('profile switching waits for an in-flight selected restore',async t=>{
 const root=path.dirname(file(t)),runtime=new Runtime(root);
 const engine=runtime.engine,cluster=runtime.settings.cluster,plans=new RestorePlans();
 const plan=plans.preview(engine,{cluster,entries:[{action:'set',key:'one',value:'backup'},{action:'set',key:'two',value:'backup'}]});
 let release,entered;const gate=new Promise(r=>{release=r;}),started=new Promise(r=>{entered=r;});
 const check=engine.checkConsistency.bind(engine);let calls=0;
 engine.checkConsistency=async()=>{if(calls++===0){entered();await gate;}return check();};
 const restore=runtime.withProfile(e=>plans.apply(e,plan.id,['one','two']));await started;
 const change=runtime.applySettings({cluster:'other'});await new Promise(r=>setImmediate(r));assert.equal(runtime.engine,engine);
 release();assert.equal((await restore).restored,2);await change;assert.notEqual(runtime.engine,engine);
 await runtime.applySettings({cluster});assert.equal(runtime.engine.store.get('one'),'backup');assert.equal(runtime.engine.store.get('two'),'backup');await runtime.engine.stop();
});
