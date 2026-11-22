// The routing graph: routes on the left, the shunt hub in the middle, the
// (provider, model) targets they reach on the right. buildGraph derives it
// from GET /state and GET /activity with the same rules as the TUI.
'use strict';

const WIRE_PREFIX = 'shunt/';
const ALIASES = Object.freeze(['sonnet', 'opus', 'haiku', 'fable']);
const LIVE_MS = 10000;
const ANTHROPIC = 'anthropic';

const isAlias = (name) => ALIASES.includes(name);
const byText = (a, b) => (a < b ? -1 : a > b ? 1 : 0);

// buildGraph returns {sources, targets, edge}: sources[i] goes to
// targets[edge[i]]. The last source ("everything else", name '') always goes
// to the last target (Anthropic). It reads nothing but its arguments.
function buildGraph(state, activity, nowMs) {
  const routes = state.config.routes || {};
  const providers = state.config.providers || {};
  const keys = state.keys || {};
  const stats = (activity && activity.routes) || {};
  const recent = (activity && activity.recent) || [];

  // Recent is newest first; an entry is known only once it finished, at
  // time + ms, so that is what must fall inside the window.
  const liveWhere = (match) => {
    const e = recent.find(match);
    return Boolean(e) && nowMs - (Date.parse(e.time) + e.ms) < LIVE_MS;
  };
  const targetKey = (r) => r.provider + '/' + r.model;
  const targetState = (provider) => {
    if (!Object.hasOwn(providers, provider)) return 'no such provider';
    const k = keys[provider] || 'missing';
    if (k === 'missing') return 'no key';
    if (k === 'invalid') return 'key invalid';
    return 'ready';
  };

  const names = Object.keys(routes).sort(byText);
  const sources = names.map((name) => {
    const r = routes[name];
    const s = stats[name];
    return {
      name,
      id: WIRE_PREFIX + name,
      provider: r.provider,
      model: r.model,
      to: targetKey(r),
      alias: isAlias(name),
      stats: s ? { requests: s.requests, errors: s.errors, status: s.status, time: s.time } : null,
      live: liveWhere((e) => e.route === name),
    };
  });
  const anthropicLive = liveWhere((e) => e.target === ANTHROPIC);
  sources.push({ name: '', id: '', provider: '', model: '', to: ANTHROPIC, alias: false, stats: null, live: anthropicLive });

  const seen = new Map();
  for (const name of names) {
    const r = routes[name];
    if (!seen.has(targetKey(r))) seen.set(targetKey(r), { provider: r.provider, model: r.model });
  }
  const targets = [...seen.values()]
    .sort((a, b) => byText(a.provider, b.provider) || byText(a.model, b.model))
    .map((t) => ({ key: targetKey(t), provider: t.provider, model: t.model, state: targetState(t.provider), live: false }));
  targets.push({ key: ANTHROPIC, provider: '', model: '', state: 'unchanged', live: anthropicLive });

  const index = new Map(targets.map((t, i) => [t.key, i]));
  const edge = names.map((name) => index.get(targetKey(routes[name])));
  edge.push(targets.length - 1);
  sources.forEach((s, i) => {
    if (s.live) targets[edge[i]].live = true;
  });
  return { sources, targets, edge };
}

// ---- edges ---------------------------------------------------------------
// Each edge is two paths, source -> hub and hub -> target, tagged
// data-edge with the route name. Paths are rebuilt only when the edges
// change, so a CSS animation on them is not restarted by every poll.

const topology = (g) => JSON.stringify(g.edge.map((t, i) => [g.sources[i].name, g.targets[t].key]));

// ends maps a route name ('' for everything else) to its source and target.
const ends = (g) => new Map(g.sources.map((s, i) => [s.name, { source: s, target: g.targets[g.edge[i]] }]));

function drawEdges(svg, g) {
  const shape = topology(g);
  if (svg.dataset.shape === shape) return;
  svg.dataset.shape = shape;
  const paths = [];
  for (const s of g.sources) {
    for (const part of ['in', 'out']) {
      const p = document.createElementNS(svg.namespaceURI, 'path');
      p.setAttribute('data-edge', s.name);
      p.setAttribute('data-part', part);
      paths.push(p);
    }
  }
  svg.replaceChildren(...paths);
}

// paintEdges marks the paths of the selected route, of the routes reaching
// the related target, and of live routes. Routes sharing a target share
// the hub -> target stretch, and SVG paints in document order, so marked
// paths move to the end; only when the order changes, since moving a node
// restarts its animation.
function paintEdges(svg, g, selected, related) {
  const byName = ends(g);
  const paths = [...svg.children];
  for (const p of paths) {
    const e = byName.get(p.dataset.edge);
    if (!e) continue;
    p.classList.toggle('is-selected', selected !== null && e.source.name === selected);
    p.classList.toggle('is-related', related !== null && e.target.key === related);
    p.classList.toggle('is-live', e.source.live);
  }
  const rank = (p) => ['is-live', 'is-related', 'is-selected'].reduce((r, c, i) => (p.classList.contains(c) ? i + 1 : r), 0);
  const order = [...paths].sort((a, b) => rank(a) - rank(b));
  if (order.some((p, i) => p !== paths[i])) svg.append(...order);
}

// layoutEdges places the paths in box coordinates: cubic curves from a
// source's right edge to the hub's left edge, and from the hub's right edge
// to a target's left edge, each at the middle of the box they touch.
function layoutEdges(box, svg, hub, g) {
  if (!g) return;
  const origin = box.getBoundingClientRect();
  const left = (r) => ({ x: r.left - origin.left, y: r.top + r.height / 2 - origin.top });
  const right = (r) => ({ x: r.right - origin.left, y: r.top + r.height / 2 - origin.top });
  const curve = (a, b) => {
    const dx = (b.x - a.x) / 2;
    return `M${a.x} ${a.y}C${a.x + dx} ${a.y} ${b.x - dx} ${b.y} ${b.x} ${b.y}`;
  };
  const hubBox = hub.getBoundingClientRect();
  const byName = ends(g);
  for (const p of svg.children) {
    const e = byName.get(p.dataset.edge);
    if (!e) continue;
    const node = p.dataset.part === 'in'
      ? box.querySelector(`[data-source="${CSS.escape(e.source.name)}"]`)
      : box.querySelector(`[data-target="${CSS.escape(e.target.key)}"]`);
    if (!node) continue;
    const r = node.getBoundingClientRect();
    p.setAttribute('d', p.dataset.part === 'in' ? curve(right(r), left(hubBox)) : curve(right(hubBox), left(r)));
  }
}
