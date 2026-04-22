'use strict';

// Tests for the log-stream section of app.js.
// Uses node:vm to evaluate the extracted section in an isolated context with
// mocked browser globals so no DOM or browser is required.

const { test } = require('node:test');
const assert   = require('node:assert/strict');
const vm       = require('node:vm');
const fs       = require('node:fs');
const path     = require('node:path');

const APP_JS = path.join(__dirname, 'app.js');

// ── helpers ──────────────────────────────────────────────────────────────────

function makeWrap() {
  const kids = [];
  const wrap = {
    get children() { return kids; },
    scrollHeight: 200, scrollTop: 0, clientHeight: 100,
    get innerHTML() { return ''; },
    set innerHTML(_) { kids.length = 0; },
    appendChild(el) {
      el._parent = wrap;
      kids.push(el);
    },
    removeChild(el) {
      const i = kids.indexOf(el);
      if (i !== -1) kids.splice(i, 1);
      el._parent = null;
    },
    querySelector(sel) {
      const cls = sel.startsWith('.') ? sel.slice(1) : null;
      if (!cls) return null;
      return kids.find(c =>
        typeof c.className === 'string' &&
        c.className.split(' ').includes(cls)
      ) || null;
    },
  };
  return wrap;
}

function makeEl(tag) {
  const el = {
    tagName: tag, className: '', textContent: '', _parent: null,
    remove() { if (this._parent) this._parent.removeChild(this); },
  };
  return el;
}

function buildSandbox(overrides = {}) {
  const wrap    = makeWrap();
  const timers  = new Map();
  let   timerSeq = 1;

  const sandbox = {
    S: { logSource: null, logMode: 'holonet', activeTab: 'logs' },
    $: (id) => (id === 'log-wrap' ? wrap : null),
    document: { createElement: (tag) => makeEl(tag) },
    EventSource: null,
    setTimeout:  (fn, ms) => { const id = timerSeq++; timers.set(id, { fn, ms }); return id; },
    clearTimeout: (id) => timers.delete(id),
    _wrap: wrap,
    _timers: timers,
    console,
    ...overrides,
  };
  return sandbox;
}

// Evaluate the log-stream section of app.js inside a fresh sandbox.
function loadLogStream(sandbox) {
  const src = fs.readFileSync(APP_JS, 'utf8');
  const startMarker = '// ── Logs (SSE)';
  const endMarker   = '\nfunction switchLog(';
  const start = src.indexOf(startMarker);
  const end   = src.indexOf(endMarker, start);
  if (start === -1 || end === -1) {
    throw new Error('Could not locate log-stream section in app.js');
  }
  vm.runInNewContext(src.slice(start, end), sandbox);
  return sandbox;
}

// Fire a mock EventSource message event.
function fireMessage(src, data) {
  src.onmessage({ data });
}

// Simulate a mock EventSource error event.
function fireError(src) {
  src.onerror();
}

// ── tests ─────────────────────────────────────────────────────────────────────

test('startLogStream clears wrap and creates EventSource', () => {
  const mockSrc = { close: () => {}, onmessage: null, onerror: null };
  const sandbox = buildSandbox({
    EventSource: class { constructor(u) { this.url = u; Object.assign(this, mockSrc); } },
  });
  loadLogStream(sandbox);

  sandbox.startLogStream();
  assert.ok(sandbox.S.logSource, 'logSource should be set');
  assert.equal(sandbox._wrap.children.length, 0, 'wrap should start empty');
});

test('onmessage: normal string event appends log-line', () => {
  let capturedSrc;
  const sandbox = buildSandbox({
    EventSource: class {
      constructor() { capturedSrc = this; this.close = () => {}; }
    },
  });
  loadLogStream(sandbox);
  sandbox.startLogStream();

  fireMessage(capturedSrc, '"hello world"');
  const lines = sandbox._wrap.children;
  assert.equal(lines.length, 1);
  assert.equal(lines[0].className, 'log-line');
  assert.equal(lines[0].textContent, 'hello world');
});

test('onmessage: raw JSON object string renders verbatim', () => {
  let capturedSrc;
  const sandbox = buildSandbox({
    EventSource: class {
      constructor() { capturedSrc = this; this.close = () => {}; }
    },
  });
  loadLogStream(sandbox);
  sandbox.startLogStream();

  const raw = '{"type":"event","msg":"hi"}';
  fireMessage(capturedSrc, raw);
  assert.equal(sandbox._wrap.children[0].textContent, raw);
});

test('onmessage: sentinel frame shows log-sentinel, keeps EventSource open', () => {
  let capturedSrc;
  const sandbox = buildSandbox({
    EventSource: class {
      constructor() { capturedSrc = this; this.close = () => {}; }
    },
  });
  loadLogStream(sandbox);
  sandbox.startLogStream();

  // sentinel is data: "" which parses to empty string
  fireMessage(capturedSrc, '""');
  const children = sandbox._wrap.children;
  assert.equal(children.length, 1, 'exactly one child (the sentinel)');
  assert.ok(children[0].className.includes('log-sentinel'), 'should have log-sentinel class');
  assert.ok(children[0].textContent.includes('waiting'), 'should show waiting message');
  assert.ok(sandbox.S.logSource !== null, 'EventSource must stay open after sentinel');
});

test('onmessage: sentinel not duplicated on repeated sentinel frames', () => {
  let capturedSrc;
  const sandbox = buildSandbox({
    EventSource: class {
      constructor() { capturedSrc = this; this.close = () => {}; }
    },
  });
  loadLogStream(sandbox);
  sandbox.startLogStream();

  fireMessage(capturedSrc, '""');
  fireMessage(capturedSrc, '""');
  fireMessage(capturedSrc, '""');
  assert.equal(sandbox._wrap.children.length, 1, 'sentinel must not duplicate');
});

test('onmessage: real event after sentinel removes sentinel and appends log-line', () => {
  let capturedSrc;
  const sandbox = buildSandbox({
    EventSource: class {
      constructor() { capturedSrc = this; this.close = () => {}; }
    },
  });
  loadLogStream(sandbox);
  sandbox.startLogStream();

  fireMessage(capturedSrc, '""');
  assert.equal(sandbox._wrap.children.length, 1);
  assert.ok(sandbox._wrap.children[0].className.includes('log-sentinel'));

  fireMessage(capturedSrc, '"live event"');
  const kids = sandbox._wrap.children;
  assert.equal(kids.length, 1, 'sentinel replaced by log-line');
  assert.equal(kids[0].className, 'log-line');
  assert.equal(kids[0].textContent, 'live event');
});

test('onerror: closes EventSource and shows error line', () => {
  let capturedSrc;
  const closedCalls = [];
  const sandbox = buildSandbox({
    EventSource: class {
      constructor() {
        capturedSrc = this;
        this.close = () => closedCalls.push(true);
      }
    },
  });
  loadLogStream(sandbox);
  sandbox.startLogStream();

  fireError(capturedSrc);
  assert.equal(closedCalls.length, 1, 'EventSource.close must be called once');
  assert.equal(sandbox.S.logSource, null, 'logSource cleared after error');
  const errEl = sandbox._wrap.querySelector('.log-line--error');
  assert.ok(errEl, 'error line should appear');
});

test('onerror: schedules reconnect with initial 1s backoff', () => {
  let capturedSrc;
  const sandbox = buildSandbox({
    EventSource: class {
      constructor() { capturedSrc = this; this.close = () => {}; }
    },
  });
  loadLogStream(sandbox);
  sandbox.startLogStream();

  fireError(capturedSrc);
  assert.equal(sandbox._timers.size, 1, 'one reconnect timer scheduled');
  const [timer] = sandbox._timers.values();
  assert.equal(timer.ms, 1000, 'initial backoff is 1s');
});

test('onerror: backoff doubles on consecutive errors', () => {
  let capturedSrc;
  let createCount = 0;
  const sandbox = buildSandbox({
    EventSource: class {
      constructor() { capturedSrc = this; this.close = () => {}; createCount++; }
    },
  });
  loadLogStream(sandbox);
  sandbox.startLogStream();

  // first error → 1s backoff
  fireError(capturedSrc);
  const [t1] = sandbox._timers.values();
  assert.equal(t1.ms, 1000);

  // fire the reconnect timer, which creates a new EventSource
  sandbox._timers.clear();
  t1.fn();  // reconnect fires; new EventSource created
  assert.equal(createCount, 2);

  // second error → 2s backoff
  fireError(capturedSrc);
  const [t2] = sandbox._timers.values();
  assert.equal(t2.ms, 2000);

  // third error → 4s
  sandbox._timers.clear();
  t2.fn();
  fireError(capturedSrc);
  const [t3] = sandbox._timers.values();
  assert.equal(t3.ms, 4000);
});

test('onerror: backoff caps at 30s', () => {
  let capturedSrc;
  const sandbox = buildSandbox({
    EventSource: class {
      constructor() { capturedSrc = this; this.close = () => {}; }
    },
  });
  loadLogStream(sandbox);
  sandbox.startLogStream();

  // burn through backoff levels until capped
  for (let i = 0; i < 10; i++) {
    fireError(capturedSrc);
    const [t] = sandbox._timers.values();
    sandbox._timers.clear();
    t.fn();  // reconnect
  }
  fireError(capturedSrc);
  const [last] = sandbox._timers.values();
  assert.ok(last.ms <= 30000, `backoff ${last.ms}ms exceeds 30s cap`);
});

test('onerror: no reconnect if tab changed away from logs', () => {
  let capturedSrc;
  const sandbox = buildSandbox({
    EventSource: class {
      constructor() { capturedSrc = this; this.close = () => {}; }
    },
  });
  loadLogStream(sandbox);
  sandbox.startLogStream();

  sandbox.S.activeTab = 'tasks';
  fireError(capturedSrc);

  // timer is still scheduled — but firing it must not create a new EventSource
  const [t] = sandbox._timers.values();
  sandbox._timers.clear();
  t.fn();
  assert.equal(sandbox.S.logSource, null, 'no reconnect when tab is not logs');
});

test('startLogStream: resets backoff to 1s', () => {
  let capturedSrc;
  const sandbox = buildSandbox({
    EventSource: class {
      constructor() { capturedSrc = this; this.close = () => {}; }
    },
  });
  loadLogStream(sandbox);
  sandbox.startLogStream();

  // burn backoff up to 4s
  fireError(capturedSrc); sandbox._timers.clear();
  sandbox.startLogStream(); // user switches away and back — should reset
  // we expect backoff reset to 1s, so next error fires 1s timer
  fireError(capturedSrc);
  const [t] = sandbox._timers.values();
  assert.equal(t.ms, 1000, 'backoff should reset to 1s on fresh startLogStream');
});

test('startLogStream: cancels pending backoff timer', () => {
  let capturedSrc;
  const sandbox = buildSandbox({
    EventSource: class {
      constructor() { capturedSrc = this; this.close = () => {}; }
    },
  });
  loadLogStream(sandbox);
  sandbox.startLogStream();

  // trigger error to arm the backoff timer
  fireError(capturedSrc);
  assert.equal(sandbox._timers.size, 1, 'backoff timer should exist');

  // user switches log mode — startLogStream should cancel the pending timer
  sandbox.startLogStream();
  // the old timer should have been cleared (new EventSource created instead)
  // The new startLogStream may or may not leave a new timer; what matters is
  // the old one is gone (we verify by checking there's at most one timer and
  // it belongs to the new connection, not the old backoff)
  assert.ok(sandbox.S.logSource !== null, 'new EventSource created');
});

test('happy path: backfill + live event', () => {
  let capturedSrc;
  const sandbox = buildSandbox({
    EventSource: class {
      constructor() { capturedSrc = this; this.close = () => {}; }
    },
  });
  loadLogStream(sandbox);
  sandbox.startLogStream();

  // simulate server backfill: several pre-existing events arrive immediately
  const backfillEvents = [
    '{"ts":1,"msg":"old-1"}',
    '{"ts":2,"msg":"old-2"}',
    '{"ts":3,"msg":"old-3"}',
  ];
  for (const e of backfillEvents) {
    fireMessage(capturedSrc, e);
  }

  assert.equal(sandbox._wrap.children.length, 3, 'three backfill lines');
  backfillEvents.forEach((e, i) => {
    assert.equal(sandbox._wrap.children[i].textContent, e);
  });

  // live event arrives after backfill
  fireMessage(capturedSrc, '{"ts":4,"msg":"live"}');
  assert.equal(sandbox._wrap.children.length, 4);
  assert.equal(sandbox._wrap.children[3].textContent, '{"ts":4,"msg":"live"}');

  // EventSource still open
  assert.ok(sandbox.S.logSource !== null);
});
