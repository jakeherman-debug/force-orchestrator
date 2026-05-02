'use strict';

// ── State ─────────────────────────────────────────────────────────────────────
const S = {
  status:             null,
  tasks:              [],
  taskFilter:         'active',
  taskOffset:         0,
  taskTotal:          0,
  convoyFilter:       0,
  convoys:            [],
  convoyStatusFilter: 'all',
  convoyTimeFilter:   'all',
  repos:              [],
  escFilter:          'Open',
  logMode:            'fleet',   // 'fleet' | 'holonet'
  logSource:          null,
  selectedID:         null,
  detail:             null,
  rejectID:           null,
  shipConvoyID:       null,
  activeTab:          'tasks',
  sortBy:             'id',
  sortDir:            'desc',
  showInfra:          false,     // toggle — hide fleet plumbing (Pilot, Librarian, Medic triage) by default
  openPRReviewPanels: new Set(), // convoy IDs whose PR review panel is expanded
};

// ── Utility ───────────────────────────────────────────────────────────────────
const $ = id => document.getElementById(id);

function fmtTS(ts) {
  if (!ts) return '';
  const d = new Date(ts);
  if (isNaN(d)) return ts;
  return d.toLocaleString(undefined, { month:'short', day:'numeric',
    hour:'2-digit', minute:'2-digit', second:'2-digit', hour12:false });
}

function fmtShortDate(ts) {
  if (!ts) return '';
  const d = new Date(ts);
  if (isNaN(d)) return ts;
  const mo = String(d.getMonth() + 1).padStart(2, '0');
  const dy = String(d.getDate()).padStart(2, '0');
  const hr = String(d.getHours()).padStart(2, '0');
  const mn = String(d.getMinutes()).padStart(2, '0');
  return `${mo}/${dy} ${hr}:${mn}`;
}

function truncate(s, n) {
  if (!s) return '';
  return s.length > n ? s.slice(0, n) + '…' : s;
}

function statusCls(st) {
  return 's-' + (st || '').replace(/\s/g, '');
}

function statusPill(st) {
  return `<span class="status ${statusCls(st)}">${st || ''}</span>`;
}

function fmtCost(dollars) {
  if (!dollars) return '$0.00';
  return '$' + dollars.toFixed(2);
}

function fmtRuntime(secs) {
  if (!secs) return '';
  if (secs < 3600) {
    const m = Math.floor(secs / 60);
    const s = secs % 60;
    return `${m}m ${String(s).padStart(2, '0')}s`;
  }
  const h = Math.floor(secs / 3600);
  const m = Math.floor((secs % 3600) / 60);
  return `${h}h ${String(m).padStart(2, '0')}m`;
}

function showToast(msg, type = 'ok') {
  const el = document.createElement('div');
  el.className = `toast ${type}`;
  el.textContent = msg;
  document.body.appendChild(el);
  setTimeout(() => el.remove(), 3000);
}

function genUUID() {
  if (typeof crypto !== 'undefined' && crypto.randomUUID) return crypto.randomUUID();
  return ([1e7]+-1e3+-4e3+-8e3+-1e11).replace(/[018]/g, c =>
    (c ^ crypto.getRandomValues(new Uint8Array(1))[0] & 15 >> c / 4).toString(16));
}

async function api(url, opts = {}) {
  const r = await fetch(url, opts);
  if (!r.ok) {
    let msg = `HTTP ${r.status}`;
    try { const j = await r.json(); msg = j.error || msg; } catch(_) {}
    throw new Error(msg);
  }
  return r.json();
}

// ── Stats polling ──────────────────────────────────────────────────────────────
async function pollStats() {
  try {
    const s = await api('/api/stats');
    $('pill-pending-count').textContent         = s.pending_count         || 0;
    $('pill-active-count').textContent          = s.active_count          || 0;
    $('pill-completed-today-count').textContent = s.completed_today_count || 0;
    $('pill-convoys-count').textContent         = s.active_convoys        || 0;
  } catch(_) {}
}

// ── Status polling ─────────────────────────────────────────────────────────────
async function pollStatus() {
  try {
    const s = await api('/api/status');
    S.status = s;
    renderHeader();
    renderStats();
  } catch(_) {}
}

function renderHeader() {
  const s = S.status;
  if (!s) return;

  // Daemon badge
  const db = $('daemon-badge');
  if (s.daemon_running) {
    db.className = 'badge badge-running';
    db.textContent = `● Daemon PID ${s.daemon_pid}`;
  } else {
    db.className = 'badge badge-stopped';
    db.textContent = '● Daemon offline';
  }

  // E-stop badge + buttons
  const estopBadge = $('estop-badge');
  const estopBtn   = $('estop-btn');
  const resumeBtn  = $('resume-btn');
  if (s.estopped) {
    estopBadge.className = 'badge badge-estop';
    estopBadge.style.display = '';
    estopBtn.style.display = 'none';
    resumeBtn.style.display = '';
  } else {
    estopBadge.style.display = 'none';
    estopBtn.style.display = '';
    resumeBtn.style.display = 'none';
  }

  $('last-refresh').textContent = 'Updated ' + new Date().toLocaleTimeString();
}

function renderStats() {
  const s = S.status;
  if (!s) return;
  const t = s.tasks || {};
  $('st-locked').textContent   = (t.Locked || 0);
  $('st-pending').textContent  = (t.Pending || 0);
  const reviewCount = (t.AwaitingCouncilReview || 0) + (t.UnderReview || 0) +
                      (t.AwaitingCaptainReview || 0) + (t.UnderCaptainReview || 0);
  $('st-review').textContent   = reviewCount;
  $('st-completed').textContent = (t.Completed || 0);
  $('st-failed').textContent   = (t.Failed || 0) + (t.Escalated || 0);
  $('st-esc').textContent      = s.open_escalations || 0;
  $('st-convoys').textContent  = s.active_convoys   || 0;
  $('st-mail').textContent     = s.unread_mail      || 0;
  $('st-spend').textContent    = fmtCost(s.total_spend_dollars || 0);

  // Tab badges
  const escEl = $('tbadge-escalations');
  escEl.textContent = s.open_escalations || '';
  escEl.className = 'tab-badge' + (s.open_escalations > 0 ? ' hot' : '');

  $('tbadge-mail').textContent = s.unread_mail || '';
  $('tbadge-mail').className = 'tab-badge' + (s.unread_mail > 0 ? ' hot' : '');

  // High-escalations banner (AUDIT-064 / Fix #2): show when >=3 HIGH-severity
  // escalations are open. Three simultaneous HIGH escalations means the
  // fleet's self-healing is genuinely exhausted — the operator is the
  // bottleneck, and this needs to be impossible to miss from any tab.
  const highEscCount = s.high_escalations || 0;
  const highBanner = $('high-esc-banner');
  if (highBanner) {
    $('high-esc-banner-count').textContent = highEscCount;
    highBanner.classList.toggle('hidden', highEscCount < 3);
  }

  // Quarantined-repo banner (D2 T1-4): show when any registered repo is
  // mode=quarantined. The astromech claim filter silently skips these
  // repos; the banner is the loud half so backlog doesn't go unnoticed.
  const quarantinedCount = s.quarantined_repos || 0;
  const quarantineBanner = $('quarantined-repo-banner');
  if (quarantineBanner) {
    const countEl = $('quarantined-repo-count');
    if (countEl) countEl.textContent = quarantinedCount;
    quarantineBanner.classList.toggle('hidden', quarantinedCount === 0);
  }

  // Ship-ready banner: show when any convoy is DraftPROpen, hide otherwise.
  // Visible from every tab so the operator can't miss it — the fleet is
  // literally blocked on their Ship It click.
  const shipCount = s.ready_to_ship || 0;
  const banner = $('ship-banner');
  if (banner) {
    $('ship-banner-count').textContent = shipCount;
    banner.classList.toggle('hidden', shipCount === 0);
  }
  const convoysBadge = $('tbadge-convoys');
  if (convoysBadge) {
    convoysBadge.textContent = shipCount > 0 ? String(shipCount) : '';
    convoysBadge.className = 'tab-badge' + (shipCount > 0 ? ' hot' : '');
  }
}

// jumpToShipReady switches to the Convoys tab and sets the filter so the
// ship-ready convoys are visible immediately. Wired to the top banner click.
function jumpToShipReady() {
  setConvoyStatusFilter('active');
  switchTab('convoys');
}

// jumpToEscalations switches to the Escalations tab with the default Open
// filter. Wired to the high-escalations banner click (AUDIT-064).
function jumpToEscalations() {
  switchTab('escalations');
}

// ── URL sync ──────────────────────────────────────────────────────────────────
function syncURL() {
  const p = new URLSearchParams();
  if (S.activeTab          !== 'tasks')   p.set('tab',           S.activeTab);
  if (S.taskFilter         !== 'active')  p.set('status',        S.taskFilter);
  const search = ($('task-search') && $('task-search').value) || '';
  if (search)                             p.set('search',        search);
  if (S.sortBy)                           p.set('sort_by',       S.sortBy);
  if (S.sortDir            !== 'asc')     p.set('sort_dir',      S.sortDir);
  if (S.escFilter          !== 'Open')    p.set('esc_status',    S.escFilter);
  if (S.convoyStatusFilter !== 'all')     p.set('convoy_status', S.convoyStatusFilter);
  if (S.convoyTimeFilter   !== 'all')     p.set('convoy_since',  S.convoyTimeFilter);
  if (S.logMode            !== 'fleet')   p.set('log_mode',      S.logMode);
  if (S.showInfra)                        p.set('show_infra',    '1');
  const qs = p.toString();
  history.pushState(null, '', qs ? '?' + qs : window.location.pathname);
}

// ── Tab switching ─────────────────────────────────────────────────────────────
function switchTab(name) {
  S.activeTab = name;
  syncURL();

  if (name !== 'tasks') {
    S.convoyFilter = 0;
    hideConvoyBanner();
  }

  document.querySelectorAll('.tab-btn').forEach(b => {
    b.classList.toggle('active', b.dataset.tab === name);
  });
  document.querySelectorAll('.tab-pane').forEach(p => {
    p.classList.toggle('active', p.id === 'tab-' + name);
  });

  switch (name) {
    case 'tasks':       loadTasks(); break;
    case 'escalations': loadEscalations(); break;
    case 'convoys':     loadConvoys(); break;
    case 'agents':      loadAgents(); break;
    case 'mail':        loadMail(); break;
    case 'knowledge':   loadMemoryRepos().then(() => loadMemories()); break;
    case 'experiments': loadExperiments(); loadFleetProgress(); break;
    case 'ec':          loadECProposals(); break;
    case 'security':    window.loadSecurityFindings && window.loadSecurityFindings(); break;
    case 'senate':      window.loadSenateChambers   && window.loadSenateChambers(); break;
    case 'logs':        startLogStream(); break;
  }
  if (name !== 'logs') stopLogStream();
}

// ── Experiments tab (D3 Phase 2) ─────────────────────────────────────────
// Minimal list + detail view. Phase 6 rebuilds around Pulse / Briefing /
// Reflection; these calls move there. Read-only — operator mutations
// (ratify, terminate) flow through `force experiment ...` for now.

let experimentStatusFilter = 'all';

function setExperimentFilter(status) {
  experimentStatusFilter = status;
  document.querySelectorAll('[data-experiment-status]').forEach(b => {
    b.classList.toggle('active', b.dataset.experimentStatus === status);
  });
  loadExperiments();
}

async function loadExperiments() {
  const wrap = document.getElementById('experiment-list-wrap');
  if (!wrap) return;
  try {
    const url = '/api/experiments' + (experimentStatusFilter && experimentStatusFilter !== 'all'
      ? '?status=' + encodeURIComponent(experimentStatusFilter) : '');
    const res = await fetch(url);
    const data = await res.json();
    const rows = (data && data.experiments) || [];
    if (!rows.length) {
      wrap.textContent = 'No experiments yet. Author one with `force experiment author <yaml>`.';
      return;
    }
    const html = ['<table class="data-table"><thead><tr>',
      '<th>id</th><th>name</th><th>status</th><th>tier</th><th>agent</th><th>outcome</th>',
      '</tr></thead><tbody>'];
    for (const e of rows) {
      const safeName = (e.name || '').replace(/[<>&]/g, c => ({'<':'&lt;','>':'&gt;','&':'&amp;'}[c]));
      html.push('<tr style="cursor:pointer" onclick="loadExperimentDetail(' + e.id + ')">');
      html.push('<td>' + e.id + '</td>');
      html.push('<td>' + safeName + '</td>');
      html.push('<td><span class="badge">' + e.status + '</span></td>');
      html.push('<td>' + (e.stakes_tier || '') + '</td>');
      html.push('<td>' + (e.subject_agent || '') + '</td>');
      html.push('<td>' + (e.outcome_reason || '—') + '</td>');
      html.push('</tr>');
    }
    html.push('</tbody></table>');
    wrap.innerHTML = html.join('');
  } catch (err) {
    wrap.textContent = 'Failed to load experiments: ' + err;
  }
}

async function loadExperimentDetail(id) {
  const wrap = document.getElementById('experiment-detail-wrap');
  if (!wrap) return;
  try {
    const res = await fetch('/api/experiments/' + encodeURIComponent(id));
    if (res.status === 404) {
      wrap.style.display = 'block';
      wrap.textContent = 'Experiment ' + id + ' not found.';
      return;
    }
    const d = await res.json();
    const armRows = (d.treatments || []).map(t => {
      const rate = t.observed_rate ? (t.observed_rate * 100).toFixed(1) + '%' : '—';
      return '<tr><td>' + (t.arm_label || '') + '</td>' +
        '<td>' + (t.target_cell_weight || 0).toFixed(2) + '</td>' +
        '<td>' + (t.prompt_template_ref || '') + '</td>' +
        '<td>' + (t.enrollment || 0) + '</td>' +
        '<td>' + (t.success_count || 0) + '</td>' +
        '<td>' + rate + '</td></tr>';
    }).join('');
    const html = [
      '<h3>Experiment ' + d.id + ' — ' + (d.name || '').replace(/[<>&]/g, c => ({'<':'&lt;','>':'&gt;','&':'&amp;'}[c])) + '</h3>',
      '<p style="color:var(--text2)">' + (d.hypothesis || '').replace(/[<>&]/g, c => ({'<':'&lt;','>':'&gt;','&':'&amp;'}[c])) + '</p>',
      '<dl class="kv-list">',
      '<dt>status</dt><dd>' + d.status + '</dd>',
      '<dt>stakes_tier</dt><dd>' + (d.stakes_tier || '') + '</dd>',
      '<dt>subject_agent</dt><dd>' + (d.subject_agent || '') + '</dd>',
      '<dt>analysis_framework</dt><dd>' + (d.analysis_framework_version || '') + '</dd>',
      '<dt>min_practical_effect</dt><dd>' + (d.min_practical_effect || 0) + '</dd>',
      d.winner_treatment_id ? '<dt>winner</dt><dd>treatment ' + d.winner_treatment_id + ' (posterior=' + Number(d.winner_posterior || 0).toFixed(4) + ')</dd>' : '',
      '</dl>',
      '<h4>Treatments</h4>',
      '<table class="data-table"><thead><tr><th>arm</th><th>weight</th><th>prompt_ref</th><th>enrol</th><th>succ</th><th>rate</th></tr></thead><tbody>',
      armRows,
      '</tbody></table>',
    ].join('');
    wrap.innerHTML = html;
    wrap.style.display = 'block';
  } catch (err) {
    wrap.style.display = 'block';
    wrap.textContent = 'Failed to load experiment ' + id + ': ' + err;
  }
}

async function loadFleetProgress() {
  const wrap = document.getElementById('experiment-fleet-progress');
  if (!wrap) return;
  try {
    const res = await fetch('/api/fleet-progress');
    const d = await res.json();
    const lines = [
      'Holdout <strong>' + (d.holdout_name || '—') + '</strong>',
      'phase=' + (d.holdout_lifecycle || '—'),
      'fraction=' + Number(d.holdout_fraction_now || 0).toFixed(4),
      'members=' + (d.holdout_members || 0),
    ];
    let html = lines.join(' · ');
    if (Array.isArray(d.windows) && d.windows.length) {
      html += '<br>';
      html += d.windows.map(w =>
        w.label + ': holdout n=' + (w.holdout_run_count || 0) + ' rate=' + Number(w.holdout_success_rate || 0).toFixed(3) +
        ' / current n=' + (w.current_run_count || 0) + ' rate=' + Number(w.current_success_rate || 0).toFixed(3)
      ).join(' · ');
    }
    wrap.innerHTML = html;
  } catch (err) {
    wrap.textContent = 'fleet-progress: ' + err;
  }
}

// ── Engineering Corps ratification (D3 Phase 3) ─────────────────────────
// Operator surface for PromotionProposals: list pending (both Librarian
// candidates and EC promotes), open detail, ratify or reject. Ratify
// requires the operator email; Reject likewise — and additionally a
// rejection_rationale ≥ 20 chars when rejection_action != 'leave_as_is'
// (concern #7). All mutations route through the same securityMiddleware
// stack as the rest of the dashboard.

let ecStatusFilter = 'pending';
let ecKindFilter = '';

function setECFilter(status) {
  ecStatusFilter = status;
  document.querySelectorAll('[data-ec-status]').forEach(b => {
    b.classList.toggle('active', b.dataset.ecStatus === status);
  });
  loadECProposals();
}

function setECKindFilter(kind) {
  ecKindFilter = kind || '';
  loadECProposals();
}

function ecEscape(s) {
  return (s || '').replace(/[<>&]/g, c => ({'<':'&lt;','>':'&gt;','&':'&amp;'}[c]));
}

async function loadECProposals() {
  const wrap = document.getElementById('ec-list-wrap');
  if (!wrap) return;
  try {
    const params = [];
    if (ecStatusFilter && ecStatusFilter !== 'pending') params.push('status=' + encodeURIComponent(ecStatusFilter));
    // status=pending is the API default but we still send it explicitly
    // so the active-filter state matches the server response.
    if (ecStatusFilter === 'pending') params.push('status=pending');
    if (ecKindFilter) params.push('kind=' + encodeURIComponent(ecKindFilter));
    const url = '/api/ec/proposals' + (params.length ? '?' + params.join('&') : '');
    const res = await fetch(url);
    const data = await res.json();
    const rows = (data && data.proposals) || [];
    if (!rows.length) {
      wrap.textContent = 'No proposals match this filter.';
      return;
    }
    const html = ['<table class="data-table"><thead><tr>',
      '<th>id</th><th>kind</th><th>rule_key</th><th>authored_by</th><th>authored_at</th><th>status</th>',
      '</tr></thead><tbody>'];
    for (const p of rows) {
      let status = 'pending';
      if (p.ratified_at) status = 'ratified by ' + ecEscape(p.ratified_by || '?');
      else if (p.rejected_at) status = 'rejected (' + ecEscape(p.rejection_action || '') + ')';
      html.push('<tr style="cursor:pointer" onclick="loadECDetail(' + p.id + ')">');
      html.push('<td>' + p.id + '</td>');
      html.push('<td>' + ecEscape(p.kind) + '</td>');
      html.push('<td>' + ecEscape(p.rule_key) + '</td>');
      html.push('<td>' + ecEscape(p.authored_by) + '</td>');
      html.push('<td>' + ecEscape(p.authored_at) + '</td>');
      html.push('<td>' + status + '</td>');
      html.push('</tr>');
    }
    html.push('</tbody></table>');
    wrap.innerHTML = html.join('');
  } catch (err) {
    wrap.textContent = 'Failed to load EC proposals: ' + err;
  }
}

async function loadECDetail(id) {
  const wrap = document.getElementById('ec-detail-wrap');
  if (!wrap) return;
  try {
    const res = await fetch('/api/ec/proposals/' + encodeURIComponent(id));
    if (res.status === 404) {
      wrap.style.display = 'block';
      wrap.textContent = 'Proposal ' + id + ' not found.';
      return;
    }
    const p = await res.json();
    const html = [
      '<h3>Proposal ' + p.id + ' (' + ecEscape(p.kind) + ')</h3>',
      '<dl class="kv-list">',
      '<dt>rule_key</dt><dd>' + ecEscape(p.rule_key) + '</dd>',
      '<dt>authored_by</dt><dd>' + ecEscape(p.authored_by) + '</dd>',
      '<dt>authored_at</dt><dd>' + ecEscape(p.authored_at) + '</dd>',
      p.experiment_id ? '<dt>experiment_id</dt><dd>' + p.experiment_id + '</dd>' : '',
      p.ratified_at ? '<dt>ratified_at</dt><dd>' + ecEscape(p.ratified_at) + ' by ' + ecEscape(p.ratified_by) + '</dd>' : '',
      p.rejected_at ? '<dt>rejected_at</dt><dd>' + ecEscape(p.rejected_at) + ' (' + ecEscape(p.rejection_action || '') + ')</dd>' : '',
      '</dl>',
      '<h4>Proposed content</h4>',
      '<pre style="white-space:pre-wrap">' + ecEscape(p.proposed_content) + '</pre>',
      '<h4>Evidence</h4>',
      '<pre style="white-space:pre-wrap">' + ecEscape(p.evidence_summary_json) + '</pre>',
    ];
    if (!p.ratified_at && !p.rejected_at) {
      html.push('<div style="margin-top:14px;display:flex;gap:8px">');
      html.push('<button class="btn btn-primary" onclick="ecRatify(' + p.id + ')">Ratify</button>');
      html.push('<button class="btn" onclick="ecRejectPrompt(' + p.id + ')">Reject…</button>');
      html.push('</div>');
    }
    wrap.innerHTML = html.join('');
    wrap.style.display = 'block';
  } catch (err) {
    wrap.style.display = 'block';
    wrap.textContent = 'Failed to load proposal ' + id + ': ' + err;
  }
}

async function ecRatify(id) {
  const op = prompt('Operator email for ratification:');
  if (!op) return;
  try {
    const res = await fetch('/api/ec/proposals/' + encodeURIComponent(id) + '/ratify', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ operator_email: op }),
    });
    if (!res.ok) {
      const txt = await res.text();
      alert('Ratify failed: ' + txt);
      return;
    }
    loadECProposals();
    loadECDetail(id);
  } catch (err) {
    alert('Ratify error: ' + err);
  }
}

async function ecRejectPrompt(id) {
  const op = prompt('Operator email:');
  if (!op) return;
  const action = prompt('rejection_action: leave_as_is | clean_revert | cascade_revert | surgical_revert | escalate', 'leave_as_is');
  if (!action) return;
  let rationale = '';
  if (action !== 'leave_as_is') {
    rationale = prompt('rejection_rationale (≥ 20 chars):') || '';
    if (rationale.length < 20) {
      alert('rationale must be ≥ 20 chars when action != leave_as_is');
      return;
    }
  }
  const reason = prompt('rejected_reason (free-form, optional):') || '';
  try {
    const res = await fetch('/api/ec/proposals/' + encodeURIComponent(id) + '/reject', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        operator_email: op,
        rejection_action: action,
        rejection_rationale: rationale,
        rejected_reason: reason,
      }),
    });
    if (!res.ok) {
      const txt = await res.text();
      alert('Reject failed: ' + txt);
      return;
    }
    loadECProposals();
    loadECDetail(id);
  } catch (err) {
    alert('Reject error: ' + err);
  }
}

// ── Tasks ─────────────────────────────────────────────────────────────────────
// Any task status should be visible in at least one of these filters besides
// "all" — orphaning a status hides in-flight work from the default views.
const FILTER_STATUS = {
  active:    'Pending,Classifying,Locked,Planned,AwaitingChancellorReview,AwaitingCouncilReview,UnderReview,AwaitingCaptainReview,UnderCaptainReview,AwaitingSubPRCI',
  pending:   'Pending,Classifying,Blocked,Planned,AwaitingChancellorReview',
  failed:    'Failed,Escalated,ConflictPending',
  done:      'Completed',
  cancelled: 'Cancelled',
  all:       '',
};

const TASK_PAGE_SIZE = 50;

async function loadTasks() {
  const status = FILTER_STATUS[S.taskFilter] || '';
  const params = [];
  if (status) params.push(`status=${encodeURIComponent(status)}`);
  if (S.convoyFilter > 0) params.push(`convoy_id=${S.convoyFilter}`);
  if (S.sortBy)  params.push(`sort_by=${encodeURIComponent(S.sortBy)}`);
  if (S.sortDir) params.push(`sort_dir=${encodeURIComponent(S.sortDir)}`);
  if (S.showInfra) params.push(`show_infrastructure=1`);
  params.push(`offset=${S.taskOffset}`);
  params.push(`limit=${TASK_PAGE_SIZE}`);
  const qs = `?${params.join('&')}`;
  try {
    const data = await api(`/api/tasks${qs}`);
    S.tasks     = data.tasks  || [];
    S.taskTotal = data.total  || 0;
    renderTasks();
    renderPagination();
  } catch(e) {
    showToast('Failed to load tasks: ' + e.message, 'err');
  }
}

function toggleShowInfra(checked) {
  S.showInfra  = !!checked;
  S.taskOffset = 0;
  syncURL();
  loadTasks();
}

function setTaskFilter(f) {
  S.taskFilter  = f;
  S.taskOffset  = 0;
  syncURL();
  document.querySelectorAll('#tab-tasks .filter-btn').forEach(b => {
    b.classList.toggle('active', b.dataset.filter === f);
  });
  loadTasks();
}

function setSortBy(col) {
  if (S.sortBy === col) {
    S.sortDir = S.sortDir === 'asc' ? 'desc' : 'asc';
  } else {
    S.sortBy  = col;
    S.sortDir = 'asc';
  }
  S.taskOffset = 0;
  syncURL();
  renderSortHeaders();
  loadTasks();
}

function renderSortHeaders() {
  document.querySelectorAll('#tasks-table th[data-sort-col]').forEach(th => {
    const col  = th.dataset.sortCol;
    const icon = th.querySelector('.sort-icon');
    if (!icon) return;
    if (col === S.sortBy) {
      icon.textContent = S.sortDir === 'asc' ? ' ↑' : ' ↓';
      th.classList.add('th-sort-active');
    } else {
      icon.textContent = ' ⇅';
      th.classList.remove('th-sort-active');
    }
  });
}

function renderTasks() {
  const query = ($('task-search').value || '').toLowerCase();
  let tasks = S.tasks;
  if (query) {
    tasks = tasks.filter(t =>
      String(t.id).includes(query) ||
      (t.payload || '').toLowerCase().includes(query) ||
      (t.repo    || '').toLowerCase().includes(query) ||
      (t.type    || '').toLowerCase().includes(query) ||
      (t.status  || '').toLowerCase().includes(query) ||
      (t.owner   || '').toLowerCase().includes(query)
    );
  }

  const tbody = $('tasks-tbody');
  if (!tasks.length) {
    tbody.innerHTML = `<tr><td colspan="11"><div class="empty-state"><span class="icon">📭</span>No tasks match this filter.</div></td></tr>`;
    $('tbadge-tasks').textContent = '';
    return;
  }

  $('tbadge-tasks').textContent = tasks.length;
  tbody.innerHTML = tasks.map(t => {
    const sel = t.id === S.selectedID ? ' selected' : '';
    const retry = t.retry_count > 0 ? `<span style="color:var(--orange)">${t.retry_count}x</span>` : '';
    const prio  = t.priority   > 0 ? `<span style="color:var(--accent)">${t.priority}</span>`
                : t.priority   < 0 ? `<span style="color:var(--text2)">${t.priority}</span>` : '0';
    const runtimeStr = t.status === 'Locked' ? fmtRuntime(t.runtime_seconds) : '';
    const blockedBy = (t.blocked_by && t.blocked_by.length > 0)
      ? 'blocked by ' + t.blocked_by.map(id => `<a onclick="openPanel(${id});event.stopPropagation()" style="cursor:pointer">#${id}</a>`).join(', ')
      : '';
    const infoCell = [
      runtimeStr ? `<span class="runtime-badge">${runtimeStr}</span>` : '',
      blockedBy,
    ].filter(Boolean).join(' ');
    const isInfra = INFRASTRUCTURE_TASK_TYPES.has(t.type || '');
    const typeCell = isInfra
      ? `<span class="dim" style="font-size:11px" title="Fleet infrastructure">${t.type || ''} <span style="opacity:.6">⚙︎</span></span>`
      : (t.type || '');
    return `<tr class="task-row${sel}${isInfra ? ' task-row-infra' : ''}" onclick="openPanel(${t.id})" data-id="${t.id}">
      <td class="mono dim">${t.id}</td>
      <td>${statusPill(t.status)}</td>
      <td class="dim" style="font-size:11px">${escHtml(t.owner || '')}</td>
      <td class="dim">${typeCell}</td>
      <td class="payload-cell">${escHtml(truncate(t.payload, 140))}</td>
      <td class="mono dim" style="font-size:11px">${escHtml(t.repo || '')}</td>
      <td style="text-align:center">${prio}</td>
      <td style="text-align:center">${retry}</td>
      <td class="dim" style="font-size:11px;white-space:nowrap">${infoCell}</td>
      <td class="mono dim" style="font-size:11px">${fmtShortDate(t.created_at)}</td>
      <td class="mono dim" style="font-size:11px;text-align:right">${fmtCost(t.cost_dollars)}</td>
    </tr>`;
  }).join('');
}

// Task types considered fleet infrastructure — kept in sync with
// store.InfrastructureTaskTypes server-side. Used only for UI styling;
// the authoritative filter is applied on the server.
const INFRASTRUCTURE_TASK_TYPES = new Set([
  'FindPRTemplate', 'CreateAskBranch', 'CleanupAskBranch',
  'RebaseAskBranch', 'RebaseAgentBranch', 'RevalidateRepoConfig',
  'WriteMemory', 'ShipConvoy', 'CIFailureTriage', 'MedicReview',
]);

function escHtml(s) {
  return String(s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

// renderMemoryRows formats a DashboardMemory[] as HTML rows — used both for
// the top-level Fleet Memories panel and for per-attempt injected-memory
// expansions.
function renderMemoryRows(memories) {
  return memories.map(m => {
    const oc = m.outcome === 'success' ? 'ok' : 'fail';
    const tags = m.topic_tags
      ? `<div class="mem-tags">${m.topic_tags.split(',').map(t => `<span class="mem-tag">${escHtml(t.trim())}</span>`).join(' ')}</div>`
      : '';
    const source = m.id && m.task_id
      ? `<span class="mem-source">#${m.id} → task #${m.task_id}</span>`
      : '';
    return `<div class="mem-row">
      <div class="mem-outcome ${oc}">${escHtml(m.outcome || '').toUpperCase()}</div>
      <div class="mem-summary">${escHtml(truncate(m.summary || '', 400))}</div>
      ${tags}
      ${m.files_changed ? `<div class="mem-files">${escHtml(m.files_changed)}</div>` : ''}
      ${source}
    </div>`;
  }).join('');
}

// toggleAttemptMemories expands/collapses the injected-memory list for a
// specific attempt row.
function toggleAttemptMemories(attemptNum) {
  const el = document.getElementById(`attempt-memories-${attemptNum}`);
  if (!el) return;
  el.style.display = el.style.display === 'none' ? 'block' : 'none';
}

// ── Task detail panel ─────────────────────────────────────────────────────────
async function openPanel(id) {
  S.selectedID = id;
  document.querySelectorAll('.task-row').forEach(r => {
    r.classList.toggle('selected', Number(r.dataset.id) === id);
  });

  const panel = $('task-panel');
  panel.classList.remove('hidden');

  $('panel-task-id').textContent   = `#${id}`;
  $('panel-task-type').textContent = '';
  $('panel-task-status').innerHTML = '';
  $('panel-title').textContent     = 'Loading…';
  $('panel-actions').innerHTML     = '';
  $('panel-body').innerHTML        = '';

  try {
    const d = await api(`/api/tasks/${id}`);
    S.detail = d;
    renderPanel(d);
  } catch(e) {
    $('panel-body').innerHTML = `<div class="panel-section"><div class="error-box">${escHtml(e.message)}</div></div>`;
  }
}

function closePanel() {
  S.selectedID = null;
  S.detail     = null;
  $('task-panel').classList.add('hidden');
  document.querySelectorAll('.task-row').forEach(r => r.classList.remove('selected'));
}

const REVIEWABLE = ['AwaitingCouncilReview','UnderReview','AwaitingCaptainReview','UnderCaptainReview'];
const CANCELLABLE = ['Pending','Locked','Blocked','Escalated','AwaitingChancellorReview','AwaitingCouncilReview','UnderReview','AwaitingCaptainReview','UnderCaptainReview'];
const RETRYABLE  = ['Failed','Escalated'];

function renderPanel(d) {
  $('panel-task-type').textContent = d.type || '';
  $('panel-task-status').innerHTML = statusPill(d.status);

  const title = truncate(d.broader_goal || d.directive || d.payload || '', 80);
  $('panel-title').textContent = title;

  // Actions
  const btns = [];
  if (REVIEWABLE.includes(d.status)) {
    btns.push(`<button class="action-btn approve-btn" onclick="approveTask(${d.id})">Approve &amp; Merge</button>`);
    btns.push(`<button class="action-btn reject-btn"  onclick="showRejectModal(${d.id})">Reject</button>`);
  }
  if (RETRYABLE.includes(d.status)) {
    btns.push(`<button class="action-btn" onclick="retryTask(${d.id})">Retry</button>`);
  }
  if (!['Completed','Cancelled'].includes(d.status)) {
    btns.push(`<button class="action-btn" onclick="resetTask(${d.id})">Reset to Pending</button>`);
  }
  if (CANCELLABLE.includes(d.status)) {
    btns.push(`<button class="action-btn cancel-btn" onclick="cancelTask(${d.id})">Cancel</button>`);
  }
  // Ship It shortcut: only when the parent convoy has genuinely finished
  // (ConvoyReadyToShip = DraftPROpen + no active tasks + no pending review).
  // Offering the button while fix tasks or rebase conflicts are still in
  // flight would let an operator ship a half-finished convoy.
  if (d.convoy_id > 0 && d.convoy_ready_to_ship) {
    btns.push(`<button class="action-btn ship-btn" onclick="showShip(${d.convoy_id})">🚢 Ship Convoy #${d.convoy_id}</button>`);
  }
  $('panel-actions').innerHTML = btns.join('');

  // Body
  const sections = [];

  // Meta
  const lockedAt = d.locked_at ? fmtTS(d.locked_at) : '—';
  const blockedByLinks = (d.blocked_by && d.blocked_by.length > 0)
    ? d.blocked_by.map(id => `<a onclick="openPanel(${id})" style="cursor:pointer">#${id}</a>`).join(', ')
    : '';

  // Branch cell: if the server returned a web URL (resolved from the repo's
  // remote), render as a clickable link; otherwise plain text. Keeps legacy
  // repos (no remote_url) and test DBs working without special casing.
  const branchCell = d.branch_name
    ? (d.branch_url
        ? `<a href="${escHtml(d.branch_url)}" target="_blank" rel="noopener">${escHtml(d.branch_name)}</a>`
        : escHtml(d.branch_name))
    : '—';

  // PR cell: only rendered when a sub-PR was opened for this task. The state
  // badge mirrors the usual status-pill semantics (Open/Merged/Closed).
  let prRow = '';
  if (d.pr_number) {
    const label = `#${d.pr_number}` + (d.pr_state ? ` <span class="dim">(${escHtml(d.pr_state)})</span>` : '');
    const prCell = d.pr_url
      ? `<a href="${escHtml(d.pr_url)}" target="_blank" rel="noopener">${label}</a>`
      : label;
    prRow = `<span class="meta-key">PR</span><span class="meta-val">${prCell}</span>`;
  }

  sections.push(`
    <div class="panel-section">
      <h3>Details</h3>
      <div class="meta-grid">
        <span class="meta-key">Repo</span>      <span class="meta-val">${escHtml(d.repo || '—')}</span>
        <span class="meta-key">Owner</span>     <span class="meta-val">${escHtml(d.owner || '—')}</span>
        <span class="meta-key">Branch</span>    <span class="meta-val">${branchCell}</span>
        ${prRow}
        <span class="meta-key">Convoy</span>    <span class="meta-val">${d.convoy_id || '—'}</span>
        <span class="meta-key">Retries</span>   <span class="meta-val">${d.retry_count} / infra:${d.infra_failures}</span>
        <span class="meta-key">Priority</span>  <span class="meta-val">${d.priority}</span>
        <span class="meta-key">Locked at</span> <span class="meta-val">${lockedAt}</span>
        <span class="meta-key">Runtime</span>   <span class="meta-val">${fmtRuntime(d.runtime_seconds) || '—'}</span>
        <span class="meta-key">Blocked by</span><span class="meta-val">${blockedByLinks || '—'}</span>
        <span class="meta-key">Cost</span>       <span class="meta-val">${fmtCost(d.cost_dollars)}</span>
      </div>
    </div>`);

  // Goal
  if (d.broader_goal) {
    sections.push(`
      <div class="panel-section">
        <h3>Broader Goal</h3>
        <div class="directive-box">${escHtml(d.broader_goal)}</div>
      </div>`);
  }

  // Directive
  sections.push(`
    <div class="panel-section">
      <h3>Directive</h3>
      <div class="directive-box">${escHtml(d.directive || d.payload || '')}</div>
    </div>`);

  // Error log
  if (d.error_log) {
    sections.push(`
      <div class="panel-section">
        <h3>Error Log</h3>
        <div class="error-box">${escHtml(d.error_log)}</div>
      </div>`);
  }

  // History — each attempt optionally expands to show the memories that
  // were injected into that attempt's prompt.
  if (d.history && d.history.length) {
    const rows = d.history.map(h => {
      const oc = h.outcome === 'success' ? 'ok' : h.outcome === 'failure' ? 'fail' : 'mid';
      const tok = `${(h.tokens_in||0).toLocaleString()} in / ${(h.tokens_out||0).toLocaleString()} out`;
      const injected = h.injected_memories || [];
      const memBadge = injected.length
        ? `<a class="attempt-mem-toggle" onclick="toggleAttemptMemories(${h.attempt});event.stopPropagation()" title="Click to view the ${injected.length} memory entries injected into this attempt">📚 ${injected.length} memor${injected.length === 1 ? 'y' : 'ies'} injected</a>`
        : '';
      const memBlock = injected.length
        ? `<div class="attempt-memories" id="attempt-memories-${h.attempt}" style="display:none">${renderMemoryRows(injected)}</div>`
        : '';
      return `<div class="attempt-row">
        <span class="attempt-num">#${h.attempt}</span>
        <span class="attempt-outcome ${oc}">${escHtml(h.agent || '')} — ${escHtml(h.outcome || '')}</span>
        <span class="attempt-tokens">${tok}</span>
        <span class="attempt-date">${fmtTS(h.created_at)}</span>
        ${memBadge}
      </div>${memBlock}`;
    }).join('');
    sections.push(`<div class="panel-section"><h3>Attempt History</h3>${rows}</div>`);
  }

  // Memories — if the most-recent attempt recorded a snapshot, that's what's
  // shown (exactly what the agent saw). Otherwise it's a live preview of what
  // WOULD be injected on the next claim.
  if (d.memories && d.memories.length) {
    const hasSnapshot = d.history && d.history.length && (d.history[d.history.length - 1].injected_memories || []).length > 0;
    const heading = hasSnapshot
      ? `Fleet Memories <span class="mem-heading-note">— snapshot from last run</span>`
      : `Fleet Memories <span class="mem-heading-note">— live preview (no run yet)</span>`;
    sections.push(`<div class="panel-section"><h3>${heading}</h3>${renderMemoryRows(d.memories)}</div>`);
  }

  // Mail for this task
  if (d.mail && d.mail.length) {
    const rows = d.mail.map(m => `
      <div class="mem-row">
        <div class="meta-grid">
          <span class="meta-key">From/To</span>
          <span class="meta-val">${escHtml(m.from_agent)} → ${escHtml(m.to_agent)}</span>
          <span class="meta-key">Subject</span>
          <span class="meta-val">${escHtml(m.subject)}</span>
        </div>
        <div class="directive-box" style="margin-top:6px;max-height:80px">${escHtml(m.body)}</div>
      </div>`).join('');
    sections.push(`<div class="panel-section"><h3>Task Mail</h3>${rows}</div>`);
  }

  $('panel-body').innerHTML = sections.join('');
}

// ── Task actions ──────────────────────────────────────────────────────────────
async function approveTask(id) {
  try {
    await api(`/api/tasks/${id}/approve`, { method: 'POST' });
    showToast(`Task #${id} approved and merged`, 'ok');
    closePanel();
    loadTasks();
    pollStatus();
  } catch(e) {
    showToast('Approve failed: ' + e.message, 'err');
  }
}

function showRejectModal(id) {
  S.rejectID = id;
  $('reject-task-id').textContent = `#${id}`;
  $('reject-reason').value = '';
  $('reject-modal').classList.remove('hidden');
  setTimeout(() => $('reject-reason').focus(), 50);
}

async function confirmReject() {
  const reason = $('reject-reason').value.trim();
  if (!reason) { showToast('Reason is required', 'err'); return; }
  try {
    await api(`/api/tasks/${S.rejectID}/reject`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ reason }),
    });
    showToast(`Task #${S.rejectID} rejected`, 'ok');
    closeModal('reject-modal');
    closePanel();
    loadTasks();
    pollStatus();
  } catch(e) {
    showToast('Reject failed: ' + e.message, 'err');
  }
}

async function retryTask(id) {
  try {
    await api(`/api/tasks/${id}/retry`, { method: 'POST' });
    showToast(`Task #${id} queued for retry`, 'ok');
    await openPanel(id);
    loadTasks();
    pollStatus();
  } catch(e) {
    showToast('Retry failed: ' + e.message, 'err');
  }
}

async function resetTask(id) {
  try {
    await api(`/api/tasks/${id}/reset`, { method: 'POST' });
    showToast(`Task #${id} reset to Pending`, 'ok');
    await openPanel(id);
    loadTasks();
    pollStatus();
  } catch(e) {
    showToast('Reset failed: ' + e.message, 'err');
  }
}

function cancelTask(id) {
  S.cancelID = id;
  $('cancel-task-id').textContent = `#${id}`;
  $('cancel-requeue-type').value = '';
  $('cancel-modal').classList.remove('hidden');
}

async function confirmCancel() {
  const requeueType = $('cancel-requeue-type').value;
  const id = S.cancelID;
  try {
    const body = requeueType ? JSON.stringify({ requeue_type: requeueType }) : undefined;
    const opts = { method: 'POST' };
    if (body) {
      opts.headers = { 'Content-Type': 'application/json' };
      opts.body = body;
    }
    const res = await api(`/api/tasks/${id}/cancel`, opts);
    if (res && res.requeued_id) {
      showToast(`Task #${id} cancelled — re-queued as ${requeueType} #${res.requeued_id}`, 'ok');
    } else {
      showToast(`Task #${id} cancelled`, 'ok');
    }
    closeModal('cancel-modal');
    closePanel();
    loadTasks();
    pollStatus();
  } catch(e) {
    showToast('Cancel failed: ' + e.message, 'err');
  }
}

// ── E-stop / Resume ───────────────────────────────────────────────────────────
async function triggerEstop() {
  if (!confirm('Trigger E-STOP? This will prevent agents from claiming new tasks.')) return;
  try {
    await api('/api/control/estop', { method: 'POST' });
    showToast('E-STOP engaged', 'err');
    pollStatus();
  } catch(e) {
    showToast('E-stop failed: ' + e.message, 'err');
  }
}

async function triggerResume() {
  try {
    await api('/api/control/resume', { method: 'POST' });
    showToast('Operations resumed', 'ok');
    pollStatus();
  } catch(e) {
    showToast('Resume failed: ' + e.message, 'err');
  }
}

// ── Escalations ───────────────────────────────────────────────────────────────
async function loadEscalations() {
  try {
    const qs = S.escFilter ? `?status=${S.escFilter}` : '';
    const escs = await api(`/api/escalations${qs}`);
    renderEscalations(escs);
  } catch(e) {
    showToast('Failed to load escalations: ' + e.message, 'err');
  }
}

function setEscFilter(f) {
  S.escFilter = f;
  syncURL();
  document.querySelectorAll('#tab-escalations .filter-btn').forEach(b => {
    b.classList.toggle('active', b.dataset.filter === f);
  });
  loadEscalations();
}

function renderEscalations(escs) {
  const el = $('esc-list');
  if (!escs || !escs.length) {
    el.innerHTML = `<div class="empty-state"><span class="icon">✅</span>No escalations.</div>`;
    return;
  }
  el.innerHTML = escs.map(e => {
    const ackd = e.acknowledged_at ? `<span class="meta-key">Acked:</span> ${fmtTS(e.acknowledged_at)}` : '';
    const actions = [];
    if (e.status === 'Open') {
      actions.push(`<button class="action-btn" onclick="ackEscalation(${e.id})">Acknowledge</button>`);
      actions.push(`<button class="action-btn" onclick="closeEscalation(${e.id})">Close</button>`);
      actions.push(`<button class="action-btn approve-btn" onclick="requeueEscalation(${e.id})">Close &amp; Requeue</button>`);
    }
    return `
      <div class="esc-card sev-${e.severity}">
        <div class="esc-header">
          <span class="esc-id">#${e.id}</span>
          <span class="sev-${e.severity}" style="font-size:10px;font-weight:700">${e.severity}</span>
          ${statusPill(e.status)}
          ${e.task_id ? `<span class="esc-task" onclick="jumpToTask(${e.task_id})">task #${e.task_id}</span>` : ''}
          <span class="esc-ts">${fmtTS(e.created_at)}</span>
        </div>
        <div class="esc-msg">${escHtml(e.message)}</div>
        ${ackd ? `<div style="font-size:11px;color:var(--text2);margin-bottom:8px">${ackd}</div>` : ''}
        <div class="esc-actions">${actions.join('')}</div>
      </div>`;
  }).join('');
}

async function ackEscalation(id) {
  try {
    await api(`/api/escalations/${id}/ack`, { method: 'POST' });
    showToast(`Escalation #${id} acknowledged`, 'ok');
    loadEscalations();
    pollStatus();
  } catch(e) { showToast(e.message, 'err'); }
}

async function closeEscalation(id) {
  try {
    await api(`/api/escalations/${id}/close`, { method: 'POST' });
    showToast(`Escalation #${id} closed`, 'ok');
    loadEscalations();
    pollStatus();
  } catch(e) { showToast(e.message, 'err'); }
}

async function requeueEscalation(id) {
  try {
    await api(`/api/escalations/${id}/requeue`, { method: 'POST' });
    showToast(`Escalation #${id} closed and task requeued`, 'ok');
    loadEscalations();
    loadTasks();
    pollStatus();
  } catch(e) { showToast(e.message, 'err'); }
}

function jumpToTask(id) {
  switchTab('tasks');
  setTaskFilter('all');
  // After tasks load, open the panel
  setTimeout(() => openPanel(id), 400);
}

function showConvoyTasks(convoyID, convoyName) {
  S.convoyFilter = convoyID;
  S.taskFilter = 'all';
  document.querySelectorAll('#tab-tasks .filter-btn').forEach(b => {
    b.classList.toggle('active', b.dataset.filter === 'all');
  });
  $('convoy-filter-label').textContent = `Showing tasks for convoy: ${convoyName}  —`;
  $('convoy-filter-banner').style.display = 'flex';
  switchTab('tasks');
}

function clearConvoyFilter() {
  S.convoyFilter = 0;
  S.taskOffset   = 0;
  hideConvoyBanner();
  loadTasks();
}

function renderPagination() {
  const el = $('task-pagination');
  if (!el) return;
  const total  = S.taskTotal;
  const offset = S.taskOffset;
  const limit  = TASK_PAGE_SIZE;
  if (total <= limit) {
    el.innerHTML = '';
    return;
  }
  const page      = Math.floor(offset / limit) + 1;
  const totalPages = Math.ceil(total / limit);
  const from      = offset + 1;
  const to        = Math.min(offset + limit, total);
  const prevDis   = offset === 0 ? ' disabled' : '';
  const nextDis   = offset + limit >= total ? ' disabled' : '';
  el.innerHTML = `
    <button class="page-btn"${prevDis} onclick="prevTaskPage()">&#8592; Prev</button>
    <span class="page-info">Page ${page} of ${totalPages} &nbsp;·&nbsp; ${from}–${to} of ${total}</span>
    <button class="page-btn"${nextDis} onclick="nextTaskPage()">Next &#8594;</button>
  `;
}

function prevTaskPage() {
  if (S.taskOffset === 0) return;
  S.taskOffset = Math.max(0, S.taskOffset - TASK_PAGE_SIZE);
  loadTasks();
}

function nextTaskPage() {
  if (S.taskOffset + TASK_PAGE_SIZE >= S.taskTotal) return;
  S.taskOffset += TASK_PAGE_SIZE;
  loadTasks();
}

function hideConvoyBanner() {
  $('convoy-filter-banner').style.display = 'none';
}

// ── Convoys ───────────────────────────────────────────────────────────────────
async function loadConvoys() {
  try {
    const convoys = await api('/api/convoys');
    S.convoys = convoys;
    renderConvoys(convoys);
    $('tbadge-convoys').textContent = convoys.length || '';
  } catch(e) {
    showToast('Failed to load convoys: ' + e.message, 'err');
  }
}

function renderConvoys(convoys) {
  // Sync filter UI state
  document.querySelectorAll('#tab-convoys .filter-btn').forEach(b => {
    b.classList.toggle('active', b.dataset.cstatus === S.convoyStatusFilter);
  });
  const timeSel = $('convoy-time-filter');
  if (timeSel) timeSel.value = S.convoyTimeFilter;

  let list = convoys || [];

  // Status filter:
  //   'active'    — anything NOT in a terminal state (Active, Failed, AwaitingDraftPR,
  //                 DraftPROpen, Shipping, etc.). Old versions of this filter hid
  //                 DraftPROpen convoys, which is exactly when the Ship It button
  //                 appears — so ops couldn't find convoys that needed their action.
  //   'completed' — terminal-success states (Completed, Shipped).
  const TERMINAL_CONVOY = new Set(['completed','shipped','cancelled','archived']);
  if (S.convoyStatusFilter !== 'all') {
    list = list.filter(c => {
      const st = (c.status || '').toLowerCase();
      if (S.convoyStatusFilter === 'active')    return !TERMINAL_CONVOY.has(st);
      if (S.convoyStatusFilter === 'completed') return st === 'completed' || st === 'shipped';
      return st === S.convoyStatusFilter;
    });
  }

  // Time filter: compare created_at against cutoff
  if (S.convoyTimeFilter !== 'all') {
    const msMap = { '1h': 3600000, '8h': 28800000, '24h': 86400000 };
    const ms = msMap[S.convoyTimeFilter];
    if (ms) {
      const cutoff = Date.now() - ms;
      list = list.filter(c => new Date(c.created_at).getTime() >= cutoff);
    }
  }

  const el = $('convoy-list');
  if (!list.length) {
    const hasFilter = S.convoyStatusFilter !== 'all' || S.convoyTimeFilter !== 'all';
    el.innerHTML = hasFilter
      ? `<div class="empty-state"><span class="icon">🔍</span>No convoys match the current filters.</div>`
      : `<div class="empty-state"><span class="icon">🚀</span>No convoys yet.</div>`;
    return;
  }

  el.innerHTML = list.map(c => {
    const pct = c.total > 0 ? Math.round(100 * c.completed / c.total) : 0;
    const approveBtn = c.has_planned
      ? `<button class="action-btn approve-btn" onclick="approveConvoy(${c.id})">Activate Planned Tasks</button>`
      : '';
    const cancelBtn = c.status === 'Active'
      ? `<button class="action-btn cancel-btn" onclick="cancelConvoy(${c.id})">Cancel Convoy</button>`
      : '';
    // Ship It: only when the fleet has truly quiesced (no pending tasks, no
    // in-flight ConvoyReview). Relying on status='DraftPROpen' alone was a bug —
    // the draft PR exists well before fix tasks, rebase conflicts, and review
    // comments are resolved.
    const shipBtn = c.ready_to_ship
      ? `<button class="action-btn ship-btn" onclick="showShip(${c.id})">Ship It</button>`
      : '';
    const reviewBadge = renderPRReviewBadge(c);
    return `
      <div class="convoy-card">
        <div class="convoy-header">
          <span class="convoy-name" style="cursor:pointer;text-decoration:underline" onclick="showConvoyTasks(${c.id}, ${escHtml(JSON.stringify(c.name || 'Convoy'))})">${escHtml(c.name || 'Convoy')}</span>
          <span class="convoy-id" style="cursor:pointer" onclick="showConvoyTasks(${c.id}, ${escHtml(JSON.stringify(c.name || 'Convoy'))})">#${c.id}</span>
          ${statusPill(c.status)}
          <span class="convoy-ts">${fmtTS(c.created_at)}</span>
        </div>
        <div class="progress-bar-wrap">
          <div class="progress-bar-fill" style="width:${pct}%"></div>
        </div>
        <div class="convoy-footer">
          <span class="convoy-counts">${c.completed} / ${c.total} tasks complete (${pct}%)</span>
          ${reviewBadge}
          <div style="flex:1"></div>
          ${approveBtn}
          ${cancelBtn}
          ${shipBtn}
        </div>
        <div id="pr-review-panel-${c.id}" class="pr-review-panel" style="display:none"></div>
      </div>`;
  }).join('');

  // Re-open any panels that were open before the DOM was rebuilt.
  S.openPRReviewPanels.forEach(id => {
    if ($(`pr-review-panel-${id}`)) togglePRReviewPanel(id, true);
    else S.openPRReviewPanels.delete(id); // convoy no longer in list
  });
}

// renderPRReviewBadge returns a clickable summary badge when the convoy has
// any PR review comments. Clicking it toggles the inline comment panel.
function renderPRReviewBadge(c) {
  const r = c.pr_review_rollup;
  if (!r || !r.total) return '';
  const parts = [];
  // Blocking indicator shown first — this is what the operator needs to know
  // before deciding whether to ship.
  if (r.bot_blocking)       parts.push(`<span title="${r.bot_blocking} bot issue(s) still in progress — fixes must land before shipping" style="color:var(--red);font-weight:600">⛔ ${r.bot_blocking} blocking</span>`);
  if (r.bot_in_scope)       parts.push(`<span title="Bot in-scope fixes (${r.bot_in_scope} total)">🔧 ${r.bot_in_scope}</span>`);
  if (r.bot_out_of_scope)   parts.push(`<span title="Follow-up features">📌 ${r.bot_out_of_scope}</span>`);
  if (r.bot_not_actionable) parts.push(`<span title="Explained to bot">💬 ${r.bot_not_actionable}</span>`);
  if (r.bot_conflicted_loop)parts.push(`<span title="Bot loop escalated" style="color:var(--red)">⚠️ ${r.bot_conflicted_loop}</span>`);
  if (r.human_awaiting)     parts.push(`<span title="Human comments awaiting operator" style="color:var(--accent)">👤 ${r.human_awaiting}</span>`);
  if (!parts.length) return '';
  return `<button class="pr-review-badge" onclick="togglePRReviewPanel(${c.id})" title="Click to view PR review comments">
    ${parts.join(' ')}
  </button>`;
}

// togglePRReviewPanel lazy-loads the convoy's PR review comments inline.
// Pass forceOpen=true to open (or refresh) without toggling — used by
// renderConvoys to restore panels that were open before a list refresh.
async function togglePRReviewPanel(convoyID, forceOpen) {
  const el = $(`pr-review-panel-${convoyID}`);
  if (!el) return;
  if (!forceOpen && el.style.display === 'block') {
    el.style.display = 'none';
    S.openPRReviewPanels.delete(convoyID);
    return;
  }
  el.style.display = 'block';
  S.openPRReviewPanels.add(convoyID);
  el.innerHTML = `<div class="dim" style="padding:10px">Loading comments…</div>`;
  try {
    const data = await api(`/api/convoys/${convoyID}/pr-review-comments`);
    renderPRReviewPanel(el, convoyID, data.comments || []);
  } catch (e) {
    el.innerHTML = `<div style="padding:10px;color:var(--red)">Failed to load: ${escHtml(e.message)}</div>`;
  }
}

function renderPRReviewPanel(el, convoyID, comments) {
  if (!comments.length) {
    el.innerHTML = `<div class="dim" style="padding:10px">No comments yet.</div>`;
    return;
  }
  const rows = comments.map(c => renderPRReviewRow(c)).join('');
  el.innerHTML = `
    <div class="pr-review-header">
      <strong>PR review comments</strong>
      <button class="action-btn" onclick="retriggerPRReview(${convoyID})" style="margin-left:auto">Re-run triage</button>
    </div>
    <table class="pr-review-table">
      <thead><tr>
        <th>Author</th><th>Where</th><th>Classification</th><th>Comment</th><th>Reply / Action</th>
      </tr></thead>
      <tbody>${rows}</tbody>
    </table>`;
}

function renderPRReviewRow(c) {
  const isHuman = c.author_kind === 'human';
  const clsBadge = prReviewClassBadge(c.classification);
  const where = c.path
    ? `<span class="mono" style="font-size:11px">${escHtml(c.path)}${c.line ? ':' + c.line : ''}</span>`
    : `<span class="dim" style="font-size:11px">PR #${c.draft_pr_number}</span>`;
  const body = `<div class="pr-review-body">${escHtml(truncate(c.body, 300))}</div>`;
  let reply;
  if (isHuman && !c.replied_at) {
    // Draft reply + operator actions.
    reply = `
      <textarea class="pr-review-draft" id="pr-draft-${c.id}">${escHtml(c.reply_body || '')}</textarea>
      <div class="pr-review-actions">
        <button class="action-btn" onclick="postHumanReply(${c.id})">Post reply</button>
        <button class="action-btn" onclick="queueFollowup(${c.id})">Queue follow-up</button>
        <button class="action-btn cancel-btn" onclick="dismissComment(${c.id})">Dismiss</button>
      </div>`;
  } else if (c.replied_at) {
    reply = `<div class="pr-review-reply">${escHtml(truncate(c.reply_body || '', 200))}</div>
             <div class="dim" style="font-size:11px">replied ${fmtShortDate(c.replied_at)}</div>`;
  } else if (c.classification === 'in_scope_fix' && c.spawned_task_id) {
    const taskPill = c.spawned_task_status ? statusPill(c.spawned_task_status) : '';
    const resolvedNote = c.thread_resolved_at
      ? `<div class="dim" style="font-size:10px">✓ resolved ${fmtShortDate(c.thread_resolved_at)}</div>`
      : '';
    reply = `<a onclick="openPanel(${c.spawned_task_id})" style="cursor:pointer">→ task #${c.spawned_task_id}</a> ${taskPill}${resolvedNote}`;
  } else if (c.classification === 'out_of_scope' && c.spawned_task_id) {
    const taskPill = c.spawned_task_status ? statusPill(c.spawned_task_status) : '';
    reply = `<a onclick="openPanel(${c.spawned_task_id})" style="cursor:pointer">→ feature #${c.spawned_task_id}</a> ${taskPill}`;
  } else if (c.classification === 'conflicted_loop') {
    reply = `<span style="color:var(--red)">loop escalated — operator required</span>`;
  } else {
    reply = `<span class="dim">—</span>`;
  }
  return `<tr class="pr-review-row${isHuman ? ' pr-review-human' : ''}">
    <td><span class="mono" style="font-size:11px">${escHtml(c.author)}</span>
        <div class="dim" style="font-size:10px">${isHuman ? '👤 human' : '🤖 bot'}</div></td>
    <td>${where}</td>
    <td>${clsBadge}</td>
    <td>${body}</td>
    <td>${reply}</td>
  </tr>`;
}

function prReviewClassBadge(cls) {
  const map = {
    'in_scope_fix':    { label: 'fix queued',  color: 'var(--green)' },
    'out_of_scope':    { label: 'follow-up',   color: 'var(--accent)' },
    'not_actionable':  { label: 'replied',     color: 'var(--text2)' },
    'conflicted_loop': { label: 'loop',        color: 'var(--red)' },
    'human':           { label: 'human',       color: 'var(--purple)' },
    'ignored':         { label: 'dismissed',   color: 'var(--text2)' },
    '':                { label: 'pending',     color: 'var(--yellow)' },
  };
  const m = map[cls] || { label: cls, color: 'var(--text2)' };
  return `<span class="pr-review-cls" style="background:${m.color}">${m.label}</span>`;
}

async function postHumanReply(rowID) {
  const textarea = $(`pr-draft-${rowID}`);
  const body = textarea ? textarea.value : '';
  try {
    await api(`/api/pr-comments/${rowID}/post-reply`, {
      method: 'POST',
      body: JSON.stringify({ body }),
    });
    showToast('Reply posted', 'ok');
    loadConvoys();
  } catch (e) {
    showToast('Post failed: ' + e.message, 'err');
  }
}

async function queueFollowup(rowID) {
  try {
    const res = await api(`/api/pr-comments/${rowID}/queue-followup`, { method: 'POST' });
    showToast(`Feature #${res.feature_id} queued`, 'ok');
    loadConvoys();
  } catch (e) {
    showToast('Queue failed: ' + e.message, 'err');
  }
}

async function dismissComment(rowID) {
  try {
    await api(`/api/pr-comments/${rowID}/dismiss`, { method: 'POST' });
    showToast('Dismissed', 'ok');
    loadConvoys();
  } catch (e) {
    showToast('Dismiss failed: ' + e.message, 'err');
  }
}

async function retriggerPRReview(convoyID) {
  try {
    await api(`/api/convoys/${convoyID}/pr-review-retry`, { method: 'POST' });
    showToast('Triage re-queued', 'ok');
  } catch (e) {
    showToast('Retry failed: ' + e.message, 'err');
  }
}

function setConvoyStatusFilter(f) {
  S.convoyStatusFilter = f;
  syncURL();
  renderConvoys(S.convoys);
}

function setConvoyTimeFilter(f) {
  S.convoyTimeFilter = f;
  syncURL();
  renderConvoys(S.convoys);
}

function cancelConvoy(id) {
  S.cancelConvoyID = id;
  $('convoy-cancel-id').textContent = `#${id}`;
  $('convoy-cancel-modal').classList.remove('hidden');
}

async function confirmCancelConvoy() {
  const id = S.cancelConvoyID;
  try {
    const r = await api(`/api/convoys/${id}/cancel`, { method: 'POST' });
    showToast(`Convoy #${id} cancelled (${r.cancelled} task(s) stopped)`, 'ok');
    closeModal('convoy-cancel-modal');
    loadConvoys();
  } catch(e) {
    showToast('Cancel failed: ' + e.message, 'err');
  }
}

function showShip(id) {
  S.shipConvoyID = id;
  $('ship-modal-convoy').textContent = `#${id}`;
  $('ship-branch-list').innerHTML = '<div class="dim" style="padding:10px">Loading diff…</div>';
  $('ship-summary-line').innerHTML = '';
  $('ship-modal').classList.remove('hidden');
  api(`/api/convoys/${id}/diff-summary`)
    .then(data => renderShipDiff(data))
    .catch(e => {
      $('ship-branch-list').innerHTML = `<div style="padding:10px;color:var(--red)">Failed to load diff: ${escHtml(e.message)}</div>`;
    });
}

function renderShipDiff(data) {
  const branches = data.ask_branches || [];
  if (!branches.length) {
    $('ship-branch-list').innerHTML = '<div class="dim" style="padding:10px">No pending diffs — all branches are clean.</div>';
    $('ship-summary-line').innerHTML = '';
    return;
  }
  let totalAdd = 0, totalDel = 0;
  $('ship-branch-list').innerHTML = branches.map(ab => {
    totalAdd += ab.total_additions || 0;
    totalDel += ab.total_deletions || 0;
    const prLink = ab.draft_pr_url
      ? `<a href="${escHtml(ab.draft_pr_url)}" target="_blank" rel="noopener">PR #${ab.draft_pr_number}</a>`
      : `PR #${ab.draft_pr_number}`;
    const fileRows = (ab.files || []).map(f =>
      `<tr><td class="ship-diff-file">${escHtml(f.path)}</td>` +
      `<td class="ship-diff-add">+${f.additions}</td>` +
      `<td class="ship-diff-del">-${f.deletions}</td></tr>`
    ).join('');
    const body = fileRows
      ? `<table class="ship-diff-table"><tbody>${fileRows}</tbody></table>`
      : '<div class="dim" style="padding:8px 10px;font-size:12px">No changed files.</div>';
    return `
      <div class="ship-branch">
        <div class="ship-branch-hdr">
          ${prLink}
          <span class="ship-branch-name">${escHtml(ab.ask_branch)}</span>
        </div>
        ${body}
      </div>`;
  }).join('');
  $('ship-summary-line').innerHTML =
    `<span>Total: <strong>+${totalAdd}</strong> additions, <strong>-${totalDel}</strong> deletions` +
    ` across ${branches.length} branch${branches.length === 1 ? '' : 'es'}</span>`;
}

async function confirmShip() {
  const id = S.shipConvoyID;
  try {
    const r = await api(`/api/convoys/${id}/ship`, { method: 'POST' });
    showToast(`Convoy #${id} shipped (${r.promoted} PR(s) promoted)`, 'ok');
    closeModal('ship-modal');
    loadConvoys();
  } catch(e) {
    showToast('Ship failed: ' + e.message, 'err');
  }
}

async function approveConvoy(id) {
  try {
    const r = await api(`/api/convoys/${id}/approve`, { method: 'POST' });
    showToast(`Activated ${r.activated} planned task(s)`, 'ok');
    loadConvoys();
    loadTasks();
    pollStatus();
  } catch(e) { showToast(e.message, 'err'); }
}

// ── Agents ─────────────────────────────────────────────────────────────────────
async function loadAgents() {
  try {
    const agents = await api('/api/agents');
    renderAgents(agents);
    $('tbadge-agents').textContent = agents.length || '';
  } catch(e) {
    showToast('Failed to load agents: ' + e.message, 'err');
  }
}

function renderAgents(agents) {
  const tbody = $('agents-tbody');
  if (!agents || !agents.length) {
    tbody.innerHTML = `<tr><td colspan="5"><div class="empty-state"><span class="icon">🤖</span>No registered agents.</div></td></tr>`;
    return;
  }
  tbody.innerHTML = agents.map(a => {
    const busy   = !!a.current_task_id;
    const cls    = busy ? 'agent-busy' : 'agent-idle';
    const taskLink = busy
      ? `<a class="esc-task" onclick="jumpToTask(${a.current_task_id})">#${a.current_task_id}</a>`
      : '<span class="dim">—</span>';
    return `<tr>
      <td class="${cls} mono" style="font-size:12px">${escHtml(a.agent_name)}</td>
      <td class="mono dim" style="font-size:11px">${escHtml(a.repo || '')}</td>
      <td>${taskLink}</td>
      <td>${a.task_status ? statusPill(a.task_status) : ''}</td>
      <td class="mono dim" style="font-size:11px">${a.locked_at ? fmtTS(a.locked_at) : ''}</td>
    </tr>`;
  }).join('');
}

// ── Mail ───────────────────────────────────────────────────────────────────────
async function loadMail() {
  try {
    const mail = await api('/api/mail');
    renderMail(mail);
  } catch(e) {
    showToast('Failed to load mail: ' + e.message, 'err');
  }
}

async function markAllMailRead() {
  try {
    const r = await api('/api/mail/read-all', { method: 'POST' });
    showToast(`Marked ${r.marked} message${r.marked === 1 ? '' : 's'} as read`, 'ok');
    loadMail();
  } catch(e) {
    showToast('Failed: ' + e.message, 'err');
  }
}

function renderMail(mail) {
  const tbody = $('mail-tbody');
  if (!mail || !mail.length) {
    tbody.innerHTML = `<tr><td colspan="6"><div class="empty-state"><span class="icon">📭</span>No mail.</div></td></tr>`;
    return;
  }
  tbody.innerHTML = mail.map(m => {
    const unread = !m.read_at ? ' unread' : ' read';
    return `<tr class="mail-row${unread}" onclick="openMail(${m.id})" data-mail='${JSON.stringify(m).replace(/'/g,"&#39;")}'>
      <td class="mono dim">${m.id}</td>
      <td class="mono">${escHtml(m.from_agent || '')}</td>
      <td class="mono dim">${escHtml(m.to_agent || '')}</td>
      <td class="dim">${escHtml(m.message_type || '')}</td>
      <td>${escHtml(m.subject || '')}</td>
      <td class="mono dim" style="font-size:11px">${fmtTS(m.created_at)}</td>
    </tr>`;
  }).join('');
}

function openMail(id) {
  // find the mail object from DOM
  const row = $('mail-tbody').querySelector(`[data-mail]`);
  // Actually retrieve from API via stored data attr is fragile; read them from a parsed list
  const allRows = $('mail-tbody').querySelectorAll('tr[data-mail]');
  let m = null;
  allRows.forEach(r => {
    try {
      const parsed = JSON.parse(r.getAttribute('data-mail').replace(/&#39;/g, "'"));
      if (parsed.id === id) m = parsed;
    } catch(_) {}
  });
  if (!m) return;

  $('mail-modal-subject').textContent = m.subject || '';
  $('mail-modal-meta').innerHTML = `
    <span class="meta-key">From</span>  <span class="meta-val">${escHtml(m.from_agent)}</span>
    <span class="meta-key">To</span>    <span class="meta-val">${escHtml(m.to_agent)}</span>
    <span class="meta-key">Type</span>  <span class="meta-val">${escHtml(m.message_type || '')}</span>
    <span class="meta-key">Task</span>  <span class="meta-val">${m.task_id || '—'}</span>
    <span class="meta-key">Date</span>  <span class="meta-val">${fmtTS(m.created_at)}</span>
  `;
  // AUDIT-002 / AUDIT-003 (Fix #2): render mail body as plain text.
  // Mail bodies come from every agent + GitHub comments + operator paste,
  // so they're effectively attacker-controlled. textContent assigns the
  // string as text (no HTML parse, no script execution, no URL auto-run).
  // If rich rendering is ever re-introduced, bundle marked + DOMPurify
  // locally under static/ and gate the call on both being loaded — never
  // reinstate the CDN.
  const mailBody = $('mail-modal-body');
  mailBody.textContent = m.body || '';
  $('mail-modal').classList.remove('hidden');

  if (!m.read_at) {
    api(`/api/mail/${id}/read`, { method: 'POST' }).catch(() => {});
    // update DOM
    const rowEl = document.querySelector(`#mail-tbody tr[onclick="openMail(${id})"]`);
    if (rowEl) rowEl.className = 'mail-row read';
    pollStatus();
  }
}

// ── Logs (SSE) ────────────────────────────────────────────────────────────────
function startLogStream() {
  stopLogStream();
  const url = S.logMode === 'fleet' ? '/api/fleet-log' : '/api/events';
  const src = new EventSource(url);
  S.logSource = src;

  const wrap = $('log-wrap');
  wrap.innerHTML = '';

  src.onmessage = evt => {
    let text;
    try {
      text = JSON.parse(evt.data);         // fleet-log: JSON-encoded string
    } catch(_) {
      text = evt.data;                     // holonet: raw JSON object string
    }
    const line = document.createElement('div');
    line.className = 'log-line';
    line.textContent = text;
    wrap.appendChild(line);
    // auto-scroll if near bottom
    if (wrap.scrollHeight - wrap.scrollTop - wrap.clientHeight < 120) {
      wrap.scrollTop = wrap.scrollHeight;
    }
    // cap at 1000 lines
    while (wrap.children.length > 1000) {
      wrap.removeChild(wrap.firstChild);
    }
  };

  src.onerror = () => {
    // EventSource auto-reconnects
  };
}

function stopLogStream() {
  if (S.logSource) {
    S.logSource.close();
    S.logSource = null;
  }
}

function switchLog(mode) {
  S.logMode = mode;
  syncURL();
  document.querySelectorAll('#tab-logs .filter-btn').forEach(b => {
    b.classList.toggle('active', b.dataset.log === mode);
  });
  if (S.activeTab === 'logs') startLogStream();
}

function clearLog() {
  $('log-wrap').innerHTML = '';
}

// ── Add task modal ─────────────────────────────────────────────────────────────
async function showAddModal() {
  if (!S.repos.length) {
    try { S.repos = await api('/api/repos'); } catch(_) {}
  }
  const sel = $('add-repo');
  sel.innerHTML = '<option value="">— select —</option>' +
    S.repos.map(r => `<option value="${escHtml(r)}">${escHtml(r)}</option>`).join('');
  $('add-payload').value  = '';
  $('add-priority').value = '0';
  $('add-type').value     = '';
  S.addIdempotencyKey = genUUID();
  onAddTypeChange();
  $('add-modal').classList.remove('hidden');
  setTimeout(() => $('add-payload').focus(), 50);
}

function onAddTypeChange() {
  const type = $('add-type').value;
  $('add-repo-row').style.display = (type === '' || type === 'Investigate' || type === 'Audit') ? '' : 'none';
  const repoLabel = $('add-repo-label');
  if (repoLabel) {
    repoLabel.textContent = 'Repo (optional — leave blank for fleet-wide)';
  }
}

async function submitAddTask() {
  const type    = $('add-type').value;
  const payload = $('add-payload').value.trim();
  const repo    = $('add-repo').value;
  const priority = parseInt($('add-priority').value || '0', 10);

  if (!payload) { showToast('Payload is required', 'err'); return; }

  try {
    const r = await api('/api/add', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ type, payload, repo, priority, idempotency_key: S.addIdempotencyKey || '' }),
    });
    if (r.duplicate) {
      showToast(`Already queued as task #${r.id}`, 'ok');
    } else {
      showToast(`Task #${r.id} queued`, 'ok');
    }
    closeModal('add-modal');
    loadTasks();
    pollStatus();
  } catch(e) {
    showToast('Failed to queue task: ' + e.message, 'err');
  }
}

// ── JIRA-from-UI ──────────────────────────────────────────────────────────────
// Inline "Queue from Jira" form rendered above the Tasks tab's stats bar.
// Posts to POST /api/feature/from-jira; the handler calls
// agents.QueueFeatureFromJira (the reusable core extracted from
// `force add-jira`). On success: surface task id + first 200 chars of
// the fetched description in the inline status area; refresh the task
// table so the new row shows up. On failure: inline error toast, form
// stays populated so the operator can correct + retry.
async function submitJiraFeature() {
  const ticketEl   = $('jira-ticket-id');
  const priorityEl = $('jira-priority');
  const planOnlyEl = $('jira-plan-only');
  const btn        = $('jira-submit-btn');
  const result     = $('jira-result');
  if (!ticketEl) return;

  const ticket   = (ticketEl.value || '').trim().toUpperCase();
  const priority = parseInt(priorityEl.value || '0', 10);
  const planOnly = !!planOnlyEl.checked;

  if (!ticket) { showToast('Ticket id required', 'err'); ticketEl.focus(); return; }
  if (!/^[A-Z]+-\d+$/.test(ticket)) {
    showToast('Ticket id must look like ABC-123', 'err');
    ticketEl.focus();
    return;
  }

  // Lock the form while the LLM fetch runs (Atlassian MCP can be slow).
  btn.disabled = true;
  const originalLabel = btn.textContent;
  btn.textContent = 'Fetching…';
  if (result) { result.textContent = `Fetching ${ticket}…`; result.style.color = 'var(--text2)'; }

  try {
    const r = await api('/api/feature/from-jira', {
      method:  'POST',
      headers: { 'Content-Type': 'application/json' },
      body:    JSON.stringify({ ticket_id: ticket, priority, plan_only: planOnly }),
    });
    showToast(`Task #${r.task_id} queued from ${ticket}`, 'ok');
    if (result) {
      result.style.color = 'var(--text1)';
      result.textContent = `#${r.task_id}: ${truncate(r.summary || '', 200)}`;
      result.title       = r.summary || '';
    }
    ticketEl.value     = '';
    priorityEl.value   = '5';
    planOnlyEl.checked = false;
    // Refresh the task list so the queued Feature shows up.
    if (typeof loadTasks === 'function') { loadTasks(); }
    if (typeof pollStats === 'function') { pollStats(); }
  } catch(e) {
    showToast('Queue from Jira failed: ' + e.message, 'err');
    if (result) {
      result.style.color = 'var(--err)';
      result.textContent = 'Failed: ' + e.message;
    }
  } finally {
    btn.disabled    = false;
    btn.textContent = originalLabel;
  }
}

// Submit-on-Enter for the ticket id input — purely a UX nicety so the
// operator doesn't have to mouse to the button.
document.addEventListener('DOMContentLoaded', () => {
  const ticketEl = document.getElementById('jira-ticket-id');
  if (ticketEl) {
    ticketEl.addEventListener('keydown', e => {
      if (e.key === 'Enter') { e.preventDefault(); submitJiraFeature(); }
    });
  }
});

// ── Modal helpers ─────────────────────────────────────────────────────────────
function closeModal(id) {
  $(id).classList.add('hidden');
}

// close modal on backdrop click
document.querySelectorAll('.modal-backdrop').forEach(el => {
  el.addEventListener('click', e => {
    if (e.target === el) el.classList.add('hidden');
  });
});

// keyboard shortcuts
document.addEventListener('keydown', e => {
  if (e.key === 'Escape') {
    document.querySelectorAll('.modal-backdrop:not(.hidden)').forEach(m => m.classList.add('hidden'));
    closePanel();
  }
});

// ── Knowledge base (Fleet Memory) ─────────────────────────────────────────────
const MEM = { outcome: '', repo: '', data: [], openID: null };

function setMemFilter(key, val) {
  MEM[key] = val;
  if (key === 'outcome') {
    document.querySelectorAll('#tab-knowledge .filter-btn').forEach(b => {
      b.classList.toggle('active', b.dataset.kout === val);
    });
  }
  loadMemories();
}

async function loadMemories() {
  const repo    = $('mem-repo-filter').value;
  const search  = ($('mem-search').value || '').trim();
  const qs = new URLSearchParams();
  if (MEM.outcome) qs.set('outcome', MEM.outcome);
  if (repo)        qs.set('repo',    repo);
  if (search)      qs.set('q',       search);
  qs.set('limit', '500');

  try {
    const data = await api('/api/memories?' + qs.toString());
    MEM.data = data;
    $('tbadge-knowledge').textContent = data.length || '';
    renderMemories(data);
  } catch(e) {
    showToast('Failed to load memories: ' + e.message, 'err');
  }
}

async function loadMemoryRepos() {
  try {
    const repos = await api('/api/repos');
    const sel = $('mem-repo-filter');
    const cur = sel.value;
    sel.innerHTML = '<option value="">All repos</option>' +
      repos.map(r => `<option value="${escHtml(r)}"${r===cur?' selected':''}>${escHtml(r)}</option>`).join('');
  } catch(_) {}
}

function renderMemories(data) {
  const tbody = $('mem-tbody');
  if (!data || !data.length) {
    tbody.innerHTML = `<tr><td colspan="8"><div class="empty-state"><span class="icon">🧠</span>No memories yet — they accumulate as tasks complete.</div></td></tr>`;
    return;
  }
  tbody.innerHTML = data.map(m => {
    const oc = m.outcome === 'success'
      ? `<span class="status s-Completed">success</span>`
      : `<span class="status s-Failed">failure</span>`;
    const taskLink = m.task_id
      ? `<span class="esc-task" onclick="jumpToTask(${m.task_id})">#${m.task_id}</span>`
      : '—';
    return `<tr>
      <td class="mono dim">${m.id}</td>
      <td class="mono dim" style="font-size:11px">${escHtml(m.repo || '')}</td>
      <td>${taskLink}</td>
      <td>${oc}</td>
      <td class="mem-summary-cell" onclick="openMemory(${m.id})" title="${escHtml(m.summary)}">${escHtml(truncate(m.summary, 120))}</td>
      <td class="mem-files-cell" title="${escHtml(m.files_changed)}">${escHtml(m.files_changed || '')}</td>
      <td class="mono dim" style="font-size:11px">${fmtTS(m.created_at)}</td>
      <td><button class="del-btn" onclick="deleteMem(${m.id})" title="Delete memory">✕</button></td>
    </tr>`;
  }).join('');
}

function openMemory(id) {
  const m = MEM.data.find(x => x.id === id);
  if (!m) return;
  MEM.openID = id;
  $('mem-modal-title').textContent = `Memory #${m.id} — ${m.repo}`;
  $('mem-modal-meta').innerHTML = `
    <span class="meta-key">Repo</span>    <span class="meta-val">${escHtml(m.repo || '—')}</span>
    <span class="meta-key">Task</span>    <span class="meta-val">${m.task_id || '—'}</span>
    <span class="meta-key">Outcome</span> <span class="meta-val">${m.outcome}</span>
    <span class="meta-key">Files</span>   <span class="meta-val">${escHtml(m.files_changed || '—')}</span>
    <span class="meta-key">Date</span>    <span class="meta-val">${fmtTS(m.created_at)}</span>
  `;
  $('mem-modal-summary').textContent = m.summary;
  $('mem-modal').classList.remove('hidden');
}

async function deleteMem(id) {
  if (!confirm(`Delete memory #${id}? This cannot be undone.`)) return;
  try {
    await api(`/api/memories/${id}`, { method: 'DELETE' });
    showToast(`Memory #${id} deleted`, 'ok');
    loadMemories();
  } catch(e) {
    showToast('Delete failed: ' + e.message, 'err');
  }
}

async function deleteMemFromModal() {
  if (!MEM.openID) return;
  await deleteMem(MEM.openID);
  closeModal('mem-modal');
}

// ── Polling ───────────────────────────────────────────────────────────────────
function startPolling() {
  pollStatus();
  setInterval(pollStatus, 5000);

  pollStats();
  setInterval(pollStats, 10000);

  setInterval(() => {
    switch(S.activeTab) {
      case 'tasks':       loadTasks();       break;
      case 'escalations': loadEscalations(); break;
      case 'convoys':     loadConvoys();     break;
      case 'agents':      loadAgents();      break;
      case 'mail':        loadMail();        break;
      case 'knowledge':   loadMemories();    break;
    }
  }, 12000);
}

// ── Boot ──────────────────────────────────────────────────────────────────────
function initFromURL() {
  const p = new URLSearchParams(window.location.search);

  const tab = p.get('tab');
  if (['tasks','escalations','convoys','agents','mail','knowledge','logs'].includes(tab)) S.activeTab = tab;

  const status = p.get('status');
  if (status && Object.prototype.hasOwnProperty.call(FILTER_STATUS, status)) S.taskFilter = status;

  const search = p.get('search');
  if (search !== null) { const el = $('task-search'); if (el) el.value = search; }

  const sb = p.get('sort_by');
  const sd = p.get('sort_dir');
  if (sb) S.sortBy  = sb;
  if (sd) S.sortDir = sd;

  const es = p.get('esc_status');
  if (['Open','Acknowledged','Closed'].includes(es)) S.escFilter = es;

  const cs = p.get('convoy_status');
  const ct = p.get('convoy_since');
  if (['all', 'active', 'completed'].includes(cs)) S.convoyStatusFilter = cs;
  if (['all', '1h', '8h', '24h'].includes(ct))     S.convoyTimeFilter   = ct;

  const lm = p.get('log_mode');
  if (['fleet', 'holonet'].includes(lm)) S.logMode = lm;

  S.showInfra = p.get('show_infra') === '1';
  const infraToggle = $('show-infra-toggle');
  if (infraToggle) infraToggle.checked = S.showInfra;
}

window.addEventListener('popstate', () => {
  initFromURL();
  // Sync tab and filter button UI without pushing another history entry
  document.querySelectorAll('.tab-btn').forEach(b => {
    b.classList.toggle('active', b.dataset.tab === S.activeTab);
  });
  document.querySelectorAll('.tab-pane').forEach(p => {
    p.classList.toggle('active', p.id === 'tab-' + S.activeTab);
  });
  document.querySelectorAll('#tab-tasks .filter-btn').forEach(b => {
    b.classList.toggle('active', b.dataset.filter === S.taskFilter);
  });
  document.querySelectorAll('#tab-escalations .filter-btn').forEach(b => {
    b.classList.toggle('active', b.dataset.filter === S.escFilter);
  });
  document.querySelectorAll('#tab-logs .filter-btn').forEach(b => {
    b.classList.toggle('active', b.dataset.log === S.logMode);
  });
  renderSortHeaders();
  if (S.activeTab !== 'logs') stopLogStream();
  switch (S.activeTab) {
    case 'tasks':       loadTasks(); break;
    case 'escalations': loadEscalations(); break;
    case 'convoys':     loadConvoys(); break;
    case 'agents':      loadAgents(); break;
    case 'mail':        loadMail(); break;
    case 'knowledge':   loadMemoryRepos().then(() => loadMemories()); break;
    case 'logs':        startLogStream(); break;
  }
});

let _searchDebounce = null;
$('task-search').addEventListener('input', () => {
  renderTasks();
  clearTimeout(_searchDebounce);
  _searchDebounce = setTimeout(syncURL, 300);
});

initFromURL();
startPolling();
switchTab(S.activeTab);
renderSortHeaders();

// ── D3 P6A.1 — Three-surface IA + nav rebuild ──────────────────────────────
//
// Top-level surfaces: pulse | briefing | reflection | legacy.
// Default landing is pulse. Hash fragments drive routing:
//   #/pulse                  → Pulse surface
//   #/briefing               → Briefing surface
//   #/briefing/decision/<id> → Briefing focus mode (subroute, 6A.10)
//   #/reflection             → Reflection surface
//   #/legacy/<tab>           → existing tab UI (tasks/escalations/...)
//
// Subsequent tasks (6A.7–6A.14) fill in the surface-specific rendering;
// 6A.3 adds the keymap (Cmd-1/2/3, j/k, ?, etc.). The Cmd-1/2/3 stub
// below is the keymap-stub-only contract for 6A.1.
const SURFACE_NAMES = ['pulse', 'briefing', 'reflection', 'legacy'];
const SURFACE_DEFAULT = 'pulse';

function currentSurfaceFromHash() {
  const h = (window.location.hash || '').replace(/^#/, '');
  if (!h) return SURFACE_DEFAULT;
  // #/pulse | #/legacy/tasks | #/briefing/decision/123 → "pulse" / "legacy" / "briefing"
  const m = h.match(/^\/?(\w+)/);
  if (!m) return SURFACE_DEFAULT;
  const name = m[1];
  return SURFACE_NAMES.includes(name) ? name : SURFACE_DEFAULT;
}

function legacyTabFromHash() {
  // For #/legacy/<tab>, return <tab>; else return null.
  const h = (window.location.hash || '').replace(/^#/, '');
  const m = h.match(/^\/?legacy\/(\w+)/);
  return m ? m[1] : null;
}

function showSurface(name) {
  if (!SURFACE_NAMES.includes(name)) name = SURFACE_DEFAULT;

  // Toggle pane visibility — only the active surface's pane is shown.
  document.querySelectorAll('.surface-pane').forEach(p => {
    p.hidden = (p.id !== 'surface-' + name + '-pane');
  });

  // Toggle nav-link active state.
  document.querySelectorAll('.surface-link').forEach(a => {
    a.classList.toggle('surface-link-active', a.dataset.surface === name);
  });

  // For legacy, honour the sub-tab if the fragment carries one;
  // else default to whatever tab the SPA is on.
  if (name === 'legacy') {
    const sub = legacyTabFromHash();
    if (sub && sub !== S.activeTab) {
      switchTab(sub);
    }
  }
}

function navigateToSurface(name, opts) {
  opts = opts || {};
  const fragment = name === 'legacy'
    ? '#/legacy/' + (opts.legacyTab || S.activeTab || 'tasks')
    : '#/' + name;
  if (window.location.hash !== fragment) {
    window.location.hash = fragment;
  } else {
    showSurface(name);
  }
}

// Hash-change routing — browser back/forward respects surface changes.
window.addEventListener('hashchange', () => showSurface(currentSurfaceFromHash()));

// Also bind the legacy tab click flow so navigating tabs updates the
// fragment (preserves "URL is the source of truth" for legacy too).
const _origSwitchTab = switchTab;
switchTab = function(name) {
  _origSwitchTab(name);
  // If we're on the legacy surface, mirror the active tab into the fragment.
  if (currentSurfaceFromHash() === 'legacy') {
    const desired = '#/legacy/' + name;
    if (window.location.hash !== desired) {
      // Avoid clobbering history if the only change is sub-tab (replace, not push).
      try { history.replaceState(null, '', desired); } catch (_) {}
    }
  }
};

// Keyboard shortcuts (Cmd-1/2/3, /, ?, Esc, j/k, etc.) are dispatched
// from keymap.js (D3 P6A.3). The 6A.1 stub listener was removed when
// 6A.3 landed. navigateToSurface is exported on window so the keymap
// can call it.
window.navigateToSurface = navigateToSurface;
window.currentSurfaceFromHash = currentSurfaceFromHash;

// Boot: route to the surface from the URL (default pulse).
showSurface(currentSurfaceFromHash());
// If the URL has no fragment, set it to #/pulse so reload/back works.
if (!window.location.hash) {
  try { history.replaceState(null, '', '#/pulse'); } catch (_) {}
}

// ── D3 P6A.2 — Heartbeat banner ───────────────────────────────────────────
// Poll /api/dashboard/health every 30s. Show the yellow banner when the
// most recent heartbeat is older than 60s (the API reports `fresh: false`).
async function refreshDashboardHealth() {
  try {
    const r = await fetch('/api/dashboard/health');
    if (!r.ok) return;
    const data = await r.json();
    const banner = document.getElementById('dashboard-health-banner');
    const msg    = document.getElementById('dashboard-health-msg');
    if (!banner || !msg) return;
    if (data.fresh) {
      banner.classList.add('hidden');
    } else {
      msg.textContent = 'Dashboard last successfully ticked ' + data.message + ' — the process may have just restarted.';
      banner.classList.remove('hidden');
    }
  } catch (_) { /* network error during poll — leave banner state untouched */ }
}
refreshDashboardHealth();
setInterval(refreshDashboardHealth, 30000);

// ── D3 P6B SPA wiring (polish-pass iteration 2 / C1) ────────────────────
//
// Wires the Reflection surface's three sub-tabs (Diagnostics, Reflection,
// Ask) to the P6B endpoints. Each renderer is a vanilla-JS function that
// fetch()es JSON, builds a small <table>/<pre> from the response, and
// appends it under the surface pane. No frameworks; no shadow DOM; no
// build step.
//
// Endpoints exercised:
//   GET  /api/drill/convoy/:id            (timeline + spend rollup)
//   GET  /api/drill/task/:id              (single-task event stream)
//   GET  /api/drill/event/:kind/:id       (single event body)
//   GET  /api/drill/search?q=…            (free-text drill search)
//   POST /api/drill/replay/:kind/:id      (re-run historical decision)
//   GET  /api/annotations                 (operator event annotations)
//   POST /api/ask                         (free-form ask)
//   GET  /api/reflection/calibration      (per-bucket calibration scoreboard)
//   GET  /api/reflection/learning         (latest learning panel)
//   POST /api/reflection/learning         (refresh now)
//   POST /api/reflection/retro/generate   (build markdown draft)
//   POST /api/reflection/retro/save       (write to docs/retros/<date>.md)

// Sub-tab switcher for the Reflection surface.
function activateReflectionTab(name) {
  document.querySelectorAll('.reflection-tab').forEach(b => {
    b.classList.toggle('reflection-tab-active', b.dataset.reflectionTab === name);
  });
  document.querySelectorAll('.reflection-pane').forEach(p => {
    p.hidden = p.id !== ('reflection-pane-' + name);
  });
}
document.querySelectorAll('.reflection-tab').forEach(b => {
  b.addEventListener('click', () => activateReflectionTab(b.dataset.reflectionTab));
});

// Tiny helpers — keep DOM-construction scoped to the Reflection surface.
function _refTextNode(s) { return document.createTextNode(s == null ? '' : String(s)); }
function _refSafeHTML(target, html) {
  // Fail-closed: if the target element doesn't exist, do nothing rather
  // than throwing. Production wiring may render a sub-tab before its
  // panes are present (race during initial route hydration).
  const el = (typeof target === 'string') ? document.getElementById(target) : target;
  if (!el) return;
  // We trust ourselves to never inject untrusted strings — payload is
  // built from JSON we just fetched. Still, route IDs and labels through
  // textContent below; this innerHTML is for layout chrome only.
  el.innerHTML = html;
}
function _refRenderTable(targetID, headers, rows) {
  const el = document.getElementById(targetID);
  if (!el) return;
  el.innerHTML = '';
  if (!rows || rows.length === 0) {
    const empty = document.createElement('div');
    empty.className = 'reflection-empty';
    empty.textContent = '(no rows)';
    el.appendChild(empty);
    return;
  }
  const table = document.createElement('table');
  table.className = 'reflection-data-table';
  const thead = document.createElement('thead');
  const headRow = document.createElement('tr');
  headers.forEach(h => {
    const th = document.createElement('th');
    th.textContent = h;
    headRow.appendChild(th);
  });
  thead.appendChild(headRow);
  table.appendChild(thead);
  const tbody = document.createElement('tbody');
  rows.forEach(r => {
    const tr = document.createElement('tr');
    r.forEach(cell => {
      const td = document.createElement('td');
      td.appendChild(_refTextNode(cell));
      tr.appendChild(td);
    });
    tbody.appendChild(tr);
  });
  table.appendChild(tbody);
  el.appendChild(table);
}

// — Drill: free-text search.
async function runDrillSearch() {
  const inputEl = document.getElementById('drill-search-input');
  if (!inputEl) return;
  const q = inputEl.value.trim();
  if (!q) {
    _refRenderTable('drill-search-results', ['kind', 'ref_id', 'snippet'], []);
    return;
  }
  try {
    const r = await fetch('/api/drill/search?q=' + encodeURIComponent(q));
    if (!r.ok) {
      _refSafeHTML('drill-search-results', '<div class="reflection-error">HTTP ' + r.status + '</div>');
      return;
    }
    const data = await r.json();
    const rows = (data.results || []).map(x => [x.kind, x.ref_id, x.snippet || '']);
    _refRenderTable('drill-search-results', ['kind', 'ref_id', 'snippet'], rows);
  } catch (e) {
    _refSafeHTML('drill-search-results', '<div class="reflection-error">' + e.message + '</div>');
  }
}
window.runDrillSearch = runDrillSearch;

// — Drill: convoy timeline.
async function loadDrillConvoy() {
  const idEl = document.getElementById('drill-convoy-id');
  const id = parseInt(idEl && idEl.value, 10);
  if (!id) return;
  try {
    const r = await fetch('/api/drill/convoy/' + id);
    if (!r.ok) {
      _refSafeHTML('drill-convoy-events', '<div class="reflection-error">HTTP ' + r.status + '</div>');
      return;
    }
    const data = await r.json();
    const rows = (data.events || []).map(e => [e.at || '', e.kind || '', e.label || '', e.ref_id || '']);
    _refRenderTable('drill-convoy-events', ['at', 'kind', 'label', 'ref_id'], rows);
  } catch (e) {
    _refSafeHTML('drill-convoy-events', '<div class="reflection-error">' + e.message + '</div>');
  }
}
window.loadDrillConvoy = loadDrillConvoy;

// — Drill: task timeline.
async function loadDrillTask() {
  const idEl = document.getElementById('drill-task-id');
  const id = parseInt(idEl && idEl.value, 10);
  if (!id) return;
  try {
    const r = await fetch('/api/drill/task/' + id);
    if (!r.ok) {
      _refSafeHTML('drill-task-events', '<div class="reflection-error">HTTP ' + r.status + '</div>');
      return;
    }
    const data = await r.json();
    const rows = (data.events || []).map(e => [e.at || '', e.kind || '', e.label || '', e.ref_id || '']);
    _refRenderTable('drill-task-events', ['at', 'kind', 'label', 'ref_id'], rows);
  } catch (e) {
    _refSafeHTML('drill-task-events', '<div class="reflection-error">' + e.message + '</div>');
  }
}
window.loadDrillTask = loadDrillTask;

// — Drill: single event detail.
async function loadDrillEvent() {
  const kindEl = document.getElementById('drill-event-kind');
  const idEl = document.getElementById('drill-event-id');
  const kind = kindEl && kindEl.value.trim();
  const id = parseInt(idEl && idEl.value, 10);
  if (!kind || !id) return;
  try {
    const r = await fetch('/api/drill/event/' + encodeURIComponent(kind) + '/' + id);
    if (!r.ok) {
      _refSafeHTML('drill-event-detail', '<div class="reflection-error">HTTP ' + r.status + '</div>');
      return;
    }
    const data = await r.json();
    const pre = document.createElement('pre');
    pre.className = 'reflection-pre';
    pre.textContent = JSON.stringify(data, null, 2);
    const target = document.getElementById('drill-event-detail');
    if (target) {
      target.innerHTML = '';
      target.appendChild(pre);
    }
  } catch (e) {
    _refSafeHTML('drill-event-detail', '<div class="reflection-error">' + e.message + '</div>');
  }
}
window.loadDrillEvent = loadDrillEvent;

// — Replay: re-run a historical decision.
async function runReplay() {
  const kindEl = document.getElementById('replay-event-kind');
  const idEl = document.getElementById('replay-event-id');
  const kind = kindEl && kindEl.value.trim();
  const id = parseInt(idEl && idEl.value, 10);
  if (!kind || !id) return;
  try {
    const r = await fetch('/api/drill/replay/' + encodeURIComponent(kind) + '/' + id, { method: 'POST' });
    if (!r.ok) {
      _refSafeHTML('replay-result', '<div class="reflection-error">HTTP ' + r.status + '</div>');
      return;
    }
    const data = await r.json();
    const pre = document.createElement('pre');
    pre.className = 'reflection-pre';
    pre.textContent = JSON.stringify(data, null, 2);
    const target = document.getElementById('replay-result');
    if (target) {
      target.innerHTML = '';
      target.appendChild(pre);
    }
  } catch (e) {
    _refSafeHTML('replay-result', '<div class="reflection-error">' + e.message + '</div>');
  }
}
window.runReplay = runReplay;

// — Annotations: list.
async function refreshAnnotations() {
  try {
    const r = await fetch('/api/annotations');
    if (!r.ok) {
      _refSafeHTML('annotations-list', '<div class="reflection-error">HTTP ' + r.status + '</div>');
      return;
    }
    const data = await r.json();
    const rows = (data.annotations || []).map(a => [a.id || '', a.kind || '', a.ref_id || '', a.flag || '', a.text || '', a.noted_at || '']);
    _refRenderTable('annotations-list', ['id', 'kind', 'ref_id', 'flag', 'text', 'noted_at'], rows);
  } catch (e) {
    _refSafeHTML('annotations-list', '<div class="reflection-error">' + e.message + '</div>');
  }
}
window.refreshAnnotations = refreshAnnotations;

// — Calibration scoreboard (per-bucket).
async function refreshCalibration() {
  try {
    const r = await fetch('/api/reflection/calibration');
    if (!r.ok) {
      _refSafeHTML('calibration-scoreboard', '<div class="reflection-error">HTTP ' + r.status + '</div>');
      return;
    }
    const data = await r.json();
    const buckets = data.sample_stats_by_bucket || [];
    const rows = buckets.map(b => [b.selection_bucket || '', b.confirmed || 0, b.overridden || 0, b.total || 0, (b.accuracy_pct != null ? b.accuracy_pct + '%' : '')]);
    _refRenderTable('calibration-scoreboard', ['bucket', 'confirmed', 'overridden', 'total', 'accuracy'], rows);
  } catch (e) {
    _refSafeHTML('calibration-scoreboard', '<div class="reflection-error">' + e.message + '</div>');
  }
}
window.refreshCalibration = refreshCalibration;

// — Fleet learning panel.
async function refreshLearning() {
  // Trigger a refresh first (POST), then GET the latest row.
  try {
    await fetch('/api/reflection/learning', { method: 'POST' });
  } catch (_) { /* refresh is best-effort */ }
  try {
    const r = await fetch('/api/reflection/learning');
    if (!r.ok) {
      _refSafeHTML('learning-panel', '<div class="reflection-error">HTTP ' + r.status + '</div>');
      return;
    }
    const data = await r.json();
    const target = document.getElementById('learning-panel');
    if (!target) return;
    target.innerHTML = '';
    if (data.prose) {
      const pre = document.createElement('pre');
      pre.className = 'reflection-pre';
      pre.textContent = data.prose;
      target.appendChild(pre);
    } else {
      target.textContent = '(no learning panel rendered yet)';
    }
    if (data.sources && data.sources.length > 0) {
      const div = document.createElement('div');
      div.className = 'reflection-help';
      div.textContent = 'Cited evidence: ' + data.sources.join(', ');
      target.appendChild(div);
    }
  } catch (e) {
    _refSafeHTML('learning-panel', '<div class="reflection-error">' + e.message + '</div>');
  }
}
window.refreshLearning = refreshLearning;

// — Retro generator + save.
let _lastRetroPayload = null;
async function generateRetro() {
  try {
    const r = await fetch('/api/reflection/retro/generate', { method: 'POST' });
    if (!r.ok) {
      _refSafeHTML('retro-markdown', 'HTTP ' + r.status);
      return;
    }
    const data = await r.json();
    _lastRetroPayload = data;
    const target = document.getElementById('retro-markdown');
    if (target) target.textContent = data.markdown || '(empty markdown)';
  } catch (e) {
    _refSafeHTML('retro-markdown', e.message);
  }
}
async function saveRetro() {
  if (!_lastRetroPayload) {
    alert('Generate a retro first.');
    return;
  }
  try {
    const r = await fetch('/api/reflection/retro/save', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        suggested_path: _lastRetroPayload.suggested_path,
        markdown: _lastRetroPayload.markdown,
      }),
    });
    if (!r.ok) {
      alert('save failed: HTTP ' + r.status);
      return;
    }
    const data = await r.json();
    alert('Saved to: ' + (data.absolute_path || data.path || _lastRetroPayload.suggested_path));
  } catch (e) {
    alert('save failed: ' + e.message);
  }
}
window.generateRetro = generateRetro;
window.saveRetro = saveRetro;

// — Ask: free-form question.
async function runAsk() {
  const qEl = document.getElementById('ask-question');
  const q = qEl && qEl.value.trim();
  if (!q) return;
  try {
    const r = await fetch('/api/ask', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ question: q }),
    });
    if (!r.ok) {
      _refSafeHTML('ask-answer', '<div class="reflection-error">HTTP ' + r.status + '</div>');
      return;
    }
    const data = await r.json();
    const target = document.getElementById('ask-answer');
    if (!target) return;
    target.innerHTML = '';
    const ans = document.createElement('div');
    ans.className = 'reflection-pre';
    ans.textContent = data.answer || '(no answer)';
    target.appendChild(ans);
    if (data.cite_links && data.cite_links.length > 0) {
      const div = document.createElement('div');
      div.className = 'reflection-help';
      div.textContent = 'Citations: ' + data.cite_links.map(c => c.kind + '/' + c.id).join(', ');
      target.appendChild(div);
    }
  } catch (e) {
    _refSafeHTML('ask-answer', '<div class="reflection-error">' + e.message + '</div>');
  }
}
window.runAsk = runAsk;

// ── D4 fix-loop-1 α — Security + Senate dashboard surfaces ────────────────
//
// Five fetch endpoints back the four views:
//
//   GET  /api/security-findings                       — list filterable rows
//   POST /api/security-findings/<id>/resolve          — operator action
//   GET  /api/rule-metrics                            — per-rule rollup
//   GET  /api/override-audit                          — bypass-comment audit
//   GET  /api/senate/chambers                         — Senator roster
//   GET  /api/senate/reviews                          — review log
//   GET  /api/senate/reviews/<id>                     — single review w/ memories
//
// Sub-tab switchers wire the security + senate top-level legacy tabs.

function activateSecurityTab(name) {
  document.querySelectorAll('#security-tabs .reflection-tab').forEach(b => {
    b.classList.toggle('reflection-tab-active', b.dataset.securityTab === name);
  });
  document.querySelectorAll('#tab-security .reflection-pane').forEach(p => {
    p.hidden = p.id !== ('security-pane-' + name);
  });
  // Auto-load on tab activation so the operator sees data without an
  // extra click. Each loader is idempotent.
  if (name === 'findings')        window.loadSecurityFindings && window.loadSecurityFindings();
  if (name === 'rule-metrics')    window.loadRuleMetrics       && window.loadRuleMetrics();
  if (name === 'override-audit')  window.loadOverrideAudit     && window.loadOverrideAudit();
}
document.querySelectorAll('#security-tabs .reflection-tab').forEach(b => {
  b.addEventListener('click', () => activateSecurityTab(b.dataset.securityTab));
});

function activateSenateTab(name) {
  document.querySelectorAll('#senate-tabs .reflection-tab').forEach(b => {
    b.classList.toggle('reflection-tab-active', b.dataset.senateTab === name);
  });
  document.querySelectorAll('#tab-senate .reflection-pane').forEach(p => {
    p.hidden = p.id !== ('senate-pane-' + name);
  });
  if (name === 'chambers') window.loadSenateChambers && window.loadSenateChambers();
  if (name === 'reviews')  window.loadSenateReviews  && window.loadSenateReviews();
}
document.querySelectorAll('#senate-tabs .reflection-tab').forEach(b => {
  b.addEventListener('click', () => activateSenateTab(b.dataset.senateTab));
});

// — /api/security-findings list.
async function loadSecurityFindings() {
  const bureau = document.getElementById('sf-bureau');
  const disp   = document.getElementById('sf-disposition');
  const ruleEl = document.getElementById('sf-rule-id');
  const params = new URLSearchParams();
  if (bureau && bureau.value) params.set('bureau', bureau.value);
  if (disp && disp.value)     params.set('disposition', disp.value);
  if (ruleEl && ruleEl.value.trim()) params.set('rule_id', ruleEl.value.trim());
  try {
    const r = await fetch('/api/security-findings?' + params.toString());
    if (!r.ok) {
      _refSafeHTML('security-findings-table', '<div class="reflection-error">HTTP ' + r.status + '</div>');
      return;
    }
    const data = await r.json();
    const findings = data.findings || [];
    const sum = document.getElementById('sf-summary');
    if (sum) sum.textContent = (data.count || 0) + ' shown · ' + (data.total || 0) + ' total';
    const headers = ['ID', 'Bureau', 'Rule', 'Severity', 'File:Line', 'Message', 'Disposition', 'Audit', 'Created', ''];
    const target = document.getElementById('security-findings-table');
    if (!target) return;
    target.innerHTML = '';
    if (findings.length === 0) {
      const empty = document.createElement('div');
      empty.className = 'reflection-empty';
      empty.textContent = '(no findings match the filter)';
      target.appendChild(empty);
      return;
    }
    const table = document.createElement('table');
    table.className = 'reflection-data-table';
    const thead = document.createElement('thead');
    const headRow = document.createElement('tr');
    headers.forEach(h => { const th = document.createElement('th'); th.textContent = h; headRow.appendChild(th); });
    thead.appendChild(headRow);
    table.appendChild(thead);
    const tbody = document.createElement('tbody');
    findings.forEach(f => {
      const tr = document.createElement('tr');
      const cells = [
        String(f.ID || f.id || ''),
        f.Bureau || f.bureau || '',
        f.RuleID || f.rule_id || '',
        f.Severity || f.severity || '',
        (f.FilePath || f.file_path || '') + ':' + (f.LineNumber || f.line_number || ''),
        f.Message || f.message || '',
        f.Disposition || f.disposition || 'open',
        f.BypassAuditID || f.bypass_audit_id || '',
        f.CreatedAt || f.created_at || '',
      ];
      cells.forEach(c => { const td = document.createElement('td'); td.appendChild(_refTextNode(c)); tr.appendChild(td); });
      // Action cell — resolve.
      const actionTd = document.createElement('td');
      const btn = document.createElement('button');
      btn.className = 'btn-sm';
      btn.textContent = 'Resolve';
      btn.onclick = () => resolveSecurityFinding(f.ID || f.id);
      actionTd.appendChild(btn);
      tr.appendChild(actionTd);
      tbody.appendChild(tr);
    });
    table.appendChild(tbody);
    target.appendChild(table);
  } catch (e) {
    _refSafeHTML('security-findings-table', '<div class="reflection-error">' + e.message + '</div>');
  }
}
window.loadSecurityFindings = loadSecurityFindings;

async function resolveSecurityFinding(id) {
  if (!id) return;
  const operator = window.prompt('Operator email (for audit log):', '');
  if (!operator) return;
  const disposition = window.prompt('Disposition (resolved | closed | suppressed | overridden):', 'resolved');
  if (!disposition) return;
  let bypassAuditID = '', bypassReason = '';
  if (disposition === 'overridden') {
    bypassAuditID = window.prompt('Bypass audit id (AUDIT-NNN):', '') || '';
    bypassReason = window.prompt('Bypass reason (>= 10 chars):', '') || '';
  }
  try {
    const r = await fetch('/api/security-findings/' + encodeURIComponent(id) + '/resolve', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        disposition: disposition,
        operator_email: operator,
        bypass_audit_id: bypassAuditID,
        bypass_reason: bypassReason,
      }),
    });
    if (!r.ok) {
      const txt = await r.text();
      alert('resolve failed: HTTP ' + r.status + ' ' + txt);
      return;
    }
    window.loadSecurityFindings && window.loadSecurityFindings();
  } catch (e) {
    alert('resolve failed: ' + e.message);
  }
}
window.resolveSecurityFinding = resolveSecurityFinding;

// — /api/rule-metrics list.
async function loadRuleMetrics() {
  const bureau = document.getElementById('rm-bureau');
  const params = new URLSearchParams();
  if (bureau && bureau.value) params.set('bureau', bureau.value);
  try {
    const r = await fetch('/api/rule-metrics?' + params.toString());
    if (!r.ok) {
      _refSafeHTML('rule-metrics-table', '<div class="reflection-error">HTTP ' + r.status + '</div>');
      return;
    }
    const data = await r.json();
    const rules = data.rules || [];
    const headers = ['Bureau', 'Rule', 'Severity', 'Total', 'TP', 'FP', 'Precision', 'Last 30d', 'Ramp'];
    const rows = rules.map(m => [
      m.bureau || '',
      m.rule_id || '',
      m.severity || '',
      m.total_firings || 0,
      m.true_positives || 0,
      m.false_positives || 0,
      ((m.precision != null) ? (m.precision * 100).toFixed(1) + '%' : '—'),
      m.last_30_day_firings || 0,
      (m.ramp_status || '') + (m.firings_to_block_ready ? (' (' + m.firings_to_block_ready + ' to go)') : ''),
    ]);
    _refRenderTable('rule-metrics-table', headers, rows);
  } catch (e) {
    _refSafeHTML('rule-metrics-table', '<div class="reflection-error">' + e.message + '</div>');
  }
}
window.loadRuleMetrics = loadRuleMetrics;

// — /api/override-audit list.
async function loadOverrideAudit() {
  const bureau = document.getElementById('oa-bureau');
  const ruleEl = document.getElementById('oa-rule-id');
  const auditEl = document.getElementById('oa-audit-id');
  const params = new URLSearchParams();
  if (bureau && bureau.value) params.set('bureau', bureau.value);
  if (ruleEl && ruleEl.value.trim()) params.set('rule_id', ruleEl.value.trim());
  if (auditEl && auditEl.value.trim()) params.set('audit_id', auditEl.value.trim());
  try {
    const r = await fetch('/api/override-audit?' + params.toString());
    if (!r.ok) {
      _refSafeHTML('override-audit-table', '<div class="reflection-error">HTTP ' + r.status + '</div>');
      return;
    }
    const data = await r.json();
    const overrides = data.overrides || [];
    const sum = document.getElementById('oa-summary');
    if (sum) sum.textContent = (data.count || 0) + ' shown · ' + (data.total || 0) + ' total';
    const headers = ['ID', 'Bureau', 'Rule', 'Audit ID', 'File:Line', 'Reason', 'Commit', 'Overridden At'];
    const rows = overrides.map(f => [
      f.ID || f.id || '',
      f.Bureau || f.bureau || '',
      f.RuleID || f.rule_id || '',
      f.BypassAuditID || f.bypass_audit_id || '',
      (f.FilePath || f.file_path || '') + ':' + (f.LineNumber || f.line_number || ''),
      f.BypassReason || f.bypass_reason || '',
      (f.CommitSHA || f.commit_sha || '').substring(0, 12),
      f.ResolvedAt || f.resolved_at || '',
    ]);
    _refRenderTable('override-audit-table', headers, rows);
  } catch (e) {
    _refSafeHTML('override-audit-table', '<div class="reflection-error">' + e.message + '</div>');
  }
}
window.loadOverrideAudit = loadOverrideAudit;

// — /api/senate/chambers list.
async function loadSenateChambers() {
  const status = document.getElementById('sc-status');
  const params = new URLSearchParams();
  if (status && status.value) params.set('status', status.value);
  try {
    const r = await fetch('/api/senate/chambers?' + params.toString());
    if (!r.ok) {
      _refSafeHTML('senate-chambers-table', '<div class="reflection-error">HTTP ' + r.status + '</div>');
      return;
    }
    const data = await r.json();
    const chambers = data.chambers || [];
    const headers = ['Senator', 'Scope', 'Status', 'Onboarded', 'Last Refreshed'];
    const rows = chambers.map(c => [
      c.SenatorName || c.senator_name || '',
      c.Scope || c.scope || '',
      c.Status || c.status || '',
      c.OnboardedAt || c.onboarded_at || '',
      c.LastRefreshedAt || c.last_refreshed_at || '',
    ]);
    _refRenderTable('senate-chambers-table', headers, rows);
  } catch (e) {
    _refSafeHTML('senate-chambers-table', '<div class="reflection-error">' + e.message + '</div>');
  }
}
window.loadSenateChambers = loadSenateChambers;

// — /api/senate/reviews list + per-id detail.
async function loadSenateReviews() {
  const fid = document.getElementById('sr-feature-id');
  const sen = document.getElementById('sr-senator');
  const pos = document.getElementById('sr-position');
  const params = new URLSearchParams();
  if (fid && fid.value) params.set('feature_id', fid.value);
  if (sen && sen.value.trim()) params.set('senator', sen.value.trim());
  if (pos && pos.value) params.set('position', pos.value);
  try {
    const r = await fetch('/api/senate/reviews?' + params.toString());
    if (!r.ok) {
      _refSafeHTML('senate-reviews-table', '<div class="reflection-error">HTTP ' + r.status + '</div>');
      return;
    }
    const data = await r.json();
    const reviews = data.reviews || [];
    const target = document.getElementById('senate-reviews-table');
    if (!target) return;
    target.innerHTML = '';
    if (reviews.length === 0) {
      const empty = document.createElement('div');
      empty.className = 'reflection-empty';
      empty.textContent = '(no reviews match the filter)';
      target.appendChild(empty);
      return;
    }
    const table = document.createElement('table');
    table.className = 'reflection-data-table';
    const thead = document.createElement('thead');
    const headRow = document.createElement('tr');
    ['ID', 'Feature', 'Title', 'Senator', 'Position', 'Confidence', 'Created'].forEach(h => {
      const th = document.createElement('th'); th.textContent = h; headRow.appendChild(th);
    });
    thead.appendChild(headRow);
    table.appendChild(thead);
    const tbody = document.createElement('tbody');
    reviews.forEach(rv => {
      const tr = document.createElement('tr');
      const cells = [
        String(rv.ID || rv.id || ''),
        String(rv.FeatureID || rv.feature_id || ''),
        rv.feature_title || '',
        rv.Senator || rv.senator || '',
        rv.Position || rv.position || '',
        ((rv.Confidence != null) ? rv.Confidence : (rv.confidence || 0)).toString(),
        rv.CreatedAt || rv.created_at || '',
      ];
      cells.forEach((c, idx) => {
        const td = document.createElement('td');
        if (idx === 0) {
          // Make the id clickable → loads detail.
          const a = document.createElement('a');
          a.href = '#';
          a.textContent = c;
          a.onclick = (ev) => { ev.preventDefault(); loadSenateReviewDetail(rv.ID || rv.id); };
          td.appendChild(a);
        } else {
          td.appendChild(_refTextNode(c));
        }
        tr.appendChild(td);
      });
      tbody.appendChild(tr);
    });
    table.appendChild(tbody);
    target.appendChild(table);
    // Clear any prior detail block.
    _refSafeHTML('senate-review-detail', '');
  } catch (e) {
    _refSafeHTML('senate-reviews-table', '<div class="reflection-error">' + e.message + '</div>');
  }
}
window.loadSenateReviews = loadSenateReviews;

async function loadSenateReviewDetail(id) {
  if (!id) return;
  try {
    const r = await fetch('/api/senate/reviews/' + encodeURIComponent(id));
    if (!r.ok) {
      _refSafeHTML('senate-review-detail', '<div class="reflection-error">HTTP ' + r.status + '</div>');
      return;
    }
    const data = await r.json();
    const target = document.getElementById('senate-review-detail');
    if (!target) return;
    target.innerHTML = '';
    const h = document.createElement('h4');
    h.textContent = 'Review ' + (data.review.ID || data.review.id || '') + ' — ' + (data.review.Senator || data.review.senator || '') + ' / ' + (data.review.Position || data.review.position || '');
    target.appendChild(h);
    const ratEl = document.createElement('pre');
    ratEl.className = 'reflection-pre';
    ratEl.textContent = data.review.Rationale || data.review.rationale || '(no rationale)';
    target.appendChild(ratEl);
    if (data.cited_memories && data.cited_memories.length > 0) {
      const help = document.createElement('div');
      help.className = 'reflection-help';
      help.textContent = 'Cited memories (' + data.cited_memories.length + '):';
      target.appendChild(help);
      const ul = document.createElement('ul');
      data.cited_memories.forEach(m => {
        const li = document.createElement('li');
        li.textContent = (m.Topic || m.topic || '(untitled)') + ' — ' + (m.Summary || m.summary || '');
        ul.appendChild(li);
      });
      target.appendChild(ul);
    }
  } catch (e) {
    _refSafeHTML('senate-review-detail', '<div class="reflection-error">' + e.message + '</div>');
  }
}
window.loadSenateReviewDetail = loadSenateReviewDetail;
