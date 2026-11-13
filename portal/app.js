'use strict';

const root = document.getElementById('root');
let st = { user: null, apps: [], host: '', docker: '', caddy: false };
let detail = null;    // { app, deploys, logs }
let selected = null;  // deploy id whose log is open
let timer = null;

const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const logo = '<img src="/logo.svg" alt="" width="26" height="20">';

async function api(path, opts = {}) {
  const res = await fetch(path, { headers: { 'Content-Type': 'application/json' }, credentials: 'same-origin', ...opts });
  if (res.status === 401 && !path.startsWith('/api/login')) { st.user = null; renderLogin(); throw new Error('signed out'); }
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || res.statusText);
  return body;
}

const STATE = {
  ready: 'Running', building: 'Building', starting: 'Starting',
  queued: 'Queued', failed: 'Failed', none: 'Never deployed',
};

function ago(iso) {
  if (!iso) return '';
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return 'just now';
  if (s < 3600) return Math.floor(s / 60) + ' min ago';
  if (s < 86400) return Math.floor(s / 3600) + ' h ago';
  if (s < 30 * 86400) return Math.floor(s / 86400) + ' d ago';
  return new Date(iso).toLocaleDateString('en-GB', { day: 'numeric', month: 'short', year: 'numeric' });
}

// A loopback address only answers on the server itself.
const localOnly = (u) => /^https?:\/\/(127\.0\.0\.1|localhost|\[::1\])/i.test(u || '');

/* ---------- login ---------- */
function renderLogin(msg = '') {
  clearInterval(timer);
  root.innerHTML = `
  <div class="login-wrap">
    <aside class="login-side">
      <div class="brand">${logo}<div><b>shoal</b><small>deploy</small></div></div>
      <p>Builds a git repository into a container and gives it an address. One machine, nothing metered.</p>
      <div class="ver">v${esc(String(st.version || '').replace(/^v/, ''))}</div>
    </aside>
    <main class="login-main">
      <form class="login" id="login">
        <h1>Sign in</h1>
        <div class="field">
          <label for="u">User</label>
          <input id="u" type="text" value="admin" autocomplete="username" spellcheck="false">
        </div>
        <div class="field">
          <label for="p">Password</label>
          <input id="p" type="password" autocomplete="current-password">
          <small>First time: read initial-passphrase in the data directory.</small>
        </div>
        <div class="err" id="err">${esc(msg)}</div>
        <button class="btn primary" type="submit">Sign in</button>
      </form>
    </main>
  </div>`;
  root.querySelector('#login').addEventListener('submit', async (e) => {
    e.preventDefault();
    const btn = e.target.querySelector('button');
    btn.disabled = true;
    try {
      await api('/api/login', { method: 'POST', body: JSON.stringify({ user: root.querySelector('#u').value.trim(), pass: root.querySelector('#p').value }) });
      await boot();
    } catch (err) {
      root.querySelector('#err').textContent = err.message;
      btn.disabled = false;
    }
  });
  root.querySelector('#p').focus();
}

/* ---------- shell ---------- */
function shell(body, current) {
  const apps = st.apps || [];
  const items = apps.map((a) => `
    <a class="item ${current === a.name ? 'on' : ''}" href="#/app/${encodeURIComponent(a.name)}">
      <span class="dot ${esc(a.status || 'none')}"></span><span class="nm">${esc(a.name)}</span>
    </a>`).join('') || '<div class="group">No apps yet</div>';
  root.innerHTML = `
  <div class="shell">
    <aside class="rail" id="rail">
      <a class="brand" href="#/">${logo}<div><b>shoal</b><small>deploy</small></div></a>
      <nav>
        <div class="group">${apps.length} ${apps.length === 1 ? 'app' : 'apps'}</div>
        ${items}
      </nav>
      <div class="foot">
        <a class="item" href="#/new"><span class="nm">New app</span></a>
        <a class="item ${current === 'settings' ? 'on' : ''}" href="#/settings"><span class="nm">Settings</span></a>
        <button class="item" id="logout" type="button"><span class="nm">Sign out</span></button>
      </div>
    </aside>
    <div class="scrim" id="scrim"></div>
    <main class="main">${body}</main>
  </div>`;
  root.querySelector('#logout').addEventListener('click', async () => {
    await api('/api/logout', { method: 'POST' });
    st.user = null;
    location.hash = '#/';
    renderLogin();
  });
  const rail = root.querySelector('#rail');
  const scrim = root.querySelector('#scrim');
  const toggle = root.querySelector('#menu');
  if (toggle) toggle.addEventListener('click', () => rail.classList.add('open'));
  scrim.addEventListener('click', () => rail.classList.remove('open'));
}

const menuBtn = '<button class="btn quiet menu-btn" id="menu" type="button">Apps</button>';

/* ---------- home ---------- */
async function renderHome() {
  clearInterval(timer);
  await refresh();
  const n = (st.apps || []).length;
  shell(n
    ? `<div class="blank">${menuBtn}<p>${n} ${n === 1 ? 'app' : 'apps'} on this server. Pick one from the list.</p></div>`
    : `<div class="blank">${menuBtn}
        <p>This server has no apps. Give Shoal a public git repository with a Dockerfile and it builds and runs it.</p>
        <div class="acts"><a class="btn primary" href="#/new">New app</a></div>
      </div>`, '');
}

async function refresh() {
  try { st = await api('/api/overview'); } catch (e) { /* login view already shown */ }
}

/* ---------- new app ---------- */
function renderNew() {
  clearInterval(timer);
  shell(`
  <div class="sheet">
    <div class="head">
      <div>${menuBtn}<h1>New app</h1><div class="sub">A name and a repository. Everything else has a default.</div></div>
    </div>
    <form class="form" id="create" style="margin-top:22px;max-width:640px">
      <div class="field">
        <label for="name">Name</label>
        <input id="name" type="text" placeholder="cashier" required spellcheck="false" autocomplete="off">
        <small>Lowercase, no spaces. This is what shows in the list.</small>
      </div>
      <div class="field">
        <label for="repo">Repository</label>
        <input id="repo" type="text" placeholder="https://github.com/owner/repo.git" spellcheck="false" autocomplete="off">
        <small>A public git URL, not a link to a single file. It needs a Dockerfile.</small>
      </div>
      <div class="err" id="err"></div>
      <div class="actions">
        <button class="btn primary" type="submit">Build and run</button>
        <a class="btn quiet" href="#/">Cancel</a>
      </div>
      <details class="block">
        <summary>Advanced</summary>
        <div class="form" style="margin-top:16px">
          <div class="row2">
            <div class="field"><label for="ref">Branch or tag</label><input id="ref" type="text" placeholder="main" spellcheck="false"></div>
            <div class="field"><label for="port">Container port</label><input id="port" type="number" value="80" min="1" max="65535"></div>
          </div>
          <div class="row2">
            <div class="field"><label for="dockerfile">Dockerfile</label><input id="dockerfile" type="text" placeholder="Dockerfile" spellcheck="false"></div>
            <div class="field"><label for="ctx">Build directory</label><input id="ctx" type="text" placeholder="." spellcheck="false"></div>
          </div>
          <div class="row2">
            <div class="field"><label for="domain">Domain</label><input id="domain" type="text" placeholder="cashier.office.local" spellcheck="false"><small>Needs Caddy running and DNS pointing here.</small></div>
            <div class="field"><label for="health">Health check path</label><input id="health" type="text" placeholder="/" spellcheck="false"><small>Use tcp for anything that is not HTTP.</small></div>
          </div>
          <label class="check"><input type="checkbox" id="db"><span>Add a PostgreSQL database<small>Shoal creates it and sets DATABASE_URL.</small></span></label>
          <div class="field"><label for="image">Use an existing image, skip the build</label><input id="image" type="text" placeholder="nginx:alpine" spellcheck="false"></div>
        </div>
      </details>
    </form>
  </div>`, '');

  const q = new URLSearchParams(location.hash.split('?')[1] || '');
  if (q.get('contoh')) {
    root.querySelector('#name').value = 'demo';
    root.querySelector('#repo').value = 'https://github.com/docker/welcome-to-docker.git';
    root.querySelector('#port').value = '80';
  }
  root.querySelector('#create').addEventListener('submit', async (e) => {
    e.preventDefault();
    const btn = e.target.querySelector('button[type=submit]');
    btn.disabled = true;
    const payload = {
      name: root.querySelector('#name').value.trim(),
      port: Number(root.querySelector('#port').value) || 80,
      repo: root.querySelector('#repo').value.trim(),
      ref: root.querySelector('#ref').value.trim(),
      dockerfile: root.querySelector('#dockerfile').value.trim(),
      context: root.querySelector('#ctx').value.trim(),
      domain: root.querySelector('#domain').value.trim(),
      health_path: root.querySelector('#health').value.trim(),
      with_db: root.querySelector('#db').checked,
      image: root.querySelector('#image').value.trim(),
      env: {},
    };
    if (!payload.repo && !payload.image) {
      root.querySelector('#err').textContent = 'Add a repository, or an image under Advanced.';
      btn.disabled = false;
      return;
    }
    try {
      await api('/api/apps', { method: 'POST', body: JSON.stringify(payload) });
      await api('/api/apps/' + encodeURIComponent(payload.name) + '/deploy', { method: 'POST' });
      location.hash = '#/app/' + encodeURIComponent(payload.name);
    } catch (err) {
      root.querySelector('#err').textContent = err.message;
      btn.disabled = false;
    }
  });
}

/* ---------- one app ---------- */
async function renderApp(name) {
  clearInterval(timer);
  await refresh();
  const data = await api('/api/apps/' + encodeURIComponent(name));
  detail = data;
  selected = data.app.deploy_id || (data.deploys[0] && data.deploys[0].id) || null;
  const a = data.app;

  shell(`
  <div class="sheet">
    <div class="head">
      <div>
        ${menuBtn}
        <h1>${esc(a.name)}</h1>
        <div class="sub">${esc(a.repo || a.image || 'no source yet')}${a.ref ? '@' + esc(a.ref) : ''}</div>
      </div>
      <div class="acts">
        <button class="btn primary" id="open" type="button">Open</button>
        <button class="btn" id="deploy" type="button">Redeploy</button>
        <a class="btn quiet" href="#/app/${encodeURIComponent(name)}/settings">Settings</a>
      </div>
    </div>
    <div class="status" id="status"><span class="dot"></span><b id="verdict"></b><span class="why" id="why"></span></div>
    <div class="split">
      <div class="col">
        <h2>Versions</h2>
        <div class="versions" id="hist"></div>
      </div>
      <div class="col">
        <h2 id="logtitle">Log</h2>
        <pre class="log" id="log"></pre>
      </div>
    </div>
  </div>`, name);

  root.querySelector('#open').addEventListener('click', () => {
    const u = detail.app.url || (liveDeploy() && liveDeploy().url);
    if (u) window.open(u, '_blank', 'noreferrer');
  });
  root.querySelector('#deploy').addEventListener('click', async (e) => {
    e.target.disabled = true;
    try { await api('/api/apps/' + encodeURIComponent(name) + '/deploy', { method: 'POST' }); selected = null; poll(); }
    catch (err) { alert(err.message); }
    finally { setTimeout(() => { e.target.disabled = false; }, 1500); }
  });
  renderDetailParts();
  poll();
}

function liveDeploy() { return detail && detail.deploys.find((d) => d.live); }

function renderDetailParts(forceLog) {
  const a = detail.app;
  const s = a.status || 'none';
  const status = root.querySelector('#status');
  if (!status) return;
  status.className = 'status ' + s;
  status.querySelector('.dot').className = 'dot ' + s;
  root.querySelector('#verdict').textContent = STATE[s] || s;

  const why = root.querySelector('#why');
  if (s === 'ready') {
    why.innerHTML = localOnly(a.url)
      ? `at <a href="${esc(a.url)}" target="_blank" rel="noreferrer">${esc(a.url)}</a>. That address only works on this server. Add a domain to reach it from elsewhere.`
      : `at <a href="${esc(a.url)}" target="_blank" rel="noreferrer">${esc(a.url)}</a>`;
  } else if (s === 'failed') {
    const bad = detail.deploys.find((d) => d.id === selected) || detail.deploys[0];
    why.textContent = (bad && bad.error) || 'The log shows where it stopped.';
  } else if (s === 'none') {
    why.textContent = 'Nothing has been built yet. Redeploy runs the first build.';
  } else {
    why.textContent = 'Usually a minute or two. This page updates on its own.';
  }
  const openBtn = root.querySelector('#open');
  openBtn.disabled = !a.url;
  openBtn.style.display = a.url ? '' : 'none';

  const rows = detail.deploys.map((d) => `
    <div class="row ${d.id === selected ? 'on' : ''}" data-id="${esc(d.id)}">
      <div>
        <div class="when">${esc(ago(d.started))}${d.live ? '<em>live</em>' : ''}</div>
        <div class="meta">${esc(d.commit && d.commit !== 'unknown' ? d.commit.slice(0, 7) : 'no commit')}</div>
        ${!d.live && d.status === 'ready' && d.image ? `<button class="btn" data-roll="${esc(d.id)}" type="button">Use this version</button>` : ''}
      </div>
      <span class="st ${esc(d.status)}">${esc(STATE[d.status] || d.status)}</span>
    </div>`).join('') || '<p class="why">No versions yet. Redeploy starts the first one.</p>';

  const hist = root.querySelector('#hist');
  if (hist.dataset.sig !== rows) {
    hist.innerHTML = rows;
    hist.dataset.sig = rows;
    hist.querySelectorAll('.row').forEach((n) => n.addEventListener('click', (e) => {
      if (e.target.closest('[data-roll]')) return;
      selected = n.dataset.id;
      renderDetailParts(true);
    }));
    hist.querySelectorAll('[data-roll]').forEach((b) => b.addEventListener('click', async () => {
      b.disabled = true;
      try {
        await api('/api/apps/' + encodeURIComponent(detail.app.name) + '/rollback', { method: 'POST', body: JSON.stringify({ deploy: b.dataset.roll }) });
        selected = null;
        poll();
      } catch (err) { alert(err.message); }
      finally { b.disabled = false; renderDetailParts(); }
    }));
  }

  const cur = selected ? detail.deploys.find((d) => d.id === selected) : null;
  const log = root.querySelector('#log');
  const title = root.querySelector('#logtitle');
  if (!log) return;
  let text;
  if (cur) { text = cur.log || '(no output yet)'; title.textContent = 'Build ' + cur.id; }
  else { text = detail.logs || '(the app has not printed anything)'; title.textContent = 'App output'; }
  const atBottom = log.scrollTop + log.clientHeight >= log.scrollHeight - 40;
  if (log.textContent !== text) { log.textContent = text; if (atBottom || forceLog) log.scrollTop = log.scrollHeight; }
}

function poll() {
  clearInterval(timer);
  let every = 2500;
  timer = setInterval(async () => {
    const m = location.hash.match(/^#\/app\/([^/?]+)/);
    if (!m) return;
    try {
      const data = await api('/api/apps/' + encodeURIComponent(decodeURIComponent(m[1])));
      detail = data;
      renderDetailParts();
      const active = data.deploys.some((d) => ['queued', 'building', 'starting'].includes(d.status));
      if (!active && every !== 8000) { clearInterval(timer); every = 8000; poll(); return; }
      if (active) {
        const cur = selected && data.deploys.find((d) => d.id === selected);
        if (cur && ['ready', 'failed'].includes(cur.status)) refresh();
      }
    } catch (e) { /* keep the last view */ }
  }, every);
}

/* ---------- app settings ---------- */
async function renderAppSettings(name) {
  clearInterval(timer);
  await refresh();
  const data = await api('/api/apps/' + encodeURIComponent(name));
  const a = data.app;
  const envRows = Object.entries(a.env || {}).map(([k, v]) => envRow(k, v)).join('') || envRow('', '');

  shell(`
  <div class="sheet">
    <div class="head">
      <div>${menuBtn}<h1>${esc(a.name)}</h1><div class="sub">Saved here. Applied on the next deploy.</div></div>
      <div class="acts"><a class="btn quiet" href="#/app/${encodeURIComponent(name)}">Back to app</a></div>
    </div>

    <div class="block">
      <h2>Environment</h2>
      <p class="lead">Values handed to the app when it starts, such as an API key. They stay on this server.</p>
      <table class="env" id="env">${envRows}</table>
      <div class="actions" style="margin-top:12px">
        <button class="btn" id="addenv" type="button">Add variable</button>
        <button class="btn primary" id="saveenv" type="button">Save</button>
        <span class="err ok" id="enverr"></span>
      </div>
    </div>

    <div class="block">
      <h2>Build</h2>
      <form class="form" id="cfg">
        <div class="field"><label for="c-repo">Repository</label><input id="c-repo" type="text" value="${esc(a.repo)}" spellcheck="false"></div>
        <div class="row2">
          <div class="field"><label for="c-ref">Branch or tag</label><input id="c-ref" type="text" value="${esc(a.ref)}" spellcheck="false"></div>
          <div class="field"><label for="c-port">Container port</label><input id="c-port" type="number" value="${a.port}"></div>
        </div>
        <div class="row2">
          <div class="field"><label for="c-dockerfile">Dockerfile</label><input id="c-dockerfile" type="text" value="${esc(a.dockerfile)}" spellcheck="false"></div>
          <div class="field"><label for="c-ctx">Build directory</label><input id="c-ctx" type="text" value="${esc(a.context)}" placeholder="." spellcheck="false"></div>
        </div>
        <div class="row2">
          <div class="field"><label for="c-domain">Domain</label><input id="c-domain" type="text" value="${esc(a.domain)}" spellcheck="false"></div>
          <div class="field"><label for="c-health">Health check path</label><input id="c-health" type="text" value="${esc(a.health_path)}" placeholder="/ or tcp" spellcheck="false"></div>
        </div>
        <div class="actions"><button class="btn primary" type="submit">Save</button><span class="err ok" id="cfgerr"></span></div>
      </form>
      <p class="lead" id="hook" style="margin-top:16px"></p>
    </div>

    <div class="block">
      <h2>Delete</h2>
      <p class="lead">Stops the container and removes the app. A database volume, if there is one, is kept.</p>
      <button class="btn danger" id="destroy" type="button">Delete this app</button>
    </div>
  </div>`, name);

  root.querySelector('#addenv').addEventListener('click', () => root.querySelector('#env').insertAdjacentHTML('beforeend', envRow('', '')));
  root.querySelector('#saveenv').addEventListener('click', async () => {
    const env = {};
    root.querySelectorAll('#env tr').forEach((tr) => { const k = tr.querySelector('.ek').value.trim(); if (k) env[k] = tr.querySelector('.ev').value; });
    const out = root.querySelector('#enverr');
    try { await api('/api/apps/' + encodeURIComponent(name), { method: 'PUT', body: JSON.stringify({ env }) }); out.className = 'err ok'; out.textContent = 'saved'; }
    catch (err) { out.className = 'err'; out.textContent = err.message; }
  });
  root.querySelector('#cfg').addEventListener('submit', async (e) => {
    e.preventDefault();
    const out = root.querySelector('#cfgerr');
    try {
      await api('/api/apps/' + encodeURIComponent(name), { method: 'PUT', body: JSON.stringify({
        repo: root.querySelector('#c-repo').value.trim(), ref: root.querySelector('#c-ref').value.trim(),
        dockerfile: root.querySelector('#c-dockerfile').value.trim(), context: root.querySelector('#c-ctx').value.trim(),
        port: Number(root.querySelector('#c-port').value) || 80, domain: root.querySelector('#c-domain').value.trim(),
        health_path: root.querySelector('#c-health').value.trim(),
      }) });
      out.className = 'err ok'; out.textContent = 'saved';
    } catch (err) { out.className = 'err'; out.textContent = err.message; }
  });
  root.querySelector('#destroy').addEventListener('click', async () => {
    if (!confirm('Delete ' + name + '? The container stops. A database volume is kept.')) return;
    await api('/api/apps/' + encodeURIComponent(name), { method: 'DELETE' });
    location.hash = '#/';
  });
  root.querySelector('#hook').textContent = 'To deploy from git on every push: POST /api/webhook/' + name + '?token=<the app token>';
}

function envRow(k, v) {
  return `<tr>
    <td><input class="ek" type="text" value="${esc(k)}" placeholder="NAME" spellcheck="false"></td>
    <td><input class="ev" type="text" value="${esc(v)}" placeholder="value" spellcheck="false"></td>
    <td><button class="btn quiet" type="button" onclick="this.closest('tr').remove()" aria-label="remove variable">x</button></td>
  </tr>`;
}

/* ---------- settings ---------- */
function renderSettings() {
  clearInterval(timer);
  shell(`
  <div class="sheet">
    <div class="head"><div>${menuBtn}<h1>Settings</h1></div></div>
    <div class="block">
      <h2>Password</h2>
      <form class="form" id="pw" style="max-width:420px">
        <div class="field"><label for="cur">Current password</label><input id="cur" type="password" autocomplete="current-password"></div>
        <div class="field"><label for="next">New password</label><input id="next" type="password" autocomplete="new-password"><small>At least 8 characters.</small></div>
        <div class="actions"><button class="btn primary" type="submit">Change password</button><span class="err ok" id="pwerr"></span></div>
      </form>
    </div>
    <div class="block">
      <h2>This server</h2>
      <div class="versions">
        <div class="row" style="cursor:default"><div><div class="when">Host</div></div><span class="meta">${esc(st.host || 'unknown')}</span></div>
        <div class="row" style="cursor:default"><div><div class="when">Docker</div></div><span class="meta">${esc(st.docker || 'not found')}</span></div>
        <div class="row" style="cursor:default"><div><div class="when">Automatic domains</div></div><span class="meta">${st.caddy ? 'caddy is running' : 'caddy is not installed'}</span></div>
        <div class="row" style="cursor:default"><div><div class="when">Shoal Deploy</div></div><span class="meta">v${esc(String(st.version || '').replace(/^v/, ''))}</span></div>
      </div>
    </div>
  </div>`, 'settings');
  root.querySelector('#pw').addEventListener('submit', async (e) => {
    e.preventDefault();
    const out = root.querySelector('#pwerr');
    try {
      await api('/api/password', { method: 'POST', body: JSON.stringify({ current: root.querySelector('#cur').value, next: root.querySelector('#next').value }) });
      out.className = 'err ok'; out.textContent = 'password changed';
      root.querySelector('#cur').value = ''; root.querySelector('#next').value = '';
    } catch (err) { out.className = 'err'; out.textContent = err.message; }
  });
}

/* ---------- router ---------- */
async function boot() {
  try { st = await api('/api/overview'); } catch (e) { return; }
  route();
}

async function route() {
  if (!st.user) {
    try { st = await api('/api/overview'); } catch (e) { return renderLogin(); }
  }
  const h = location.hash || '#/';
  const appSet = h.match(/^#\/app\/([^/?]+)\/settings/);
  if (appSet) return renderAppSettings(decodeURIComponent(appSet[1]));
  if (h.startsWith('#/new')) return renderNew();
  if (h.startsWith('#/settings')) return renderSettings();
  if (h.startsWith('#/app/')) return renderApp(decodeURIComponent(h.slice(6).split('?')[0]));
  return renderHome();
}

window.addEventListener('hashchange', () => {
  clearInterval(timer);
  route().catch((e) => { root.innerHTML = `<main class="sheet"><div class="err">${esc(e.message)}</div><p><a class="btn quiet" href="#/">Back</a></p></main>`; });
});
boot();
