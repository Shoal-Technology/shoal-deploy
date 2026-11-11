package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Statuses of a deploy.
const (
	statusQueued   = "queued"
	statusBuilding = "building"
	statusStarting = "starting"
	statusReady    = "ready"
	statusFailed   = "failed"
)

const maxDeployLog = 256 << 10

type User struct {
	Name string `json:"name"`
	Hash string `json:"hash"`
}

// App is one deployable unit: a git repository plus the container it produces.
type App struct {
	Name       string            `json:"name"`
	Repo       string            `json:"repo"`
	Image      string            `json:"image"`
	Ref        string            `json:"ref"`
	Dockerfile string            `json:"dockerfile"`
	Context    string            `json:"context"`
	Domain     string            `json:"domain"`
	Port       int               `json:"port"`
	HealthPath string            `json:"health_path"`
	Env        map[string]string `json:"env"`
	WithDB     bool              `json:"with_db"`
	DBPassword string            `json:"db_password"`
	Token      string            `json:"token"`
	Current    string            `json:"current"`
	Created    time.Time         `json:"created"`
}

type Deploy struct {
	ID       string    `json:"id"`
	App      string    `json:"app"`
	Status   string    `json:"status"`
	Image    string    `json:"image"`
	Commit   string    `json:"commit"`
	Port     int       `json:"port"`
	URL      string    `json:"url"`
	Error    string    `json:"error"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	Log      string    `json:"log"`
}

type State struct {
	Users      map[string]*User   `json:"users"`
	Apps       map[string]*App    `json:"apps"`
	Deploys    map[string]*Deploy `json:"deploys"`
	SessionKey string             `json:"session_key"`
}

// Store keeps all state in one JSON file: a control plane for a single box does
// not need a database.
type Store struct {
	mu   sync.Mutex
	path string
	st   State
}

func openStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "state.json")}
	s.st = State{
		Users:   map[string]*User{},
		Apps:    map[string]*App{},
		Deploys: map[string]*Deploy{},
	}
	raw, err := os.ReadFile(s.path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &s.st); err != nil {
			return nil, fmt.Errorf("state file %s: %w", s.path, err)
		}
		if s.st.Users == nil {
			s.st.Users = map[string]*User{}
		}
		if s.st.Apps == nil {
			s.st.Apps = map[string]*App{}
		}
		if s.st.Deploys == nil {
			s.st.Deploys = map[string]*Deploy{}
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, err
	}
	if s.st.SessionKey == "" {
		s.st.SessionKey = randHex(32)
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	// Keep the newest deploy logs bounded so the state file stays small.
	for id, d := range s.st.Deploys {
		if len(d.Log) > maxDeployLog {
			d.Log = d.Log[len(d.Log)-maxDeployLog:]
		}
		s.st.Deploys[id] = d
	}
	raw, err := json.MarshalIndent(&s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Update runs fn with the lock held and saves afterwards.
func (s *Store) Update(fn func(st *State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.st)
	return s.saveLocked()
}

// View runs fn with the lock held, without saving.
func (s *Store) View(fn func(st *State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.st)
}

func (s *Store) app(name string) (App, bool) {
	var out App
	var ok bool
	s.View(func(st *State) {
		if a, found := st.Apps[name]; found {
			out, ok = *a, true
		}
	})
	return out, ok
}

func (s *Store) setDeploy(d Deploy) error {
	return s.Update(func(st *State) { st.Deploys[d.ID] = &d })
}

func (s *Store) appendLog(id, line string) {
	s.Update(func(st *State) {
		if d, ok := st.Deploys[id]; ok {
			d.Log += line + "\n"
		}
	})
}

func (s *Store) setImage(id, image string) {
	s.Update(func(st *State) {
		if d, ok := st.Deploys[id]; ok {
			d.Image = image
		}
	})
}

func (s *Store) setStatus(id, status, errMsg string) {
	s.Update(func(st *State) {
		if d, ok := st.Deploys[id]; ok {
			d.Status = status
			if errMsg != "" {
				d.Error = errMsg
			}
			if status == statusReady || status == statusFailed {
				d.Finished = time.Now()
			}
		}
	})
}

// --- users and sessions ---

func hashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}

func checkPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

func (s *Store) signSession(user string, exp time.Time) string {
	payload := user + "|" + fmt.Sprint(exp.Unix())
	mac := hmac.New(sha256.New, []byte(s.st.SessionKey))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Store) validSession(v string) (string, bool) {
	parts := strings.Split(v, ".")
	if len(parts) != 2 {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, []byte(s.st.SessionKey))
	mac.Write(raw)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return "", false
	}
	payload := string(raw)
	i := strings.LastIndex(payload, "|")
	if i < 0 {
		return "", false
	}
	var exp int64
	if _, err := fmt.Sscan(payload[i+1:], &exp); err != nil {
		return "", false
	}
	if time.Now().Unix() > exp {
		return "", false
	}
	return payload[:i], true
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
