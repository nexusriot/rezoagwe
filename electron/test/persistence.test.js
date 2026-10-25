'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { Persister } = require('../src/core/persistence');

function temp(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'rezoagwe-persistence-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return dir;
}

test('persistence health exposes disk full, clears after recovery, and retains corruption',async t=>{
 const file=path.join(temp(t),'node.json');fs.writeFileSync(file,'{broken');
 const p=new Persister(file);assert.throws(()=>p.load());assert.equal(fs.readFileSync(p.health.recoveryFile,'utf8'),'{broken');
 const write=p.writeAtomic.bind(p);p.writeAtomic=async()=>{throw Object.assign(new Error('disk full'),{code:'ENOSPC'});};
 p.save(1,{entries:{}});await p.flush();assert.equal(p.health.pending,true);assert.match(p.health.error,/disk full/);
 p.writeAtomic=write;p.save(1,{entries:{}});await p.flush();assert.equal(p.health.pending,false);assert.equal(p.health.error,'');assert.ok(p.health.lastSuccessMs>0);
});
