/**
 * The cluster as a picture.
 *
 * A peer list says who this node talks to; it cannot show that two of those
 * peers do not talk to each other, or that the cluster has quietly split in two.
 * Both are visible here, drawn from the peer lists gossip already carries.
 *
 * The layout arrives with the topology (computed once, in the engine); this file
 * owns only the projection onto the canvas, the hit testing and the SVG.
 */
(function attach(root, factory) {
  const api = factory();
  if (typeof module === 'object' && module.exports) module.exports = api;
  else root.Graph = api;
}(typeof globalThis !== 'undefined' ? globalThis : this, function build() {
  const SVG_NS = 'http://www.w3.org/2000/svg';

  /** Margin so a label under the outermost node is not clipped by the viewport. */
  const EDGE_MARGIN = 46;

  /** Screen position of a normalised point, honouring the current zoom and pan. */
  function project(point, size, view) {
    const radius = Math.max(1, Math.min(size.width, size.height) / 2 - EDGE_MARGIN);
    return {
      x: size.width / 2 + point.x * radius * view.scale + view.panX,
      y: size.height / 2 + point.y * radius * view.scale + view.panY,
    };
  }

  /** The node under a click, or null. A generous radius: a pointer is coarser than a dot. */
  function nodeAt(point, nodes, size, view, tolerance = 26) {
    let best = null;
    let bestDistance = Infinity;
    for (const node of nodes) {
      const at = project(node, size, view);
      const distance = Math.hypot(at.x - point.x, at.y - point.y);
      if (distance <= tolerance && distance < bestDistance) {
        best = node;
        bestDistance = distance;
      }
    }
    return best;
  }

  function radiusFor(role) {
    if (role === 'self') return 13;
    if (role === 'direct') return 10;
    return 7;
  }

  function el(name, attrs) {
    const node = document.createElementNS(SVG_NS, name);
    for (const [k, v] of Object.entries(attrs || {})) node.setAttribute(k, String(v));
    return node;
  }

  /**
   * Draws the topology into an existing <svg>.
   *
   * Links are drawn first so a node always sits on top of its own edges, and an
   * unconfirmed link is dashed: gossip carries each node's own peer list, so a
   * link only one end has reported is a claim, not a fact.
   */
  function draw(svg, topology, opts) {
    const { size, view, selected, nowMs, colorFor, isStale } = opts;
    while (svg.firstChild) svg.removeChild(svg.firstChild);
    svg.setAttribute('viewBox', `0 0 ${size.width} ${size.height}`);

    const byAddr = new Map(topology.nodes.map((n) => [n.addr, n]));

    const linkLayer = el('g', { class: 'graph-links' });
    for (const link of topology.links) {
      const a = byAddr.get(link.a);
      const b = byAddr.get(link.b);
      if (!a || !b) continue;
      const from = project(a, size, view);
      const to = project(b, size, view);
      const highlighted = selected && (link.a === selected || link.b === selected);
      linkLayer.appendChild(el('line', {
        class: `graph-link link-${link.kind}${highlighted ? ' link-selected' : ''}`,
        x1: from.x, y1: from.y, x2: to.x, y2: to.y,
      }));
    }
    svg.appendChild(linkLayer);

    const nodeLayer = el('g', { class: 'graph-nodes' });
    for (const node of topology.nodes) {
      const at = project(node, size, view);
      const radius = radiusFor(node.role);
      const stale = isStale(node.lastSeenMs, nowMs);
      const group = el('g', {
        class: `graph-node node-${node.role}${stale ? ' node-stale' : ''}`
          + `${node.addr === selected ? ' node-selected' : ''}`,
        'data-addr': node.addr,
      });
      if (node.addr === selected) {
        group.appendChild(el('circle', { class: 'node-halo', cx: at.x, cy: at.y, r: radius + 6 }));
      }
      const fill = node.role === 'indirect' ? 'none' : colorFor(node.role === 'self' ? 'self-node' : node.addr);
      group.appendChild(el('circle', {
        class: 'node-dot', cx: at.x, cy: at.y, r: radius, fill,
      }));
      const label = el('text', { class: 'node-label', x: at.x, y: at.y + radius + 14 });
      label.textContent = node.label.length > 22 ? `${node.label.slice(0, 21)}…` : node.label;
      group.appendChild(label);
      nodeLayer.appendChild(group);
    }
    svg.appendChild(nodeLayer);
    return { nodes: topology.nodes.length, links: topology.links.length };
  }

  return { project, nodeAt, radiusFor, draw, EDGE_MARGIN, SVG_NS };
}));
