'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const { NodeEngine } = require('../src/core/node-engine');
const { KvStore } = require('../src/core/kvstore');
const W = require('../src/proto/wire');
const { healthChecks } = require('../src/core/diagnostics');

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

// The Android port encodes with encodeDefaults=false, so a phone with no store
// limits sends `"limits":{}`. Read raw, the missing fields compared unequal to
// this node's zeros and every Android peer was reported as "limits differ".
test('a hello whose limits omit their zero fields is equal limits, not a warning',()=>{
 assert.deepEqual(W.decodeHello({from:'p',limits:{}}).limits,{max_value_bytes:0,max_keys:0});
 assert.deepEqual(W.decodeHello({from:'p',limits:{max_keys:'7',junk:1}}).limits,{max_value_bytes:0,max_keys:7});
 const e=new NodeEngine({},null);
 e.handleHello(W.decodeHello({from:'10.0.0.2:3137',limits:{}}));
 assert.equal(e.peers.has('10.0.0.2:3137'),true);
 assert.deepEqual(e.diagnostics().replicationWarnings,[]);
});

test('a departed peer takes its advertised limits with it',()=>{
 const e=new NodeEngine({},null);
 e.handleHello(W.decodeHello({from:'10.0.0.2:3137',limits:{max_keys:5}}));
 assert.equal(e.diagnostics().replicationWarnings.length,1);
 // The rule reads the snapshot's top level; the list once sat inside each
 // peer row, where the engine produced it and no finding ever saw it.
 assert.ok(healthChecks(e.diagnostics(),Date.now()).some((c)=>c.title==='Replication limits differ'));
 e.forgetPeer('10.0.0.2:3137');
 assert.equal(e.peerLimits.has('10.0.0.2:3137'),false);
});

// Found on an IPv6-only Wi-Fi with the Android app, and the same code is here:
// an unbracketed IPv6 address is one no Go peer can parse.
test('addresses bracket an IPv6 host, and the unbracketed form is refused',()=>{
 const A=require('../src/net/addr');
 assert.equal(A.joinHostPort('2a00:f3c::1',3137),'[2a00:f3c::1]:3137');
 assert.equal(A.joinHostPort('[2a00:f3c::1]',3137),'[2a00:f3c::1]:3137');
 assert.equal(A.joinHostPort('10.0.0.2',3137),'10.0.0.2:3137');
 assert.equal(A.validPeerAddr('2a00:f3c::1:3137'),false);
 assert.equal(A.validPeerAddr('[2a00:f3c::1]:3137'),true);
 assert.equal(A.validPeerAddr(':3137'),true);
 const e=new NodeEngine({advertiseHost:'2a00:f3c::1',port:3137},null);
 assert.equal(e.addr,'[2a00:f3c::1]:3137');
});

test('the desktop refuses an IPv6 advertise host out loud, since its sockets are IPv4',()=>{
 const S=require('../src/core/settings');
 const typed={...S.DEFAULTS,advertiseHost:' 2a00:f3c::1 '};
 assert.equal(S.sanitize(typed).advertiseHost,'');
 assert.match(S.refusals(typed).join('\n'),/IPv6.*IPv4 only/);
 assert.equal(S.sanitize({...S.DEFAULTS,advertiseHost:'192.168.1.5'}).advertiseHost,'192.168.1.5');
 assert.deepEqual(S.refusals({...S.DEFAULTS,advertiseHost:'192.168.1.5'}),[]);
});
