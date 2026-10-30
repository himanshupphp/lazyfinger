package heal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/himanshupphp/lazyfinger/internal/config"
	"github.com/himanshupphp/lazyfinger/internal/patch"
	"github.com/himanshupphp/lazyfinger/internal/registry"
)

type risk int

const (
	low    risk = iota // declared dependencies, verified packages, safe maintenance
	medium             // editing source files
	always             // needs a human even in autofix
)

type note struct {
	ok   bool
	text string
}

func needsApproval(mode config.Mode, r risk) bool {
	return r == always || mode == config.Analyze
}

func editRisk(changes []patch.Change) risk {
	if len(changes) > 0 {
		return medium
	}
	return low
}

func assessCommands(ctx context.Context, root string, cmds []string) (risk, []note) {
	r := low
	var notes []note
	for _, c := range cmds {
		cr, n := assessCommand(ctx, root, c)
		r = max(r, cr)
		notes = append(notes, n...)
	}
	return r, notes
}

func assessCommand(ctx context.Context, root, c string) (risk, []note) {
	c = strings.Join(strings.Fields(c), " ")
	if strings.ContainsAny(c, "&|;<>`$\n") {
		return always, []note{{false, "This command chains or redirects other commands, so it always needs your approval."}}
	}
	if r, n, ok := assessProjectCommand(root, c); ok {
		return r, n
	}
	in, ok := parseInstall(c)
	if !ok {
		return always, nil // not a recognized install or maintenance command
	}
	if in.unsafe != "" {
		return always, []note{{false, in.unsafe}}
	}
	if len(in.packages) == 0 {
		return low, nil // installs only what the project already declares
	}
	if in.ecosystem == "go" {
		return always, []note{{false, "Go modules can't be checked for popularity, so this needs your approval."}}
	}

	findings := make([]registry.Finding, len(in.packages))
	var wg sync.WaitGroup
	for i, p := range in.packages {
		wg.Add(1)
		go func() {
			defer wg.Done()
			findings[i] = registry.Check(ctx, in.ecosystem, p)
		}()
	}
	wg.Wait()

	r := low
	var notes []note
	for _, f := range findings {
		notes = append(notes, note{f.OK(), f.Describe()})
		if !f.OK() {
			r = always
		}
	}
	return r, notes
}

var destructive = []string{
	"migrate:fresh", "migrate:reset", "migrate:refresh", "migrate:rollback", "db:wipe",
	"db:drop", "db:reset", "manage.py flush", "dropdb", "rm -rf", "rm -r ", "drop table", "drop database",
}

// Framework maintenance commands that only clear caches or create links.
var safeMaintenance = map[string]bool{
	"php artisan config:clear": true, "php artisan cache:clear": true, "php artisan route:clear": true,
	"php artisan view:clear": true, "php artisan event:clear": true, "php artisan optimize:clear": true,
	"php artisan storage:link": true, "php artisan package:discover": true,
	"composer dump-autoload": true, "php bin/console cache:clear": true,
}

// assessProjectCommand handles framework commands whose safety depends on the
// project's current state, like creating .env only when it doesn't exist.
func assessProjectCommand(root, c string) (risk, []note, bool) {
	lower := strings.ToLower(c)
	for _, d := range destructive {
		if strings.Contains(lower, d) {
			return always, []note{{false, "This command can delete data. Review it carefully before approving."}}, true
		}
	}
	if safeMaintenance[c] {
		return low, nil, true
	}
	switch c {
	case "cp .env.example .env", "copy .env.example .env":
		if fileExists(filepath.Join(root, ".env")) {
			return always, []note{{false, "This would overwrite your existing .env file."}}, true
		}
		return low, []note{{true, "Creates .env from .env.example. No existing file is touched."}}, true
	case "php artisan key:generate":
		if envHasValue(root, "APP_KEY") {
			return always, []note{{false, "This would replace your existing APP_KEY, which signs everyone out and breaks previously encrypted data."}}, true
		}
		return low, []note{{true, "Sets APP_KEY, which is currently empty."}}, true
	case "php artisan migrate", "bin/rails db:migrate", "python manage.py migrate", "python3 manage.py migrate":
		return always, []note{{false, "This changes your database schema, so it always needs your approval."}}, true
	}
	return 0, nil, false
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func envHasValue(root, key string) bool {
	data, err := os.ReadFile(filepath.Join(root, ".env"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimPrefix(strings.TrimSpace(line), "export ")
		k, v, ok := strings.Cut(t, "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.Trim(strings.TrimSpace(v), `"'`) != ""
		}
	}
	return false
}

type install struct {
	ecosystem string // "npm", "pypi", "crates", "packagist", "rubygems", "nuget", "pub", "go"
	packages  []string
	unsafe    string // why it can't be auto-approved, if anything
}

var (
	pipValueFlags   = map[string]bool{"-r": true, "--requirement": true, "-c": true, "--constraint": true, "-e": true, "--editable": true}
	cargoValueFlags = map[string]bool{"-F": true, "--features": true, "--rename": true, "-p": true, "--package": true}
	gemValueFlags   = map[string]bool{"-v": true, "--version": true, "--group": true}
	nugetValueFlags = map[string]bool{"-v": true, "--version": true, "-f": true, "--framework": true}
	unsafeFlags     = map[string]string{
		"-g":                "This is a global install, so it always needs your approval.",
		"--global":          "This is a global install, so it always needs your approval.",
		"-i":                "This uses a custom package index, so it always needs your approval.",
		"--index-url":       "This uses a custom package index, so it always needs your approval.",
		"--extra-index-url": "This uses a custom package index, so it always needs your approval.",
		"--registry":        "This uses a custom package registry, so it always needs your approval.",
		"--source":          "This uses a custom package source, so it always needs your approval.",
		"--git":             "This installs from a git repository, so it always needs your approval.",
		"--path":            "This installs from a local path, so it always needs your approval.",
	}
)

func parseInstall(c string) (install, bool) {
	t := strings.Fields(c)
	if len(t) >= 3 && strings.HasPrefix(t[0], "python") && t[1] == "-m" {
		t = t[2:] // python -m pip install ...
	}
	if len(t) == 0 {
		return install{}, false
	}
	sub := func(i int) string {
		if len(t) > i {
			return t[i]
		}
		return ""
	}
	switch t[0] {
	case "npm", "pnpm", "bun":
		if s := sub(1); s == "install" || s == "i" || s == "add" || s == "ci" {
			return parseArgs("npm", t[2:], nil), true
		}
	case "yarn":
		if len(t) == 1 {
			return install{ecosystem: "npm"}, true
		}
		if s := sub(1); s == "add" || s == "install" {
			return parseArgs("npm", t[2:], nil), true
		}
	case "pip", "pip3":
		if sub(1) == "install" {
			return parseArgs("pypi", t[2:], pipValueFlags), true
		}
	case "uv":
		switch {
		case sub(1) == "sync":
			return install{ecosystem: "pypi"}, true
		case sub(1) == "add":
			return parseArgs("pypi", t[2:], pipValueFlags), true
		case sub(1) == "pip" && sub(2) == "install":
			return parseArgs("pypi", t[3:], pipValueFlags), true
		}
	case "poetry":
		if sub(1) == "install" {
			return install{ecosystem: "pypi"}, true
		}
		if sub(1) == "add" {
			return parseArgs("pypi", t[2:], nil), true
		}
	case "cargo":
		if sub(1) == "fetch" {
			return install{ecosystem: "crates"}, true
		}
		if sub(1) == "add" {
			return parseArgs("crates", t[2:], cargoValueFlags), true
		}
	case "go":
		if sub(1) == "mod" && (sub(2) == "tidy" || sub(2) == "download") {
			return install{ecosystem: "go"}, true
		}
		if sub(1) == "get" {
			return parseArgs("go", t[2:], nil), true
		}
	case "composer":
		if sub(1) == "install" {
			return install{ecosystem: "packagist"}, true
		}
		if sub(1) == "require" {
			return parseArgs("packagist", t[2:], nil), true
		}
	case "gem":
		if sub(1) == "install" {
			return parseArgs("rubygems", t[2:], gemValueFlags), true
		}
	case "bundle":
		if sub(1) == "install" {
			return install{ecosystem: "rubygems"}, true
		}
		if sub(1) == "add" {
			return parseArgs("rubygems", t[2:], gemValueFlags), true
		}
	case "dotnet":
		if sub(1) == "restore" {
			return install{ecosystem: "nuget"}, true
		}
		// dotnet add [project] package <id> [--version x]
		if sub(1) == "add" {
			for j := 2; j < len(t); j++ {
				if t[j] == "package" {
					return parseArgs("nuget", t[j+1:], nugetValueFlags), true
				}
			}
		}
	case "flutter", "dart":
		if sub(1) == "pub" && sub(2) == "get" {
			return install{ecosystem: "pub"}, true
		}
		if sub(1) == "pub" && sub(2) == "add" {
			return parseArgs("pub", t[3:], nil), true
		}
	}
	return install{}, false
}

func parseArgs(eco string, args []string, valueFlags map[string]bool) install {
	in := install{ecosystem: eco}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			name := strings.SplitN(a, "=", 2)[0]
			if why, bad := unsafeFlags[name]; bad {
				in.unsafe = why
				return in
			}
			if valueFlags[name] && !strings.Contains(a, "=") {
				i++ // skip the flag's value, e.g. -r requirements.txt
			}
			continue
		}
		if looksLikeSource(eco, a) {
			in.unsafe = "This installs from a URL, git repository or local path, so it always needs your approval."
			return in
		}
		in.packages = append(in.packages, packageName(eco, a))
	}
	return in
}

func looksLikeSource(eco, a string) bool {
	for _, p := range []string{".", "/", "~", "git+", "github:", "file:", "link:", "http:", "https:"} {
		if strings.HasPrefix(a, p) {
			return true
		}
	}
	if strings.Contains(a, "://") || strings.Contains(a, `\`) {
		return true
	}
	for _, s := range []string{".tgz", ".tar.gz", ".whl", ".zip"} {
		if strings.HasSuffix(a, s) {
			return true
		}
	}
	// npm treats "user/repo" as a GitHub shorthand; scoped packages start with "@",
	// and Composer and Go names legitimately contain a slash.
	return eco != "go" && eco != "packagist" && strings.Contains(a, "/") && !strings.HasPrefix(a, "@")
}

func packageName(eco, a string) string {
	switch eco {
	case "npm":
		if strings.HasPrefix(a, "@") {
			if i := strings.Index(a[1:], "@"); i >= 0 {
				return a[:i+1] // @types/node@20 -> @types/node
			}
			return a
		}
		if i := strings.Index(a, "@"); i > 0 {
			return a[:i]
		}
	case "pypi":
		if i := strings.IndexAny(a, "=<>!~[;@ "); i > 0 {
			a = a[:i] // requests[socks]>=2 -> requests
		}
		return strings.ToLower(a)
	case "packagist", "pub":
		a = strings.TrimPrefix(strings.TrimPrefix(a, "dev:"), "override:") // pub add dev:pkg
		if i := strings.IndexAny(a, ":@"); i > 0 {
			a = a[:i] // vendor/pkg:^2.0  or  http:^1.2.0
		}
		return strings.ToLower(a)
	case "crates", "go", "rubygems", "nuget":
		if i := strings.IndexAny(a, ":@"); i > 0 {
			return a[:i]
		}
	}
	return a
}
