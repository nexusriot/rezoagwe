/**
 * Presentation helpers shared by the renderer and its tests.
 *
 * Written as a UMD-ish module so the same file is a <script> in the window and a
 * require() in `node --test`: these are the rules a screenshot cannot check —
 * which screen pairs with which, when a second pane fits, how an age reads.
 */
(function attach(root, factory) {
  const api = factory();
  if (typeof module === 'object' && module.exports) module.exports = api;
  else root.Shared = api;
}(typeof globalThis !== 'undefined' ? globalThis : this, function build() {
  /** The screens, in the order they appear in the rail. */
  const SCREENS = [
    { id: 'keys', title: 'Keys', icon: 'K' },
    { id: 'chat', title: 'Chat', icon: 'C' },
    { id: 'peers', title: 'Peers', icon: 'P' },
    { id: 'graph', title: 'Graph', icon: 'G' },
    { id: 'activity', title: 'Activity', icon: 'A' },
    { id: 'diag', title: 'Diag', icon: 'D' },
    { id: 'bootstrap', title: 'Bootstrap', icon: 'B' },
    { id: 'settings', title: 'Settings', icon: 'S' },
  ];

  /**
   * What to show in the second pane next to the given screen.
   *
   * The pairings are chosen so the side pane answers the question the main one
   * raises — peers while chatting, the graph while looking at peers — and never
   * shows the same screen twice.
   */
  function companionOf(main) {
    switch (main) {
      case 'keys': return 'activity';
      case 'chat': return 'peers';
      case 'peers': return 'graph';
      case 'graph': return 'diag';
      case 'activity': return 'graph';
      case 'diag': return 'graph';
      case 'bootstrap': return 'peers';
      case 'settings': return 'diag';
      default: return 'graph';
    }
  }

  /** Two panes need enough width that neither ends up narrower than a usable list. */
  const TWO_PANE_MIN_WIDTH = 1100;
  /** A short window has no height to spare for two stacked headers. */
  const SHORT_HEIGHT = 520;

  /** Whether a second pane fits: width alone is not enough, a short window has no room either. */
  function splitFits(width, height) {
    return width >= TWO_PANE_MIN_WIDTH && height > SHORT_HEIGHT;
  }

  function formatUptime(seconds) {
    const s = Math.max(0, Math.floor(seconds));
    const h = Math.floor(s / 3600);
    const m = Math.floor((s % 3600) / 60);
    const sec = s % 60;
    if (h > 0) return `${h}h${String(m).padStart(2, '0')}m${String(sec).padStart(2, '0')}s`;
    if (m > 0) return `${m}m${String(sec).padStart(2, '0')}s`;
    return `${sec}s`;
  }

  function formatBytes(bytes) {
    const b = Number(bytes) || 0;
    if (b < 1024) return `${b}B`;
    if (b < 1024 * 1024) return `${(b / 1024).toFixed(1)}kB`;
    return `${(b / (1024 * 1024)).toFixed(1)}MB`;
  }

  function formatTime(unixSeconds) {
    if (!unixSeconds) return '--:--:--';
    return new Date(unixSeconds * 1000).toTimeString().slice(0, 8);
  }

  /** Colours a sender consistently, so the same peer keeps its tint across screens. */
  const PALETTE = [
    '#e57373', '#81c784', '#64b5f6', '#ba68c8',
    '#4dd0e1', '#ffb74d', '#aed581', '#fff176',
    '#4fc3f7', '#9575cd', '#f06292', '#4db6ac',
  ];

  function colorFor(seed) {
    if (!seed) return '#8a97a3';
    let h = 0;
    for (let i = 0; i < seed.length; i++) h = (h * 131 + seed.charCodeAt(i)) | 0;
    return PALETTE[((h % PALETTE.length) + PALETTE.length) % PALETTE.length];
  }

  /** How a node's connection state reads in the header. */
  function statusLabel(status) {
    if (!status || !status.running) return { label: 'STOPPED', tone: 'muted' };
    if (status.peers > 0) return { label: 'CONNECTED', tone: 'ok' };
    return { label: 'DEGRADED', tone: 'bad' };
  }

  /** Seconds left before an entry expires, or null when it never does. */
  function ttlRemaining(expiresAt, nowSec) {
    if (!expiresAt) return null;
    return Math.max(0, expiresAt - nowSec);
  }

  /** Below this a graph node is drawn as fading: nothing has been heard from it lately. */
  const STALE_AFTER_MS = 30_000;

  function isStale(lastSeenMs, nowMs) {
    return lastSeenMs > 0 && nowMs - lastSeenMs > STALE_AFTER_MS;
  }

  return {
    SCREENS,
    companionOf,
    splitFits,
    TWO_PANE_MIN_WIDTH,
    SHORT_HEIGHT,
    formatUptime,
    formatBytes,
    formatTime,
    colorFor,
    statusLabel,
    ttlRemaining,
    isStale,
    STALE_AFTER_MS,
    PALETTE,
  };
}));
