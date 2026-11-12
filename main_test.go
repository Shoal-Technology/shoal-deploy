package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*Server, *Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := hashPassword("test-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(s *State) { s.Users["admin"] = &User{Name: "admin", Hash: hash} }); err != nil {
		t.Fatal(err)
	}
	dep := &Deployer{store: st, dataDir: dir}
	return &Server{store: st, dep: dep, host: "test"}, st
}

func TestStateSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	st, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(s *State) {
		s.Apps["web"] = &App{Name: "web", Repo: "https://example.test/repo.git", Port: 8080, Env: map[string]string{"A": "1"}}
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	app, ok := reopened.app("web")
	if !ok || app.Port != 8080 || app.Env["A"] != "1" {
		t.Fatalf("app did not survive reload: %+v", app)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
		t.Fatalf("state file missing: %v", err)
	}
}

func TestSessionSignature(t *testing.T) {
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tok := st.signSession("admin", time.Now().Add(time.Hour))
	if user, ok := st.validSession(tok); !ok || user != "admin" {
		t.Fatalf("valid session rejected: %q %v", user, ok)
	}
	if _, ok := st.validSession(tok + "x"); ok {
		t.Fatal("tampered session accepted")
	}
	if _, ok := st.validSession(st.signSession("admin", time.Now().Add(-time.Hour))); ok {
		t.Fatal("expired session accepted")
	}
}

func TestLoginAndAuthWall(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	// No cookie: the API must refuse.
	res, err := http.Get(ts.URL + "/api/overview")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("overview without session: %d", res.StatusCode)
	}

	// Wrong passphrase: 401 and no cookie.
	res, err = http.Post(ts.URL+"/api/login", "application/json",
		strings.NewReader(`{"user":"admin","pass":"nope"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong passphrase: %d", res.StatusCode)
	}
	if len(res.Cookies()) != 0 {
		t.Fatal("wrong passphrase still set a cookie")
	}

	// Right passphrase: cookie, and the cookie opens the API.
	res, err = http.Post(ts.URL+"/api/login", "application/json",
		strings.NewReader(`{"user":"admin","pass":"test-passphrase"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK || len(res.Cookies()) != 1 {
		t.Fatalf("login: %d cookies %d", res.StatusCode, len(res.Cookies()))
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/overview", nil)
	req.AddCookie(res.Cookies()[0])
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("overview with session: %d", res.StatusCode)
	}
	var body struct {
		Apps []appView `json:"apps"`
		User string    `json:"user"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.User != "admin" || body.Apps == nil {
		t.Fatalf("unexpected overview: %+v", body)
	}
}

func TestCreateAppRejectsBadNameAndSecrets(t *testing.T) {
	srv, st := newTestServer(t)
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	cookie := login(t, ts.URL)

	post := func(body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/apps", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	res := post(`{"name":"bad name","port":80}`)
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("name with a space accepted: %d", res.StatusCode)
	}

	res = post(`{"name":"web","port":80,"with_db":true}`)
	var created appView
	json.NewDecoder(res.Body).Decode(&created)
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", res.StatusCode)
	}
	if created.DBPassword != "" || created.Token != "" {
		t.Fatal("create response leaked a generated secret")
	}

	// Duplicate names are refused, and the stored app keeps a token and a db passphrase.
	res = post(`{"name":"web","port":80}`)
	res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate app: %d", res.StatusCode)
	}
	app, _ := st.app("web")
	if app.Token == "" || app.DBPassword == "" {
		t.Fatal("app has no webhook token or db passphrase")
	}
}

func TestWebhookNeedsTheToken(t *testing.T) {
	srv, st := newTestServer(t)
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	if err := st.Update(func(s *State) {
		s.Apps["web"] = &App{Name: "web", Port: 80, Token: "right-token", Env: map[string]string{}}
	}); err != nil {
		t.Fatal(err)
	}
	res, err := http.Post(ts.URL+"/api/webhook/web?token=wrong", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong webhook token: %d", res.StatusCode)
	}
}

func TestPasswordChangeRules(t *testing.T) {
	srv, st := newTestServer(t)
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	cookie := login(t, ts.URL)

	post := func(body string) int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/password", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if got := post(`{"current":"test-passphrase","next":"short"}`); got != http.StatusBadRequest {
		t.Fatalf("short passphrase accepted: %d", got)
	}
	if got := post(`{"current":"wrong-one","next":"long-enough-pw"}`); got != http.StatusForbidden {
		t.Fatalf("wrong current passphrase accepted: %d", got)
	}
	if got := post(`{"current":"test-passphrase","next":"long-enough-pw"}`); got != http.StatusOK {
		t.Fatalf("password change failed: %d", got)
	}
	var hash string
	st.View(func(s *State) { hash = s.Users["admin"].Hash })
	if !checkPassword(hash, "long-enough-pw") {
		t.Fatal("new passphrase does not verify")
	}
}

// helpers

func login(t *testing.T, base string) *http.Cookie {
	t.Helper()
	res, err := http.Post(base+"/api/login", "application/json",
		strings.NewReader(`{"user":"admin","pass":"test-passphrase"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if len(res.Cookies()) != 1 {
		t.Fatalf("login failed: %d", res.StatusCode)
	}
	return res.Cookies()[0]
}

func TestPortHint(t *testing.T) {
	cases := []struct {
		declared string
		port     int
		want     bool
	}{
		{"3000/tcp", 80, true},    // nginx-style mismatch
		{"3000/tcp", 3000, false}, // agrees
		{"80/tcp 8080/tcp", 8080, false},
		{"", 80, false}, // unknown: say nothing
		{"  ", 80, false},
	}
	for _, c := range cases {
		got := portHint(c.declared, c.port)
		if (got != "") != c.want {
			t.Fatalf("portHint(%q, %d) = %q, want hint=%v", c.declared, c.port, got, c.want)
		}
	}
}

func TestFailedFirstDeployShowsAsFailed(t *testing.T) {
	st := &State{Apps: map[string]*App{"api": {Name: "api"}}, Deploys: map[string]*Deploy{
		"20260101-000000-aaaa": {ID: "20260101-000000-aaaa", App: "api", Status: statusReady},
		"20260102-000000-bbbb": {ID: "20260102-000000-bbbb", App: "api", Status: statusFailed, Error: "boom"},
		"20260103-000000-cccc": {ID: "20260103-000000-cccc", App: "other", Status: statusReady},
	}}
	if got := newAppView(st, st.Apps["api"]).Status; got != statusFailed {
		t.Fatalf("status = %q, want %q", got, statusFailed)
	}
	if got := newAppView(st, st.Apps["api"]).DeployID; got != "20260102-000000-bbbb" {
		t.Fatalf("deploy = %q, want newest deploy of that app", got)
	}
}

func TestCaddyfileGoesWhereCaddyReadsIt(t *testing.T) {
	// A package install reloads /etc/caddy/Caddyfile on boot. Writing the routes
	// anywhere else means every domain dies at the first reboot.
	sys := t.TempDir()
	old := caddySysDir
	caddySysDir = sys
	defer func() { caddySysDir = old }()

	d := &Deployer{dataDir: t.TempDir()}
	want := filepath.Join(sys, "Caddyfile")
	if got := d.caddyfilePath(); got != want {
		t.Fatalf("caddyfilePath = %q, want %q", got, want)
	}

	// No /etc/caddy (hand-run setup): fall back to the data dir.
	caddySysDir = filepath.Join(sys, "missing")
	if got := d.caddyfilePath(); got != filepath.Join(d.dataDir, "Caddyfile") {
		t.Fatalf("fallback caddyfilePath = %q", got)
	}
}

func TestImportLineIsAddedOnceAndKeepsExistingConfig(t *testing.T) {
	sys := t.TempDir()
	old := caddySysDir
	caddySysDir = sys
	defer func() { caddySysDir = old }()

	main := filepath.Join(sys, "Caddyfile")
	existing := "# my own sites\nwiki.kantor.local {\n\treverse_proxy 127.0.0.1:9000\n}\n"
	if err := os.WriteFile(main, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &Deployer{dataDir: t.TempDir()}
	routes := d.routesPath()

	// twice, as every deploy calls writeCaddy
	for i := 0; i < 2; i++ {
		if err := d.importRoutes(main, routes); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(main)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), existing) {
		t.Fatalf("existing config was lost:\n%s", got)
	}
	if n := strings.Count(string(got), "import "+strings.TrimSuffix(routes, ".caddy")+"*.caddy"); n != 1 {
		t.Fatalf("import appears %d times, want 1:\n%s", n, got)
	}
}

func TestCloneHint(t *testing.T) {
	cases := map[string]string{
		"fatal: repository 'https://github.com/x/y/' not found":                              "private or the URL is wrong",
		"fatal: could not read Username for 'https://github.com': terminal prompts disabled": "private or the URL is wrong",
		"fatal: Remote branch nope not found in upstream origin":                             "branch or tag does not exist",
		"fatal: unable to access: server certificate verification failed. CAfile: none":      "no CA bundle",
		"fatal: Could not resolve host: github.com":                                          "DNS failed",
	}
	for msg, want := range cases {
		if got := cloneHint(msg); !strings.Contains(got, want) {
			t.Errorf("cloneHint(%q) = %q, want it to mention %q", msg, got, want)
		}
	}
	if got := cloneHint("something else entirely"); got != "" {
		t.Errorf("unexpected hint for unknown error: %q", got)
	}
}

func TestStreamCmdKeepsTheReason(t *testing.T) {
	var logs []string
	err := streamCmd(context.Background(), func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
		"sh", "-c", "echo 'fatal: repository not found'; exit 128")
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "repository not found") || !strings.Contains(msg, "exit status 128") {
		t.Fatalf("error lost the reason: %q", msg)
	}
	if len(logs) == 0 || !strings.Contains(logs[0], "repository not found") {
		t.Fatalf("output was not streamed: %v", logs)
	}
	if h := cloneHint(msg); !strings.Contains(h, "private or the URL is wrong") {
		t.Fatalf("hint missing, got %q", h)
	}
}

func TestSolePort(t *testing.T) {
	cases := []struct {
		declared string
		want     int
		ok       bool
	}{
		{"8000/tcp ", 8000, true},
		{"8080/tcp 9000/tcp ", 0, false}, // two ports: nothing safe to pick
		{"", 0, false},
		{"53/udp ", 0, false}, // tcp only, a udp port says nothing about http
	}
	for _, c := range cases {
		if got, ok := solePort(c.declared); got != c.want || ok != c.ok {
			t.Errorf("solePort(%q) = %d,%v want %d,%v", c.declared, got, ok, c.want, c.ok)
		}
	}
}

func TestParseListeners(t *testing.T) {
	// Taken from a real container: docker's DNS on 127.0.0.11 shares the same
	// address prefix as the app on 127.0.0.1 and must not count as a listener.
	const tcp = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0
   1: 0B00007F:1234 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2 1 0000000000000000 100 0 0 10 0
   2: 0100007F:1770 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 3 1 0000000000000000 100 0 0 10 0
   3: 00000000:1F90 00000000:0000 01 00000000:00000000 00:00000000 00000000     0        0 4 1 0000000000000000 100 0 0 10 0
`
	got := parseListeners(tcp)
	if len(got) != 2 {
		t.Fatalf("want the app's two listeners, got %+v", got)
	}
	if got[0].port != 3000 || got[0].loopback {
		t.Errorf("first listener = %+v, want 3000 on 0.0.0.0", got[0])
	}
	if got[1].port != 6000 || !got[1].loopback {
		t.Errorf("second listener = %+v, want 6000 on 127.0.0.1", got[1])
	}
}

func TestHealthWaitStopsWhenTheContainerDies(t *testing.T) {
	// A closed port is what a dead container looks like from the outside.
	dead := func() bool { return false }
	start := time.Now()
	err := waitHealthy(context.Background(), "http://127.0.0.1:1/healthz", 90*time.Second, func(string, ...any) {}, dead)
	if err == nil || !strings.Contains(err.Error(), "container exited") {
		t.Fatalf("want an exited-container error, got %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("waited %s before giving up on a dead container", d)
	}
	if err := waitTCP(context.Background(), 1, 90*time.Second, func(string, ...any) {}, dead); err == nil ||
		!strings.Contains(err.Error(), "container exited") {
		t.Fatalf("waitTCP want an exited-container error, got %v", err)
	}
	// A live container that just is not listening yet still waits for the timeout.
	if err := waitTCP(context.Background(), 1, 2*time.Second, func(string, ...any) {}, func() bool { return true }); err == nil {
		t.Fatal("want a timeout error")
	}
}

func TestImagesToPruneKeepsWhatRollbackCanReach(t *testing.T) {
	st := &State{Deploys: map[string]*Deploy{
		"a": {ID: "a", App: "api", Status: statusReady, Image: "shoaldeploy/api:old"},
		"b": {ID: "b", App: "api", Status: statusFailed, Image: "shoaldeploy/api:broken"},
		"c": {ID: "c", App: "other", Status: statusReady, Image: "shoaldeploy/other:x"},
	}}
	// newest first, as docker images returns them
	images := []string{
		"shoaldeploy/api:newest",
		"shoaldeploy/api:broken",
		"shoaldeploy/api:old",
		"shoaldeploy/api:ancient",
	}
	got := imagesToPrune(images, st, "api", 3)
	want := []string{"shoaldeploy/api:ancient"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("prune = %v, want %v", got, want)
	}
	// A ready deploy's image survives even when it is the oldest on disk.
	if got := imagesToPrune(images, st, "api", 1); len(got) != 2 || got[0] == "shoaldeploy/api:old" {
		t.Fatalf("prune with n=1 = %v, want broken and ancient, never the rollback target", got)
	}
}
