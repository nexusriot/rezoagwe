'use strict';

const { REFUSAL } = require('./kvstore');

/**
 * A write the store would not take, and why.
 *
 * Thrown rather than returned because these are not the ordinary outcome: a
 * compare-and-swap that loses its race still returns null, which is a normal
 * thing for a lock to do. These are configuration or protocol limits, and for
 * a while they were reported to the user as success — the value silently
 * absent, the UI showing nothing at all.
 */
class StoreRefusal extends Error {
  constructor(reason, key) {
    super(`${key}: ${StoreRefusal.explain(reason)}`);
    this.name = 'StoreRefusal';
    this.reason = reason;
    this.key = key;
  }

  /** What to tell someone looking at the screen. */
  static explain(reason) {
    switch (reason) {
      case REFUSAL.UNSHIPPABLE:
        return 'too large to replicate to any peer — no setting makes a value this big travel';
      case REFUSAL.TOO_LARGE:
        return "larger than this node's configured value limit";
      case REFUSAL.TOO_MANY_KEYS:
        return 'the store is at its configured key limit';
      case REFUSAL.VERSION_MISMATCH:
        return 'the key no longer holds the expected version';
      default:
        return 'refused by the store';
    }
  }
}

module.exports = { StoreRefusal };
