'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { Persister } = require('../src/core/persistence');

function temp(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'rezoagwe-journal-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return dir;
}

function file(t) { return path.join(temp(t), 'state.json'); }

for(const mode of ['journal','journal-sync'])test(`${mode}: checkpoint, tail recovery, removal and durability`,async t=>{
 const f=file(t),p=new Persister(f,{durability:mode});p.load();
 for(let i=1;i<=65;i++){p.save(i,{clock:i,entries:{k:{value:String(i)}}});await p.flush();assert.equal(p.health.error,'');}
 assert.equal(p.health.durable,mode==='journal-sync');fs.appendFileSync(f+'.wal','{"torn');
 const q=new Persister(f,{durability:mode});assert.equal(q.load().entries.k.value,'65');q.save(66,{clock:66,entries:{}});await q.flush();assert.deepEqual(Object.keys(new Persister(f).load().entries),[]);
});

test('journal corruption blocks overwrites and survives snapshot mode transition',async t=>{
 const f=file(t),p=new Persister(f,{durability:'journal'});p.save(1,{entries:{k:{value:'old'}}});await p.flush();
 const q=new Persister(f);q.load();q.save(2,{entries:{k:{value:'new'}}});await q.flush();assert.equal(new Persister(f).load().entries.k.value,'new');
 const r=new Persister(f,{durability:'journal'});r.load();r.save(3,{entries:{k:{value:'latest'}}});await r.flush();fs.appendFileSync(f+'.wal','{bad}\n');const before=fs.readFileSync(f+'.wal');
 const bad=new Persister(f);assert.throws(()=>bad.load());bad.save(4,{entries:{}});await bad.flush();assert.deepEqual(fs.readFileSync(f+'.wal'),before);
});

test('a missing checkpoint must not recover a partial journal as a complete store',async t=>{
 const f=file(t),p=new Persister(f,{durability:'journal'});
 // Checkpoint contains keys not repeated in the remaining delta journal.
 for(let i=1;i<=65;i++){p.save(i,{clock:i,entries:{stable:{value:'keep'},changed:{value:String(i)}}});await p.flush();}
 fs.unlinkSync(f);const before=fs.readFileSync(f+'.wal');const q=new Persister(f);
 assert.throws(()=>q.load(),/checkpoint/i);q.save(1,{entries:{}});await q.flush();assert.deepEqual(fs.readFileSync(f+'.wal'),before);assert.equal(fs.existsSync(f),false);
});

test('shared Go and Electron journal replays UTF-8, removals and prototype keys',t=>{
 const corpus=require('../../testdata/journal.json'),f=file(t);fs.writeFileSync(f,JSON.stringify(corpus.checkpoint));fs.writeFileSync(f+'.wal',corpus.records.map(r=>JSON.stringify(r)).join('\n')+'\n');const s=new Persister(f).load();
 assert.equal(s.clock,5);assert.equal(s.node_id,'journal-fixture');assert.equal(s.entries.keep.value,'checkpoint value');assert.equal(s.entries.go.value,'Go <>& 😀\nrecord');assert.equal(s.entries.__proto__.value,'JS 😀 <>&');assert.equal(s.entries.deleted.deleted,true);assert.equal(Object.hasOwn(s.entries,'removed'),false);
});
