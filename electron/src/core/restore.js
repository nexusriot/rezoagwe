'use strict';
const {randomUUID}=require('node:crypto');
const W=require('../proto/wire');
const {representable}=require('./kvstore');
class RestorePlans {
  constructor(){this.plans=new Map();}
  preview(engine,backup,{deleteMissing=false,allowCrossCluster=false}={}){
    if(!backup||!Array.isArray(backup.entries))throw new Error('Backup must contain an entries array');
    if(backup.cluster!==engine.config.cluster&&!allowCrossCluster)throw new Error(`Backup cluster ${backup.cluster} differs from ${engine.config.cluster}; allow cross-cluster restore explicitly`);
    const desired=new Map();
    for(const raw of backup.entries){
      if(!raw||!['set','delete'].includes(raw.action)||typeof raw.key!=='string'||!representable(raw.key)||(raw.value!==undefined&&typeof raw.value!=='string')||!representable(raw.value||'')||(raw.expires_at!==undefined&&(!Number.isSafeInteger(raw.expires_at)||raw.expires_at<0)))throw new Error('Invalid backup entry');
      if(desired.has(raw.key))throw new Error(`Duplicate key: ${raw.key}`);
      const u=W.decodeKVUpdate(raw);
      if(u.action==='set'&&u.expiresAt&&u.expiresAt<=Math.floor(engine.now()/1000)){u.action='delete';u.value='';u.expiresAt=0;}
      desired.set(u.key,u);
    }
    const current=new Map(engine.store.updates().map(u=>[u.key,u]));
    if(deleteMissing)for(const key of current.keys())if(!desired.has(key))desired.set(key,{key,action:'delete',value:'',expiresAt:0});
    const items=[];
    for(const [key,to] of desired){
      const from=current.get(key)||null;
      if(from&&from.action===to.action&&(to.action==='delete'||(from.value===to.value&&from.expiresAt===to.expiresAt)))continue;
      items.push({key,current:from,desired:to,change:to.action==='delete'?'delete':from?'overwrite':'add'});
    }
    items.sort((a,b)=>W.compareWireStrings(a.key,b.key));
    const plan={id:randomUUID(),nodeId:engine.nodeId,cluster:engine.config.cluster,source:backup.node,sourceCluster:backup.cluster,created:Date.now(),items};
    while(this.plans.size>=4)this.plans.delete(this.plans.keys().next().value);
    this.plans.set(plan.id,plan);return this.page(engine,plan.id,0);
  }
  plan(engine,id){const p=this.plans.get(id);if(!p||Date.now()-p.created>600000||p.nodeId!==engine.nodeId||p.cluster!==engine.config.cluster)throw new Error('Restore preview expired or profile changed; preview again');return p;}
  page(engine,id,offset=0){const p=this.plan(engine,id);offset=Math.max(0,Math.floor(Number(offset)||0));
    const preview=u=>u?{...u,value:(u.value||'').slice(0,512),truncated:(u.value||'').length>512}:null;
    return {id,source:p.source,sourceCluster:p.sourceCluster,total:p.items.length,offset,next:offset+100<p.items.length?offset+100:-1,
      items:p.items.slice(offset,offset+100).map(i=>({...i,current:preview(i.current),desired:preview(i.desired)}))};
  }
  detail(engine,id,key){const item=this.plan(engine,id).items.find(i=>i.key===key);if(!item)throw new Error('Unknown preview key');return item;}
  async apply(engine,id,keys){
    const p=this.plan(engine,id),selected=new Set(keys);
    if(!selected.size||selected.size>500)throw new Error('Select 1–500 keys');
    if([...selected].some(k=>!p.items.some(i=>i.key===k)))throw new Error('Key outside preview');
    const before=await engine.checkConsistency();
    const floor=Math.max(engine.store.clock,...before.peers.filter(p=>p.reachable).map(p=>p.clock));
    if(!Number.isSafeInteger(floor)||floor+selected.size>Number.MAX_SAFE_INTEGER)throw new Error('Version counter exhausted');
    const report={restored:0,changed:0,refused:0,items:[]};
    for(const i of p.items.filter(i=>selected.has(i.key))){
      const now=engine.store.updatesFor([i.key])[0]||null;
      if(JSON.stringify(now)!==JSON.stringify(i.current)){report.changed++;report.items.push({key:i.key,status:'changed'});continue;}
      engine.store.clock=Math.max(engine.store.clock,floor);
      const u=i.desired.action==='delete'?engine.store.remove(i.key):engine.store.write(i.key,i.desired.value,{expiresAt:i.desired.expiresAt});
      if(!u){report.refused++;report.items.push({key:i.key,status:'refused'});continue;}
      engine.metrics.kvLocalWrites++;await engine.broadcast(W.Kind.KV,W.encodeKVUpdate(u));report.restored++;report.items.push({key:i.key,status:'restored'});
    }
    engine.publishEntries();report.after=await engine.checkConsistency();return report;
  }
}
module.exports={RestorePlans};
