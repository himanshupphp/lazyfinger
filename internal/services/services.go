// Package services finds the databases and caches a project expects to talk
// to, checks whether they're running, and can start them in Docker.
package services

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Service struct {
	Kind     string // postgres, mysql, mariadb, redis, mongodb
	Host     string
	Port     int
	User     string
	Password string
	Database string
	From     string // where it was found, e.g. ".env DB_CONNECTION"
	Compose  string // the project's Docker Compose service for it, if any
}

var (
	labels       = map[string]string{"postgres": "PostgreSQL", "mysql": "MySQL", "mariadb": "MariaDB", "redis": "Redis", "mongodb": "MongoDB"}
	defaultPorts = map[string]int{"postgres": 5432, "mysql": 3306, "mariadb": 3306, "redis": 6379, "mongodb": 27017}
	innerPorts   = defaultPorts
	dataDirs     = map[string]string{"postgres": "/var/lib/postgresql/data", "mysql": "/var/lib/mysql", "mariadb": "/var/lib/mysql", "redis": "/data", "mongodb": "/data/db"}
	jdbcRe       = regexp.MustCompile(`jdbc:(postgresql|mysql|mariadb)://([^/:?\s]+)(?::(\d+))?/([\w-]+)`)
	springUserRe = regexp.MustCompile(`(?m)^\s*(?:spring\.datasource\.)?username\s*[:=]\s*(\S+)`)
	springPassRe = regexp.MustCompile(`(?m)^\s*(?:spring\.datasource\.)?password\s*[:=]\s*(\S+)`)
	slugRe       = regexp.MustCompile(`[^a-z0-9]+`)
)

func (s Service) Label() string { return labels[s.Kind] }

func (s Service) dialHost() string {
	if s.Host == "" || strings.EqualFold(s.Host, "localhost") {
		return "127.0.0.1"
	}
	return s.Host
}

func (s Service) Addr() string { return net.JoinHostPort(s.dialHost(), strconv.Itoa(s.Port)) }

func (s Service) Local() bool {
	switch strings.ToLower(s.Host) {
	case "", "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	}
	return false
}

func (s Service) Reachable() bool {
	c, err := net.DialTimeout("tcp", s.Addr(), 1500*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// Detect finds the services a project expects, from .env (or .env.example
// before .env exists), Spring config, and the project's Compose file.
func Detect(root string) []Service {
	var out []Service
	add := func(s Service) {
		if s.Kind == "" {
			return
		}
		if s.Port == 0 {
			s.Port = defaultPorts[s.Kind]
		}
		for _, o := range out {
			if o.Kind == s.Kind && o.Port == s.Port && o.dialHost() == s.dialHost() {
				return
			}
		}
		out = append(out, s)
	}

	env, from := readEnv(filepath.Join(root, ".env")), ".env"
	if env == nil {
		env, from = readEnv(filepath.Join(root, ".env.example")), ".env.example"
	}
	if env != nil {
		kind := map[string]string{"mysql": "mysql", "mariadb": "mariadb", "pgsql": "postgres", "postgres": "postgres", "postgresql": "postgres"}[strings.ToLower(env["DB_CONNECTION"])]
		if kind != "" && env["DB_URL"] == "" && env["DATABASE_URL"] == "" {
			add(Service{Kind: kind, Host: orDefault(env["DB_HOST"], "127.0.0.1"), Port: atoi(env["DB_PORT"]),
				User: env["DB_USERNAME"], Password: nullable(env["DB_PASSWORD"]), Database: env["DB_DATABASE"], From: from + " DB_CONNECTION"})
		}
		for _, k := range []string{"DATABASE_URL", "DB_URL", "MONGODB_URI", "MONGO_URI", "MONGO_URL", "REDIS_URL"} {
			if s, ok := fromURL(env[k]); ok {
				s.From = from + " " + k
				add(s)
			}
		}
		if env["REDIS_URL"] == "" && usesRedis(env) {
			add(Service{Kind: "redis", Host: orDefault(env["REDIS_HOST"], "127.0.0.1"), Port: atoi(env["REDIS_PORT"]),
				Password: nullable(env["REDIS_PASSWORD"]), From: from + " (Redis driver)"})
		}
	}

	for _, f := range []string{"src/main/resources/application.properties", "src/main/resources/application.yml", "src/main/resources/application.yaml"} {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			continue
		}
		text := string(b)
		if m := jdbcRe.FindStringSubmatch(text); m != nil {
			s := Service{Kind: map[string]string{"postgresql": "postgres", "mysql": "mysql", "mariadb": "mariadb"}[m[1]],
				Host: m[2], Port: atoi(m[3]), Database: m[4], From: f}
			if u := springUserRe.FindStringSubmatch(text); u != nil && !strings.Contains(u[1], "${") {
				s.User = u[1]
			}
			if p := springPassRe.FindStringSubmatch(text); p != nil && !strings.Contains(p[1], "${") {
				s.Password = p[1]
			}
			add(s)
		}
	}

	byKind, _ := compose(root)
	for i := range out {
		out[i].Compose = byKind[out[i].Kind]
	}
	return out
}

func usesRedis(env map[string]string) bool {
	for _, k := range []string{"CACHE_STORE", "CACHE_DRIVER", "SESSION_DRIVER", "QUEUE_CONNECTION", "BROADCAST_CONNECTION", "BROADCAST_DRIVER"} {
		if strings.EqualFold(env[k], "redis") {
			return true
		}
	}
	return false
}

func fromURL(raw string) (Service, bool) {
	if raw == "" || strings.Contains(raw, "${") {
		return Service{}, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return Service{}, false
	}
	kind := map[string]string{"postgres": "postgres", "postgresql": "postgres", "postgis": "postgres", "mysql": "mysql",
		"mysql2": "mysql", "mariadb": "mariadb", "redis": "redis", "rediss": "redis", "mongodb": "mongodb"}[strings.ToLower(u.Scheme)]
	if kind == "" {
		return Service{}, false // includes mongodb+srv, which is always remote
	}
	s := Service{Kind: kind, Host: u.Hostname(), Port: atoi(u.Port()), Database: strings.TrimPrefix(u.Path, "/")}
	if u.User != nil {
		s.User = u.User.Username()
		s.Password, _ = u.User.Password()
	}
	return s, true
}

// compose reads the project's Compose file: which service provides each kind
// of database, and the set of all service names.
func compose(root string) (map[string]string, map[string]bool) {
	byKind, names := map[string]string{}, map[string]bool{}
	for _, f := range []string{"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"} {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			continue
		}
		inServices, svcIndent, current := false, -1, ""
		for _, l := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
			t := strings.TrimSpace(l)
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			indent := len(l) - len(strings.TrimLeft(l, " "))
			if indent == 0 {
				inServices, svcIndent, current = t == "services:", -1, ""
				continue
			}
			if !inServices {
				continue
			}
			if svcIndent < 0 {
				svcIndent = indent
			}
			if indent == svcIndent && strings.HasSuffix(t, ":") {
				current = strings.Trim(strings.TrimSuffix(t, ":"), `"'`)
				names[current] = true
				continue
			}
			if current != "" && strings.HasPrefix(t, "image:") {
				if k := kindFromImage(strings.Trim(strings.TrimSpace(t[6:]), `"'`)); k != "" && byKind[k] == "" {
					byKind[k] = current
				}
			}
		}
		break
	}
	return byKind, names
}

// ComposeNames lists the project's Compose service names (e.g. "mysql" for Laravel Sail).
func ComposeNames(root string) map[string]bool {
	_, names := compose(root)
	return names
}

func kindFromImage(img string) string {
	base := strings.ToLower(img)
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	switch {
	case strings.Contains(base, "postgres"), strings.Contains(base, "postgis"), strings.Contains(base, "pgvector"):
		return "postgres"
	case strings.Contains(base, "mariadb"):
		return "mariadb"
	case strings.Contains(base, "mysql"):
		return "mysql"
	case strings.Contains(base, "redis"), strings.Contains(base, "valkey"):
		return "redis"
	case strings.Contains(base, "mongo"):
		return "mongodb"
	}
	return ""
}

// ContainerName is the name LazyFinger gives a service's container for this project.
func (s Service) ContainerName(root string) string {
	slug := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(filepath.Base(root)), "-"), "-")
	if slug == "" {
		slug = "app"
	}
	return "lazy-" + slug + "-" + s.Kind
}

// DockerArgs builds a `docker run` that matches the project's own settings,
// reachable only from this computer, with data kept in a named volume.
func (s Service) DockerArgs(root string) ([]string, error) {
	name := s.ContainerName(root)
	args := []string{"docker", "run", "-d", "--name", name, "--restart", "unless-stopped",
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", s.Port, innerPorts[s.Kind]),
		"-v", name + "-data:" + dataDirs[s.Kind]}
	env := func(k, v string) { args = append(args, "-e", k+"="+v) }

	switch s.Kind {
	case "postgres":
		user := s.User
		if user == "" {
			user = "postgres"
		}
		env("POSTGRES_USER", user)
		if s.Password == "" {
			env("POSTGRES_HOST_AUTH_METHOD", "trust")
		} else {
			env("POSTGRES_PASSWORD", s.Password)
		}
		if s.Database != "" {
			env("POSTGRES_DB", s.Database)
		}
		args = append(args, "postgres:16")
	case "mysql", "mariadb":
		if s.User == "" || s.User == "root" {
			if s.Password == "" {
				env("MYSQL_ALLOW_EMPTY_PASSWORD", "yes")
			} else {
				env("MYSQL_ROOT_PASSWORD", s.Password)
			}
		} else {
			if s.Password == "" {
				return nil, fmt.Errorf("%s can't create the user %q without a password. Set DB_PASSWORD in .env first", s.Label(), s.User)
			}
			env("MYSQL_RANDOM_ROOT_PASSWORD", "yes")
			env("MYSQL_USER", s.User)
			env("MYSQL_PASSWORD", s.Password)
		}
		if s.Database != "" {
			env("MYSQL_DATABASE", s.Database)
		}
		if s.Kind == "mariadb" {
			args = append(args, "mariadb:11")
		} else {
			args = append(args, "mysql:8.4")
		}
	case "redis":
		args = append(args, "redis:7")
		if s.Password != "" {
			args = append(args, "redis-server", "--requirepass", s.Password)
		}
	case "mongodb":
		if s.User != "" && s.Password != "" {
			env("MONGO_INITDB_ROOT_USERNAME", s.User)
			env("MONGO_INITDB_ROOT_PASSWORD", s.Password)
		}
		args = append(args, "mongo:7")
	}
	return args, nil
}

func ContainerExists(name string) bool {
	out, err := exec.Command("docker", "ps", "-a", "--filter", "name=^/"+name+"$", "--format", "{{.Names}}").Output()
	return err == nil && strings.TrimSpace(string(out)) == name
}

// DockerStatus is "ready", "missing", "stopped", or "permission".
func DockerStatus() string {
	if _, err := exec.LookPath("docker"); err != nil {
		return "missing"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "info").CombinedOutput()
	if err == nil {
		return "ready"
	}
	if strings.Contains(strings.ToLower(string(out)), "permission denied") {
		return "permission"
	}
	return "stopped"
}

// StartDockerCommand returns how to start Docker. background means it's a
// desktop app that can be launched without waiting.
func StartDockerCommand() (args []string, background bool) {
	switch runtime.GOOS {
	case "darwin":
		return []string{"open", "-a", "Docker"}, true
	case "windows":
		return []string{`C:\Program Files\Docker\Docker\Docker Desktop.exe`}, true
	}
	return []string{"sudo", "systemctl", "start", "docker"}, false
}

func WaitDocker(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if DockerStatus() == "ready" {
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return false
}

func WaitReachable(s Service, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.Reachable() {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

func readEnv(path string) map[string]string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(t, "export "), "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	return out
}

func nullable(v string) string {
	if strings.EqualFold(v, "null") {
		return ""
	}
	return v
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func atoi(s string) int { n, _ := strconv.Atoi(strings.TrimSpace(s)); return n }
