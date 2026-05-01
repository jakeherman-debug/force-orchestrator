// D3 P6A.3 — Central keymap.
//
// Pattern P26 contract: every shortcut registered here MUST appear in the
// help-overlay table (help-overlay.html) with an identical key string,
// and vice-versa. The audit test parses both files and asserts the sets
// match exactly. Editing one without the other fails CI.
//
// Action callbacks live in app.js (window globals). The keymap dispatches
// by key string + optional context predicate. Prefix bindings (`g p`,
// `g b`, `g r`) use a 1-second window after the leader.
//
// Context predicates:
//   - briefing-list  — true when current surface is briefing AND in list view
//   - briefing-focus — true when current surface is briefing AND in focus mode
//   - reflection     — true when current surface is reflection
//   - global         — always true (default)
//
// `a` and `r` (approve/reject) only fire in briefing-focus context, so
// pressing `a` in Pulse does nothing — the brief calls this out.

(function(){
  'use strict';

  // The full binding table. Maintained in lock-step with help-overlay.html.
  // Each row: [key, context, actionFn, description].
  // The Pattern P26 audit greps the bind() calls below and reads the
  // help-overlay table to confirm the key sets agree.
  const BINDINGS = [
    // Surface navigation
    bind('Cmd-1',   'global',         () => navigateToSurface('pulse'),      'Switch to Pulse'),
    bind('Cmd-2',   'global',         () => navigateToSurface('briefing'),   'Switch to Briefing'),
    bind('Cmd-3',   'global',         () => navigateToSurface('reflection'), 'Switch to Reflection'),
    bind('g p',     'global',         () => navigateToSurface('pulse'),      'Goto Pulse (vim-style)'),
    bind('g b',     'global',         () => navigateToSurface('briefing'),   'Goto Briefing (vim-style)'),
    bind('g r',     'global',         () => navigateToSurface('reflection'), 'Goto Reflection (vim-style)'),

    // Search / Ask
    bind('/',       'global',         () => focusSearch(),                  'Focus search / Ask Force'),

    // Help overlay
    bind('?',       'global',         () => toggleHelpOverlay(),            'Toggle help overlay'),
    bind('Esc',     'global',         () => onEscape(),                     'Close overlay or focus mode'),

    // Briefing list navigation
    bind('j',       'briefing-list',  () => briefingMoveCursor(+1),         'Next row (Briefing list)'),
    bind('k',       'briefing-list',  () => briefingMoveCursor(-1),         'Previous row (Briefing list)'),
    bind('Enter',   'briefing-list',  () => briefingOpenFocused(),          'Open focused row (Briefing)'),

    // Briefing focus mode actions
    bind('a',       'briefing-focus', () => briefingApprove(),              'Approve focused decision'),
    bind('r',       'briefing-focus', () => briefingReject(),               'Reject focused decision'),

    // Drill (placeholder route in 6A; functional in 6B)
    bind('D',       'global',         () => drillFocused(),                 'Drill — open drill view of focused row'),

    // Reflection list navigation (j/k under reflection context too)
    bind('j',       'reflection',     () => reflectionMoveCursor(+1),       'Next row (Reflection lists)'),
    bind('k',       'reflection',     () => reflectionMoveCursor(-1),       'Previous row (Reflection lists)'),
  ];

  // bind() builds a binding row. The Pattern P26 AST audit walks BINDINGS
  // and the help-overlay table to make sure the two stay in lock-step.
  function bind(key, context, action, description) {
    return { key, context, action, description };
  }

  // ── Context resolution ──────────────────────────────────────────────────
  function activeContexts() {
    // Determine which contexts are currently in effect. Multiple may be
    // active (e.g., briefing-focus implies briefing).
    const out = ['global'];
    const surface = (typeof currentSurfaceFromHash === 'function')
      ? currentSurfaceFromHash() : 'pulse';
    if (surface === 'briefing') {
      out.push('briefing');
      // Detect focus mode by checking if a #/briefing/decision/<id> fragment
      // is active or if a focus-mode container is visible.
      const focusEl = document.querySelector('#briefing-focus-mode:not([hidden])');
      const inFocus = (window.location.hash || '').match(/^#\/briefing\/decision\//);
      if (focusEl || inFocus) {
        out.push('briefing-focus');
      } else {
        out.push('briefing-list');
      }
    } else if (surface === 'reflection') {
      out.push('reflection');
    }
    return out;
  }

  function bindingMatches(binding, ctxs) {
    return ctxs.indexOf(binding.context) !== -1;
  }

  // ── Key string normalisation ────────────────────────────────────────────
  // Converts a KeyboardEvent into a canonical key string that matches the
  // BINDINGS table. Modifiers prefix the key: Cmd-1, Shift-A, etc.
  function eventToKey(e) {
    // Treat Cmd (metaKey) and Ctrl as the same modifier name "Cmd" for
    // cross-platform consistency. The brief uses Cmd-1/2/3 wording.
    const cmd = e.metaKey || e.ctrlKey;
    if (cmd && /^[1-9]$/.test(e.key)) return 'Cmd-' + e.key;
    if (e.key === 'Escape') return 'Esc';
    if (e.key === 'Enter') return 'Enter';
    if (e.key === '?') return '?';
    if (e.key === '/' && !cmd) return '/';
    // Single-letter keys (case-sensitive: 'a' vs 'A' vs Shift-A).
    // Capital-D is intentional for the Drill binding.
    if (e.key && e.key.length === 1 && !cmd && !e.altKey) return e.key;
    return null;
  }

  // ── Prefix binding state ────────────────────────────────────────────────
  let prefixBuf = '';
  let prefixTimer = 0;
  function clearPrefix() {
    prefixBuf = '';
    if (prefixTimer) { clearTimeout(prefixTimer); prefixTimer = 0; }
  }

  // ── Dispatch ────────────────────────────────────────────────────────────
  function dispatch(e) {
    // Skip when typing into form controls.
    const tag = (e.target && e.target.tagName) || '';
    if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT'
        || (e.target && e.target.isContentEditable)) {
      // Esc still dismisses overlays even when typing (per the brief).
      if (e.key === 'Escape') {
        const k = 'Esc';
        const ctxs = activeContexts();
        for (const b of BINDINGS) {
          if (b.key === k && bindingMatches(b, ctxs)) {
            try { b.action(); } catch (err) { /* swallow */ }
            return;
          }
        }
      }
      return;
    }

    const k = eventToKey(e);
    if (!k) return;

    // Prefix handling: if we have an outstanding prefix, build the combined
    // key string (e.g., "g p"). If still incomplete, swallow the keystroke.
    if (prefixBuf) {
      const combined = prefixBuf + ' ' + k;
      clearPrefix();
      const ctxs = activeContexts();
      for (const b of BINDINGS) {
        if (b.key === combined && bindingMatches(b, ctxs)) {
          e.preventDefault();
          try { b.action(); } catch (err) {}
          return;
        }
      }
      // Unknown prefix combo — do nothing, swallow.
      return;
    }

    // Begin a prefix if this is a recognised leader (`g`).
    if (k === 'g') {
      prefixBuf = 'g';
      prefixTimer = setTimeout(clearPrefix, 1000);
      // Don't preventDefault — single-`g` may be reserved for other UIs.
      return;
    }

    // Direct match.
    const ctxs = activeContexts();
    for (const b of BINDINGS) {
      if (b.key === k && bindingMatches(b, ctxs)) {
        e.preventDefault();
        try { b.action(); } catch (err) {}
        return;
      }
    }
  }

  document.addEventListener('keydown', dispatch);

  // ── Help overlay ────────────────────────────────────────────────────────
  // First press fetches help-overlay.html and inserts it into the mount;
  // subsequent presses just toggle visibility. Cached after first load.
  let helpFetched = false;
  function toggleHelpOverlay() {
    const overlay = document.getElementById('help-overlay');
    if (!overlay) return;
    if (!helpFetched) {
      const mount = document.getElementById('help-overlay-mount');
      fetch('/help-overlay.html')
        .then(r => r.text())
        .then(html => {
          // Note: server-controlled, same-origin, CSP-locked — safe.
          // The file ships in our embed.FS; no untrusted input.
          if (mount) mount.innerHTML = html;
          helpFetched = true;
          overlay.classList.remove('hidden');
        })
        .catch(() => { /* network blip — leave overlay hidden */ });
      return;
    }
    overlay.classList.toggle('hidden');
  }

  function onEscape() {
    const overlay = document.getElementById('help-overlay');
    if (overlay && !overlay.classList.contains('hidden')) {
      overlay.classList.add('hidden');
      return;
    }
    // If in briefing focus mode, return to list. The briefing focus
    // container is added in 6A.10; for now this is a no-op when absent.
    if (typeof briefingExitFocus === 'function') briefingExitFocus();
  }

  function focusSearch() {
    const inp = document.getElementById('surface-search');
    if (inp) inp.focus();
  }

  // ── Stub action handlers for bindings whose features aren't yet built ──
  // Each task that adds the feature replaces the stub with the real handler.
  // Defining stubs here means the keymap audit (Pattern P26) doesn't fail
  // before the dependent task lands.
  function nyi(name) {
    return function(){
      // Quietly no-op. Print to console for debugging during dev.
      try { console.debug('[keymap] ' + name + ' is not yet implemented'); } catch (_) {}
    };
  }
  if (typeof window.briefingMoveCursor   === 'undefined') window.briefingMoveCursor   = nyi('briefingMoveCursor');
  if (typeof window.briefingOpenFocused  === 'undefined') window.briefingOpenFocused  = nyi('briefingOpenFocused');
  if (typeof window.briefingApprove      === 'undefined') window.briefingApprove      = nyi('briefingApprove');
  if (typeof window.briefingReject       === 'undefined') window.briefingReject       = nyi('briefingReject');
  if (typeof window.briefingExitFocus    === 'undefined') window.briefingExitFocus    = nyi('briefingExitFocus');
  if (typeof window.reflectionMoveCursor === 'undefined') window.reflectionMoveCursor = nyi('reflectionMoveCursor');
  if (typeof window.drillFocused         === 'undefined') window.drillFocused         = nyi('drillFocused');

  // Click on the `?` overlay button (added in index.html).
  document.addEventListener('click', e => {
    if (e.target && e.target.id === 'help-overlay-btn') {
      toggleHelpOverlay();
      e.preventDefault();
    }
    if (e.target && e.target.id === 'help-overlay-backdrop') {
      const overlay = document.getElementById('help-overlay');
      if (overlay) overlay.classList.add('hidden');
    }
  });

  // Expose for tests / for app.js bridging.
  window._keymapBindings = BINDINGS;
  window.toggleHelpOverlay = toggleHelpOverlay;
})();
