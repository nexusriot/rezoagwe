'use strict';
const fs=require('node:fs');
const path=require('node:path');
const {createHash}=require('node:crypto');
const hash=s=>createHash('sha256').update(s,'utf8').digest('hex');
function syncDir(dir){const fd=fs.openSync(dir,'r');try{fs.fsyncSync(fd);}finally{fs.closeSync(fd);}}
class Journal {
  constructor(file,mode='snapshot'){if(!['snapshot','journal','journal-sync'].includes(mode))throw new Error('Invalid durability mode');this.file=file;this.mode=mode;this.previous={entries:{}};this.records=0;}
  load(snapshot){
    let state=snapshot||{entries:{}};state.entries=Object.assign(Object.create(null),state.entries||{});
    if(fs.existsSync(this.file+'.wal')){
      if(snapshot===null || snapshot===undefined)throw new Error('Journal checkpoint is missing; recovery blocked');
      const bytes=fs.readFileSync(this.file+'.wal');let start=0;
      for(let end=bytes.indexOf(10);end>=0;end=bytes.indexOf(10,start)){
        const rec=JSON.parse(bytes.subarray(start,end).toString('utf8'));
        if(hash(rec.data)!==rec.sha256)throw new Error(`Journal checksum mismatch at ${start}`);
        const d=JSON.parse(rec.data);
        state={node_id:d.node_id,clock:d.clock,chat:d.chat,chat_seq:d.chat_seq,entries:state.entries};
        for(const [k,v] of Object.entries(d.set||{}))state.entries[k]=v;
        for(const k of d.remove||[])delete state.entries[k];
        this.records++;start=end+1;
      }
      if(start<bytes.length)fs.truncateSync(this.file+'.wal',start);
      snapshot=state;
    }
    this.previous=state;return snapshot;
  }
  async save(state,checkpoint){
    if(this.mode==='snapshot'){
      if(fs.existsSync(this.file+'.wal')){this.mode='journal';try{await this.save(state,checkpoint);}finally{this.mode='snapshot';}}
      await checkpoint(state);
      try{await fs.promises.unlink(this.file+'.wal');}catch(e){if(e.code!=='ENOENT')throw e;}
      this.previous=state;return;
    }
    await fs.promises.mkdir(path.dirname(this.file),{recursive:true});
    if(!fs.existsSync(this.file))await checkpoint({entries:{}});
    const set=Object.create(null),remove=[];
    for(const [k,v] of Object.entries(state.entries||{}))if(JSON.stringify(v)!==JSON.stringify(this.previous.entries?.[k]))set[k]=v;
    for(const k of Object.keys(this.previous.entries||{}))if(!Object.hasOwn(state.entries||{},k))remove.push(k);
    const data=JSON.stringify({node_id:state.node_id||'',clock:state.clock||0,chat:state.chat||[],chat_seq:state.chat_seq||0,set,remove});
    const line=Buffer.from(JSON.stringify({data,sha256:hash(data)})+'\n');
    const f=await fs.promises.open(this.file+'.wal','a',0o600);const before=(await f.stat()).size;
    try{await f.writeFile(line);if(this.mode==='journal-sync')await f.sync();}catch(e){await f.truncate(before);throw e;}finally{await f.close();}
    if(this.mode==='journal-sync')syncDir(path.dirname(this.file));
    this.previous=state;this.records++;
    if(this.records>=64||before+line.length>16*1024*1024){
      await checkpoint(state);
      const f=await fs.promises.open(this.file+'.wal','w',0o600);try{if(this.mode==='journal-sync')await f.sync();}finally{await f.close();}this.records=0;
    }
  }
}
module.exports={Journal,syncDir};
