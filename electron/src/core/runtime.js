'use strict';

const path = require('node:path');
const { EventEmitter } = require('node:events');

const { NodeEngine } = require('./node-engine');
const { BootstrapServer } = require('./bootstrap-server');
const { initializeProfiles, previewProfile } = require('./profiles');
const {
  SettingsStore, toNodeConfig, toBootstrapConfig, nodeNeedsRestart,
  refusals, bootstrapNeedsRestart,
} = require('./settings');

/**
 * Owns both roles for the whole process.
 *
 * The engine is replaced rather than reconfigured when the port, cluster or key
 * changes: those are baked into a bound socket and a derived framing key, so
 * changing them means a restart. Doing it here rather than silently ignoring the
 * change is what keeps a settings screen trustworthy.
 *
 * It re-emits every engine event under a stable name, so the main process can
 * forward to the renderer without rewiring after a rebuild.
 */
class Runtime extends EventEmitter {
  constructor(dataDir, opts = {}) {
    super();
    this.dataDir = dataDir;
    this.opts = opts;
    this.settingsStore = new SettingsStore(path.join(dataDir, 'settings.json'));
    this.engine = null;
    this.bootstrap = null;
    this.lastError = null;
    this.build();
  }

  get settings() {
    return this.settingsStore.get();
  }

  build() {
    const s = this.settings;
    const profile = initializeProfiles(this.dataDir, s.cluster);
    this.engine = new NodeEngine(toNodeConfig(s), path.join(initializeProfiles(this.dataDir, s.cluster), 'node.json'), this.opts);
    this.bootstrap = new BootstrapServer(
      toBootstrapConfig(s), path.join(profile, 'bootstrap.json'), this.opts,
    );
    this.wire();
  }

  wire() {
    for (const event of ['entries', 'peers', 'chat', 'activity', 'status', 'metrics', 'topology']) {
      this.engine.on(event, (payload) => this.emit(event, payload));
    }
    this.bootstrap.on('roster', (payload) => this.emit('bootstrap-roster', payload));
    this.bootstrap.on('status', (payload) => this.emit('bootstrap-status', payload));
    this.bootstrap.on('warning', (text) => this.emit('warning', text));
  }

  /**
   * Runs an operation that touches a socket, reporting the failure rather than
   * letting it escape. Binding a port is the one thing a user can plausibly get
   * wrong, and an unhandled rejection there would take the window with it.
   */
  async guard(fn) {
    try {
      await fn();
      this.lastError = '';
      return { ok: true };
    } catch (e) {
      this.lastError = e.message || String(e);
      this.emit('error-message', this.lastError);
      return { ok: false, error: this.lastError };
    }
  }

  startNode() {
    return this.guard(() => this.engine.start());
  }

  stopNode() {
    return this.guard(() => this.engine.stop());
  }

  startBootstrap() {
    return this.guard(() => this.bootstrap.start());
  }

  stopBootstrap() {
    return this.guard(() => this.bootstrap.stop());
  }

  /** Applies new settings, restarting whichever role they affect and preserving what was running. */
  previewCluster(cluster) {
    const destination = String(cluster || '').trim() || this.settings.cluster;
    return previewProfile(this.dataDir, this.settings.cluster, destination, this.engine.store.updates().length);
  }

  // A multi-key operation must finish before a profile's engine is retired.
  // Otherwise a delayed continuation can save through the old persister after
  // the new engine has opened the same file.
  withProfile(operation) {
    const work = (this.settingsWork || Promise.resolve()).then(() => operation(this.engine));
    this.settingsWork = work.catch(() => {});
    return work;
  }

  applySettings(partial) {
    return this.withProfile(() => this.applySettingsNow(partial));
  }

  async applySettingsNow(partial) {
    const previous = this.settings;
    // A refused value is reported before anything else happens. The form
    // repaints from what comes back, so an unusable port simply snaps back to
    // the old one while the screen says "Settings applied" — which reads as
    // the field not working rather than as the value being wrong.
    for (const why of refusals(partial, previous)) this.emit('warning', `Settings: ${why}`);
    const updated = this.settingsStore.save(partial);

    if (nodeNeedsRestart(previous, updated)) {
      const wasRunning = this.engine.isRunning;
      const stopped = await this.guard(() => this.engine.stop());
      if (!stopped.ok || (this.engine.persister && this.engine.persister.health.pending && this.engine.persister.health.error)) {
        this.settingsStore.save(previous);
        throw new Error(stopped.error || `Profile not switched: ${this.engine.persister.health.error}`);
      }
      this.engine.removeAllListeners();
      this.engine = new NodeEngine(toNodeConfig(updated), path.join(initializeProfiles(this.dataDir, updated.cluster), 'node.json'), this.opts);
      this.bootstrap.removeAllListeners();
      this.wire();
      this.engine.publishEntries();
      this.engine.publishPeers();
      this.engine.publishChat();
      this.engine.publishMetrics();
      if (wasRunning) await this.startNode();
    } else if (previous.nick !== updated.nick) {
      await this.guard(() => this.engine.renameSelf(updated.nick));
    }

    if (bootstrapNeedsRestart(previous, updated)) {
      const wasRunning = this.bootstrap.isRunning;
      await this.guard(() => this.bootstrap.stop());
      if (previous.cluster !== updated.cluster) {
        this.bootstrap.removeAllListeners();
        this.engine.removeAllListeners();
        this.bootstrap = new BootstrapServer(toBootstrapConfig(updated), path.join(initializeProfiles(this.dataDir, updated.cluster), 'bootstrap.json'), this.opts);
        this.wire();
      } else this.bootstrap.config = { ...this.bootstrap.config, ...toBootstrapConfig(updated) };
      if (wasRunning) await this.startBootstrap();
      this.bootstrap.publish();
    }
    return updated;
  }

  /**
   * The full current state, for a renderer that has just connected or reloaded.
   *
   * `lastError` is in here because autostart runs before the window has
   * finished loading: the toast `guard()` emits for a failed bind is sent to a
   * renderer that does not exist yet and is dropped. The app then opened on
   * "STOPPED" with no reason anywhere, which is exactly the case — a port
   * already taken — a user is most likely to hit and least able to guess.
   */
  snapshot() {
    return {
      lastError: this.lastError || '',
      settings: this.settings,
      status: this.engine.status(),
      entries: this.engine.store.page().entries,
      peers: this.engine.peerList(),
      chat: this.engine.chat(),
      activity: this.engine.activity(),
      metrics: this.engine.metrics.snapshot(),
      topology: this.engine.topology(),
      bootstrapStatus: this.bootstrap.status(),
      bootstrapRoster: this.bootstrap.roster(),
    };
  }

  async shutdown() {
    await this.guard(() => this.engine.stop());
    await this.guard(() => this.bootstrap.stop());
  }
}

module.exports = { Runtime };
