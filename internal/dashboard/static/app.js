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
  activeTab:          'tasks',
  sortBy:             'id',
  sortDir:            'desc',
  showInfra:          false,     // toggle — hide fleet plumbing (Pilot, Librarian, Medic triage) by default
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
    case 'logs':        startLogStream(); break;
  }
  if (name !== 'logs') stopLogStream();
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
  $('panel-actions').innerHTML = btns.join('');

  // Body
  const sections = [];

  // Meta
  const lockedAt = d.locked_at ? fmtTS(d.locked_at) : '—';
  const blockedByLinks = (d.blocked_by && d.blocked_by.length > 0)
    ? d.blocked_by.map(id => `<a onclick="openPanel(${id})" style="cursor:pointer">#${id}</a>`).join(', ')
    : '';
  sections.push(`
    <div class="panel-section">
      <h3>Details</h3>
      <div class="meta-grid">
        <span class="meta-key">Repo</span>      <span class="meta-val">${escHtml(d.repo || '—')}</span>
        <span class="meta-key">Owner</span>     <span class="meta-val">${escHtml(d.owner || '—')}</span>
        <span class="meta-key">Branch</span>    <span class="meta-val">${escHtml(d.branch_name || '—')}</span>
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

  // Status filter: 'active' matches Active and Failed; 'completed' matches only Completed
  if (S.convoyStatusFilter !== 'all') {
    list = list.filter(c => {
      const st = (c.status || '').toLowerCase();
      if (S.convoyStatusFilter === 'active') return st === 'active' || st === 'failed';
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
        </div>
        <div id="pr-review-panel-${c.id}" class="pr-review-panel" style="display:none"></div>
      </div>`;
  }).join('');
}

// renderPRReviewBadge returns a clickable summary badge when the convoy has
// any PR review comments. Clicking it toggles the inline comment panel.
function renderPRReviewBadge(c) {
  const r = c.pr_review_rollup;
  if (!r || !r.total) return '';
  const parts = [];
  if (r.bot_in_scope)      parts.push(`<span title="Bot fixes queued">🔧 ${r.bot_in_scope}</span>`);
  if (r.bot_out_of_scope)  parts.push(`<span title="Follow-up features">📌 ${r.bot_out_of_scope}</span>`);
  if (r.bot_not_actionable)parts.push(`<span title="Explained to bot">💬 ${r.bot_not_actionable}</span>`);
  if (r.bot_conflicted_loop)parts.push(`<span title="Bot loop escalated" style="color:var(--red)">⚠️ ${r.bot_conflicted_loop}</span>`);
  if (r.human_awaiting)    parts.push(`<span title="Human comments awaiting operator" style="color:var(--accent)">👤 ${r.human_awaiting}</span>`);
  if (!parts.length) return '';
  return `<button class="pr-review-badge" onclick="togglePRReviewPanel(${c.id})" title="Click to view PR review comments">
    ${parts.join(' ')}
  </button>`;
}

// togglePRReviewPanel lazy-loads the convoy's PR review comments inline.
async function togglePRReviewPanel(convoyID) {
  const el = $(`pr-review-panel-${convoyID}`);
  if (!el) return;
  if (el.style.display === 'block') {
    el.style.display = 'none';
    return;
  }
  el.style.display = 'block';
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
    reply = `<a onclick="openPanel(${c.spawned_task_id})" style="cursor:pointer">→ task #${c.spawned_task_id}</a>`;
  } else if (c.classification === 'out_of_scope' && c.spawned_task_id) {
    reply = `<a onclick="openPanel(${c.spawned_task_id})" style="cursor:pointer">→ feature #${c.spawned_task_id}</a>`;
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
  $('mail-modal-body').innerHTML = typeof marked !== 'undefined'
    ? marked.parse(m.body || '')
    : escHtml(m.body || '');
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
    const existing = wrap.querySelector('.log-line--error');
    if (existing) existing.remove();

    let text = evt.data;
    try {
      const parsed = JSON.parse(evt.data);
      if (typeof parsed === 'string') text = parsed;
    } catch(_) {}
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
    if (!wrap.querySelector('.log-line--error')) {
      const errLine = document.createElement('div');
      errLine.className = 'log-line log-line--error';
      errLine.textContent = '[holonet stream error — reconnecting…]';
      wrap.appendChild(errLine);
      if (wrap.scrollHeight - wrap.scrollTop - wrap.clientHeight < 120) {
        wrap.scrollTop = wrap.scrollHeight;
      }
    }
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
