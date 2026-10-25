'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { Persister } = require('../src/core/persistence');

function temp(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'rezoagwe-profiles-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return dir;
}

function file(t) { return path.join(temp(t), 'state.json'); }

test('legacy profile migration retains journal-only changes',async t=>{
 const f=file(t),root=path.dirname(f),legacy=path.join(root,'node.json');const p=new Persister(legacy,{durability:'journal'});p.save(1,{entries:{k:{value:'journal-only'}}});await p.flush();
 const {initializeProfiles,previewProfile}=require('../src/core/profiles');const dir=initializeProfiles(root,'test');assert.equal(new Persister(path.join(dir,'node.json')).load().entries.k.value,'journal-only');assert.equal(previewProfile(root,'other','test',0).destinationEntries,null);
});
