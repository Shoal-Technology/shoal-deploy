'use strict';

const root = document.getElementById('root');
let st = { user: null, apps: [], host: '', docker: '', caddy: false };
let detail = null;      // { app, deploys, logs }
let selected = null;    // deploy id
let inFlight = false;   // a deploy or rollback is in flight
let timer = null;

const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const logo = '<img src="/logo.svg" alt="" width="30" height="23">';

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

/* Plain-language vocabulary. The portal is used by people who do not know what
   a container, a build context or a health check is, so the UI never says those
   words unless the user opened the technical section on purpose. */
const STATE = {
  ready: 'Sedang jalan',
  building: 'Sedang dibangun',
  starting: 'Sedang dinyalakan',
  queued: 'Menunggu giliran',
  failed: 'Gagal',
  none: 'Belum pernah dijalankan',
};
const VERDICT = {
  ready: 'Aplikasi jalan.',
  building: 'Sedang dibangun.',
  starting: 'Sedang dinyalakan.',
  queued: 'Menunggu giliran dibangun.',
  failed: 'Deploy terakhir gagal.',
  none: 'Aplikasi ini belum pernah dijalankan.',
};

function ago(iso) {
  if (!iso) return '—';
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return 'baru saja';
  if (s < 3600) return Math.floor(s / 60) + ' menit lalu';
  if (s < 86400) return Math.floor(s / 3600) + ' jam lalu';
  if (s < 30 * 86400) return Math.floor(s / 86400) + ' hari lalu';
  return new Date(iso).toLocaleDateString('id-ID', { day: 'numeric', month: 'short', year: 'numeric' });
}

// 127.0.0.1 addresses only work on the server itself; say so instead of letting
// a beginner paste it into their own browser and think the app is broken.
const localOnly = (u) => /^https?:\/\/(127\.0\.0\.1|localhost|\[::1\])/i.test(u || '');

/* ---------- login ---------- */
function renderLogin(msg = '') {
  clearInterval(timer);
  root.innerHTML = `
  <div class="login-wrap">
    <form class="login" id="login">
      <div class="mark">
        ${logo}
        <b>shoal deploy</b>
        <span>v${esc(String(st.version || '').replace(/^v/, '') || '0.1.9')}</span>
        <em>Satu kotak, semua aplikasi.</em>
      </div>
      <div class="field">
        <label for="u">Nama pengguna</label>
        <input id="u" type="text" value="admin" autocomplete="username" spellcheck="false">
      </div>
      <div class="field">
        <label for="p">Kata sandi</label>
        <input id="p" type="password" autocomplete="current-password">
        <small>pertama kali: buka file initial-passphrase di folder data</small>
      </div>
      <div class="err" id="err">${esc(msg)}</div>
      <button class="btn primary wide" type="submit">Masuk</button>
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

/* ---------- frame ---------- */
function frame(body, active = '') {
  root.innerHTML = `
  <header class="top">
    <a class="lockup" href="#/">
      ${logo}
      <span class="words"><b>shoal</b><span>deploy</span></span>
    </a>
    <div class="right">
      <a href="#/" ${active === 'apps' ? 'style="color:var(--accent)"' : ''}>Aplikasi</a>
      <a href="#/settings" ${active === 'settings' ? 'style="color:var(--accent)"' : ''}>Pengaturan</a>
      <button id="logout" type="button">Keluar</button>
    </div>
  </header>
  <main class="page">${body}</main>`;
  const out = root.querySelector('#logout');
  if (out) out.addEventListener('click', async () => {
    await api('/api/logout', { method: 'POST' });
    st.user = null;
    location.hash = '#/';
    renderLogin();
  });
}

const sig = `<footer class="sig"><span>shoal deploy${st.host ? ' · ' + esc(st.host) : ''}</span><span>${st.caddy ? 'tls aktif' : 'tanpa domain'}</span></footer>`;

/* ---------- home ---------- */
async function renderHome() {
  clearInterval(timer);
  await refresh();
  const apps = st.apps || [];
  const live = apps.filter((a) => a.status === 'ready').length;

  const cards = apps.map((a) => {
    const s = a.status || 'none';
    return `
    <a class="card" href="#/app/${encodeURIComponent(a.name)}">
      <div>
        <div class="name"><span>${esc(a.name)}</span></div>
        <div class="state">
          <span class="dot ${esc(s)}"></span>
          <b>${esc(STATE[s] || s)}</b>
          ${a.url ? `<span class="url">· ${esc(a.url.replace(/^https?:\/\//, ''))}</span>` : ''}
        </div>
      </div>
      <div class="go">
        ${s === 'ready' && a.url && !localOnly(a.url)
          ? `<a class="btn primary small" href="${esc(a.url)}" target="_blank" rel="noreferrer" onclick="event.stopPropagation()">Buka</a>`
          : `<span class="btn small">Lihat</span>`}
      </div>
    </a>`;
  }).join('');

  const body = apps.length ? `
    <div class="lede">
      <h1>Aplikasi <i>kamu.</i></h1>
      <p>Semua yang berjalan di server ini. Klik satu aplikasi untuk melihat catatannya, memakai versi lama, atau menyalakannya lagi.</p>
      <div class="count"><span><b>${apps.length}</b> aplikasi</span><span><b>${live}</b> sedang jalan</span></div>
    </div>
    <div class="cards">${cards}</div>
    <div class="actions end" style="margin-top:26px"><a class="btn primary" href="#/new">+ Tambah aplikasi</a></div>
    ${sig}` : `
    <div class="empty">
      <div class="lede">
        <h1>Belum ada <i>apa-apa.</i></h1>
        <p>Server ini kosong. Kalau kamu punya kode aplikasi di GitHub atau GitLab, tempel alamatnya — shoal yang membangun dan menjalankannya.</p>
      </div>
      <div class="steps">
        <div class="step"><div class="n">01</div><div><div class="t">Beri nama</div><div class="d">Satu kata, huruf kecil. Ini yang muncul di daftar.</div></div></div>
        <div class="step"><div class="n">02</div><div><div class="t">Tempel alamat kodenya</div><div class="d">Link repo GitHub atau GitLab. Repo-nya harus punya file <span class="mono">Dockerfile</span> di folder utama.</div></div></div>
        <div class="step"><div class="n">03</div><div><div class="t">Tunggu, lalu buka</div><div class="d">Shoal membangun aplikasinya, mengecek sampai benar-benar hidup, lalu memberi kamu alamat untuk dibuka.</div></div></div>
      </div>
      <div class="actions"><a class="btn primary" href="#/new">+ Tambah aplikasi</a><a class="btn ghost" href="#/new?contoh=1">Coba contoh dulu</a></div>
      ${sig}
    </div>`;

  frame(body, 'apps');
}

async function refresh() {
  try { st = await api('/api/overview'); } catch (e) { /* login view already shown */ }
}

/* ---------- add app ---------- */
function renderNew() {
  clearInterval(timer);
  const body = `
    <div class="lede">
      <h1>Tambah <i>aplikasi.</i></h1>
      <p>Cukup dua hal: nama dan alamat kodenya. Sisanya sudah diisi otomatis dan bisa kamu ubah nanti.</p>
    </div>
    <form class="form" id="create">
      <div class="field">
        <span class="q">1. Mau disebut apa?</span>
        <input id="name" type="text" placeholder="kasir" required spellcheck="false" autocomplete="off">
        <small>huruf kecil, tanpa spasi — misalnya <span class="mono">kasir</span> atau <span class="mono">laporan-bulanan</span></small>
      </div>
      <div class="field">
        <span class="q">2. Kodenya ada di mana?</span>
        <input id="repo" type="text" placeholder="https://github.com/nama/repo.git" spellcheck="false" autocomplete="off">
        <small>tempel link repo git yang <b>publik</b> — bukan link halaman file, dan harus ada <span class="mono">Dockerfile</span> di dalamnya.</small>
      </div>
      <div class="err" id="err"></div>
      <div class="actions">
        <button class="btn primary" type="submit">Bangun dan jalankan</button>
        <a class="btn ghost" href="#/">Batal</a>
      </div>

      <details class="panel" style="margin-top:10px">
        <summary>Pengaturan lanjutan</summary>
        <div class="body">
          <p class="note">Kosongkan saja kalau tidak yakin. Nilai di bawah ini yang dipakai shoal untuk repo biasa.</p>
          <div class="row2">
            <div class="field"><label for="ref">Cabang atau tag</label><input id="ref" type="text" placeholder="main" spellcheck="false"></div>
            <div class="field"><label for="port">Port di dalam container</label><input id="port" type="number" value="80" min="1" max="65535"></div>
          </div>
          <div class="row2" style="margin-top:16px">
            <div class="field"><label for="dockerfile">Nama Dockerfile</label><input id="dockerfile" type="text" placeholder="Dockerfile" spellcheck="false"></div>
            <div class="field"><label for="ctx">Folder build</label><input id="ctx" type="text" placeholder="." spellcheck="false"></div>
          </div>
          <div class="row2" style="margin-top:16px">
            <div class="field"><label for="domain">Domain</label><input id="domain" type="text" placeholder="kasir.perusahaan.local" spellcheck="false"><small>butuh caddy aktif dan DNS diarahkan ke server ini</small></div>
            <div class="field"><label for="health">Alamat pengecekan</label><input id="health" type="text" placeholder="/" spellcheck="false"><small>dibuka dulu sebelum aplikasi dianggap hidup; tulis <span class="mono">tcp</span> untuk yang bukan web</small></div>
          </div>
          <label class="check" style="margin-top:18px"><input type="checkbox" id="db"><span>Tambahkan database PostgreSQL<small>Shoal membuatkan databasenya dan mengisi <span class="mono">DATABASE_URL</span> sendiri.</small></span></label>
          <div class="field" style="margin-top:16px"><label for="image">Pakai image jadi (lewati build)</label><input id="image" type="text" placeholder="nginx:alpine" spellcheck="false"><small>untuk mencoba cepat tanpa repo</small></div>
        </div>
      </details>
    </form>`;

  frame(body, 'apps');

  const q = new URLSearchParams((location.hash.split('?')[1] || ''));
  if (q.get('contoh')) {
    root.querySelector('#name').value = 'contoh';
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
      root.querySelector('#err').textContent = 'Isi alamat kodenya dulu, atau isi bagian "pakai image jadi" di pengaturan lanjutan.';
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
  await refresh();   // the frame renders from st.apps
  const data = await api('/api/apps/' + encodeURIComponent(name));
  detail = data;
  selected = data.app.deploy_id || (data.deploys[0] && data.deploys[0].id) || null;
  const a = data.app;

  const body = `
    <div class="lede">
      <h1>${esc(a.name)}</h1>
      <div class="count"><span>${esc(a.repo || a.image || 'belum ada kode')}${a.ref ? '@' + esc(a.ref) : ''}</span></div>
    </div>

    <div class="hero">
      <div class="line"><span class="verdict" id="verdict">—</span></div>
      <div class="why" id="why"></div>
      <div class="open">
        <button class="btn primary" id="open">Buka aplikasi</button>
        <button class="btn ghost" id="deploy">Bangun ulang</button>
      </div>
      <div class="addr" id="addr"></div>
    </div>

    <details class="panel" open>
      <summary>Riwayat versi</summary>
      <div class="body">
        <div class="hist" id="hist"></div>
        <p class="note" style="margin-top:14px">Versi lama tetap tersimpan. Kalau versi baru rusak, pilih versi sebelumnya lalu tekan <b>Pakai versi ini</b>.</p>
      </div>
    </details>

    <details class="panel">
      <summary>Catatan build</summary>
      <div class="body">
        <p class="note" id="logtitle">Pilih satu versi di atas untuk membaca catatannya.</p>
        <pre class="log" id="log"></pre>
      </div>
    </details>

    <details class="panel">
      <summary>Variabel lingkungan</summary>
      <div class="body">
        <p class="note">Nilai yang dikirim ke aplikasi saat dijalankan, misalnya kunci API. Disimpan di server ini.</p>
        <table class="env-table" id="env"></table>
        <div class="actions end">
          <button class="btn small" id="addenv">+ Tambah</button>
          <button class="btn small" id="saveenv">Simpan</button>
          <span class="err ok" id="enverr"></span>
        </div>
      </div>
    </details>

    <details class="panel">
      <summary>Pengaturan teknis</summary>
      <div class="body">
        <p class="note">Berlaku pada deploy berikutnya.</p>
        <form class="form" id="cfg" style="gap:16px">
          <div class="field"><label for="c-repo">Alamat kode</label><input id="c-repo" type="text" value="${esc(a.repo)}" spellcheck="false"></div>
          <div class="row2">
            <div class="field"><label for="c-ref">Cabang atau tag</label><input id="c-ref" type="text" value="${esc(a.ref)}" spellcheck="false"></div>
            <div class="field"><label for="c-port">Port container</label><input id="c-port" type="number" value="${a.port}"></div>
          </div>
          <div class="row2">
            <div class="field"><label for="c-dockerfile">Dockerfile</label><input id="c-dockerfile" type="text" value="${esc(a.dockerfile)}" spellcheck="false"></div>
            <div class="field"><label for="c-ctx">Folder build</label><input id="c-ctx" type="text" value="${esc(a.context)}" placeholder="." spellcheck="false"></div>
          </div>
          <div class="row2">
            <div class="field"><label for="c-domain">Domain</label><input id="c-domain" type="text" value="${esc(a.domain)}" spellcheck="false"></div>
            <div class="field"><label for="c-health">Alamat pengecekan</label><input id="c-health" type="text" value="${esc(a.health_path)}" placeholder="/ atau tcp" spellcheck="false"></div>
          </div>
          <div class="actions"><button class="btn small" type="submit">Simpan konfigurasi</button><span class="err ok" id="cfgerr"></span></div>
        </form>
        <div class="note" id="hook" style="margin-top:16px"></div>
        <div class="actions" style="margin-top:22px">
          <button class="btn small ghost" id="destroy" style="color:var(--danger)">Hapus aplikasi ini</button>
        </div>
      </div>
    </details>

    <div class="actions" style="margin-top:30px"><a class="btn ghost small" href="#/">← Semua aplikasi</a></div>
    ${sig}`;

  frame(body, 'apps');

  root.querySelector('#open').addEventListener('click', () => {
    const u = detail.app.url || (liveDeploy() && liveDeploy().url);
    if (u) window.open(u, '_blank', 'noreferrer');
  });
  root.querySelector('#deploy').addEventListener('click', async (e) => {
    e.target.disabled = true;
    try {
      await api('/api/apps/' + encodeURIComponent(name) + '/deploy', { method: 'POST' });
      selected = null;
      poll();
    } catch (err) {
      alert(err.message);
    } finally {
      setTimeout(() => { e.target.disabled = false; }, 1500);
    }
  });
  if (!Object.keys(a.env || {}).length) root.querySelector('#env').innerHTML = envRow('', '');
  root.querySelector('#addenv').addEventListener('click', () => {
    root.querySelector('#env').insertAdjacentHTML('beforeend', envRow('', ''));
  });
  root.querySelector('#saveenv').addEventListener('click', async () => {
    const env = {};
    root.querySelectorAll('#env tr').forEach((tr) => {
      const k = tr.querySelector('.ek').value.trim();
      if (k) env[k] = tr.querySelector('.ev').value;
    });
    try {
      await api('/api/apps/' + encodeURIComponent(name), { method: 'PUT', body: JSON.stringify({ env }) });
      root.querySelector('#enverr').textContent = 'tersimpan';
      setTimeout(() => (root.querySelector('#enverr').textContent = ''), 2500);
    } catch (err) { root.querySelector('#enverr').className = 'err'; root.querySelector('#enverr').textContent = err.message; }
  });
  root.querySelector('#cfg').addEventListener('submit', async (e) => {
    e.preventDefault();
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
    } catch (err) { root.querySelector('#cfgerr').className = 'err'; root.querySelector('#cfgerr').textContent = err.message; }
  });
  root.querySelector('#destroy').addEventListener('click', async () => {
    if (!confirm('Hapus ' + name + '? Containernya dihentikan. Volume database tetap disimpan.')) return;
    await api('/api/apps/' + encodeURIComponent(name), { method: 'DELETE' });
    location.hash = '#/';
  });
  root.querySelector('#hook').textContent = 'Untuk deploy otomatis dari git: POST /api/webhook/' + name + '?token=<token aplikasi>';
  renderDetailParts();
  poll();
}

function liveDeploy() {
  return detail && detail.deploys.find((d) => d.live);
}

function envRow(k, v) {
  return `<tr>
    <td><input class="ek" type="text" value="${esc(k)}" placeholder="NAMA" spellcheck="false"></td>
    <td><input class="ev" type="text" value="${esc(v)}" placeholder="nilai" spellcheck="false"></td>
    <td><button class="btn small ghost del" type="button" onclick="this.closest('tr').remove()" aria-label="hapus variabel" title="hapus">×</button></td>
  </tr>`;
}

function renderDetailParts(forceLog) {
  const a = detail.app;
  const s = a.status || 'none';

  const verdict = root.querySelector('#verdict');
  if (!verdict) return;
  verdict.textContent = VERDICT[s] || s;
  verdict.className = 'verdict ' + s;

  const why = root.querySelector('#why');
  if (s === 'ready') {
    why.textContent = localOnly(a.url)
      ? 'Berjalan di server ini. Alamat di bawah hanya bisa dibuka dari server; tambahkan domain supaya bisa dibuka dari komputer lain.'
      : 'Siapa pun di jaringan ini bisa membukanya lewat alamat di bawah.';
  } else if (s === 'failed') {
    // The failing deploy is never live, so read the error off the selected row.
    const bad = detail.deploys.find((d) => d.id === selected) || detail.deploys[0];
    why.textContent = (bad && bad.error) || 'Buka "Catatan build" untuk melihat bagian mana yang gagal.';
  } else if (s === 'none') {
    why.textContent = 'Tekan "Bangun ulang" untuk membangun dan menjalankannya.';
  } else {
    why.textContent = 'Biasanya selesai dalam satu sampai tiga menit. Halaman ini memperbarui sendiri.';
  }

  const openBtn = root.querySelector('#open');
  openBtn.disabled = !a.url;
  openBtn.style.display = a.url ? '' : 'none';
  const addr = root.querySelector('#addr');
  addr.innerHTML = a.url
    ? `Alamat: <a href="${esc(a.url)}" target="_blank" rel="noreferrer">${esc(a.url)}</a>`
    : 'Belum punya alamat — tambahkan domain di pengaturan teknis.';

  const rows = detail.deploys.map((d) => `
    <div class="row ${d.id === selected ? 'sel' : ''}" data-id="${esc(d.id)}">
      <div>
        <div class="when">${esc(ago(d.started))}${d.live ? '<em>dipakai sekarang</em>' : ''}</div>
        <div class="meta">
          <span>${esc(d.commit && d.commit !== 'unknown' ? d.commit.slice(0, 7) : 'tanpa commit')}</span>
          <span>${esc(d.id)}</span>
        </div>
      </div>
      <div class="right">
        <span class="bracket ${esc(d.status)}">${esc((STATE[d.status] || d.status).toUpperCase())}</span>
        ${!d.live && d.status === 'ready' && d.image ? `<button class="btn small" data-roll="${esc(d.id)}" type="button">Pakai versi ini</button>` : ''}
      </div>
    </div>`).join('') || `<p class="note">Belum ada riwayat. Tekan "Bangun ulang" untuk memulai yang pertama.</p>`;

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
        await api('/api/apps/' + encodeURIComponent(detail.app.name) + '/rollback', {
          method: 'POST', body: JSON.stringify({ deploy: b.dataset.roll }),
        });
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
  if (cur) {
    text = cur.log || '(belum ada keluaran)';
    title.innerHTML = 'Catatan versi <b class="mono">' + esc(cur.id) + '</b>';
  } else {
    text = detail.logs || '(aplikasi belum mengeluarkan apa pun)';
    title.textContent = 'Keluaran aplikasi yang sedang jalan:';
  }
  const atBottom = log.scrollTop + log.clientHeight >= log.scrollHeight - 40;
  if (log.textContent !== text) { log.textContent = text; if (atBottom || forceLog) log.scrollTop = log.scrollHeight; }
}

function poll() {
  clearInterval(timer);
  let every = 2500;
  timer = setInterval(async () => {
    const m = location.hash.match(/^#\/app\/([^?]+)/);
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

/* ---------- settings ---------- */
function renderSettings() {
  clearInterval(timer);
  const body = `
    <div class="lede">
      <h1>Pengaturan.</h1>
      <p>Ganti kata sandi portal. Semua data ada di folder data pada server ini; tidak ada yang dikirim ke luar.</p>
    </div>
    <form class="form" id="pw" style="max-width:420px">
      <div class="field"><label for="cur">Kata sandi sekarang</label><input id="cur" type="password" autocomplete="current-password"></div>
      <div class="field"><label for="next">Kata sandi baru</label><input id="next" type="password" autocomplete="new-password"><small>minimal 8 karakter</small></div>
      <div class="actions"><button class="btn small" type="submit">Ganti kata sandi</button><span class="err ok" id="pwerr"></span></div>
    </form>
    <details class="panel" style="margin-top:34px">
      <summary>Status server</summary>
      <div class="body">
        <div class="hist">
          <div class="row" style="cursor:default"><div class="when">Nama server</div><div class="right mono">${esc(st.host || '—')}</div></div>
          <div class="row" style="cursor:default"><div class="when">Docker</div><div class="right mono">${esc(st.docker || 'tidak terdeteksi')}</div></div>
          <div class="row" style="cursor:default"><div class="when">Domain otomatis</div><div class="right mono">${st.caddy ? 'aktif' : 'belum dipasang'}</div></div>
          <div class="row" style="cursor:default"><div class="when">Versi shoal deploy</div><div class="right mono">v${esc(String(st.version || '').replace(/^v/, ''))}</div></div>
        </div>
      </div>
    </details>
    ${sig}`;
  frame(body, 'settings');
  root.querySelector('#pw').addEventListener('submit', async (e) => {
    e.preventDefault();
    const out = root.querySelector('#pwerr');
    out.className = 'err ok';
    try {
      await api('/api/password', {
        method: 'POST',
        body: JSON.stringify({ current: root.querySelector('#cur').value, next: root.querySelector('#next').value }),
      });
      out.textContent = 'kata sandi diganti';
      root.querySelector('#cur').value = '';
      root.querySelector('#next').value = '';
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
  if (h.startsWith('#/new')) return renderNew();
  if (h.startsWith('#/settings')) return renderSettings();
  if (h.startsWith('#/app/')) return renderApp(decodeURIComponent(h.slice(6).split('?')[0]));
  return renderHome();
}

window.addEventListener('hashchange', () => {
  clearInterval(timer);
  route().catch((e) => {
    root.innerHTML = `<main class="page"><div class="err">${esc(e.message)}</div><p><a class="btn ghost small" href="#/">Kembali</a></p></main>`;
  });
});
boot();
