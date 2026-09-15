const HUB_API = window.location.origin;
let currentOffset = 0;
const PAGE_SIZE = 20;
let currentView = 'browse';

async function api(path) {
  const res = await fetch(HUB_API + path);
  if (!res.ok) throw new Error(`API error: ${res.status}`);
  return res.json();
}

function showView(view) {
  document.querySelectorAll('.view').forEach(v => v.classList.remove('active'));
  document.querySelectorAll('.nav-link').forEach(n => n.classList.remove('active'));
  document.getElementById('view-' + view).classList.add('active');
  const navEl = document.getElementById('nav-' + view);
  if (navEl) navEl.classList.add('active');
  currentView = view;

  if (view === 'browse') loadSkills(true);
  if (view === 'categories') loadCategories();
  if (view === 'stats') loadStats();
}

async function loadSkills(reset) {
  if (reset) { currentOffset = 0; document.getElementById('skills-grid').innerHTML = ''; }

  const q = document.getElementById('search-input').value;
  const sort = document.getElementById('sort-select').value;
  const verified = document.getElementById('verified-only').checked;

  let url = `/hub/v1/skills?offset=${currentOffset}&limit=${PAGE_SIZE}&sort=${sort}`;
  if (q) url += `&q=${encodeURIComponent(q)}`;
  if (verified) url += `&verified=true`;

  try {
    const data = await api(url);
    const grid = document.getElementById('skills-grid');

    if (data.items.length === 0 && currentOffset === 0) {
      grid.innerHTML = `<div class="empty-state"><h3>No skills found</h3><p>Try a different search or check back later.</p></div>`;
      document.getElementById('load-more').style.display = 'none';
      return;
    }

    for (const skill of data.items) {
      grid.innerHTML += renderCard(skill);
    }

    currentOffset += data.items.length;
    document.getElementById('load-more').style.display = currentOffset < data.total ? 'block' : 'none';
  } catch (err) {
    document.getElementById('skills-grid').innerHTML =
      `<div class="empty-state"><h3>Hub Unavailable</h3><p>${esc(err.message)}</p></div>`;
  }
}

function loadMore() { loadSkills(false); }

function renderCard(skill) {
  const badge = skill.verified
    ? '<span class="badge badge-verified">Verified</span>'
    : '<span class="badge badge-unverified">Unverified</span>';
  const tags = (skill.tags || []).slice(0, 3).map(t => `<span class="tag">${esc(t)}</span>`).join('');

  return `<div class="skill-card" data-action="detail" data-name="${esc(skill.name)}">
    <div class="card-header">
      <div><span class="card-name">${esc(skill.name)}</span> <span class="card-version">v${esc(skill.version)}</span></div>
      ${badge}
    </div>
    <div class="card-desc">${esc(skill.description)}</div>
    <div class="card-footer">
      <div class="card-tags">${tags}</div>
      <span>${skill.downloads.toLocaleString()} downloads</span>
    </div>
  </div>`;
}

async function showDetail(name) {
  document.querySelectorAll('.view').forEach(v => v.classList.remove('active'));
  document.getElementById('view-detail').classList.add('active');
  document.querySelectorAll('.nav-link').forEach(n => n.classList.remove('active'));

  try {
    const skill = await api(`/hub/v1/skills/${encodeURIComponent(name)}`);
    const badge = skill.verified
      ? '<span class="badge badge-verified">Verified</span>'
      : '<span class="badge badge-unverified">Unverified</span>';

    const perms = Object.entries(skill.permissions || {}).map(([k, v]) => {
      const val = Array.isArray(v) ? v.join(', ') : String(v);
      return `<li><strong>${esc(k)}:</strong> ${esc(val)}</li>`;
    }).join('');

    const versions = (skill.versions || []).map(v => `<span class="tag">${esc(v)}</span>`).join(' ');

    const secrets = Array.isArray(skill.permissions?.secrets) ? skill.permissions.secrets : [];
    const hasOAuth = secrets.some(s => /GMAIL_|GOOGLE_|GITHUB_/.test(s));
    const setupType = secrets.length === 0 ? 'ready' : hasOAuth ? 'oauth' : 'token';
    const setupLabel = setupType === 'ready' ? 'No setup needed'
      : setupType === 'oauth' ? 'OAuth sign-in required'
      : 'API token required';

    document.getElementById('skill-detail').innerHTML = `
      <div class="skill-detail-page">
        <h2>${esc(skill.name)} ${badge}</h2>
        <div class="meta">by ${esc(skill.author)} &middot; ${skill.downloads.toLocaleString()} downloads &middot; ${esc(skill.category)}</div>

        <div class="install-card">
          <div class="install-card-left">
            <div class="install-cmd-box" data-action="copy" data-name="${esc(skill.name)}">
              <code>fathom install ${esc(skill.name)}</code>
              <span class="copy-hint" id="copy-hint">click to copy</span>
            </div>
            <div class="setup-type setup-${setupType}">${setupLabel}</div>
          </div>
          <button class="install-btn" data-action="copy" data-name="${esc(skill.name)}">Install</button>
        </div>

        <div class="perms-card">
          <h3>Permissions Required</h3>
          <div class="perms-grid">
            <div class="perm-item"><span class="perm-label">Network</span><span class="perm-val">${skill.permissions?.network ? (Array.isArray(skill.permissions.network) ? skill.permissions.network.length + ' endpoint(s)' : 'yes') : 'none'}</span></div>
            <div class="perm-item"><span class="perm-label">Filesystem</span><span class="perm-val">${esc(String(skill.permissions?.filesystem || 'none'))}</span></div>
            <div class="perm-item"><span class="perm-label">Shell</span><span class="perm-val">${esc(String(skill.permissions?.shell || 'none'))}</span></div>
            <div class="perm-item"><span class="perm-label">Memory</span><span class="perm-val">${esc(String(skill.permissions?.memory || 'none'))}</span></div>
          </div>
          ${secrets.length > 0 ? '<div class="perm-secrets"><span class="perm-label">Secrets</span><span class="perm-val">' + secrets.map(s => '<code>' + esc(s) + '</code>').join(' ') + '</span></div>' : ''}
        </div>

        <div class="section"><h3>Description</h3><p>${esc(skill.description)}</p></div>
        ${skill.readme ? `<div class="section"><h3>README</h3><pre>${esc(skill.readme)}</pre></div>` : ''}
        <div class="section"><h3>All Permissions</h3><ul class="perm-list">${perms || '<li>No special permissions</li>'}</ul></div>
        <div class="section"><h3>Versions</h3><div class="card-tags">${versions}</div></div>
      </div>`;
  } catch (err) {
    document.getElementById('skill-detail').innerHTML = `<div class="empty-state"><h3>Error</h3><p>${esc(err.message)}</p></div>`;
  }
}

async function loadCategories() {
  try {
    const data = await api('/hub/v1/categories');
    const grid = document.getElementById('categories-grid');
    if (data.categories.length === 0) {
      grid.innerHTML = '<div class="empty-state"><p>No categories yet.</p></div>';
      return;
    }
    grid.innerHTML = data.categories.map(c =>
      `<div class="category-card" data-action="category" data-name="${esc(c.name)}">
        <div class="cat-name">${esc(c.name)}</div>
        <div class="cat-count">${c.count} skill${c.count !== 1 ? 's' : ''}</div>
      </div>`
    ).join('');
  } catch (err) {
    document.getElementById('categories-grid').innerHTML = `<div class="empty-state"><p>${esc(err.message)}</p></div>`;
  }
}

function filterByCategory(cat) {
  showView('browse');
  document.getElementById('search-input').value = cat;
  loadSkills(true);
}

async function loadStats() {
  try {
    const stats = await api('/hub/v1/stats');
    document.getElementById('stats-cards').innerHTML = `
      <div class="stat-card"><div class="stat-value">${stats.totalSkills}</div><div class="stat-label">Total Skills</div></div>
      <div class="stat-card"><div class="stat-value">${stats.verified}</div><div class="stat-label">Verified Skills</div></div>
      <div class="stat-card"><div class="stat-value">${stats.totalDownloads.toLocaleString()}</div><div class="stat-label">Total Downloads</div></div>
      <div class="stat-card"><div class="stat-value">${stats.categories}</div><div class="stat-label">Categories</div></div>`;
  } catch (err) {
    document.getElementById('stats-cards').innerHTML = `<div class="empty-state"><p>${esc(err.message)}</p></div>`;
  }
}

function esc(s) {
  if (!s) return '';
  // Escapes both quote styles and the backtick so the result is safe in any
  // HTML text or quoted-attribute context. (Skill metadata is attacker-
  // controlled — anyone can publish to the hub — so this must be airtight.)
  return String(s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;')
    .replace(/`/g, '&#96;');
}

// Event delegation for skill/category interactions. We deliberately avoid
// inline onclick="fn('${name}')" handlers: the name is attacker-controlled
// and HTML-attribute decoding happens before the JS string is parsed, so a
// name containing a quote could break out and execute (stored XSS). Reading
// the value from data-* via the DOM never re-parses it as code.
document.addEventListener('click', (e) => {
  const el = e.target.closest('[data-action]');
  if (!el) return;
  const name = el.dataset.name || '';
  switch (el.dataset.action) {
    case 'detail': showDetail(name); break;
    case 'copy': copyInstallCmd(name); break;
    case 'category': filterByCategory(name); break;
  }
});

let searchTimeout;
document.getElementById('search-input').addEventListener('input', () => {
  clearTimeout(searchTimeout);
  searchTimeout = setTimeout(() => loadSkills(true), 300);
});
document.getElementById('sort-select').addEventListener('change', () => loadSkills(true));
document.getElementById('verified-only').addEventListener('change', () => loadSkills(true));

function copyInstallCmd(name) {
  const cmd = `fathom install ${name}`;
  navigator.clipboard.writeText(cmd).then(() => {
    const hint = document.getElementById('copy-hint');
    if (hint) { hint.textContent = 'copied!'; setTimeout(() => { hint.textContent = 'click to copy'; }, 2000); }
  }).catch(() => {});
}

loadSkills(true);
