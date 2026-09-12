'use strict';

const { contextBridge, ipcRenderer } = require('electron');

// A thin, explicit bridge. The renderer only ever sees these methods; it never
// gets ipcRenderer or any node API directly (contextIsolation is on and the
// renderer is sandboxed).

const invoke = (channel, ...args) => ipcRenderer.invoke(channel, ...args);

/** Subscribes to a main-process event, returning the unsubscribe function. */
const on = (channel, fn) => {
  const listener = (_event, payload) => fn(payload);
  ipcRenderer.on(channel, listener);
  return () => ipcRenderer.removeListener(channel, listener);
};

contextBridge.exposeInMainWorld('rezoagwe', {
  app: {
    snapshot: () => invoke('app:snapshot'),
    settings: () => invoke('app:settings'),
    applySettings: (partial) => invoke('app:applySettings', partial),
  },
  node: {
    start: () => invoke('node:start'),
    stop: () => invoke('node:stop'),
    submit: (text) => invoke('node:submit', text),
    history: (key) => invoke('node:history', key),
  },
  kv: {
    set: (req) => invoke('kv:set', req),
    remove: (req) => invoke('kv:delete', req),
  },
  peer: {
    sync: (addr) => invoke('peer:sync', addr),
    add: (addr) => invoke('peer:add', addr),
    forget: (addr) => invoke('peer:forget', addr),
    gossip: (addr) => invoke('peer:gossip', addr),
  },
  bootstrap: {
    start: () => invoke('bootstrap:start'),
    stop: () => invoke('bootstrap:stop'),
  },
  diag: {
    snapshot: () => invoke('diag:snapshot'),
    copyReport: () => invoke('diag:copyReport'),
  },
  store: {
    export: () => invoke('store:export'),
    import: (asLocalWrites) => invoke('store:import', asLocalWrites),
  },
  events: {
    onEntries: (fn) => on('node:entries', fn),
    onPeers: (fn) => on('node:peers', fn),
    onChat: (fn) => on('node:chat', fn),
    onActivity: (fn) => on('node:activity', fn),
    onStatus: (fn) => on('node:status', fn),
    onMetrics: (fn) => on('node:metrics', fn),
    onTopology: (fn) => on('node:topology', fn),
    onBootstrapRoster: (fn) => on('bootstrap:roster', fn),
    onBootstrapStatus: (fn) => on('bootstrap:status', fn),
    onToast: (fn) => on('toast', fn),
  },
});
