'use strict';

const fs = require('node:fs');
const path = require('node:path');

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
    this.lastGen = 0;
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
      if (e.code === 'ENOENT') return null;
      throw e;
    }
    return JSON.parse(raw);
  }

  /**
   * Persists state if gen is newer than the last write. Writes are serialised;
   * while one is in flight only the newest request is kept, since a snapshot
   * that has already been superseded is never worth the disk it would cost.
   */
  save(gen, state) {
    if (gen !== 0 && gen <= this.lastGen) return;
    this.lastGen = gen;
    this.queued = state;
    if (!this.pending) this.pending = this.drain();
  }

  async drain() {
    while (this.queued) {
      const state = this.queued;
      this.queued = null;
      try {
        await this.writeAtomic(state);
      } catch (e) {
        this.onError(e);
      }
    }
    this.pending = null;
  }

  async writeAtomic(state) {
    const dir = path.dirname(this.path);
    if (dir) await fs.promises.mkdir(dir, { recursive: true });
    const tmp = `${this.path}.tmp`;
    await fs.promises.writeFile(tmp, JSON.stringify(state, null, 2), 'utf8');
    // Rename is atomic on the same filesystem: a reader sees either the old file
    // or the fully written new one, never a truncated write.
    await fs.promises.rename(tmp, this.path);
  }

  /** Resolves once every queued write has landed. Shutdown and the tests wait on it. */
  flush() {
    return this.pending || Promise.resolve();
  }
}

module.exports = { Persister };
