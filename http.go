package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"time"
)

//go:embed portal
var portalFS embed.FS

type Server struct {
	store *Store
	dep   *Deployer
	host  string
}

type appView struct {
	App
	Status   string `json:"status"`
	URL      string `json:"url"`
	DeployID string `json:"deploy_id"`
}

type deployView struct {
	Deploy
	Live bool `json:"live"`
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("GET /api/me", s.authJSON(s.me))
	mux.HandleFunc("POST /api/password", s.authJSON(s.changePassword))
	mux.HandleFunc("GET /api/overview", s.authJSON(s.overview))
	mux.HandleFunc("GET /api/apps", s.authJSON(s.listApps))
	mux.HandleFunc("POST /api/apps", s.authJSON(s.createApp))
	mux.HandleFunc("GET /api/apps/{name}", s.authJSON(s.getApp))
	mux.HandleFunc("PUT /api/apps/{name}", s.authJSON(s.updateApp))
	mux.HandleFunc("DELETE /api/apps/{name}", s.authJSON(s.deleteApp))
	mux.HandleFunc("POST /api/apps/{name}/deploy", s.authJSON(s.deploy))
	mux.HandleFunc("POST /api/apps/{name}/rollback", s.authJSON(s.rollback))
	mux.HandleFunc("GET /api/apps/{name}/logs", s.authJSON(s.appLogs))
	mux.HandleFunc("GET /api/deploys/{id}", s.authJSON(s.getDeploy))
	mux.HandleFunc("POST /api/webhook/{name}", s.webhook)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	static, _ := fs.Sub(portalFS, "portal")
	mux.Handle("GET /", http.FileServerFS(static))
	return mux
}

// --- auth ---

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		User string `json:"user"`
		Pass string `json:"pass"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	var user *User
	s.store.View(func(st *State) {
		if u, ok := st.Users[body.User]; ok {
			user = u
		}
	})
	if user == nil || !checkPassword(user.Hash, body.Pass) {
		time.Sleep(400 * time.Millisecond)
		writeErr(w, http.StatusUnauthorized, "wrong user or password")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: "sd_session", Value: s.store.signSession(user.Name, time.Now().Add(7*24*time.Hour)),
		Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 7 * 24 * 3600,
	})
	writeJSON(w, http.StatusOK, map[string]any{"user": user.Name})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "sd_session", Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) currentUser(r *http.Request) (string, bool) {
	c, err := r.Cookie("sd_session")
	if err != nil {
		return "", false
	}
	return s.store.validSession(c.Value)
}

func (s *Server) authJSON(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := s.currentUser(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "not signed in")
			return
		}
		next(w, r, user)
	}
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, user string) {
	writeJSON(w, http.StatusOK, map[string]any{"user": user})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, user string) {
	var body struct {
		Current string `json:"current"`
		Next    string `json:"next"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if len(body.Next) < 8 {
		writeErr(w, http.StatusBadRequest, "new passphrase needs at least 8 characters")
		return
	}
	var okCur bool
	var hash string
	s.store.View(func(st *State) { okCur = checkPassword(st.Users[user].Hash, body.Current) })
	if !okCur {
		writeErr(w, http.StatusForbidden, "current passphrase is wrong")
		return
	}
	var err error
	if hash, err = hashPassword(body.Next); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.store.Update(func(st *State) { st.Users[user].Hash = hash })
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- apps ---

// newAppView must be called with the store lock already held.
func newAppView(st *State, a *App) appView {
	v := appView{App: *a, Status: "none"}
	v.DBPassword, v.Token = "", ""
	// A failed first deploy never becomes Current, so fall back to the newest
	// deploy row, or the UI claims the app was never run.
	id := a.Current
	if _, ok := st.Deploys[id]; !ok {
		id = ""
		for did, d := range st.Deploys {
			if d.App == a.Name && did > id {
				id = did
			}
		}
	}
	if d, ok := st.Deploys[id]; ok {
		v.Status, v.URL, v.DeployID = d.Status, d.URL, d.ID
	}
	return v
}

func (s *Server) view(a *App) appView {
	var v appView
	s.store.View(func(st *State) { v = newAppView(st, a) })
	return v
}

func (s *Server) listApps(w http.ResponseWriter, r *http.Request, user string) {
	out := []appView{}
	s.store.View(func(st *State) {
		names := make([]string, 0, len(st.Apps))
		for n := range st.Apps {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			out = append(out, newAppView(st, st.Apps[n]))
		}
	})
	writeJSON(w, http.StatusOK, map[string]any{"apps": out})
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request, user string) {
	apps := []appView{}
	s.store.View(func(st *State) {
		names := make([]string, 0, len(st.Apps))
		for n := range st.Apps {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			apps = append(apps, newAppView(st, st.Apps[n]))
		}
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"user": user, "apps": apps, "host": s.host, "version": version,
		"docker": dockerVersion(r.Context()), "caddy": s.dep.caddy != "",
	})
}

func (s *Server) createApp(w http.ResponseWriter, r *http.Request, user string) {
	var body App
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	body.Name = strings.ToLower(strings.TrimSpace(body.Name))
	if body.Name == "" || strings.ContainsAny(body.Name, " /\\$'\"`;|&") {
		writeErr(w, http.StatusBadRequest, "name must be one word: letters, digits, dash or underscore")
		return
	}
	if body.Port == 0 {
		body.Port = 80
	}
	if body.Env == nil {
		body.Env = map[string]string{}
	}
	body.Token = randHex(16)
	body.Created = time.Now().UTC()
	if body.WithDB {
		body.DBPassword = randHex(16)
	}
	var exists bool
	s.store.View(func(st *State) { _, exists = st.Apps[body.Name] })
	if exists {
		writeErr(w, http.StatusConflict, "an app named "+body.Name+" already exists")
		return
	}
	if err := s.store.Update(func(st *State) { st.Apps[body.Name] = &body }); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, s.view(&body))
}

func (s *Server) getApp(w http.ResponseWriter, r *http.Request, user string) {
	app, ok := s.store.app(r.PathValue("name"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no such app")
		return
	}
	resp := map[string]any{"app": s.view(&app), "deploys": []deployView{}, "logs": "",
		// One app, one signed-in admin: the webhook secret and the database
		// connection string are shown here and nowhere else.
		"webhook":  "/api/webhook/" + app.Name + "?token=" + app.Token,
		"database": ""}
	s.store.View(func(st *State) {
		ds := []deployView{}
		for _, d := range st.Deploys {
			if d.App == app.Name {
				ds = append(ds, deployView{Deploy: *d, Live: d.ID == app.Current})
			}
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i].Started.After(ds[j].Started) })
		resp["deploys"] = ds
	})
	if app.WithDB && app.DBPassword != "" {
		resp["database"] = s.dep.databaseURL(app)
	}
	if app.Current != "" {
		if out, err := containerLogs(r.Context(), s.dep.ctrName(app.Name, app.Current), 200); err == nil {
			resp["logs"] = out
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) updateApp(w http.ResponseWriter, r *http.Request, user string) {
	name := r.PathValue("name")
	if _, ok := s.store.app(name); !ok {
		writeErr(w, http.StatusNotFound, "no such app")
		return
	}
	var body struct {
		Repo       *string            `json:"repo"`
		Ref        *string            `json:"ref"`
		Dockerfile *string            `json:"dockerfile"`
		Image      *string            `json:"image"`
		Context    *string            `json:"context"`
		Domain     *string            `json:"domain"`
		Port       *int               `json:"port"`
		HealthPath *string            `json:"health_path"`
		Env        *map[string]string `json:"env"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	s.store.Update(func(st *State) {
		a := st.Apps[name]
		if body.Repo != nil {
			a.Repo = *body.Repo
		}
		if body.Ref != nil {
			a.Ref = *body.Ref
		}
		if body.Dockerfile != nil {
			a.Dockerfile = *body.Dockerfile
		}
		if body.Image != nil {
			a.Image = strings.TrimSpace(*body.Image)
		}
		if body.Context != nil {
			a.Context = *body.Context
		}
		if body.Domain != nil {
			a.Domain = strings.TrimSpace(*body.Domain)
		}
		if body.Port != nil && *body.Port > 0 {
			a.Port = *body.Port
		}
		if body.HealthPath != nil {
			a.HealthPath = *body.HealthPath
		}
		if body.Env != nil {
			a.Env = *body.Env
		}
	})
	app, _ := s.store.app(name)
	if err := s.dep.writeCaddy(); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"app": s.view(&app), "warning": "caddy: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"app": s.view(&app)})
}

func (s *Server) deleteApp(w http.ResponseWriter, r *http.Request, user string) {
	name := r.PathValue("name")
	if _, ok := s.store.app(name); !ok {
		writeErr(w, http.StatusNotFound, "no such app")
		return
	}
	var ctrs []string
	s.store.View(func(st *State) {
		for id, d := range st.Deploys {
			if d.App == name {
				ctrs = append(ctrs, s.dep.ctrName(name, id))
			}
		}
	})
	for _, c := range ctrs {
		s.dep.rm(c)
	}
	s.dep.rm(s.dep.dbName(name))
	if r.URL.Query().Get("data") == "1" {
		execQuiet(r.Context(), "docker", "volume", "rm", "shoaldeploy-"+name+"-db")
	}
	for _, img := range s.imagesOf(name) {
		execQuiet(r.Context(), "docker", "rmi", img)
	}
	execQuiet(r.Context(), "docker", "network", "rm", s.dep.netName(name))
	s.store.Update(func(st *State) {
		delete(st.Apps, name)
		for id, d := range st.Deploys {
			if d.App == name {
				delete(st.Deploys, id)
			}
		}
	})
	if err := s.dep.writeCaddy(); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warning": "caddy: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) imagesOf(name string) []string {
	out, err := dockerRun(rctx(), "images", "--format", "{{.Repository}}:{{.Tag}}", "shoaldeploy/"+name)
	if err != nil || out == "" {
		return nil
	}
	return strings.Fields(out)
}

func (s *Server) deploy(w http.ResponseWriter, r *http.Request, user string) {
	dep, err := s.dep.startDeploy(r.PathValue("name"), "")
	if errors.Is(err, errNotFound) {
		writeErr(w, http.StatusNotFound, "no such app")
		return
	}
	if errors.Is(err, errBusy) {
		writeErr(w, http.StatusConflict, errBusy.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, dep)
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request, user string) {
	var body struct {
		Deploy string `json:"deploy"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	name := r.PathValue("name")
	var image string
	s.store.View(func(st *State) {
		if d, ok := st.Deploys[body.Deploy]; ok && d.App == name {
			image = d.Image
		}
	})
	if image == "" {
		writeErr(w, http.StatusNotFound, "that deploy has no image to roll back to")
		return
	}
	dep, err := s.dep.startDeploy(name, image)
	if errors.Is(err, errBusy) {
		writeErr(w, http.StatusConflict, errBusy.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, dep)
}

func (s *Server) getDeploy(w http.ResponseWriter, r *http.Request, user string) {
	var dep Deploy
	var ok bool
	s.store.View(func(st *State) {
		if d, found := st.Deploys[r.PathValue("id")]; found {
			dep, ok = *d, true
		}
	})
	if !ok {
		writeErr(w, http.StatusNotFound, "no such deploy")
		return
	}
	writeJSON(w, http.StatusOK, dep)
}

func (s *Server) appLogs(w http.ResponseWriter, r *http.Request, user string) {
	app, ok := s.store.app(r.PathValue("name"))
	if !ok || app.Current == "" {
		writeErr(w, http.StatusNotFound, "no live deploy")
		return
	}
	out, err := containerLogs(r.Context(), s.dep.ctrName(app.Name, app.Current), 200)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": out})
}

// webhook triggers a deploy for CI: POST /api/webhook/<app>?token=<app token>.
func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	app, ok := s.store.app(r.PathValue("name"))
	if !ok || r.URL.Query().Get("token") == "" || r.URL.Query().Get("token") != app.Token {
		writeErr(w, http.StatusUnauthorized, "bad token")
		return
	}
	dep, err := s.dep.startDeploy(app.Name, "")
	if errors.Is(err, errBusy) {
		writeErr(w, http.StatusConflict, errBusy.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, dep)
}

// --- small helpers ---

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func rctx() context.Context { return context.Background() }

func execQuiet(ctx context.Context, name string, args ...string) {
	exec.CommandContext(ctx, name, args...).Run()
}
