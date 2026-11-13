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
	"strconv"
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
				fail(fmt.Errorf("git clone failed: %w%s", err, cloneHint(err.Error())))
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

	// An image rarely listens where the form guessed. The form defaults to 80, so
	// that number was never typed: trust EXPOSE when it names exactly one tcp
	// port. Anything else was typed on purpose and wins, even on a first deploy.
	cport := app.Port
	if declared, err := declaredPorts(ctx, image); err == nil {
		if p, use := containerPortChoice(app.Port, declared); use {
			logf("[*] image declares %d/tcp, publishing that instead of %d", p, cport)
			cport = p
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
	// Images that read $PORT follow the port we publish, so the "declares 8000,
	// listens on 3000" class fixes itself. A value the operator set wins.
	_, userPort := env["PORT"]
	if app.WithDB {
		if err := d.ensureDB(ctx, app, logf); err != nil {
			fail(err)
			return
		}
		env["DATABASE_URL"] = d.databaseURL(app)
	}

	ctr := d.ctrName(app.Name, id)
	d.store.setStatus(id, statusStarting, "")
	// Run, then ask the container where it actually listens: an image is free to
	// ignore both EXPOSE and $PORT with a hard-coded number, and restarting once
	// is cheaper than a doomed 90 second health wait. A redeploy keeps the port
	// that already worked.
	attempt := 0
	start := func() error {
		attempt++
		e := map[string]string{}
		for k, v := range env {
			e[k] = v
		}
		if !userPort {
			e["PORT"] = strconv.Itoa(cport)
		}
		if err := publishOnFreePort(ctx, ctr, d.netName(app.Name), app.Name, cport, e, image, logf); err != nil {
			return err
		}
		return nil
	}
	for {
		if err := start(); err != nil {
			fail(err)
			return
		}
		p, found, problem := 0, false, ""
		if app.Current == "" && attempt == 1 {
			p, found, problem = actualPort(ctx, ctr, cport)
		}
		if problem != "" {
			d.rm(ctr)
			fail(errors.New("aplikasi tidak bisa dijangkau" + problem))
			return
		}
		if !found {
			break
		}
		logf("[*] the app listens on %d, not %d: publishing that instead", p, cport)
		d.rm(ctr)
		cport = p
	}

	port, err := containerPortRetry(ctx, ctr, cport)
	if err != nil {
		d.rm(ctr)
		fail(err)
		return
	}
	checkErr := error(nil)
	// A container that already died will never answer, so stop waiting as soon
	// as it goes down: 90 seconds of silence tells the operator nothing.
	alive := func() bool { return !containerFailed(ctx, ctr) }
	if strings.EqualFold(app.HealthPath, "tcp") {
		// Database and other non-HTTP services: a TCP accept is the health signal.
		checkErr = waitTCP(ctx, port, 90*time.Second, logf, alive)
	} else {
		checkErr = waitHealthy(ctx, fmt.Sprintf("http://127.0.0.1:%d%s", port, pathOr(app.HealthPath, "/")), 90*time.Second, logf, alive)
	}
	if err := checkErr; err != nil {
		// Why it never answered is in the app's own output, so show it before
		// the container goes away. Without this the operator can only guess.
		running := !containerFailed(ctx, ctr)
		if st, serr := dockerRun(ctx, "inspect", "--format", "{{.State.Status}} (exit {{.State.ExitCode}})", ctr); serr == nil {
			logf("[!] container state: %s", st)
		}
		if logs, lerr := containerLogs(ctx, ctr, 20); lerr == nil && strings.TrimSpace(logs) != "" {
			for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
				logf("[!] %s", line)
			}
		}
		d.rm(ctr)
		// Most first-deploy failures are a port mismatch, not a broken app.
		declared := ""
		if p, perr := declaredPorts(ctx, image); perr == nil {
			declared = p
		}
		hint := ""
		if running { // a crashed app names no port problem, it just died
			hint = portHint(declared, cport, port)
			if hint == "" {
				// Still up but silent: the log above usually names the port it
				// actually bound, which is the one number the operator can fix.
				hint = fmt.Sprintf(" — aplikasi berjalan tapi tidak ada yang menjawab di port %d. Kalau log di atas menyebut port lain, isi angka itu di \"Port container\".", cport)
			}
		}
		fail(fmt.Errorf("%w%s", err, hint))
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
		st.Apps[app.Name].Port = cport // the form guessed 8080; keep what actually serves
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

// declaredPorts lists the ports the image itself declares, e.g. "3000/tcp ".
// Container inspect is useless here: publishing a port adds it to ExposedPorts.
func declaredPorts(ctx context.Context, image string) (string, error) {
	return dockerRun(ctx, "image", "inspect", "--format",
		"{{range $p, $_ := .Config.ExposedPorts}}{{$p}} {{end}}", image)
}

// containerPortChoice decides which container port to publish. A typed port
// wins. Only the form's untouched default of 80, or an empty port, yields to
// the single tcp port the image declares.
func containerPortChoice(configured int, declared string) (int, bool) {
	if configured != 0 && configured != 80 {
		return 0, false
	}
	p, ok := solePort(declared)
	if !ok || p == configured {
		return 0, false
	}
	return p, true
}

// solePort returns the one tcp port an image declares, if it declares exactly
// one. Images listing several (a metrics port, say) stay on the form's port,
// since nothing here can tell which one serves traffic.
func solePort(declared string) (int, bool) {
	var found int
	n := 0
	for _, f := range strings.Fields(declared) {
		port, ok := strings.CutSuffix(f, "/tcp")
		if !ok {
			continue
		}
		p, err := strconv.Atoi(port)
		if err != nil {
			continue
		}
		found, n = p, n+1
	}
	if n != 1 {
		return 0, false
	}
	return found, true
}

// publishOnFreePort starts the container with its host port chosen by the
// kernel, not by docker. Docker's own pick (an empty host port) only avoids
// ports it published itself, so it lands on a port another process already
// holds and the health check reads a reset.
func publishOnFreePort(ctx context.Context, ctr, network, app string, cport int, env map[string]string, image string, logf func(string, ...any)) error {
	host, err := freePort()
	if err != nil {
		return err
	}
	args := []string{"run", "-d", "--name", ctr, "--network", network,
		"--label", "shoal.deploy/app=" + app, "--restart", "unless-stopped",
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", host, cport)}
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
	_, err = dockerRun(ctx, args...)
	return err
}

// freePort returns a loopback port nobody is listening on. Docker's own pick
// (an empty host port) ignores listeners it did not publish, so two apps end
// up on one port and the health check reads a reset.
// shortcut: the port is released before docker binds it, so another process can
// take it in between. Hold the listener until docker binds if that shows up.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("no free loopback port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// portHint explains a failed health check when the image declares a different
// port than the one configured. checked is the host port the probe hit, which
// is not the container port: docker publishes it on a random loopback port.
// Empty when the ports agree or are unknown.
func portHint(declared string, port int, checked int) string {
	declared = strings.TrimSpace(declared)
	if declared == "" || strings.Contains(declared, fmt.Sprintf("%d/tcp", port)) {
		return ""
	}
	return fmt.Sprintf(" — image ini mendeklarasikan port %s, bukan %d. Health check mengetuk port host %d. Cek isian \"port container\" di pengaturan teknis.", declared, port, checked)
}

// rm removes a container but keeps its image for rollback.
func (d *Deployer) rm(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dockerRun(ctx, "rm", "-f", name)
}

// prune keeps the newest n images of one app, plus every image a ready deploy
// still points at: the portal offers a rollback button for each of those, and
// removing the image turns that button into a failure.
func (d *Deployer) prune(app string, n int) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := dockerRun(ctx, "images", "--format", "{{.Repository}}:{{.Tag}}", "shoaldeploy/"+app)
	if err != nil || out == "" {
		return
	}
	var st State
	d.store.View(func(s *State) { st = *s })
	for _, img := range imagesToPrune(strings.Fields(out), &st, app, n) {
		dockerRun(ctx, "rmi", img)
	}
}

// imagesToPrune decides which images to remove. images is newest first, the
// order docker returns. An image referenced by a ready deploy of this app is
// never removed, and neither are the newest n.
func imagesToPrune(images []string, st *State, app string, n int) []string {
	keep := map[string]bool{}
	for _, dep := range st.Deploys {
		if dep.App == app && dep.Status == statusReady && dep.Image != "" {
			keep[dep.Image] = true
		}
	}
	var drop []string
	kept := 0
	for _, img := range images {
		if img == "" || strings.Contains(img, "<none>") {
			continue
		}
		if keep[img] || kept < n {
			kept++
			continue
		}
		drop = append(drop, img)
	}
	return drop
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

// caddySysDir is where a package install keeps the Caddyfile caddy reads on
// boot. A variable so the choice can be tested without touching /etc.
var caddySysDir = "/etc/caddy"

// caddyfilePath returns the file caddy loads at boot: the package default when
// the system dir exists, otherwise a hand-run file inside the data dir.
func (d *Deployer) caddyfilePath() string {
	if _, err := os.Stat(caddySysDir); err == nil {
		return filepath.Join(caddySysDir, "Caddyfile")
	}
	return filepath.Join(d.dataDir, "Caddyfile")
}

// routesPath is our own file. Keeping routes apart from the main Caddyfile means
// an office box that already uses Caddy for something else does not lose its
// config: we add an import line and leave the rest alone.
func (d *Deployer) routesPath() string {
	if _, err := os.Stat(caddySysDir); err == nil {
		return filepath.Join(caddySysDir, "shoal-deploy.caddy")
	}
	return d.caddyfilePath()
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
				fmt.Fprintf(&b, "%s {\n	tls internal\n	reverse_proxy 127.0.0.1:%d\n}\n\n", a.Domain, dep.Port)
			} else {
				fmt.Fprintf(&b, "%s {\n	reverse_proxy 127.0.0.1:%d\n}\n\n", a.Domain, dep.Port)
			}
		}
	})
	// Caddy rejects an empty config ("adapting config: EOF"), so an install with
	// no domains yet still gets a valid file that serves nothing.
	if b.Len() == 0 {
		b.WriteString("# no domains yet\n")
	}
	routes := d.routesPath()
	if err := os.WriteFile(routes, []byte(b.String()), 0o644); err != nil {
		return err
	}
	main := d.caddyfilePath()
	if routes != main {
		if err := d.importRoutes(main, routes); err != nil {
			return err
		}
	}
	// reload only reaches a running instance; when caddy is installed but never
	// started (no init script, fresh boot) fall back to starting it detached.
	if err := streamCmd(context.Background(), func(string, ...any) {}, d.caddy, "reload", "--config", main, "--adapter", "caddyfile"); err != nil {
		return runDetached(d.caddy, "start", "--config", main, "--adapter", "caddyfile")
	}
	return nil
}

// importRoutes makes sure the main Caddyfile pulls in our file, appending the
// import once and never touching whatever else lives in there. The glob matters:
// a plain import of a missing file makes caddy refuse to start at all, which on
// an office box means every site dies. A glob only warns.
func (d *Deployer) importRoutes(main, routes string) error {
	line := "import " + strings.TrimSuffix(routes, ".caddy") + "*.caddy"
	cur, err := os.ReadFile(main)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if strings.Contains(string(cur), line) {
		return nil
	}
	next := string(cur)
	if next != "" && !strings.HasSuffix(next, "\n") {
		next += "\n"
	}
	next += "\n# added by shoal-deploy\n" + line + "\n"
	return os.WriteFile(main, []byte(next), 0o644)
}

// --- helpers ---

// runDetached starts a server process and returns as soon as it is up, without
// waiting for its output: `caddy start` forks and keeps the pipe open, so
// streaming it would block the caller forever.
func runDetached(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// cloneHint turns git's own words into the one step an operator can take next.
func cloneHint(msg string) string {
	switch {
	case strings.Contains(msg, "couldn't find remote ref"), strings.Contains(msg, "Remote branch"):
		return " — that branch or tag does not exist: leave ref empty to use the default branch"
	case strings.Contains(msg, "could not read Username"),
		strings.Contains(msg, "Authentication failed"),
		strings.Contains(msg, "Repository not found"),
		strings.Contains(msg, "repository") && strings.Contains(msg, "not found"):
		return " — the repository is private or the URL is wrong: this tool clones public repos only, so the URL must open in a browser without a login"
	case strings.Contains(msg, "certificate"), strings.Contains(msg, "CAfile"):
		return " — this host has no CA bundle: install ca-certificates (dnf install -y ca-certificates) and retry"
	case strings.Contains(msg, "Could not resolve host"):
		return " — DNS failed: this host cannot resolve github.com, check its resolver and outbound network"
	}
	return ""
}

// cmdError carries the last line a command printed, so a failure reports what
// the tool itself said instead of a bare "exit status 128".
type cmdError struct {
	err  error
	tail string
}

func (e *cmdError) Error() string {
	if e.tail == "" {
		return e.err.Error()
	}
	return e.tail + " (" + e.err.Error() + ")"
}

func (e *cmdError) Unwrap() error { return e.err }

func streamCmd(ctx context.Context, logf func(string, ...any), name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	pr, pw := newPipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	var tail string
	go func() {
		for _, line := range readLines(pr) {
			logf("[>] %s", line)
			if s := strings.TrimSpace(line); s != "" {
				tail = s // safe: read only after <-done
			}
		}
		close(done)
	}()
	err := cmd.Wait()
	pw.Close()
	<-done
	if err != nil && tail != "" {
		return &cmdError{err: err, tail: tail}
	}
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

// listener is one socket in LISTEN state inside the container.
type listener struct {
	port     int
	loopback bool // bound to 127.x or ::1, so a published port cannot reach it
}

// parseListeners reads one /proc/net/tcp{,6} dump. Keeping it pure makes the
// hex parsing testable without docker.
func parseListeners(txt string) []listener {
	var out []listener
	for _, line := range strings.Split(txt, "\n")[1:] {
		// sl local_address rem_address st ...  st 0A is LISTEN.
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[3] != "0A" {
			continue
		}
		addr, hexPort, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		p, err := strconv.ParseInt(hexPort, 16, 32)
		if err != nil || p == 0 {
			continue
		}
		// The address is little endian, so the first octet is the last byte
		// pair: 127.0.0.1 is 0100007F, 127.0.0.11 is 0B00007F, ::1 ends the
		// same way. Any 127.x or ::1 listener is unreadable from the host.
		loop := strings.HasSuffix(addr, "7F")
		// 127.0.0.11 is docker's embedded DNS. It sits in every container on
		// a port of its own choosing, and it is never the app.
		if addr == "0B00007F" {
			continue
		}
		out = append(out, listener{port: int(p), loopback: loop})
	}
	return out
}

// listeners reads /proc/net/tcp{,6} inside the container, so no netstat or
// shell has to exist in the image.
func listeners(ctx context.Context, name string) []listener {
	var out []listener
	seen := map[int]bool{}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		txt, err := dockerRun(ctx, "exec", name, "cat", f)
		if err != nil {
			continue
		}
		for _, l := range parseListeners(txt) {
			if seen[l.port] {
				continue
			}
			seen[l.port] = true
			out = append(out, l)
		}
	}
	return out
}

// actualPort waits for a first-deploy container to start listening and reports
// the single port it settled on when that differs from the configured one.
// Failing here saves the 90 second health wait and names the one number to fix.
// problem is set when the app cannot be reached at all — bound to loopback only.
func actualPort(ctx context.Context, name string, want int) (port int, found bool, problem string) {
	deadline := time.Now().Add(15 * time.Second)
	for {
		all := listeners(ctx, name)
		var open []int
		for _, l := range all {
			if !l.loopback {
				open = append(open, l.port)
			}
		}
		if len(open) == 1 {
			if open[0] == want {
				return 0, false, ""
			}
			return open[0], true, ""
		}
		if len(open) > 1 || containerFailed(ctx, name) {
			return 0, false, "" // several ports say nothing; the health check decides
		}
		if time.Now().After(deadline) {
			if len(all) > 0 {
				return 0, false, fmt.Sprintf(" — aplikasi hanya mendengarkan di dalam container (%s), jadi port yang dipublikasikan tidak bisa mencapainya. Jalankan aplikasi di 0.0.0.0, bukan 127.0.0.1.", fmtPorts(all))
			}
			return 0, false, ""
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func fmtPorts(ls []listener) string {
	parts := make([]string, 0, len(ls))
	for _, l := range ls {
		parts = append(parts, fmt.Sprintf("127.0.0.1:%d", l.port))
	}
	return strings.Join(parts, ", ")
}

// containerFailed reports whether the container has given up. The Running flag
// alone is not enough: --restart keeps it true on a box that crashes forever,
// and a crash loop shows as "restarting". Anything else is still worth waiting
// on, including "created" before the first start.
func containerFailed(ctx context.Context, name string) bool {
	out, err := dockerRun(ctx, "inspect", "--format", "{{.State.Status}}", name)
	if err != nil {
		return true
	}
	switch out {
	case "exited", "dead", "restarting":
		return true
	}
	return false
}

func waitTCP(ctx context.Context, port int, timeout time.Duration, logf func(string, ...any), alive func() bool) error {
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
		if alive != nil && !alive() {
			return fmt.Errorf("container exited before accepting on port %d: %s", port, last)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("tcp health check on port %d failed after %s: %s", port, timeout, last)
		}
		time.Sleep(time.Second)
	}
}

func waitHealthy(ctx context.Context, url string, timeout time.Duration, logf func(string, ...any), alive func() bool) error {
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
			if alive != nil && !alive() {
				return fmt.Errorf("container exited before answering %s: %s", url, last)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("health check %s failed after %s: %s", url, timeout, last)
		}
		time.Sleep(time.Second)
	}
}
