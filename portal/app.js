'use strict';

const root = document.getElementById('root');
let st = { user: null, apps: [], host: '', docker: '', caddy: false };
let detail = null;      // { app, deploys, logs }
let selected = null;    // deploy id
let inFlight = false;   // a deploy or rollback is in flight
let timer = null;

const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

async function api(path, opts = {}) {
  const res = await fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    credentials: 'same-origin',
    ...opts,
  });
  if (res.status === 401 && !path.startsWith('/api/login')) { st.user = null; renderLogin(); throw new Error('signed out'); }
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || res.statusText);
  return body;
}

/* ---------- login ---------- */
function renderLogin(msg = '') {
  clearInterval(timer);
  root.innerHTML = `
  <div class="login-wrap">
    <form class="login" id="login">
      <div class="mark"><b>SHOAL DEPLOY</b><span>v0.1</span></div>
      <div class="field">
        <label for="u">User</label>
        <input id="u" type="text" value="admin" autocomplete="username" spellcheck="false">
      </div>
      <div class="field">
        <label for="p">Passphrase</label>
        <input id="p" type="password" autocomplete="current-password">
        <small>first run: see initial-passphrase in the data directory</small>
      </div>
      <div class="err" id="err">${esc(msg)}</div>
      <button class="btn primary" type="submit">Sign in</button>
    </form>
  </div>`;
  root.querySelector('#login').addEventListener('submit', async (e) => {
    e.preventDefault();
    const btn = e.target.querySelector('button');
    btn.disabled = true;
    try {
      await api('/api/login', {
        method: 'POST',
        body: JSON.stringify({ user: root.querySelector('#u').value.trim(), pass: root.querySelector('#p').value }),
      });
      await boot();
    } catch (err) {
      root.querySelector('#err').textContent = err.message;
      btn.disabled = false;
    }
  });
  root.querySelector('#p').focus();
}

/* ---------- shell ---------- */
function rail(active) {
  const items = st.apps.map((a) => `
    <a class="app-link ${a.name === active ? 'active' : ''}" href="#/app/${encodeURIComponent(a.name)}">
      <span class="dot ${a.status}"></span>
      <span><span class="nm">${esc(a.name)}</span><span class="sub">${esc(a.domain || (a.url ? a.url.replace(/^https?:\/\//, '') : 'no route'))}</span></span>
    </a>`).join('') || '<div class="app-link"><span class="dot"></span><span class="sub">no apps yet</span></div>';
  return `
  <aside class="rail">
    <div class="brand"><b>SHOAL DEPLOY</b><span>v0.1.0</span></div>
    <div class="rail-actions"><a class="btn" style="display:block;text-align:center" href="#/new">+ New app</a></div>
    <nav class="app-list">${items}</nav>
    <div class="rail-foot">
      <div>${esc(st.host)}</div>
      <div>docker ${esc(st.docker)}</div>
      <div>caddy ${st.caddy ? 'active' : 'off'}</div>
      <div style="margin-top:6px"><a href="#/settings" class="dim">[settings]</a> &nbsp; <a href="#" id="logout" class="dim">[logout]</a></div>
    </div>
  </aside>`;
}

function shell(active, topbar, body) {
  root.innerHTML = `<div class="shell">${rail(active)}<main class="main"><div class="topbar">${topbar}</div>${body}</main></div>`;
  const out = root.querySelector('#logout');
  if (out) out.addEventListener('click', async (e) => { e.preventDefault(); await api('/api/logout', { method: 'POST' }); st.user = null; renderLogin(); });
}

/* ---------- overview ---------- */
async function renderHome() {
  clearInterval(timer);
  await refresh();
  const body = st.apps.length ? `
    <div class="wrap">
      <div class="head"><div>
        <h1>Applications</h1>
        <div class="meta"><span>${st.apps.length} app${st.apps.length === 1 ? '' : 's'}</span><span>${st.apps.filter((a) => a.status === 'ready').length} live</span></div>
      </div><a class="btn primary" href="#/new">+ New app</a></div>
      <div class="deploys" style="margin-top:26px">
        ${st.apps.map((a) => `
          <a class="deploy" href="#/app/${encodeURIComponent(a.name)}">
            <div><div class="id">${esc(a.name)}</div>
              <div class="sub"><span>${esc(a.repo || 'no source')}</span><span>${esc(a.url || a.domain || '—')}</span></div></div>
            <div class="right"><span class="bracket ${a.status}">[${esc((a.status || 'none').toUpperCase())}]</span></div>
          </a>`).join('')}
      </div>
    </div>` : `
    <div class="wrap"><div class="empty-state">
      <h1>Nothing deployed yet.</h1>
      <p>Point an app at a git repository with a Dockerfile. Shoal Deploy builds it, runs it as a container, health-checks it and puts it behind a route — no pipeline files, no cluster.</p>
      <a class="btn primary" href="#/new">+ New app</a>
    </div></div>`;
  shell('', `<div class="where">control plane · <b>${esc(st.user)}</b></div>`, body);
}

async function refresh() {
  try {
    const data = await api('/api/overview');
    st = data;
  } catch (e) { /* login view already shown */ }
}

/* ---------- new app ---------- */
function renderNew() {
  clearInterval(timer);
  const body = `
  <div class="wrap">
    <div class="head"><div><h1>New app</h1>
      <div class="meta"><span>repository + Dockerfile</span><span>one container, one route</span></div></div></div>
    <form class="form" id="create" style="margin-top:26px">
      <div class="row2">
        <div class="field"><label for="name">Name</label><input id="name" type="text" placeholder="office-api" required spellcheck="false"><small>lowercase, one word</small></div>
        <div class="field"><label for="port">Container port</label><input id="port" type="number" value="80" min="1" max="65535"></div>
      </div>
      <div class="field"><label for="repo">Repository</label><input id="repo" type="text" placeholder="https://github.com/org/repo.git" spellcheck="false"><small>https or ssh url; leave empty to deploy an existing image name typed in the image field below</small></div>
      <div class="row2">
        <div class="field"><label for="ref">Branch or tag</label><input id="ref" type="text" placeholder="main" spellcheck="false"></div>
        <div class="field"><label for="dockerfile">Dockerfile</label><input id="dockerfile" type="text" placeholder="Dockerfile" spellcheck="false"></div>
      </div>
      <div class="field"><label for="ctx">Build context</label><input id="ctx" type="text" placeholder="." spellcheck="false"><small>folder sent to docker build, relative to the repository root</small></div>
      <div class="row2">
        <div class="field"><label for="domain">Domain</label><input id="domain" type="text" placeholder="app.example.com" spellcheck="false"><small>optional; needs caddy running with DNS pointed here</small></div>
        <div class="field"><label for="health">Health path</label><input id="health" type="text" placeholder="/" spellcheck="false"><small>checked before the route switches; use "tcp" for services that do not speak HTTP</small></div>
      </div>
      <label class="check"><input type="checkbox" id="db"> Add a PostgreSQL database and inject DATABASE_URL</label>
      <div class="field"><label for="image">Image override (optional)</label><input id="image" type="text" placeholder="nginx:alpine" spellcheck="false"><small>skips the build; useful for a first smoke test</small></div>
      <div class="err" id="err"></div>
      <div class="actions"><button class="btn primary" type="submit">Create app</button><a class="btn ghost" href="#/">Cancel</a></div>
    </form>
  </div>`;
  shell('', `<div class="where">control plane · <b>${esc(st.user)}</b></div>`, body);
  root.querySelector('#create').addEventListener('submit', async (e) => {
    e.preventDefault();
    const btn = e.target.querySelector('button');
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
      env: {},
    };
    const image = root.querySelector('#image').value.trim();
    try {
      await api('/api/apps', { method: 'POST', body: JSON.stringify(payload) });
      const app = (await api('/api/apps/' + encodeURIComponent(payload.name))).app;
      if (image) {
        // A first deploy from a plain image: record it so the app has something live.
        await api('/api/apps/' + encodeURIComponent(payload.name), {
          method: 'PUT', body: JSON.stringify({ repo: '' }),
        });
        await api('/api/apps/' + encodeURIComponent(payload.name) + '/deploy', { method: 'POST' });
      } else {
        await api('/api/apps/' + encodeURIComponent(app.name) + '/deploy', { method: 'POST' });
      }
      location.hash = '#/app/' + encodeURIComponent(payload.name);
    } catch (err) {
      root.querySelector('#err').textContent = err.message;
      btn.disabled = false;
    }
  });
}

/* ---------- app detail ---------- */
async function renderApp(name) {
  clearInterval(timer);
  const data = await api('/api/apps/' + encodeURIComponent(name));
  detail = data;
  selected = data.app.deploy_id || (data.deploys[0] && data.deploys[0].id) || null;

  const a = data.app;
  const body = `
  <div class="wrap">
    <div class="head">
      <div>
        <h1>${esc(a.name)}</h1>
        <div class="meta">
          <span>${esc(a.repo || 'no repository')}${a.ref ? '@' + esc(a.ref) : ''}</span>
          ${a.url ? `<a href="${esc(a.url)}" target="_blank" rel="noreferrer">${esc(a.url)}</a>` : '<span>no route</span>'}
        </div>
      </div>
      <div style="display:flex;gap:8px">
        <button class="btn" id="logs">App logs</button>
        <button class="btn primary" id="deploy">Deploy</button>
      </div>
    </div>

    <div class="band">
      <div><div class="k">Status</div><div class="v" id="b-status">—</div></div>
      <div><div class="k">Deploy</div><div class="v" id="b-deploy">—</div></div>
      <div><div class="k">Commit</div><div class="v" id="b-commit">—</div></div>
      <div><div class="k">Port</div><div class="v" id="b-port">—</div></div>
    </div>

    <div class="grid">
      <div>
        <div class="section-h"><h2>Deploys</h2><span class="tag" id="count"></span></div>
        <div class="deploys" id="deploys"></div>
      </div>
      <div>
        <div class="section-h"><h2 id="logtitle">Build log</h2><span class="tag" id="livetag"></span><button class="btn small" id="rollback" style="display:none">Roll back to this deploy</button></div>
        <pre class="log" id="log"></pre>
      </div>
    </div>

    <div style="margin-top:40px">
      <div class="section-h"><h2>Environment</h2><span class="tag">injected at run time</span></div>
      <table class="env-table" id="env"></table>
      <div class="actions">
        <button class="btn small" id="addenv">+ add variable</button>
        <button class="btn small" id="saveenv">Save environment</button>
        <span class="err" id="enverr"></span>
      </div>
    </div>

    <div style="margin-top:40px">
      <div class="section-h"><h2>Configuration</h2><span class="tag">applied on the next deploy</span></div>
      <div class="form" style="margin-top:14px">
        <div class="row2">
          <div class="field"><label>Repository</label><input id="c-repo" type="text" value="${esc(a.repo)}" spellcheck="false"></div>
          <div class="field"><label>Branch or tag</label><input id="c-ref" type="text" value="${esc(a.ref)}" spellcheck="false"></div>
        </div>
        <div class="row2">
          <div class="field"><label>Dockerfile</label><input id="c-dockerfile" type="text" value="${esc(a.dockerfile)}" spellcheck="false"></div>
          <div class="field"><label>Build context</label><input id="c-ctx" type="text" value="${esc(a.context)}" placeholder="." spellcheck="false"></div>
        </div>
        <div class="field"><label>Container port</label><input id="c-port" type="number" value="${a.port}"></div>
        <div class="row2">
          <div class="field"><label>Domain</label><input id="c-domain" type="text" value="${esc(a.domain)}" spellcheck="false"></div>
          <div class="field"><label>Health path</label><input id="c-health" type="text" value="${esc(a.health_path)}" placeholder="/ or tcp" spellcheck="false"></div>
        </div>
        <div class="actions">
          <button class="btn small" id="savecfg">Save configuration</button>
          <button class="btn small ghost" id="destroy" style="margin-left:auto;color:var(--fail)">Delete app</button>
          <span class="err" id="cfgerr"></span>
        </div>
        <div class="hint" id="hook"></div>
      </div>
    </div>
  </div>`;

  shell(a.name, `<div class="where">app · <b>${esc(a.name)}</b> · ${esc(a.status)}</div>`, body);

  root.querySelector('#deploy').addEventListener('click', async (e) => {
    e.target.disabled = true;
    try {
      await api('/api/apps/' + encodeURIComponent(name) + '/deploy', { method: 'POST' });
      poll();
    } catch (err) {
      alert(err.message);   // e.g. 409 while another deploy of this app runs
    } finally {
      setTimeout(() => { e.target.disabled = false; }, 1500);
    }
  });
  root.querySelector('#logs').addEventListener('click', async () => {
    const out = await api('/api/apps/' + encodeURIComponent(name) + '/logs');
    selected = null;
    detail.logs = out.logs || '(no output)';
    renderDetailParts(true);
  });
  if (!Object.keys(a.env || {}).length) root.querySelector('#env').innerHTML = envRow('', '');
  root.querySelector('#addenv').addEventListener('click', () => {
    const tb = root.querySelector('#env');
    tb.insertAdjacentHTML('beforeend', envRow('', ''));
  });
  root.querySelector('#saveenv').addEventListener('click', async () => {
    const env = {};
    root.querySelectorAll('#env tr').forEach((tr) => {
      const k = tr.querySelector('.ek').value.trim();
      const v = tr.querySelector('.ev').value;
      if (k) env[k] = v;
    });
    try {
      await api('/api/apps/' + encodeURIComponent(name), { method: 'PUT', body: JSON.stringify({ env }) });
      root.querySelector('#enverr').textContent = 'saved';
      setTimeout(() => (root.querySelector('#enverr').textContent = ''), 2500);
    } catch (err) { root.querySelector('#enverr').textContent = err.message; }
  });
  root.querySelector('#savecfg').addEventListener('click', async () => {
    try {
      await api('/api/apps/' + encodeURIComponent(name), {
        method: 'PUT',
        body: JSON.stringify({
          repo: root.querySelector('#c-repo').value.trim(),
          ref: root.querySelector('#c-ref').value.trim(),
          dockerfile: root.querySelector('#c-dockerfile').value.trim(),
          context: root.querySelector('#c-ctx').value.trim(),
          port: Number(root.querySelector('#c-port').value) || 80,
          domain: root.querySelector('#c-domain').value.trim(),
          health_path: root.querySelector('#c-health').value.trim(),
        }),
      });
      renderApp(name);
    } catch (err) { root.querySelector('#cfgerr').textContent = err.message; }
  });
  root.querySelector('#destroy').addEventListener('click', async () => {
    if (!confirm('Delete ' + name + '? Containers are removed. Database volume is kept unless you pass data=1 via the API.')) return;
    await api('/api/apps/' + encodeURIComponent(name), { method: 'DELETE' });
    location.hash = '#/';
  });
  root.querySelector('#hook').textContent = 'webhook: POST /api/webhook/' + name + '?token=<app token>';
  renderDetailParts();
  poll();
}

function envRow(k, v) {
  return `<tr>
    <td><input class="ek" type="text" value="${esc(k)}" placeholder="KEY" spellcheck="false"></td>
    <td><input class="ev" type="text" value="${esc(v)}" placeholder="value" spellcheck="false"></td>
    <td><button class="btn small ghost" onclick="this.closest('tr').remove()">x</button></td>
  </tr>`;
}

function renderDetailParts(forceLog) {
  const a = detail.app;
  const live = detail.deploys.find((d) => d.live);
  const bs = root.querySelector('#b-status');
  bs.textContent = (a.status || 'none').toUpperCase();
  bs.className = 'v ' + (a.status || '');
  root.querySelector('#b-deploy').textContent = live ? live.id : '—';
  root.querySelector('#b-commit').textContent = (live && live.commit) || '—';
  root.querySelector('#b-port').textContent = live && live.port ? String(live.port) : '—';

  const list = root.querySelector('#deploys');
  const rows = detail.deploys.map((d) => `
    <div class="deploy ${d.id === selected ? 'sel' : ''}" data-id="${esc(d.id)}">
      <div>
        <div class="id">${d.live ? '● ' : ''}${esc(d.id)}</div>
        <div class="sub"><span>${esc(d.commit || 'no commit')}</span><span>${new Date(d.started).toLocaleTimeString()}</span></div>
      </div>
      <div class="right"><span class="bracket ${d.status}">[${esc(d.status.toUpperCase())}]</span></div>
    </div>`).join('');
  if (list.dataset.sig !== rows) { list.innerHTML = rows; list.dataset.sig = rows; }
  list.querySelectorAll('.deploy').forEach((n) => n.addEventListener('click', () => {
    selected = n.dataset.id;
    renderDetailParts(true);
  }));
  root.querySelector('#count').textContent = detail.deploys.length + ' total';

  const cur = selected ? detail.deploys.find((d) => d.id === selected) : null;
  const log = root.querySelector('#log');
  let text;
  if (cur) { text = cur.log || '(waiting for output)'; root.querySelector('#logtitle').textContent = 'Build log · ' + cur.id; }
  else { text = detail.logs || '(no container output yet)'; root.querySelector('#logtitle').textContent = 'App logs'; }
  const running = cur && ['queued', 'building', 'starting'].includes(cur.status);
  root.querySelector('#livetag').textContent = running ? '[running]' : '';
  const rb = root.querySelector('#rollback');
  const canRoll = cur && !cur.live && cur.status === 'ready' && cur.image;
  rb.style.display = canRoll ? '' : 'none';
  rb.disabled = !!inFlight;
  if (canRoll) rb.onclick = async () => {
    inFlight = true;
    try {
      await api('/api/apps/' + encodeURIComponent(decodeURIComponent(location.hash.replace('#/app/', ''))) + '/rollback',
        { method: 'POST', body: JSON.stringify({ deploy: cur.id }) });
      selected = null;
      poll();
    } catch (err) { alert(err.message); }
    finally { inFlight = false; renderDetailParts(); }
  };
  const atBottom = log.scrollTop + log.clientHeight >= log.scrollHeight - 40;
  if (log.textContent !== text) { log.textContent = text; if (atBottom || forceLog) log.scrollTop = log.scrollHeight; }
}

function poll() {
  clearInterval(timer);
  let fast = 2500;
  timer = setInterval(async () => {
    const name = location.hash.replace('#/app/', '');
    if (!name) return;
    try {
      const data = await api('/api/apps/' + encodeURIComponent(decodeURIComponent(name)));
      detail = data;
      renderDetailParts();
      const active = data.deploys.some((d) => ['queued', 'building', 'starting'].includes(d.status));
      if (!active && fast !== 8000) { clearInterval(timer); fast = 8000; poll(); }
      if (active) {
        const cur = selected && data.deploys.find((d) => d.id === selected);
        if (cur && ['ready', 'failed'].includes(cur.status)) refresh();
      }
    } catch (e) { /* keep the last view */ }
  }, fast);
}

/* ---------- settings ---------- */
function renderSettings() {
  clearInterval(timer);
  const body = `
  <div class="wrap">
    <div class="head"><div><h1>Settings</h1><div class="meta"><span>account ${esc(st.user)}</span><span>${esc(st.host)}</span></div></div></div>
    <div class="form" style="margin-top:26px">
      <div class="field"><label>Current passphrase</label><input id="cur" type="password" autocomplete="current-password"></div>
      <div class="field"><label>New passphrase</label><input id="next" type="password" autocomplete="new-password"><small>at least 8 characters</small></div>
      <div class="actions"><button class="btn small" id="savepw">Change passphrase</button><span class="err" id="pwerr"></span></div>
      <div class="hint">State lives in the data directory as one JSON file. Containers, images and the database volume live in Docker. Nothing is sent anywhere else.</div>
    </div>
  </div>`;
  shell('', `<div class="where">control plane · <b>${esc(st.user)}</b></div>`, body);
  root.querySelector('#savepw').addEventListener('click', async () => {
    try {
      await api('/api/password', {
        method: 'POST',
        body: JSON.stringify({ current: root.querySelector('#cur').value, next: root.querySelector('#next').value }),
      });
      root.querySelector('#pwerr').textContent = 'changed';
      root.querySelector('#cur').value = '';
      root.querySelector('#next').value = '';
    } catch (err) { root.querySelector('#pwerr').textContent = err.message; }
  });
}

/* ---------- router ---------- */
async function boot() {
  try {
    st = await api('/api/overview');
  } catch (e) { return; }
  route();
}

async function route() {
  const h = location.hash || '#/';
  if (!st.user) return renderLogin();
  if (h === '#/' || h === '') return renderHome();
  if (h === '#/new') return renderNew();
  if (h === '#/settings') return renderSettings();
  if (h.startsWith('#/app/')) return renderApp(decodeURIComponent(h.slice(6)));
  return renderHome();
}

window.addEventListener('hashchange', () => { clearInterval(timer); route().catch((e) => { root.innerHTML = `<div class="wrap"><div class="err">${esc(e.message)}</div></div>`; }); });
boot();
