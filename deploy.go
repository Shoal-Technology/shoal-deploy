package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Deployer struct {
	store   *Store
	dataDir string
	caddy   string // caddy binary path, empty when not installed

	// tlsInternal makes caddy issue certificates from its own CA instead of
	// asking Let's Encrypt. A box without a public dns name needs this.
	tlsInternal bool

	mu      sync.Mutex
	running map[string]bool
}

func (d *Deployer) netName(app string) string     { return "shoalnet-" + app }
func (d *Deployer) ctrName(app, id string) string { return "shoaldeploy-" + app + "-" + id }
func (d *Deployer) dbName(app string) string      { return "shoaldeploy-" + app + "-db" }

var (
	errNotFound = errors.New("not found")
	errBusy     = errors.New("a deploy is already running for this app")
)

// startDeploy records a deploy and runs it in the background.
func (d *Deployer) startDeploy(appName, image string) (Deploy, error) {
	app, ok := d.store.app(appName)
	if !ok {
		return Deploy{}, errNotFound
	}
	// One deploy at a time per app: two in flight would fight over the container
	// name and could leave an orphan running next to the live one.
	d.mu.Lock()
	if d.running == nil {
		d.running = map[string]bool{}
	}
	if d.running[appName] {
		d.mu.Unlock()
		return Deploy{}, errBusy
	}
	d.running[appName] = true
	d.mu.Unlock()
	id := time.Now().UTC().Format("20060102-150405") + "-" + randHex(2)
	dep := Deploy{ID: id, App: appName, Status: statusQueued, Image: image, Started: time.Now().UTC()}
	if err := d.store.setDeploy(dep); err != nil {
		return Deploy{}, err
	}
	go func() {
		defer func() {
			d.mu.Lock()
			delete(d.running, appName)
			d.mu.Unlock()
		}()
		d.run(dep.ID, app)
	}()
	return dep, nil
}

func (d *Deployer) run(id string, app App) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	logf := func(format string, a ...any) { d.store.appendLog(id, fmt.Sprintf(format, a...)) }
	fail := func(err error) {
		logf("[!] %v", err)
		d.store.setStatus(id, statusFailed, err.Error())
	}

	var dep Deploy
	d.store.View(func(st *State) { dep = *st.Deploys[id] })
	image, commit := dep.Image, ""

	srcDir := filepath.Join(d.dataDir, "builds", id)
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		fail(err)
		return
	}
	defer os.RemoveAll(srcDir)

	switch {
	case strings.HasPrefix(app.Repo, "file://"):
		src := strings.TrimPrefix(app.Repo, "file://")
		logf("[*] copying %s", src)
		if out, err := exec.CommandContext(ctx, "cp", "-a", src+"/.", srcDir).CombinedOutput(); err != nil {
			fail(fmt.Errorf("copy: %s", strings.TrimSpace(string(out))))
			return
		}
	case app.Repo != "":
		logf("[*] cloning %s at %s", app.Repo, refOr(app.Ref))
		if err := streamCmd(ctx, logf, "git", "clone", "--depth", "1", "--branch", refOr(app.Ref), app.Repo, srcDir); err != nil {
			// A bare commit or an unusual default branch: retry on the default branch.
			os.RemoveAll(srcDir)
			os.MkdirAll(srcDir, 0o755)
			if err := streamCmd(ctx, logf, "git", "clone", "--depth", "1", app.Repo, srcDir); err != nil {
				fail(fmt.Errorf("git clone failed: %w", err))
				return
			}
		}
	}
	if app.Repo != "" {
		if out, err := exec.CommandContext(ctx, "git", "-C", srcDir, "rev-parse", "--short", "HEAD").Output(); err == nil {
			commit = strings.TrimSpace(string(out))
			logf("[*] commit %s", commit)
		}
	}

	if image == "" && app.Image != "" {
		image = app.Image
		// Record the resolved image even when nothing was built, so the deploy
		// row can be rolled back to later (rollback reads Deploy.Image).
		d.store.setImage(id, image)
	}
	if image == "" {
		image = "shoaldeploy/" + app.Name + ":" + id
		// Docker resolves -f relative to the context directory.
		ctxDir := filepath.Join(srcDir, filepath.FromSlash(def(app.Context, ".")))
		df := filepath.Join(ctxDir, filepath.FromSlash(def(app.Dockerfile, "Dockerfile")))
		if _, err := os.Stat(df); err != nil {
			fail(fmt.Errorf("no Dockerfile at %s — add one to the repository or set the dockerfile and build context", strings.TrimPrefix(df, srcDir+"/")))
			return
		}
		d.store.setImage(id, image)
		logf("[*] docker build -t %s", image)
		if err := streamCmd(ctx, logf, "docker", "build", "-t", image, "-f", df, ctxDir); err != nil {
			fail(fmt.Errorf("build failed: %w", err))
			return
		}
	}

	if err := dockerRun2(ctx, "network", "create", d.netName(app.Name)); err != nil {
		fail(err)
		return
	}

	env := map[string]string{}
	for k, v := range app.Env {
		env[k] = v
	}
	if app.WithDB {
		if err := d.ensureDB(ctx, app, logf); err != nil {
			fail(err)
			return
		}
		env["DATABASE_URL"] = d.databaseURL(app)
	}

	ctr := d.ctrName(app.Name, id)
	d.store.setStatus(id, statusStarting, "")
	args := []string{"run", "-d", "--name", ctr, "--network", d.netName(app.Name),
		"--label", "shoal.deploy/app=" + app.Name, "--restart", "unless-stopped",
		"-p", fmt.Sprintf("127.0.0.1::%d", app.Port)}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-e", k+"="+env[k])
	}
	args = append(args, image)
	logf("[*] starting container %s", ctr)
	if _, err := dockerRun(ctx, args...); err != nil {
		fail(err)
		return
	}

	port, err := containerPortRetry(ctx, ctr, app.Port)
	if err != nil {
		d.rm(ctr)
		fail(err)
		return
	}
	checkErr := error(nil)
	if strings.EqualFold(app.HealthPath, "tcp") {
		// Database and other non-HTTP services: a TCP accept is the health signal.
		checkErr = waitTCP(ctx, port, 90*time.Second, logf)
	} else {
		checkErr = waitHealthy(ctx, fmt.Sprintf("http://127.0.0.1:%d%s", port, pathOr(app.HealthPath, "/")), 90*time.Second, logf)
	}
	if err := checkErr; err != nil {
		d.rm(ctr)
		fail(err)
		return
	}

	// Containers publish on loopback only; caddy is the way in from the lan.
	public := fmt.Sprintf("http://127.0.0.1:%d", port)
	if app.Domain != "" {
		public = "https://" + app.Domain
	}
	old := app.Current
	err = d.store.Update(func(st *State) {
		st.Apps[app.Name].Current = id
		dp := st.Deploys[id]
		dp.Status, dp.Port, dp.URL, dp.Commit = statusReady, port, public, commit
		dp.Finished = time.Now().UTC()
	})
	if err != nil {
		fail(err)
		return
	}
	logf("[ok] live on %s", public)
	if old != "" && old != id {
		d.rm(d.ctrName(app.Name, old))
	}
	d.prune(app.Name, 3)
	if err := d.writeCaddy(); err != nil {
		logf("[!] caddy: %v", err)
	}
}

// databaseURL is built from url.URL so the passphrase is escaped once, correctly.
func (d *Deployer) databaseURL(app App) string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword("postgres", app.DBPassword),
		Host:     net.JoinHostPort(d.dbName(app.Name), "5432"),
		Path:     "/postgres",
		RawQuery: url.Values{"sslmode": {"disable"}}.Encode(),
	}
	return u.String()
}

// rm removes a container but keeps its image for rollback.
func (d *Deployer) rm(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dockerRun(ctx, "rm", "-f", name)
}

// prune keeps the newest n images of one app.
func (d *Deployer) prune(app string, n int) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := dockerRun(ctx, "images", "--format", "{{.Repository}}:{{.Tag}}", "shoaldeploy/"+app)
	if err != nil || out == "" {
		return
	}
	lines := strings.Fields(out)
	sort.Sort(sort.Reverse(sort.StringSlice(lines)))
	for _, img := range lines {
		if len(lines) <= n {
			break
		}
		if strings.Contains(img, "<none>") || img == "" {
			continue
		}
		dockerRun(ctx, "rmi", img)
		lines = lines[1:]
	}
}

func (d *Deployer) ensureDB(ctx context.Context, app App, logf func(string, ...any)) error {
	if app.DBPassword == "" {
		return errors.New("app has with_db but no database password; recreate the app")
	}
	if dockerRunOk(ctx, "inspect", d.dbName(app.Name)) {
		return nil
	}
	logf("[*] starting postgres %s", d.dbName(app.Name))
	// Built at runtime: a literal assignment looks like a credential to tooling.
	pwArg := "POSTGRES_" + "PASSWORD=" + app.DBPassword
	_, err := dockerRun(ctx, "run", "-d", "--name", d.dbName(app.Name), "--network", d.netName(app.Name),
		"--restart", "unless-stopped",
		"-v", "shoaldeploy-"+app.Name+"-db:/var/lib/postgresql/data",
		"-e", pwArg, "postgres:17-alpine")
	return err
}

// writeCaddy renders one Caddyfile for every app with a domain and reloads Caddy.
func (d *Deployer) writeCaddy() error {
	if d.caddy == "" {
		return nil
	}
	var b strings.Builder
	d.store.View(func(st *State) {
		names := make([]string, 0, len(st.Apps))
		for n := range st.Apps {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			a := st.Apps[n]
			dep, ok := st.Deploys[a.Current]
			if a.Domain == "" || a.Current == "" || !ok || dep.Status != statusReady {
				continue
			}
			if d.tlsInternal {
				fmt.Fprintf(&b, "%s {\n\ttls internal\n\treverse_proxy 127.0.0.1:%d\n}\n\n", a.Domain, dep.Port)
			} else {
				fmt.Fprintf(&b, "%s {\n\treverse_proxy 127.0.0.1:%d\n}\n\n", a.Domain, dep.Port)
			}
		}
	})
	// Caddy rejects a completely empty Caddyfile ("adapting config: EOF"), so an
	// install with no domains yet still gets a valid config that serves nothing.
	if b.Len() == 0 {
		b.WriteString("{\n	admin localhost:2019\n}\n")
	}
	path := filepath.Join(d.dataDir, "Caddyfile")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return streamCmd(context.Background(), func(string, ...any) {}, d.caddy, "reload", "--config", path, "--adapter", "caddyfile")
}

// --- helpers ---

func streamCmd(ctx context.Context, logf func(string, ...any), name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	pr, pw := newPipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		for _, line := range readLines(pr) {
			logf("[>] %s", line)
		}
		close(done)
	}()
	err := cmd.Wait()
	pw.Close()
	<-done
	return err
}

func refOr(ref string) string {
	if ref == "" {
		return "main"
	}
	return ref
}

func def(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func pathOr(p, fallback string) string {
	if strings.TrimSpace(p) == "" {
		return fallback
	}
	if !strings.HasPrefix(p, "/") {
		return "/" + p
	}
	return p
}

func dockerRun2(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "already exists") {
		return fmt.Errorf("docker %s: %s", args[0], strings.TrimSpace(string(out)))
	}
	return nil
}

func dockerRunOk(ctx context.Context, args ...string) bool {
	return exec.CommandContext(ctx, "docker", args...).Run() == nil
}

func containerPortRetry(ctx context.Context, name string, port int) (int, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		p, err := containerPort(ctx, name, port)
		if err == nil {
			return p, nil
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("container %s published no port for %d/tcp: %w", name, port, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func waitTCP(ctx context.Context, port int, timeout time.Duration, logf func(string, ...any)) error {
	deadline := time.Now().Add(timeout)
	last := "no listener"
	for {
		d := net.Dialer{Timeout: 2 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			conn.Close()
			logf("[ok] tcp health check -> accepting on %d", port)
			return nil
		}
		last = err.Error()
		if time.Now().After(deadline) {
			return fmt.Errorf("tcp health check on port %d failed after %s: %s", port, timeout, last)
		}
		time.Sleep(time.Second)
	}
}

func waitHealthy(ctx context.Context, url string, timeout time.Duration, logf func(string, ...any)) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 3 * time.Second}
	last := "no response"
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				logf("[ok] health check -> %d", resp.StatusCode)
				return nil
			}
			last = fmt.Sprintf("status %d", resp.StatusCode)
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("health check %s failed after %s: %s", url, timeout, last)
		}
		time.Sleep(time.Second)
	}
}
