'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const { NodeEngine } = require('../src/core/node-engine');
const { KvStore } = require('../src/core/kvstore');
const W = require('../src/proto/wire');

test('digest v2 names empty and expiry conflicts and refuses a legacy comparison',async()=>{
 const a=new KvStore('a'),b=new KvStore('b');
 a.apply({action:'set',key:'k',value:'',version:W.version(1,'same')});b.apply({action:'set',key:'k',value:'nonempty',version:W.version(1,'same')});
 const buckets=Array.from({length:16},(_,i)=>i);
 const result=await NodeEngine.prototype.namedDifferences.call({store:a,requestFingerprint:async()=>({digestVersion:2,entries:b.bucketEntries(16,buckets)})},'peer',buckets);
 assert.equal(result.differences.length,1);assert.equal(result.differences[0].sameVersion,true);
 const legacy=await NodeEngine.prototype.fingerprintPeer.call({nickOf:()=>'',requestFingerprint:async()=>b.fingerprint()},'peer',a.fingerprint(16,2));
 assert.equal(legacy.agrees,false);assert.match(legacy.error,/unsupported/);
 const ttl=new KvStore('ttl');ttl.apply({action:'set',key:'k',value:'',version:W.version(1,'same'),expiresAt:4102444800});
 assert.notDeepEqual(a.fingerprint(16,2).buckets,ttl.fingerprint(16,2).buckets);
});

test('repair concurrency is bounded; known oversized values are skipped, deletes still sent',async()=>{
 const e=new NodeEngine({},null);e.peerLimits.set('peer',{max_value_bytes:2,max_keys:0});const sent=[];let release;
 e.send=async(a,k,p)=>{sent.push(p);await new Promise(r=>{release=r;});};
 const update={key:'k',value:'long',action:'set',version:{counter:1,node:'n'},expiresAt:0};
 await e.sendUpdates('peer',[update]);assert.equal(sent.length,0);
 const a=e.sendUpdates('peer',[{...update,action:'delete',value:''}]);await new Promise(r=>setImmediate(r));const releaseA=release;
 const b=e.sendUpdates('peer',[{...update,action:'delete',value:''}]);await new Promise(r=>setImmediate(r));
 await e.sendUpdates('peer',[update]);assert.equal(e.repairsActive,2);assert.equal(sent.length,2);releaseA();release();await Promise.all([a,b]);assert.equal(e.repairsActive,0);
 const hello=W.decodeHello(W.encodeHello({from:'p',limits:{max_value_bytes:1,max_keys:3}}));assert.equal(hello.limits.max_keys,3);
});
