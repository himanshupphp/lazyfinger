package gather

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/himanshupphp/lazyfinger/internal/patch"
)

const (
	maxFiles       = 6
	wholeFileLines = 400   // send small files whole
	window         = 40    // lines around each error location
	snippetBudget  = 60000 // characters of source across all files
)

type FileSnippet struct {
	Path       string
	Start, End int
	Whole      bool
	Content    string
}

type Context struct {
	Command   string
	ExitCode  int
	Log       string
	AppLog    string // recent framework log entry (Laravel), redacted
	Files     []FileSnippet
	Manifests map[string]string
	Lockfiles []string
	Runtimes  []string
	Framework string
	Hints     string
	Env       string // .env variable names only, never values
	GitStatus string
	Platform  string
}

var (
	cursorRe  = regexp.MustCompile(`\x1b\[\d*(?:;\d*)?[Hf]`)
	ansiRe    = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)
	errLineRe = regexp.MustCompile(`(?i)(error|err!|failed|exception|traceback|panic|cannot find|not found|fatal|undefined)`)
	// path:line(:col)  or  path(line,col): covers most compilers, linters and stack traces
	refRe = regexp.MustCompile(`((?:[A-Za-z]:)?[\w.\-/\\@]+\.[A-Za-z0-9]{1,6})(?::(\d+)|\((\d+),\d+\))`)
	pyRe  = regexp.MustCompile(`File "([^"]+)", line (\d+)`)
	// "in /var/www/app/User.php on line 12" (PHP's own error format)
	phpRe = regexp.MustCompile(`(?:in )?([\w.\-/\\@:]+\.php) on line (\d+)`)
	// "at com.app.UserService.save(UserService.java:42)" (JVM stack frames)
	jvmRe = regexp.MustCompile(`at ([\w$.]+)\.[\w$<>]+\(([\w$]+\.(?:java|kt|scala|groovy)):(\d+)\)`)
	// Laravel: "(View: /path/resources/views/home.blade.php)"
	bladeViewRe = regexp.MustCompile(`\(View: ([^)]+\.blade\.php)\)`)
	// Compiled Blade views end with a comment naming their source template.
	compiledViewRe = regexp.MustCompile(`/\*\*PATH (.+?\.blade\.php) ENDPATH\*\*/`)
	logEntryRe     = regexp.MustCompile(`(?m)^\[[^\]]+\] [\w-]+\.(?:ERROR|CRITICAL|ALERT|EMERGENCY):`)
	digitRe        = regexp.MustCompile(`\d+`)

	manifestNames = []string{
		"package.json", "pyproject.toml", "requirements.txt", "setup.py", "Pipfile",
		"Cargo.toml", "go.mod", "composer.json", "Gemfile",
		"pom.xml", "build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts",
		"pubspec.yaml", "mix.exs", "Package.swift", "CMakeLists.txt", "Makefile",
	}
	manifestGlobs = []string{"*.csproj", "*.fsproj", "*.sln"}
	lockfileNames = []string{
		"package-lock.json", "pnpm-lock.yaml", "yarn.lock", "bun.lock", "bun.lockb",
		"poetry.lock", "uv.lock", "Pipfile.lock", "Cargo.lock", "go.sum",
		"composer.lock", "Gemfile.lock", "pubspec.lock", "mix.lock", "Package.resolved",
		"packages.lock.json", "gradle.lockfile",
	}

	secretRes = []struct {
		re   *regexp.Regexp
		repl string
	}{
		{regexp.MustCompile(`(?i)((?:api[_-]?key|secret|token|password|passwd|authorization)["']?\s*[=:]\s*["']?)[^\s"',;]{6,}`), "${1}[REDACTED]"},
		{regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_\-]{16,}|ghp_[A-Za-z0-9]{20,}|xox[abprs]-[A-Za-z0-9\-]{10,}|AKIA[0-9A-Z]{16})\b`), "[REDACTED]"},
		{regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._\-]{16,}`), "${1}[REDACTED]"},
		{regexp.MustCompile(`base64:[A-Za-z0-9+/=]{20,}`), "base64:[REDACTED]"}, // Laravel APP_KEY
	}
)

// Clean strips color codes and collapses progress-bar redraws (\r) to their final state.
func Clean(raw []byte) string {
	s := cursorRe.ReplaceAllString(string(raw), "\n") // ConPTY cursor jumps become line breaks
	s = ansiRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		l = strings.TrimRight(l, "\r")
		if j := strings.LastIndex(l, "\r"); j >= 0 {
			l = l[j+1:]
		}
		lines[i] = l
	}
	return strings.Join(lines, "\n")
}

func Redact(s string) string {
	for _, r := range secretRes {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}

// Fingerprint identifies "the same error" across runs, ignoring numbers.
func Fingerprint(log string) string {
	var sig []string
	for _, l := range strings.Split(log, "\n") {
		if errLineRe.MatchString(l) {
			sig = append(sig, digitRe.ReplaceAllString(strings.TrimSpace(l), "#"))
		}
	}
	if len(sig) == 0 {
		lines := strings.Split(strings.TrimSpace(log), "\n")
		sig = lines[max(0, len(lines)-5):]
	}
	h := sha256.Sum256([]byte(strings.Join(sig, "\n")))
	return hex.EncodeToString(h[:8])
}

func Collect(root string, args []string, exitCode int, log string) *Context {
	c := &Context{
		Command:   strings.Join(args, " "),
		ExitCode:  exitCode,
		Log:       Redact(trimLog(log)),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
		Manifests: map[string]string{},
	}
	for _, m := range manifestNames {
		if b, err := os.ReadFile(filepath.Join(root, m)); err == nil {
			c.Manifests[m] = truncate(string(b), 4000)
		}
	}
	for _, g := range manifestGlobs {
		matches, _ := filepath.Glob(filepath.Join(root, g))
		for _, m := range matches {
			if len(c.Manifests) >= 12 {
				break
			}
			if b, err := os.ReadFile(m); err == nil {
				c.Manifests[filepath.Base(m)] = truncate(string(b), 4000)
			}
		}
	}
	for _, l := range lockfileNames {
		if _, err := os.Stat(filepath.Join(root, l)); err == nil {
			c.Lockfiles = append(c.Lockfiles, l)
		}
	}
	c.Framework, c.Hints = detectFramework(root, c.Manifests)

	appLog := ""
	if c.Framework == "Laravel" {
		appLog = laravelLog(root)
	}
	c.AppLog = Redact(appLog)
	c.Files = snippets(root, findRefs(root, log+"\n"+appLog))
	c.Runtimes = runtimeVersions(c.Manifests)
	c.Env = envSummary(root)

	if out, err := exec.Command("git", "-C", root, "status", "--porcelain").Output(); err == nil {
		c.GitStatus = Redact(firstLines(string(out), 40))
	}
	return c
}

func (c *Context) Prompt() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Command: %s\nExit code: %d\nPlatform: %s\n", c.Command, c.ExitCode, c.Platform)
	if c.Framework != "" {
		fmt.Fprintf(&b, "Framework: %s\n", c.Framework)
	}
	if len(c.Runtimes) > 0 {
		fmt.Fprintf(&b, "Installed tools: %s\n", strings.Join(c.Runtimes, "; "))
	}
	if len(c.Lockfiles) > 0 {
		fmt.Fprintf(&b, "Lockfiles present: %s\n", strings.Join(c.Lockfiles, ", "))
	}
	if c.Env != "" {
		fmt.Fprintf(&b, "\n## Environment file (names only)\n%s\n", c.Env)
	}
	if c.Hints != "" {
		fmt.Fprintf(&b, "\n## Notes for this framework\n%s\n", c.Hints)
	}
	fmt.Fprintf(&b, "\n## Command output\n```\n%s\n```\n", c.Log)
	if c.AppLog != "" {
		fmt.Fprintf(&b, "\n## Most recent error in the application log\n```\n%s\n```\n", c.AppLog)
	}
	for _, f := range c.Files {
		if f.Whole {
			fmt.Fprintf(&b, "\n## File: %s\n", filepath.ToSlash(f.Path))
		} else {
			fmt.Fprintf(&b, "\n## File: %s (lines %d-%d)\n", filepath.ToSlash(f.Path), f.Start, f.End)
		}
		fmt.Fprintf(&b, "```\n%s\n```\n", f.Content)
	}
	names := make([]string, 0, len(c.Manifests))
	for n := range c.Manifests {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "\n## %s\n```\n%s\n```\n", n, c.Manifests[n])
	}
	if c.GitStatus != "" {
		fmt.Fprintf(&b, "\n## git status\n```\n%s\n```\n", c.GitStatus)
	}
	return b.String()
}

// trimLog keeps the head, the tail, and error-looking lines from the middle.
func trimLog(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i, l := range lines {
		if len(l) > 500 { // minified bundles produce enormous lines
			lines[i] = l[:500] + "..."
		}
	}
	const head, tailN, middleMax = 20, 120, 60
	if len(lines) <= head+tailN {
		return strings.Join(lines, "\n")
	}
	middle := lines[head : len(lines)-tailN]
	out := append([]string{}, lines[:head]...)
	var kept []string
	for _, l := range middle {
		if len(kept) < middleMax && errLineRe.MatchString(l) {
			kept = append(kept, l)
		}
	}
	out = append(out, fmt.Sprintf("... (%d lines omitted, error lines kept) ...", len(middle)-len(kept)))
	out = append(out, kept...)
	out = append(out, lines[len(lines)-tailN:]...)
	return strings.Join(out, "\n")
}

type ref struct {
	path  string
	lines []int
}

func findRefs(root, log string) []ref {
	var order []string
	seen := map[string]*ref{}
	idx := &fileIndex{root: root}
	add := func(p, n string) {
		rel, ok := patch.Within(root, p)
		if ok {
			if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
				ok = false
			}
		}
		if !ok && filepath.IsAbs(p) {
			rel, ok = trailingMatch(root, p) // Docker / Sail paths like /var/www/html/app/...
		}
		if !ok && !filepath.IsAbs(p) {
			rel, ok = idx.locate(p)
		}
		if !ok {
			return
		}
		if src, isView := bladeSource(root, rel); isView {
			if src == "" {
				return
			}
			rel, n = src, "" // compiled line numbers don't match the template
		}
		rel, ok = resolve(root, rel)
		if !ok {
			return
		}
		r, exists := seen[rel]
		if !exists {
			if len(order) >= maxFiles {
				return
			}
			r = &ref{path: rel}
			seen[rel] = r
			order = append(order, rel)
		}
		if line, err := strconv.Atoi(n); err == nil && line > 0 {
			r.lines = append(r.lines, line)
		}
	}
	for _, m := range bladeViewRe.FindAllStringSubmatch(log, -1) {
		add(m[1], "")
	}
	// JVM frames: the package gives the directory, which beats a bare file name.
	for _, m := range jvmRe.FindAllStringSubmatch(log, -1) {
		pkg := m[1]
		if i := strings.LastIndex(pkg, "."); i >= 0 {
			pkg = pkg[:i]
		} else {
			pkg = ""
		}
		add(filepath.Join(strings.ReplaceAll(pkg, ".", "/"), m[2]), m[3])
	}
	for _, m := range refRe.FindAllStringSubmatch(log, -1) {
		n := m[2]
		if n == "" {
			n = m[3]
		}
		add(m[1], n)
	}
	for _, m := range pyRe.FindAllStringSubmatch(log, -1) {
		add(m[1], m[2])
	}
	for _, m := range phpRe.FindAllStringSubmatch(log, -1) {
		add(m[1], m[2])
	}
	out := make([]ref, 0, len(order))
	for _, p := range order {
		out = append(out, *seen[p])
	}
	return out
}

// trailingMatch maps an absolute path from another machine or container to
// the project by trying shorter and shorter tails: /var/www/html/app/X.php -> app/X.php.
func trailingMatch(root, p string) (string, bool) {
	parts := strings.Split(filepath.ToSlash(p), "/")
	for k := 1; k < len(parts) && k < 12; k++ {
		tail := filepath.FromSlash(strings.Join(parts[k:], "/"))
		if tail == "" {
			continue
		}
		if fi, err := os.Stat(filepath.Join(root, tail)); err == nil && fi.Mode().IsRegular() {
			return tail, true
		}
	}
	return "", false
}

// bladeSource maps a compiled Laravel view back to its .blade.php template.
// isView reports whether rel was a compiled view at all.
func bladeSource(root, rel string) (src string, isView bool) {
	if !strings.HasPrefix(filepath.ToSlash(rel), "storage/framework/views/") {
		return "", false
	}
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return "", true
	}
	m := compiledViewRe.FindSubmatch(data)
	if m == nil {
		return "", true
	}
	path := string(m[1])
	if r, ok := patch.Within(root, path); ok {
		if _, err := os.Stat(filepath.Join(root, r)); err == nil {
			return r, true
		}
	}
	if r, ok := trailingMatch(root, path); ok {
		return r, true
	}
	return "", true
}

// resolve only accepts real, small, text-like files inside the project that
// belong to you, not to dependencies or caches.
func resolve(root, p string) (string, bool) {
	rel, ok := patch.Within(root, p)
	if !ok {
		return "", false
	}
	slash := filepath.ToSlash(rel)
	for _, skip := range []string{"node_modules/", "vendor/", ".git/", ".lazyfinger/", "storage/framework/", "bootstrap/cache/"} {
		if strings.HasPrefix(slash, skip) || strings.Contains(slash, "/"+skip) {
			return "", false
		}
	}
	if strings.HasPrefix(filepath.Base(rel), ".env") { // never send secrets files
		return "", false
	}
	fi, err := os.Stat(filepath.Join(root, rel))
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > 1<<20 {
		return "", false
	}
	return rel, true
}

func snippets(root string, refs []ref) []FileSnippet {
	var out []FileSnippet
	budget := snippetBudget
	for _, r := range refs {
		data, err := os.ReadFile(filepath.Join(root, r.path))
		if err != nil || !utf8.Valid(data) {
			continue
		}
		text := strings.ReplaceAll(string(data), "\r\n", "\n")
		lines := strings.Split(text, "\n")

		if len(lines) <= wholeFileLines || len(r.lines) == 0 {
			if len(lines) > wholeFileLines {
				text = strings.Join(lines[:wholeFileLines], "\n")
			}
			if len(text) > budget {
				return out
			}
			budget -= len(text)
			out = append(out, FileSnippet{Path: r.path, Whole: len(lines) <= wholeFileLines, Start: 1, End: min(len(lines), wholeFileLines), Content: text})
			continue
		}
		for _, rg := range ranges(r.lines, len(lines)) {
			content := strings.Join(lines[rg[0]-1:rg[1]], "\n")
			if len(content) > budget {
				return out
			}
			budget -= len(content)
			out = append(out, FileSnippet{Path: r.path, Start: rg[0], End: rg[1], Content: content})
		}
	}
	return out
}

// ranges merges overlapping windows around each referenced line.
func ranges(lines []int, total int) [][2]int {
	sort.Ints(lines)
	var out [][2]int
	for _, n := range lines {
		if n > total {
			continue
		}
		s, e := max(1, n-window), min(total, n+window)
		if k := len(out) - 1; k >= 0 && s <= out[k][1]+1 {
			out[k][1] = max(out[k][1], e)
			continue
		}
		out = append(out, [2]int{s, e})
	}
	return out
}

// fileIndex finds files that errors name by a partial path, such as a Java
// source under src/main/java or a test file named only by its base name.
type fileIndex struct {
	root   string
	byName map[string][]string
}

var skipDirs = map[string]bool{
	".git": true, ".lazyfinger": true, "node_modules": true, "vendor": true, "target": true,
	"build": true, "dist": true, "out": true, "bin": true, "obj": true, ".gradle": true,
	".idea": true, ".venv": true, "venv": true, "__pycache__": true, ".dart_tool": true,
	"Pods": true, "_build": true, "deps": true, ".next": true, "storage": true,
}

func (x *fileIndex) locate(p string) (string, bool) {
	if x.byName == nil {
		x.byName = map[string][]string{}
		count := 0
		_ = filepath.WalkDir(x.root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if skipDirs[d.Name()] && path != x.root {
					return filepath.SkipDir
				}
				return nil
			}
			if count++; count > 20000 { // keep huge repos fast
				return filepath.SkipAll
			}
			rel, _ := filepath.Rel(x.root, path)
			x.byName[d.Name()] = append(x.byName[d.Name()], rel)
			return nil
		})
	}
	want := filepath.ToSlash(filepath.Clean(p))
	candidates := x.byName[filepath.Base(want)]
	var match []string
	for _, c := range candidates {
		if s := filepath.ToSlash(c); s == want || strings.HasSuffix(s, "/"+want) {
			match = append(match, c)
		}
	}
	if len(match) == 1 {
		return match[0], true
	}
	if len(match) == 0 && len(candidates) == 1 && !strings.Contains(want, "/") {
		return candidates[0], true // a bare file name that exists exactly once
	}
	return "", false // missing or ambiguous: don't guess
}

const laravelHints = `Laravel project. Common causes of failure:
- Dependencies not installed (vendor/ missing): propose "composer install".
- Missing .env: propose "cp .env.example .env".
- Missing APP_KEY ("No application encryption key has been specified"): propose "php artisan key:generate".
- Stale config, route or view caches: propose "php artisan optimize:clear".
- Database unreachable or wrong credentials: say which DB_* variables in .env the user must set. This can't be fixed with code.
- Missing tables: propose "php artisan migrate". Never propose migrate:fresh, migrate:reset, migrate:refresh, migrate:rollback or db:wipe.
- Missing public storage link: propose "php artisan storage:link".
- PHP version too old for composer.json: say which version is required.
Edit application code under app/, routes/, config/, database/ and resources/. Never edit vendor/, storage/ or bootstrap/cache/. Never edit .env; tell the user which variable to set instead.`

// detectFramework names well-known frameworks and returns hints for the model.
func detectFramework(root string, m map[string]string) (string, string) {
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(root, name))
		return err == nil
	}
	switch {
	case has("artisan") && strings.Contains(m["composer.json"], "laravel/framework"):
		return "Laravel", laravelHints
	case has("bin/console") && strings.Contains(m["composer.json"], "symfony/"):
		return "Symfony", "Symfony project. Edit code under src/, config/ and templates/. Never edit vendor/ or var/. Propose \"php bin/console cache:clear\" when config changes don't apply."
	case has("manage.py"):
		return "Django", "Django project. Edit code in the project's apps. Propose \"python manage.py makemigrations\" or \"python manage.py migrate\" as commands; never edit migrations that were already applied, and never propose \"flush\"."
	case has("bin/rails"):
		return "Rails", "Rails project. Edit code under app/, config/ and db/. Propose \"bundle install\" for missing gems and \"bin/rails db:migrate\" for pending migrations; never propose db:drop or db:reset."
	case strings.Contains(m["package.json"], "\"next\""):
		return "Next.js", "Next.js project. Edit code under app/, pages/, components/ and lib/. Never edit .next/."
	case strings.Contains(m["pom.xml"]+m["build.gradle"]+m["build.gradle.kts"], "spring-boot"):
		return "Spring Boot", "Spring Boot project. Edit code under src/main and src/test. Configuration lives in application.properties or application.yml."
	case strings.Contains(m["pubspec.yaml"], "flutter:"):
		return "Flutter", "Flutter project. Edit code under lib/ and test/. Propose \"flutter pub get\" for missing packages."
	}
	return "", ""
}

// laravelLog returns the newest error entry from Laravel's log if it was
// written in the last few minutes, so it likely belongs to this run.
func laravelLog(root string) string {
	matches, _ := filepath.Glob(filepath.Join(root, "storage", "logs", "laravel*.log"))
	var newest string
	var newestT time.Time
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.ModTime().After(newestT) {
			newest, newestT = m, fi.ModTime()
		}
	}
	if newest == "" || time.Since(newestT) > 10*time.Minute {
		return ""
	}
	f, err := os.Open(newest)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	const maxRead = 64 << 10
	off := fi.Size() - maxRead
	if off < 0 {
		off = 0
	}
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	text := string(buf)
	locs := logEntryRe.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		return ""
	}
	return firstLines(text[locs[len(locs)-1][0]:], 60)
}

// runtimeVersions reports the versions of the tools this project needs, or
// that they're missing, which explains many install and build failures.
func runtimeVersions(m map[string]string) []string {
	has := func(names ...string) bool {
		for _, n := range names {
			if _, ok := m[n]; ok {
				return true
			}
		}
		return false
	}
	py := "python3"
	if runtime.GOOS == "windows" {
		py = "python"
	}
	var probes [][]string
	if has("package.json") {
		probes = append(probes, []string{"node", "--version"})
	}
	if has("composer.json") {
		probes = append(probes, []string{"php", "--version"}, []string{"composer", "--version"})
	}
	if has("pyproject.toml", "requirements.txt", "setup.py", "Pipfile") {
		probes = append(probes, []string{py, "--version"})
	}
	if has("Cargo.toml") {
		probes = append(probes, []string{"rustc", "--version"})
	}
	if has("go.mod") {
		probes = append(probes, []string{"go", "version"})
	}
	if has("Gemfile") {
		probes = append(probes, []string{"ruby", "--version"})
	}
	if has("pom.xml", "build.gradle", "build.gradle.kts") {
		probes = append(probes, []string{"java", "-version"})
	}
	if has("pubspec.yaml") {
		probes = append(probes, []string{"dart", "--version"})
	}
	if has("mix.exs") {
		probes = append(probes, []string{"elixir", "--version"})
	}
	if has("Package.swift") {
		probes = append(probes, []string{"swift", "--version"})
	}
	for name := range m {
		if strings.HasSuffix(name, ".csproj") || strings.HasSuffix(name, ".fsproj") || strings.HasSuffix(name, ".sln") {
			probes = append(probes, []string{"dotnet", "--version"})
			break
		}
	}

	var out []string
	for _, p := range probes {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		b, err := exec.CommandContext(ctx, p[0], p[1:]...).CombinedOutput()
		cancel()
		line := ""
		for _, l := range strings.Split(string(b), "\n") {
			if t := strings.TrimSpace(l); t != "" {
				line = t
				break
			}
		}
		if errors.Is(err, exec.ErrNotFound) {
			line = "not installed"
		} else if line == "" {
			continue
		}
		if len(line) > 120 {
			line = line[:120]
		}
		out = append(out, p[0]+": "+line)
	}
	return out
}

// envSummary compares .env with its example file and reports variable names
// only. Values are read locally to check for emptiness and never leave here.
func envSummary(root string) string {
	example := ""
	for _, n := range []string{".env.example", ".env.sample", ".env.dist"} {
		if _, err := os.Stat(filepath.Join(root, n)); err == nil {
			example = n
			break
		}
	}
	if example == "" {
		return ""
	}
	exKeys, _, err := readEnv(filepath.Join(root, example))
	if err != nil {
		return ""
	}
	_, envVals, err := readEnv(filepath.Join(root, ".env"))
	if err != nil {
		return fmt.Sprintf(".env is missing. %s exists and defines %d variables.", example, len(exKeys))
	}
	var missing, empty []string
	for _, k := range exKeys {
		v, ok := envVals[k]
		switch {
		case !ok:
			missing = append(missing, k)
		case v == "":
			empty = append(empty, k)
		}
	}
	s := ".env exists."
	if len(missing) > 0 {
		s += fmt.Sprintf(" In %s but not in .env: %s.", example, strings.Join(capList(missing, 20), ", "))
	}
	if len(empty) > 0 {
		s += fmt.Sprintf(" Empty in .env: %s.", strings.Join(capList(empty, 20), ", "))
	}
	return s
}

func readEnv(path string) ([]string, map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var keys []string
	vals := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		t = strings.TrimPrefix(t, "export ")
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, seen := vals[k]; !seen {
			keys = append(keys, k)
		}
		vals[k] = v
	}
	return keys, vals, nil
}

func capList(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(s[:n:n], fmt.Sprintf("and %d more", len(s)-n))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n... (truncated)"
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = append(lines[:n], fmt.Sprintf("... (%d more)", len(lines)-n))
	}
	return strings.Join(lines, "\n")
}
