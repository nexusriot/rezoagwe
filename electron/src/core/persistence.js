'use strict';

const fs = require('node:fs');
const path = require('node:path');
const {Journal,syncDir}=require('./journal');

/**
 * Writes a JSON state file atomically (temp file + rename), gated on a
 * generation number so a slow, out-of-order save can never overwrite the file
 * with a stale snapshot.
 *
 * Errors are reported through onError rather than thrown: every call site is a
 * mutation that has already succeeded in memory, and failing a write should not
 * unwind it.
 */
class Persister {
  constructor(filePath, opts = {}) {
    this.path = filePath;
    this.journal=new Journal(filePath,opts.durability||'snapshot');
    this.lastGen = 0;
    this.health = { mode: this.journal.mode, durable: false, lastDurableMs: 0, enabled: true, pending: false, lastSuccessMs: 0, error: '', recoveryFile: '' };
    this.blocked = false;
    this.onError = opts.onError || (() => {});
    this.pending = null;
    this.queued = null;
  }

  /** Reads the persisted state; null when the file does not exist yet. Throws on a corrupt file. */
  load() {
    let raw;
    try {
      raw = fs.readFileSync(this.path, 'utf8');
    } catch (e) {
      if (e.code === 'ENOENT') return this.replay(null);
      this.protect(e);
      throw e;
    }
    let parsed;try { parsed=JSON.parse(raw); } catch (e) { this.protect(e); throw e; }
    return this.replay(parsed);
  }

  /**
   * Persists state if gen is newer than the last write. Writes are serialised;
   * while one is in flight only the newest request is kept, since a snapshot
   * that has already been superseded is never worth the disk it would cost.
   */
  replay(state) {try{return this.journal.load(state);}catch(e){this.blocked=true;this.health.error=e.message;throw e;}}

  save(gen, state) {
    if (gen !== 0 && (gen < this.lastGen || (gen === this.lastGen && !this.health.error))) return;
    this.lastGen = gen;
    this.health.pending = true;
    this.health.durable = false;
    if (this.blocked) return;
    this.queued = state;
    if (!this.pending) this.pending = this.drain();
  }

  async drain() {
    while (this.queued) {
      const state = this.queued;
      this.queued = null;
      try {
        await this.journal.save(state,s=>this.writeAtomic(s));
        this.health.lastSuccessMs = Date.now();
        this.health.error = '';
        this.health.pending = !!this.queued;
        this.health.durable=this.journal.mode==='journal-sync'&&!this.queued;
        if(this.health.durable)this.health.lastDurableMs=this.health.lastSuccessMs;
      } catch (e) {
        this.health.error = e.message;
        this.onError(e);
      }
    }
    this.pending = null;
  }

  protect(error) {
    this.health.error = error.message;
    const backup = `${this.path}.corrupt-${Date.now()}-${require('crypto').randomUUID()}`;
    try {
      if (!fs.statSync(this.path).isFile()) throw new Error('state is not a regular file');
      fs.renameSync(this.path, backup);
      this.health.recoveryFile = backup;
      if(fs.existsSync(this.path+'.wal'))fs.renameSync(this.path+'.wal',backup+'.wal');
    } catch (e) { this.blocked = true; this.health.error += `; recovery blocked: ${e.message}`; }
  }

  async writeAtomic(state) {
    const dir = path.dirname(this.path);
    if (dir) await fs.promises.mkdir(dir, { recursive: true });
    const tmp = `${this.path}.tmp`;
    await fs.promises.writeFile(tmp, JSON.stringify(state, null, 2), 'utf8');
    // Rename is atomic on the same filesystem: a reader sees either the old file
    // or the fully written new one, never a truncated write.
    if(this.journal.mode==='journal-sync'){const f=await fs.promises.open(tmp,'r+');try{await f.sync();}finally{await f.close();}}
    await fs.promises.rename(tmp, this.path);
    if(this.journal.mode==='journal-sync')syncDir(path.dirname(this.path));
  }

  /** Resolves once every queued write has landed. Shutdown and the tests wait on it. */
  flush() {
    return this.pending || Promise.resolve();
  }
}

module.exports = { Persister };
