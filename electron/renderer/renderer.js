/* eslint-env browser */
'use strict';

/**
 * The window onto a running node.
 *
 * Screens are mounted once and patched afterwards rather than rebuilt on every
 * event: a node emits on every gossip tick, and rebuilding would take the focus
 * out of the chat composer and the settings form roughly twice a second.
 */

const api = window.rezoagwe;
const S = window.Shared;
const G = window.Graph;

window.__errors = [];
window.addEventListener('error', (e) => window.__errors.push(String(e.message)));
window.addEventListener('unhandledrejection', (e) => window.__errors.push(String(e.reason)));

const state = {
  settings: null,
  status: { running: false, peers: 0, keys: 0, tombstones: 0, uptimeSec: 0, addr: '', nick: '', cluster: '' },
  entries: [],
  peers: [],
  chat: [],
  activity: [],
  metrics: null,
  topology: { nodes: [], links: [] },
  bootstrapStatus: { running: false, port: 0, nodes: 0, uptimeSec: 0 },
  bootstrapRoster: [],
  diag: null,
  version: '',
};

const ui = {
  screen: 'keys',
  split: false,
  splitAvailable: false,
  filter: '',
  notice: null,
  graph: { scale: 1, panX: 0, panY: 0, selected: null },
  chatDraft: '',
};

const nowSec = () => Math.floor(Date.now() / 1000);

// ---- tiny DOM helper --------------------------------------------------------

function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === null || v === undefined || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'text') el.textContent = v;
    else if (k === 'html') throw new Error('refusing to set innerHTML');
    else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
    else if (k === 'color') el.style.color = v;
    else if (k === 'value') el.value = v;
    else if (k === 'checked') el.checked = !!v;
    else el.setAttribute(k, String(v));
  }
  for (const child of children.flat()) {
    if (child === null || child === undefined || child === false) continue;
    el.append(typeof child === 'string' || typeof child === 'number' ? String(child) : child);
  }
  return el;
}

const clear = (el) => {
  while (el.firstChild) el.removeChild(el.firstChild);
};

/**
 * Appends children, skipping the absent ones.
 *
 * Element.append turns a null into the text "null", so a conditional child has
 * to be filtered rather than passed through — the header printed the word for
 * every chip it was not showing.
 */
const add = (parent, ...children) => {
  for (const child of children.flat()) {
    if (child === null || child === undefined || child === false) continue;
    parent.append(child);
  }
  return parent;
};

function toast(kind, text) {
  const box = document.getElementById('toasts');
  const node = h('div', { class: `toast ${kind}`, text });
  box.append(node);
  setTimeout(() => node.remove(), 6000);
}

// ---- modal ------------------------------------------------------------------

function openModal(build) {
  const root = document.getElementById('modal-root');
  clear(root);
  const close = () => clear(root);
  const backdrop = h('div', {
    class: 'modal-backdrop',
    onclick: (e) => {
      if (e.target === backdrop) close();
    },
  });
  const modal = h('div', { class: 'modal' });
  backdrop.append(modal);
  root.append(backdrop);
  build(modal, close);
  // Escape closes: a dialog that can only be dismissed by hitting the right
  // button is a trap when the button is off screen.
  const onKey = (e) => {
    if (e.key === 'Escape') {
      close();
      document.removeEventListener('keydown', onKey);
    }
  };
  document.addEventListener('keydown', onKey);
  const first = modal.querySelector('input, textarea, button');
  if (first) first.focus();
  return close;
}

// ---- key dialog -------------------------------------------------------------

function keyDialog({ title, entry, keyEditable, guardHint, onSave }) {
  openModal((modal, close) => {
    const keyInput = h('input', { type: 'text', value: entry ? entry.key : '', disabled: !keyEditable });
    const valueInput = h('textarea', { rows: 4 }, entry ? entry.value : '');
    const remaining = entry && entry.expiresAt ? Math.max(0, entry.expiresAt - nowSec()) : 0;
    const ttlInput = h('input', { type: 'number', min: '0', value: remaining ? String(remaining) : '' });
    const guardInput = h('input', { type: 'checkbox' });

    modal.append(
      h('h2', { text: title }),
      h('label', { class: 'field' }, h('span', { text: 'Key' }), keyInput),
      h('label', { class: 'field' }, h('span', { text: 'Value' }), valueInput),
      h('label', { class: 'field' },
        h('span', { text: 'TTL in seconds (blank = never expires)' }), ttlInput),
      h('label', { class: 'check' }, guardInput, guardHint),
      h('div', { class: 'hint', text: entry && entry.version
        ? `Guarding compares against v${entry.version.counter}, the version shown when this opened.`
        : 'Guarding refuses the write if the key already exists.' }),
      h('div', { class: 'actions' },
        h('button', { class: 'btn', type: 'button', onclick: close, text: 'Cancel' }),
        h('button', {
          class: 'btn primary',
          type: 'button',
          text: 'Save',
          onclick: async () => {
            const key = keyInput.value.trim();
            if (!key) return;
            close();
            await onSave({
              key,
              value: valueInput.value,
              ttlSeconds: Number(ttlInput.value) || 0,
              guard: guardInput.checked,
            });
          },
        })),
    );
  });
}

function historyDialog(key) {
  openModal(async (modal, close) => {
    modal.append(h('h2', { text: `History of ${key}` }));
    const body = h('div', {}, h('div', { class: 'empty', text: 'Loading…' }));
    modal.append(body, h('div', { class: 'actions' },
      h('button', { class: 'btn', type: 'button', onclick: close, text: 'Close' })));
    const history = await api.node.history(key);
    clear(body);
    if (!history.length) {
      body.append(h('div', { class: 'empty', text: 'No recorded versions.' }));
      return;
    }
    // Newest first: the last thing that happened is what you came for.
    for (const entry of [...history].reverse()) {
      body.append(h('div', { class: 'history-entry' },
        h('div', {
          class: 'head',
          text: `v${entry.version.counter} ${entry.deleted ? 'delete' : 'set'} by ${entry.writer}`
            + ` ${entry.local ? '(local)' : '(remote)'}`,
        }),
        entry.deleted ? null : h('div', { class: 'val', text: entry.value.slice(0, 400) })));
    }
  });
}

function confirmDialog({ title, body, confirmLabel, danger, onConfirm }) {
  openModal((modal, close) => {
    modal.append(
      h('h2', { text: title }),
      h('div', { text: body }),
      h('div', { class: 'actions' },
        h('button', { class: 'btn', type: 'button', onclick: close, text: 'Cancel' }),
        h('button', {
          class: `btn ${danger ? 'danger' : 'primary'}`,
          type: 'button',
          text: confirmLabel,
          onclick: () => {
            close();
            onConfirm();
          },
        })),
    );
  });
}

// ---- screens ----------------------------------------------------------------

const screens = {};

/** Keys: the store itself, with TTL, version and a guarded save. */
screens.keys = () => {
  const rows = h('div', { class: 'rows' });
  const noticeBar = h('div', {});
  const filterInput = h('input', {
    type: 'text',
    placeholder: 'Filter keys and values',
    value: ui.filter,
    oninput: (e) => {
      ui.filter = e.target.value;
      render();
    },
  });

  const save = async (req, entry) => {
    ui.notice = null;
    const result = await api.kv.set({ ...req, expect: entry ? entry.version : null });
    if (result && result.refused) {
      // A refused guarded write used to leave only a line in the chat log, which
      // is not the screen the user is looking at: the edit simply vanished.
      ui.notice = entry
        ? `Guarded write to ${req.key} refused: a peer changed it since the form opened.`
        : `Guarded write to ${req.key} refused: it already exists.`;
      render();
    }
  };

  const root = h('div', { class: 'pane-body-inner' },
    h('div', { class: 'toolbar' },
      h('button', {
        class: 'btn primary',
        type: 'button',
        text: 'New key',
        onclick: () => keyDialog({
          title: 'New key',
          entry: null,
          keyEditable: true,
          guardHint: 'Guard: only write if the key does not exist',
          onSave: (req) => save(req, null),
        }),
      }),
      filterInput,
      h('span', { class: 'spacer' }),
      h('button', { class: 'btn ghost', type: 'button', text: 'Export', onclick: () => api.store.export() }),
      h('button', {
        class: 'btn ghost', type: 'button', text: 'Import', onclick: () => api.store.import(false),
      }),
      h('button', {
        class: 'btn ghost',
        type: 'button',
        text: 'Seed',
        title: 'Import, re-stamping every entry as a local write so it wins',
        onclick: () => api.store.import(true),
      })),
    noticeBar,
    rows);

  const update = () => {
    clear(noticeBar);
    if (ui.notice) {
      noticeBar.append(h('div', { class: 'notice' },
        h('span', { text: ui.notice }),
        h('span', { class: 'spacer' }),
        h('button', {
          class: 'btn tiny',
          type: 'button',
          text: 'Dismiss',
          onclick: () => {
            ui.notice = null;
            render();
          },
        })));
    }

    const needle = ui.filter.trim().toLowerCase();
    const visible = needle
      ? state.entries.filter((e) => e.key.toLowerCase().includes(needle) || e.value.toLowerCase().includes(needle))
      : state.entries;

    clear(rows);
    if (!visible.length) {
      rows.append(h('div', {
        class: 'empty',
        text: state.entries.length ? 'No key matches the filter.' : 'No keys yet.',
      }));
      return;
    }
    const now = nowSec();
    for (const entry of visible) {
      const ttl = S.ttlRemaining(entry.expiresAt, now);
      rows.append(h('div', { class: 'kv-row' },
        h('div', { class: 'kv-main' },
          h('div', { class: 'kv-key' },
            h('span', { text: entry.key }),
            ttl === null ? null : h('span', { class: 'pill ttl', text: `expires in ${S.formatUptime(ttl)}` })),
          h('div', { class: 'kv-value', text: entry.value.replace(/\n/g, ' ⏎ ') || '(empty)' }),
          h('div', { class: 'kv-meta', text: `v${entry.version.counter}` })),
        h('div', { class: 'kv-actions' },
          h('button', {
            class: 'btn tiny',
            type: 'button',
            text: 'Edit',
            onclick: () => keyDialog({
              title: `Edit ${entry.key}`,
              entry,
              keyEditable: false,
              guardHint: 'Guard: refuse if a peer changed it meanwhile',
              onSave: (req) => save(req, entry),
            }),
          }),
          h('button', {
            class: 'btn tiny', type: 'button', text: 'History', onclick: () => historyDialog(entry.key),
          }),
          h('button', {
            class: 'btn tiny danger',
            type: 'button',
            text: 'Delete',
            onclick: () => confirmDialog({
              title: `Delete ${entry.key}?`,
              body: 'The delete replicates as a versioned tombstone, so a stale write cannot resurrect it.',
              confirmLabel: 'Delete',
              danger: true,
              onConfirm: () => api.kv.remove({ key: entry.key, guard: false }),
            }),
          }))));
    }
  };

  return { root, update };
};

/** Chat: messages, emotes, direct messages and the slash commands the TUI has. */
screens.chat = () => {
  const log = h('div', { class: 'chat-log' });
  const input = h('input', {
    type: 'text',
    placeholder: 'Message, or /help for commands',
    value: ui.chatDraft,
    oninput: (e) => {
      ui.chatDraft = e.target.value;
    },
    onkeydown: (e) => {
      if (e.key === 'Enter') submitChat();
    },
  });

  async function submitChat() {
    const text = input.value.trim();
    if (!text) return;
    input.value = '';
    ui.chatDraft = '';
    await api.node.submit(text);
  }

  const root = h('div', { class: 'pane-body-inner' },
    log,
    h('div', { class: 'composer' },
      input,
      h('button', { class: 'btn primary', type: 'button', text: 'Send', onclick: submitChat })));

  const update = () => {
    // Only stick to the bottom when the reader is already there; yanking the log
    // down while someone scrolls back through history is worse than a missed line.
    const atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 40;
    clear(log);
    if (!state.chat.length) {
      log.append(h('div', { class: 'empty', text: 'Nothing said yet. Try /help.' }));
      return;
    }
    for (const entry of state.chat) {
      const own = entry.sender && entry.sender === state.status.addr;
      const who = own ? 'you' : entry.nick || entry.sender || '?';
      const kind = entry.kind || '';
      let body;
      if (kind === 'system') {
        body = h('span', { class: 'text', text: `» ${entry.text}` });
      } else if (kind === 'action') {
        body = h('span', { class: 'text', text: `* ${who} ${entry.text}` });
      } else if (kind === 'dm') {
        body = h('span', { class: 'text', text: entry.text });
      } else {
        body = h('span', { class: 'text', text: entry.text });
      }
      const line = h('div', { class: `chat-line ${kind || 'msg'}` },
        h('span', { class: 'ts', text: S.formatTime(entry.ts) }),
        kind === 'system' || kind === 'action' ? null : h('span', {
          class: 'who',
          color: own ? 'var(--ok)' : S.colorFor(entry.sender),
          text: kind === 'dm' ? (own ? `dm → ${entry.to}` : `dm ← ${who}`) : who,
        }),
        body);
      log.append(line);
    }
    if (atBottom) log.scrollTop = log.scrollHeight;
  };

  return { root, update, focus: () => input.focus() };
};

/** Peers: who is in the cluster, and the control for the node itself. */
screens.peers = () => {
  const card = h('div', { class: 'card' });
  const rows = h('div', { class: 'rows' });
  const root = h('div', { class: 'pane-body-inner' }, card, rows);

  const update = () => {
    clear(card);
    const s = state.status;
    card.append(
      h('h3', { text: 'This node' }),
      h('div', { class: 'line' }, h('span', { class: 'k', text: 'address' }),
        h('span', { class: 'v', text: s.addr || '(not bound)' })),
      h('div', { class: 'line' }, h('span', { class: 'k', text: 'identity' }),
        h('span', { class: 'v', text: `${s.nick} · ${(s.nodeId || '').slice(0, 8)} · cluster ${s.cluster}` })),
      h('div', { class: 'line' }, h('span', { class: 'k', text: 'seeds' }),
        h('span', { class: 'v', text: (state.settings && state.settings.seeds) || '(none)' })),
      h('div', { class: 'btn-row' }));

    clear(rows);
    if (!state.peers.length) {
      rows.append(h('div', {
        class: 'empty',
        text: s.running
          ? 'No peers yet. Check that a bootstrap seed is reachable, or that another node is on this network.'
          : 'The node is stopped.',
      }));
      return;
    }
    const now = Date.now();
    for (const peer of state.peers) {
      rows.append(h('div', { class: 'peer-row' },
        h('div', { class: 'grow' },
          h('div', { class: 'peer-name', color: S.colorFor(peer.addr), text: peer.nick || peer.addr }),
          h('div', { class: 'peer-addr', text: peer.addr })),
        h('span', {
          class: 'peer-age',
          text: peer.lastSeenMs ? `seen ${S.formatUptime((now - peer.lastSeenMs) / 1000)} ago` : 'never heard from',
        }),
        h('button', {
          class: 'btn tiny', type: 'button', text: 'Sync', title: 'Pull this peer’s store',
          onclick: () => api.peer.sync(peer.addr),
        }),
        h('button', {
          class: 'btn tiny',
          type: 'button',
          text: 'Gossip',
          title: 'Send this peer our membership view and a digest now',
          onclick: () => api.peer.gossip(peer.addr),
        }),
        h('button', {
          class: 'btn tiny danger',
          type: 'button',
          text: 'Forget',
          title: 'Drop the link on purpose and watch the cluster heal',
          onclick: () => api.peer.forget(peer.addr),
        })));
    }
  };

  return { root, update };
};

/** Graph: the cluster drawn from the peer lists gossip already carries. */
screens.graph = () => {
  const summary = h('div', { class: 'summary' });
  const svg = document.createElementNS(G.SVG_NS, 'svg');
  svg.setAttribute('class', 'graph');
  const detail = h('div', {});
  const wrap = h('div', { class: 'graph-wrap' }, svg,
    h('div', { class: 'legend' },
      h('span', {}, h('i', { class: 'sw sw-self' }), 'this node'),
      h('span', {}, h('i', { class: 'sw sw-peer' }), 'peer'),
      h('span', {}, h('i', { class: 'sw sw-indirect' }), 'heard of only'),
      h('span', {}, 'dashed link = one end only')),
    detail);
  const root = h('div', { class: 'pane-body-inner' },
    summary,
    wrap,
    h('div', { class: 'toolbar' },
      h('button', {
        class: 'btn tiny',
        type: 'button',
        text: 'Reset view',
        onclick: () => {
          ui.graph.scale = 1;
          ui.graph.panX = 0;
          ui.graph.panY = 0;
          update();
        },
      }),
      h('span', { class: 'chip', text: 'scroll to zoom · drag to pan · click a node for detail' })));

  const size = () => ({
    width: Math.max(200, wrap.clientWidth || 600),
    height: Math.max(160, wrap.clientHeight || 400),
  });

  svg.addEventListener('wheel', (e) => {
    e.preventDefault();
    const factor = e.deltaY < 0 ? 1.12 : 1 / 1.12;
    ui.graph.scale = Math.min(4, Math.max(0.4, ui.graph.scale * factor));
    update();
  }, { passive: false });

  let dragging = null;
  svg.addEventListener('mousedown', (e) => {
    dragging = { x: e.clientX, y: e.clientY, panX: ui.graph.panX, panY: ui.graph.panY, moved: false };
    svg.classList.add('dragging');
  });
  window.addEventListener('mousemove', (e) => {
    if (!dragging) return;
    const dx = e.clientX - dragging.x;
    const dy = e.clientY - dragging.y;
    if (Math.abs(dx) + Math.abs(dy) > 3) dragging.moved = true;
    ui.graph.panX = dragging.panX + dx;
    ui.graph.panY = dragging.panY + dy;
    update();
  });
  window.addEventListener('mouseup', () => {
    if (dragging) svg.classList.remove('dragging');
    dragging = null;
  });
  svg.addEventListener('click', (e) => {
    // A click that ended a drag is not a selection.
    if (dragging && dragging.moved) return;
    const rect = svg.getBoundingClientRect();
    const point = { x: e.clientX - rect.left, y: e.clientY - rect.top };
    const node = G.nodeAt(point, state.topology.nodes, size(), ui.graph);
    ui.graph.selected = node ? node.addr : null;
    update();
  });

  const update = () => {
    const topology = state.topology;
    const direct = topology.nodes.filter((n) => n.role === 'direct').length;
    const indirect = topology.nodes.filter((n) => n.role === 'indirect').length;
    const groups = componentsOf(topology);

    clear(summary);
    add(summary,
      h('span', { text: `${topology.nodes.length} nodes` }),
      h('span', { text: `${topology.links.length} links` }),
      h('span', { text: `${direct} direct` }),
      indirect ? h('span', { text: `${indirect} via gossip` }) : null,
      groups.length > 1
        ? h('span', { class: 'bad', text: `partitioned: ${groups.length} groups that cannot reach each other` })
        : null,
      topology.nodes.length <= 1
        ? h('span', { text: 'this node alone — peers appear here as they are learned' })
        : null);

    G.draw(svg, topology, {
      size: size(),
      view: ui.graph,
      selected: ui.graph.selected,
      nowMs: Date.now(),
      colorFor: (seed) => (seed === 'self-node' ? '#00b37e' : S.colorFor(seed)),
      isStale: S.isStale,
    });

    clear(detail);
    const selected = topology.nodes.find((n) => n.addr === ui.graph.selected);
    if (!selected) return;
    const neighbours = topology.links
      .map((l) => (l.a === selected.addr ? l.b : l.b === selected.addr ? l.a : null))
      .filter(Boolean);
    const roleText = selected.role === 'self'
      ? 'This machine.'
      : selected.role === 'direct'
        ? 'A peer of this node.'
        : 'Known only from gossip: no packets exchanged with it.';
    detail.append(h('div', { class: 'graph-detail' },
      h('div', { class: 'detail-title', color: S.colorFor(selected.addr), text: selected.label }),
      h('div', { class: 'peer-addr', text: selected.addr }),
      h('div', { class: 'detail-note', text: roleText }),
      h('div', { class: 'kv-meta mt4', text: `links ${selected.degree}`
        + (selected.advertised >= 0 ? ` · advertises ${selected.advertised} peers` : '')
        + (selected.lastSeenMs
          ? ` · heard ${S.formatUptime((Date.now() - selected.lastSeenMs) / 1000)} ago`
          : '') }),
      neighbours.length
        ? h('div', { class: 'kv-meta mt4', text: `talks to ${neighbours.join(', ')}` })
        : null,
      h('div', { class: 'btn-row' },
        selected.role === 'direct' ? h('button', {
          class: 'btn tiny', type: 'button', text: 'Sync from', onclick: () => api.peer.sync(selected.addr),
        }) : null,
        selected.role === 'direct' ? h('button', {
          class: 'btn tiny danger', type: 'button', text: 'Forget', onclick: () => api.peer.forget(selected.addr),
        }) : null,
        selected.role === 'indirect' ? h('button', {
          class: 'btn tiny', type: 'button', text: 'Add as peer', onclick: () => api.peer.add(selected.addr),
        }) : null,
        h('button', {
          class: 'btn tiny ghost',
          type: 'button',
          text: 'Close',
          onclick: () => {
            ui.graph.selected = null;
            update();
          },
        }))));
  };

  return { root, update };
};

/** The connected groups of the graph, so a split cluster is visible rather than implied. */
function componentsOf(topology) {
  const remaining = new Set(topology.nodes.map((n) => n.addr));
  const adjacency = new Map();
  for (const link of topology.links) {
    if (!adjacency.has(link.a)) adjacency.set(link.a, new Set());
    if (!adjacency.has(link.b)) adjacency.set(link.b, new Set());
    adjacency.get(link.a).add(link.b);
    adjacency.get(link.b).add(link.a);
  }
  const groups = [];
  while (remaining.size) {
    const seed = remaining.values().next().value;
    remaining.delete(seed);
    const group = [];
    const queue = [seed];
    while (queue.length) {
      const current = queue.shift();
      group.push(current);
      for (const next of adjacency.get(current) || []) if (remaining.delete(next)) queue.push(next);
    }
    groups.push(group);
  }
  return groups;
}

/** Activity: replication as it happens, alongside the counters. */
screens.activity = () => {
  const card = h('div', { class: 'card' });
  const log = h('div', { class: 'log' });
  const root = h('div', { class: 'pane-body-inner' }, card,
    h('div', { class: 'toolbar' }, h('span', { class: 'chip', text: 'newest first' })), log);

  const metric = (label, value) => h('div', { class: 'metric-row' },
    h('span', { class: 'k', text: label }), h('span', { class: 'v', text: String(value) }));

  const update = () => {
    const m = state.metrics;
    clear(card);
    if (m) {
      card.append(
        h('h3', { text: 'Counters' }),
        h('div', { class: 'grid2' },
          h('div', {},
            h('div', { class: 'kv-meta', text: 'REPLICATION' }),
            metric('local writes', m.kvLocalWrites),
            metric('remote applied', m.kvApplied),
            metric('stale rejected', m.kvRejectedStale),
            metric('guarded writes refused', m.kvCasFailures),
            metric('expired', m.kvExpired),
            metric('tombstones reclaimed', m.kvGced)),
          h('div', {},
            h('div', { class: 'kv-meta', text: 'ANTI-ENTROPY' }),
            metric('digests sent', m.aeRounds),
            metric('entries pushed', m.aePushed),
            metric('entries pulled', m.aePulled),
            metric('snapshots served', m.stateSyncOut),
            metric('snapshots received', m.stateSyncIn)),
          h('div', {},
            h('div', { class: 'kv-meta', text: 'TRAFFIC' }),
            metric('packets sent', m.packetsSent),
            metric('packets received', m.packetsReceived),
            metric('bytes', `${S.formatBytes(m.bytesSent)} / ${S.formatBytes(m.bytesReceived)}`),
            metric('send errors', m.sendErrors),
            metric('stream errors', m.streamErrors)),
          h('div', {},
            h('div', { class: 'kv-meta', text: 'DROPPED' }),
            metric('failed authentication', m.authFailures),
            metric('replays', m.replayDrops),
            metric('clock skew', m.skewDrops),
            metric('malformed', m.malformedDrops))));
    }

    clear(log);
    if (!state.activity.length) {
      log.append(h('div', { class: 'empty', text: 'Nothing replicated yet.' }));
      return;
    }
    for (const line of [...state.activity].reverse()) log.append(h('div', { class: 'log-line', text: line }));
  };

  return { root, update };
};

/** Diagnostics: the whole node in one screen, starting with what is wrong. */
screens.diag = () => {
  const body = h('div', {});
  const root = h('div', { class: 'pane-body-inner' },
    h('div', { class: 'toolbar' },
      h('button', {
        class: 'btn', type: 'button', text: 'Copy report', onclick: () => api.diag.copyReport(),
      }),
      h('button', { class: 'btn ghost', type: 'button', text: 'Refresh', onclick: () => refresh() }),
      h('span', { class: 'chip', text: 'refreshed every 2s' })),
    body);

  const line = (k, v) => h('div', { class: 'line' },
    h('span', { class: 'k', text: k }), h('span', { class: 'v', text: v }));

  const section = (title, ...rows) => h('div', { class: 'card' }, h('h3', { text: title }), ...rows.flat());

  let timer = null;

  async function refresh() {
    const result = await api.diag.snapshot();
    state.diag = result;
    paint();
  }

  function paint() {
    clear(body);
    if (!state.diag) {
      body.append(h('div', { class: 'empty', text: 'Collecting…' }));
      return;
    }
    const { diagnostics: d, checks, bootstrap } = state.diag;
    const now = Date.now();

    body.append(section('Health', checks.map((c) => h('div', { class: `check ${c.severity}` },
      h('div', { class: 'title', text: `${symbolFor(c.severity)} ${c.title}` }),
      h('div', { class: 'detail', text: c.detail })))));

    body.append(section('Identity',
      line('address', d.addr),
      line('nickname', d.nick),
      line('node id', d.nodeId),
      line('cluster', d.cluster),
      line('key fingerprint', `${d.keyFingerprint}${d.pskSet ? ' (psk set)' : ' (no psk)'}`),
      h('div', { class: 'kv-meta mt6', text:
        'Two nodes talk only when this fingerprint matches. Compare it with another machine before '
        + 'suspecting the network.' })));

    body.append(section('Sockets',
      line('state', d.running ? `running, up ${S.formatUptime(d.uptimeSec)}` : 'stopped'),
      line('port', `configured ${d.configuredPort}, bound ${d.boundPort}`),
      line('stream listener', d.streamListener ? 'yes' : 'no'),
      line('advertising', d.advertiseHost || `${d.localIpv4} (auto)`),
      line('seeds', d.seeds.join(', ') || '(none)'),
      line('intervals', `gossip ${d.gossipIntervalMs / 1000}s · heartbeat ${d.heartbeatIntervalMs / 1000}s · `
        + `evict ${d.evictThresholdMs / 1000}s · sweep ${d.sweepIntervalMs / 1000}s`),
      line('bootstrap role', bootstrap.running
        ? `listening on ${bootstrap.port}, ${bootstrap.nodes} registered`
        : 'stopped'),
      line('state file', d.dataFile || '(none)')));

    const m = d.metrics;
    body.append(section('Traffic',
      line('packets', `${m.packetsSent} sent · ${m.packetsReceived} received`),
      line('bytes', `${S.formatBytes(m.bytesSent)} sent · ${S.formatBytes(m.bytesReceived)} received`),
      line('rate', `${m.rates.packetsSent.toFixed(1)}/s out · ${m.rates.packetsReceived.toFixed(1)}/s in`),
      line('errors', `${m.sendErrors} send · ${m.streamErrors} stream`),
      line('dropped', `${m.authFailures} auth · ${m.replayDrops} replay · ${m.skewDrops} skew · `
        + `${m.malformedDrops} malformed`),
      line('anti-entropy', `${m.aeRounds} rounds · ${m.aePushed} pushed · ${m.aePulled} pulled`),
      line('state sync', `${m.stateSyncOut} served · ${m.stateSyncIn} received`),
      m.kinds.map((k) => line(k.kind, `${k.sent} sent · ${k.received} received`))));

    body.append(section('Store',
      line('keys', String(d.store.keys)),
      line('tombstones', `${d.store.tombstones}${d.tombstoneTtlSec > 0
        ? `, GC after ${d.tombstoneTtlSec}s` : ', kept forever'}`),
      line('value bytes', S.formatBytes(d.store.valueBytes)),
      line('lamport clock', String(d.store.clock)),
      line('keys with history', String(d.store.historyKeys)),
      d.store.largestKey
        ? line('largest value', `${d.store.largestKey} (${S.formatBytes(d.store.largestValueBytes)})`)
        : null,
      line('digest cursor', d.store.digestCursor || '(start of keyspace)'),
      line('buffers', `${d.chatLines} chat lines · ${d.activityLines} activity lines`)));

    body.append(section(`Peers (${d.peers.length})`,
      d.peers.length ? d.peers.map((p) => h('div', { class: 'check' },
        h('div', { class: 'title', color: S.colorFor(p.addr), text: p.nick || p.addr }),
        h('div', { class: 'kv-meta', text: `${p.addr} · in ${p.packetsIn} pkt / ${S.formatBytes(p.bytesIn)}`
          + ` · out ${p.packetsOut} pkt / ${S.formatBytes(p.bytesOut)}`
          + (p.rejected ? ` · ${p.rejected} rejected` : '')
          + (p.sendErrors ? ` · ${p.sendErrors} send errors` : '') }),
        h('div', { class: 'kv-meta', text:
          (p.lastSeenMs ? `seen ${S.formatUptime((now - p.lastSeenMs) / 1000)} ago` : 'never heard from')
          + (p.advertisedPeers >= 0 ? ` · advertises ${p.advertisedPeers} peers` : ' · never gossiped') })))
        : h('div', { class: 'kv-meta', text: 'No peers. Nothing replicates until one is learned.' })));

    if (d.strangers.length) {
      body.append(section(`Unknown sources (${d.strangers.length})`,
        h('div', { class: 'kv-meta', text:
          'These addresses sent packets without being peers — the sign of a node whose advertised address '
          + 'is not the one it sends from.' }),
        d.strangers.map((p) => line(p.addr, `in ${p.packetsIn} pkt / ${S.formatBytes(p.bytesIn)}`
          + ` · rejected ${p.rejected}`))));
    }

    const groups = componentsOf(d.topology);
    body.append(section('Topology',
      line('nodes', String(d.topology.nodes.length)),
      line('links', String(d.topology.links.length)),
      line('groups', String(groups.length)),
      groups.map((g, i) => line(`group ${i + 1}`, `${g.length}: ${g.join(', ')}`)),
      d.topology.links.filter((l) => l.kind === 'observed').map((l) => line('unconfirmed', `${l.a} → ${l.b}`))));
  }

  return {
    root,
    update: paint,
    mounted: () => {
      refresh();
      timer = setInterval(refresh, 2000);
    },
    unmounted: () => clearInterval(timer),
  };
};

function symbolFor(severity) {
  if (severity === 'ok') return '✓';
  if (severity === 'warn') return '!';
  if (severity === 'error') return '✗';
  return '·';
}

/** Bootstrap: the rendezvous role, so this machine can be the meeting point. */
screens.bootstrap = () => {
  const card = h('div', { class: 'card' });
  const rows = h('div', { class: 'rows' });
  const root = h('div', { class: 'pane-body-inner' }, card, rows);

  const update = () => {
    const s = state.bootstrapStatus;
    clear(card);
    card.append(
      h('h3', { text: 'Rendezvous service' }),
      h('div', { class: 'line' }, h('span', { class: 'k', text: 'state' }),
        h('span', {
          class: 'v',
          text: s.running
            ? `listening on udp+tcp :${s.boundPort || s.port} · ${s.nodes} nodes · up ${S.formatUptime(s.uptimeSec)}`
            : `stopped · port ${s.port}`,
        })),
      h('div', { class: 'line' }, h('span', { class: 'k', text: 'cluster' }),
        h('span', { class: 'v', text: s.cluster || (state.settings ? state.settings.cluster : '') })),
      h('div', { class: 'kv-meta mt6', text:
        'It knows the set of node addresses and nothing else: never KV data, never chat. Point other nodes '
        + `at this machine's address on port ${s.port}.` }),
      h('div', { class: 'btn-row' },
        s.running
          ? h('button', { class: 'btn', type: 'button', text: 'Stop', onclick: () => api.bootstrap.stop() })
          : h('button', {
            class: 'btn primary', type: 'button', text: 'Start', onclick: () => api.bootstrap.start(),
          })));

    clear(rows);
    if (!state.bootstrapRoster.length) {
      rows.append(h('div', { class: 'empty', text: 'No nodes registered.' }));
      return;
    }
    const now = nowSec();
    for (const entry of state.bootstrapRoster) {
      rows.append(h('div', { class: 'peer-row' },
        h('div', { class: 'grow' },
          h('div', { class: 'peer-name', text: entry.nick || entry.addr }),
          h('div', { class: 'peer-addr', text: entry.addr })),
        // A frozen "seen 0s ago" under a stopped service reads as a live roster:
        // nothing is checking in, so nothing is ageing either.
        h('span', {
          class: 'peer-age',
          text: s.running ? `seen ${S.formatUptime(now - entry.lastSeen)} ago` : 'registered',
        })));
    }
  };

  return { root, update };
};

/** Settings: everything the two roles are configured with. */
screens.settings = () => {
  const s = state.settings || {};
  const fields = {};
  const field = (name, label, opts = {}) => {
    const input = h('input', {
      type: opts.type || 'text',
      value: s[name] === undefined ? '' : String(s[name]),
    });
    fields[name] = input;
    return h('label', { class: 'field' }, h('span', { text: label }), input);
  };
  const check = (name, label) => {
    const input = h('input', { type: 'checkbox', checked: !!s[name] });
    fields[name] = input;
    return h('label', { class: 'check mb10' }, input, label);
  };

  const form = h('div', { class: 'card form' },
    h('h3', { text: 'Node' }),
    field('nick', 'Nickname'),
    field('port', 'Node port', { type: 'number' }),
    field('advertiseHost', 'Advertise host (blank = this machine’s LAN address)'),
    field('seeds', 'Bootstrap seeds, comma separated'),
    field('cluster', 'Cluster name'),
    field('psk', 'Pre-shared key', { type: 'password' }),
    field('tombstoneTtlSec', 'Tombstone GC after (seconds, 0 = never)', { type: 'number' }),
    h('h3', { class: 'h3-gap', text: 'Rendezvous' }),
    field('bootstrapPort', 'Bootstrap port', { type: 'number' }),
    h('h3', { class: 'h3-gap', text: 'On launch' }),
    check('autoStartNode', 'Start the node automatically'),
    check('autoStartBootstrap', 'Start the rendezvous service automatically'),
    h('div', { class: 'kv-meta my8', text:
      'Every packet is authenticated. Without a pre-shared key the framing key comes from the cluster name '
      + 'alone, which keeps two clusters on one network apart but provides no secrecy. Nodes only talk to '
      + 'peers with the same key and cluster; spaces around either are trimmed, since a pasted key that '
      + 'brings one along would otherwise cut this node off with nothing on screen to show it.' }),
    h('div', { class: 'btn-row' },
      h('button', {
        class: 'btn primary',
        type: 'button',
        text: 'Apply',
        onclick: async () => {
          const partial = {
            nick: fields.nick.value,
            port: Number(fields.port.value),
            advertiseHost: fields.advertiseHost.value,
            seeds: fields.seeds.value,
            cluster: fields.cluster.value,
            psk: fields.psk.value,
            tombstoneTtlSec: Number(fields.tombstoneTtlSec.value),
            bootstrapPort: Number(fields.bootstrapPort.value),
            autoStartNode: fields.autoStartNode.checked,
            autoStartBootstrap: fields.autoStartBootstrap.checked,
          };
          state.settings = await api.app.applySettings(partial);
          toast('ok', 'Settings applied');
          render();
        },
      })),
    h('div', { class: 'kv-meta mt8', text:
      'Changing the port, cluster or key restarts whichever role is running: those are baked into the '
      + 'socket and the codec.' }));

  // The form is deliberately not repainted on engine events: it would take the
  // cursor out of a half-typed key twice a second.
  return { root: form, update: () => {} };
};

// ---- shell ------------------------------------------------------------------

const mounted = new Map();

function paneFor(id) {
  return document.getElementById(id);
}

function mountScreen(paneId, name) {
  const pane = paneFor(paneId);
  if (!pane) return;
  const current = mounted.get(paneId);
  if (current && current.name === name) {
    current.screen.update();
    return;
  }
  if (current && current.screen.unmounted) current.screen.unmounted();
  const screen = screens[name]();
  const body = pane.querySelector('.pane-body');
  clear(body);
  body.append(screen.root);
  pane.dataset.screen = name;
  const caption = pane.querySelector('.caption-title');
  if (caption) caption.textContent = (S.SCREENS.find((s) => s.id === name) || {}).title || name;
  mounted.set(paneId, { name, screen });
  screen.update();
  if (screen.mounted) screen.mounted();
}

function buildRail() {
  const rail = document.getElementById('rail');
  clear(rail);
  for (const entry of S.SCREENS) {
    rail.append(h('button', {
      class: 'rail-item',
      type: 'button',
      'data-screen': entry.id,
      onclick: () => show(entry.id),
    }, h('span', { class: 'glyph', text: entry.icon }), h('span', { text: entry.title })));
  }
}

function ensureSidePane() {
  const panes = document.getElementById('panes');
  let side = document.getElementById('pane-side');
  if (ui.split && ui.splitAvailable) {
    if (!side) {
      side = h('section', { class: 'pane', id: 'pane-side' },
        h('div', { class: 'pane-caption' },
          h('span', { class: 'caption-title' }),
          h('button', {
            class: 'btn tiny ghost',
            type: 'button',
            title: 'Show this screen in the main pane',
            text: '←',
            onclick: () => show(companion()),
          }),
          h('button', {
            class: 'btn tiny ghost',
            type: 'button',
            title: 'Close the second pane',
            text: '✕',
            onclick: () => setSplit(false),
          })),
        h('div', { class: 'pane-body' }));
      panes.append(side);
    }
  } else if (side) {
    const current = mounted.get('pane-side');
    if (current && current.screen.unmounted) current.screen.unmounted();
    mounted.delete('pane-side');
    side.remove();
  }
  document.body.classList.toggle('split', !!(ui.split && ui.splitAvailable));
}

const companion = () => S.companionOf(ui.screen);

function show(name) {
  ui.screen = name;
  render();
  const current = mounted.get('pane-main');
  if (current && current.screen.focus) current.screen.focus();
}

function setSplit(on) {
  ui.split = on;
  render();
}

function renderTopbar() {
  const s = state.status;
  const { label, tone } = S.statusLabel(s);
  const dot = document.getElementById('status-dot');
  dot.className = `dot ${tone}`;
  const labelEl = document.getElementById('status-label');
  labelEl.textContent = label;
  labelEl.className = `status-label ${tone}`;
  document.getElementById('status-addr').textContent = s.addr || '';

  const chips = document.getElementById('status-chips');
  clear(chips);
  const chip = (name, value, alert) => h('span', { class: `chip${alert ? ' alert' : ''}` },
    name, ' ', h('b', { text: String(value) }));
  // Filtered, not passed straight in: Element.append turns a null into the text
  // "null", so an absent chip would print the word.
  chips.append(...[
    chip('peers', s.peers, s.running && s.peers === 0),
    chip('keys', s.keys),
    s.tombstones ? chip('tombs', s.tombstones) : null,
    chip('up', S.formatUptime(s.uptimeSec)),
    state.bootstrapStatus.running ? chip('rendezvous', `${state.bootstrapStatus.nodes} nodes`) : null,
  ].filter(Boolean));

  const nodeBtn = document.getElementById('btn-node');
  nodeBtn.textContent = s.running ? 'Stop node' : 'Start node';
  nodeBtn.className = s.running ? 'btn' : 'btn primary';

  const splitBtn = document.getElementById('btn-split');
  splitBtn.disabled = !ui.splitAvailable;
  splitBtn.className = `btn ghost${ui.split && ui.splitAvailable ? ' on' : ''}`;
  splitBtn.title = ui.splitAvailable
    ? 'Show a second screen beside this one'
    : 'The window is too narrow for two panes';
}

function render() {
  ui.splitAvailable = S.splitFits(window.innerWidth, window.innerHeight);
  renderTopbar();
  ensureSidePane();
  mountScreen('pane-main', ui.screen);
  if (ui.split && ui.splitAvailable) mountScreen('pane-side', companion());
  for (const item of document.querySelectorAll('.rail-item')) {
    const name = item.dataset.screen;
    item.classList.toggle('active', name === ui.screen);
    item.classList.toggle('companion', ui.split && ui.splitAvailable && name === companion());
  }
}

/** Repaints only the screens that care about the change, so typing survives an event. */
function updateScreens(...names) {
  for (const [, entry] of mounted) {
    if (names.length === 0 || names.includes(entry.name)) entry.screen.update();
  }
}

async function boot() {
  buildRail();

  document.getElementById('btn-node').addEventListener('click', async () => {
    if (state.status.running) await api.node.stop();
    else await api.node.start();
  });
  document.getElementById('btn-split').addEventListener('click', () => setSplit(!ui.split));

  window.addEventListener('resize', () => {
    const available = S.splitFits(window.innerWidth, window.innerHeight);
    const changed = available !== ui.splitAvailable;
    ui.splitAvailable = available;
    if (changed) render();
    else updateScreens('graph');
  });

  api.events.onStatus((status) => {
    state.status = status;
    renderTopbar();
    updateScreens('peers', 'diag');
  });
  api.events.onEntries((entries) => {
    state.entries = entries;
    updateScreens('keys');
  });
  api.events.onPeers((peers) => {
    state.peers = peers;
    updateScreens('peers');
  });
  api.events.onChat((chat) => {
    state.chat = chat;
    updateScreens('chat');
  });
  api.events.onActivity((activity) => {
    state.activity = activity;
    updateScreens('activity');
  });
  api.events.onMetrics((metrics) => {
    state.metrics = metrics;
    updateScreens('activity');
  });
  api.events.onTopology((topology) => {
    state.topology = topology;
    updateScreens('graph');
  });
  api.events.onBootstrapRoster((roster) => {
    state.bootstrapRoster = roster;
    updateScreens('bootstrap');
  });
  api.events.onBootstrapStatus((status) => {
    state.bootstrapStatus = status;
    renderTopbar();
    updateScreens('bootstrap');
  });
  api.events.onToast(({ kind, text }) => toast(kind, text));

  const snapshot = await api.app.snapshot();
  Object.assign(state, snapshot);
  ui.split = S.splitFits(window.innerWidth, window.innerHeight);
  render();

  // Ages are only true for an instant; without a tick the peer list shows how
  // stale a peer was when the screen opened.
  setInterval(() => {
    renderTopbar();
    updateScreens('peers', 'keys', 'bootstrap', 'graph');
  }, 1000);
}

// Test hooks for `electron . --selftest`: driving the real UI is the only way to
// catch a preload the sandbox broke or a screen that throws before its first paint.
window.__test = {
  show,
  setSplit,
  state,
  ui,
  submit: (text) => api.node.submit(text),
};

boot().catch((e) => {
  window.__errors.push(String(e && e.message));
  document.body.append(h('div', { class: 'empty', text: `Failed to start: ${e.message}` }));
});
