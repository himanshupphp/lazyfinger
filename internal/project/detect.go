// Package project works out what kind of project a folder holds and how to
// install, set up, and start it.
package project

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

type StepKind int

const (
	Install StepKind = iota
	Setup
	Start
)

type Step struct {
	Title    string
	Args     []string
	Kind     StepKind
	Ask      bool                    // confirm separately, e.g. database changes
	Skip     func(root string) bool  // already done?
	PortArgs func(port int) []string // the start command on another port, when known
}

func (s Step) Command() string { return strings.Join(s.Args, " ") }

type Project struct {
	Name    string
	Steps   []Step
	Needs   []Need
	EnvSkip []string // .env keys that a setup step fills in, e.g. APP_KEY
}

func (p *Project) StartStep() *Step {
	for i := range p.Steps {
		if p.Steps[i].Kind == Start {
			return &p.Steps[i]
		}
	}
	return nil
}

var (
	onWindows = runtime.GOOS == "windows"
	itoa      = strconv.Itoa
	firstNum  = regexp.MustCompile(`\d+`)
	goVerRe   = regexp.MustCompile(`(?m)^go\s+(\d+\.\d+(?:\.\d+)?)`)
	reqPyRe   = regexp.MustCompile(`requires-python\s*=\s*"([^"]+)"`)
)

// Detect returns how to run the project in root, or nil if no built-in
// detector recognizes it.
func Detect(root string) *Project {
	detectors := []func(string) *Project{
		laravel, symfony, django, pythonWeb, rails, node, spring,
		goProject, rust, dotnet, flutter, plainPHP, compose, pythonScript, static,
	}
	for _, d := range detectors {
		if p := d(root); p != nil {
			return p
		}
	}
	return nil
}

// ---------------------------------------------------------------- helpers

func exists(root, rel string) bool {
	_, err := os.Stat(filepath.Join(root, rel))
	return err == nil
}

func read(root, rel string) string {
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil || len(b) > 1<<20 {
		return ""
	}
	return string(b)
}

type packageJSON struct {
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	Engines         map[string]string `json:"engines"`
	Main            string            `json:"main"`
}

func readPackage(root string) *packageJSON {
	s := read(root, "package.json")
	if s == "" {
		return nil
	}
	var p packageJSON
	if json.Unmarshal([]byte(s), &p) != nil {
		return nil
	}
	return &p
}

func (p *packageJSON) has(dep string) bool {
	_, a := p.Dependencies[dep]
	_, b := p.DevDependencies[dep]
	return a || b
}

type composerJSON struct {
	Require map[string]string `json:"require"`
}

func readComposer(root string) *composerJSON {
	s := read(root, "composer.json")
	if s == "" {
		return nil
	}
	var c composerJSON
	if json.Unmarshal([]byte(s), &c) != nil {
		return nil
	}
	return &c
}

func packageManager(root string) string {
	switch {
	case exists(root, "pnpm-lock.yaml"):
		return "pnpm"
	case exists(root, "yarn.lock"):
		return "yarn"
	case exists(root, "bun.lock"), exists(root, "bun.lockb"):
		return "bun"
	}
	return "npm"
}

// scriptArgs runs a package.json script with extra arguments. npm needs "--"
// to pass them through; the others pass them directly.
func scriptArgs(pm, script string, extra ...string) []string {
	args := []string{pm, "run", script}
	if len(extra) > 0 && pm == "npm" {
		args = append(args, "--")
	}
	return append(args, extra...)
}

func nodeNeed(root string, pkg *packageJSON) Need {
	n := Need{Tool: "node", Label: "Node.js"}
	if pkg != nil && pkg.Engines["node"] != "" {
		n.Constraint, n.From = pkg.Engines["node"], "package.json engines"
	}
	for _, f := range []string{".nvmrc", ".node-version"} {
		if v := strings.TrimPrefix(strings.TrimSpace(read(root, f)), "v"); v != "" && n.Constraint == "" {
			n.Constraint, n.From = v, f
		}
	}
	if strings.HasPrefix(n.Constraint, "lts") || n.Constraint == "node" {
		n.Constraint = ""
	}
	return n
}

func phpNeed(c *composerJSON) Need {
	n := Need{Tool: "php", Label: "PHP"}
	if c != nil && c.Require["php"] != "" {
		n.Constraint, n.From = c.Require["php"], "composer.json"
	}
	return n
}

func envHas(root, key string) bool {
	for _, line := range strings.Split(read(root, ".env"), "\n") {
		t := strings.TrimPrefix(strings.TrimSpace(line), "export ")
		k, v, ok := strings.Cut(t, "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.Trim(strings.TrimSpace(v), `"'`) != ""
		}
	}
	return false
}

// ---------------------------------------------------------------- PHP

func laravel(root string) *Project {
	c := readComposer(root)
	if c == nil || !exists(root, "artisan") {
		return nil
	}
	fw, ok := c.Require["laravel/framework"]
	if !ok {
		return nil
	}
	name := "Laravel"
	if m := firstNum.FindString(fw); m != "" {
		name += " " + m
	}
	p := &Project{Name: name, EnvSkip: []string{"APP_KEY"}}
	p.Needs = []Need{phpNeed(c), {Tool: "composer", Label: "Composer"}}
	p.Steps = append(p.Steps, Step{Title: "install PHP dependencies", Args: []string{"composer", "install"}, Kind: Install})
	if pkg := readPackage(root); pkg != nil {
		pm := packageManager(root)
		p.Needs = append(p.Needs, nodeNeed(root, pkg), Need{Tool: pm, Label: pm})
		p.Steps = append(p.Steps, Step{Title: "install JavaScript dependencies", Args: []string{pm, "install"}, Kind: Install})
		if _, ok := pkg.Scripts["build"]; ok {
			p.Steps = append(p.Steps, Step{Title: "build front-end assets", Args: scriptArgs(pm, "build"), Kind: Setup})
		}
	}
	p.Steps = append(p.Steps,
		Step{Title: "set the app key", Args: []string{"php", "artisan", "key:generate"}, Kind: Setup,
			Skip: func(r string) bool { return envHas(r, "APP_KEY") }},
		Step{Title: "set up the database", Args: []string{"php", "artisan", "migrate"}, Kind: Setup, Ask: true},
		Step{Title: "start the app", Args: []string{"php", "artisan", "serve"}, Kind: Start,
			PortArgs: func(n int) []string { return []string{"php", "artisan", "serve", "--port=" + itoa(n)} }},
	)
	return p
}

func symfony(root string) *Project {
	c := readComposer(root)
	if c == nil || !exists(root, "bin/console") {
		return nil
	}
	if _, ok := c.Require["symfony/framework-bundle"]; !ok {
		return nil
	}
	serve := func(n int) []string { return []string{"php", "-S", "127.0.0.1:" + itoa(n), "-t", "public"} }
	return &Project{
		Name:  "Symfony",
		Needs: []Need{phpNeed(c), {Tool: "composer", Label: "Composer"}},
		Steps: []Step{
			{Title: "install PHP dependencies", Args: []string{"composer", "install"}, Kind: Install},
			{Title: "start the app", Args: serve(8000), Kind: Start, PortArgs: serve},
		},
	}
}

func plainPHP(root string) *Project {
	docroot := ""
	switch {
	case exists(root, "public/index.php"):
		docroot = "public"
	case exists(root, "index.php"):
		docroot = "."
	default:
		return nil
	}
	c := readComposer(root)
	serve := func(n int) []string { return []string{"php", "-S", "127.0.0.1:" + itoa(n), "-t", docroot} }
	p := &Project{Name: "PHP", Needs: []Need{phpNeed(c)}}
	if c != nil {
		p.Needs = append(p.Needs, Need{Tool: "composer", Label: "Composer"})
		p.Steps = append(p.Steps, Step{Title: "install PHP dependencies", Args: []string{"composer", "install"}, Kind: Install})
	}
	p.Steps = append(p.Steps, Step{Title: "start the app", Args: serve(8000), Kind: Start, PortArgs: serve})
	return p
}

// ---------------------------------------------------------------- JavaScript

func node(root string) *Project {
	pkg := readPackage(root)
	if pkg == nil {
		return nil
	}
	pm := packageManager(root)
	name := "Node.js"
	switch {
	case pkg.has("next"):
		name = "Next.js"
	case pkg.has("nuxt"):
		name = "Nuxt"
	case pkg.has("@sveltejs/kit"):
		name = "SvelteKit"
	case pkg.has("@angular/core"):
		name = "Angular"
	case pkg.has("vite"):
		name = "Vite"
	case pkg.has("express"):
		name = "Express"
	}
	var start []string
	script := ""
	for _, s := range []string{"dev", "start", "serve"} {
		if _, ok := pkg.Scripts[s]; ok {
			script, start = s, scriptArgs(pm, s)
			break
		}
	}
	if start == nil && pkg.Main != "" {
		start = []string{"node", pkg.Main}
	}
	if start == nil && exists(root, "index.js") {
		start = []string{"node", "index.js"}
	}
	if start == nil {
		return nil
	}
	st := Step{Title: "start the app", Args: start, Kind: Start}
	if script != "" {
		switch {
		case pkg.has("next"):
			st.PortArgs = func(n int) []string { return scriptArgs(pm, script, "-p", itoa(n)) }
		case pkg.has("vite"), pkg.has("nuxt"), pkg.has("@sveltejs/kit"), pkg.has("@angular/core"):
			st.PortArgs = func(n int) []string { return scriptArgs(pm, script, "--port", itoa(n)) }
		}
	}
	return &Project{
		Name:  name,
		Needs: []Need{nodeNeed(root, pkg), {Tool: pm, Label: pm}},
		Steps: []Step{{Title: "install dependencies", Args: []string{pm, "install"}, Kind: Install}, st},
	}
}

// ---------------------------------------------------------------- Python

type pyEnv struct {
	steps  []Step
	python []string // how to invoke the project's Python
	needs  []Need
}

func pythonEnv(root string) pyEnv {
	py := "python3"
	if onWindows {
		py = "python"
	}
	need := Need{Tool: py, Label: "Python"}
	if v := strings.TrimSpace(read(root, ".python-version")); v != "" {
		need.Constraint, need.From = v, ".python-version"
	} else if m := reqPyRe.FindStringSubmatch(read(root, "pyproject.toml")); m != nil {
		need.Constraint, need.From = m[1], "pyproject.toml"
	}

	switch {
	case exists(root, "uv.lock"):
		return pyEnv{
			steps:  []Step{{Title: "install Python dependencies", Args: []string{"uv", "sync"}, Kind: Install}},
			python: []string{"uv", "run", "python"},
			needs:  []Need{{Tool: "uv", Label: "uv"}},
		}
	case exists(root, "poetry.lock"):
		return pyEnv{
			steps:  []Step{{Title: "install Python dependencies", Args: []string{"poetry", "install"}, Kind: Install}},
			python: []string{"poetry", "run", "python"},
			needs:  []Need{need, {Tool: "poetry", Label: "Poetry"}},
		}
	}

	venvPy := filepath.Join(".venv", "bin", "python")
	if onWindows {
		venvPy = filepath.Join(".venv", "Scripts", "python.exe")
	}
	steps := []Step{{Title: "create a virtual environment", Args: []string{py, "-m", "venv", ".venv"}, Kind: Setup,
		Skip: func(r string) bool { return exists(r, venvPy) }}}
	switch {
	case exists(root, "requirements.txt"):
		steps = append(steps, Step{Title: "install Python dependencies", Args: []string{venvPy, "-m", "pip", "install", "-r", "requirements.txt"}, Kind: Install})
	case exists(root, "pyproject.toml"):
		steps = append(steps, Step{Title: "install Python dependencies", Args: []string{venvPy, "-m", "pip", "install", "-e", "."}, Kind: Install})
	}
	return pyEnv{steps: steps, python: []string{venvPy}, needs: []Need{need}}
}

func (e pyEnv) run(args ...string) []string {
	return append(append([]string{}, e.python...), args...)
}

func django(root string) *Project {
	if !exists(root, "manage.py") {
		return nil
	}
	e := pythonEnv(root)
	steps := append([]Step{}, e.steps...)
	steps = append(steps,
		Step{Title: "set up the database", Args: e.run("manage.py", "migrate"), Kind: Setup, Ask: true},
		Step{Title: "start the app", Args: e.run("manage.py", "runserver"), Kind: Start,
			PortArgs: func(n int) []string { return e.run("manage.py", "runserver", itoa(n)) }},
	)
	return &Project{Name: "Django", Needs: e.needs, Steps: steps}
}

func pythonWeb(root string) *Project {
	for _, f := range []string{"main.py", "app.py", "app/main.py", "src/main.py", "api/main.py", "server.py", "wsgi.py"} {
		src := read(root, f)
		if src == "" {
			continue
		}
		module := strings.TrimSuffix(strings.ReplaceAll(f, "/", "."), ".py")
		e := pythonEnv(root)
		steps := append([]Step{}, e.steps...)
		switch {
		case strings.Contains(src, "FastAPI("):
			target := module + ":" + appVar(src, "FastAPI(")
			steps = append(steps, Step{Title: "start the app", Args: e.run("-m", "uvicorn", target, "--reload"), Kind: Start,
				PortArgs: func(n int) []string { return e.run("-m", "uvicorn", target, "--reload", "--port", itoa(n)) }})
			return &Project{Name: "FastAPI", Needs: e.needs, Steps: steps}
		case strings.Contains(src, "Flask("):
			steps = append(steps, Step{Title: "start the app", Args: e.run("-m", "flask", "--app", module, "run"), Kind: Start,
				PortArgs: func(n int) []string { return e.run("-m", "flask", "--app", module, "run", "--port", itoa(n)) }})
			return &Project{Name: "Flask", Needs: e.needs, Steps: steps}
		}
	}
	return nil
}

func appVar(src, ctor string) string {
	re := regexp.MustCompile(`(?m)^(\w+)\s*=\s*` + regexp.QuoteMeta(ctor))
	if m := re.FindStringSubmatch(src); m != nil {
		return m[1]
	}
	return "app"
}

func pythonScript(root string) *Project {
	for _, f := range []string{"main.py", "app.py"} {
		if exists(root, f) {
			e := pythonEnv(root)
			steps := append(append([]Step{}, e.steps...), Step{Title: "start the app", Args: e.run(f), Kind: Start})
			return &Project{Name: "Python", Needs: e.needs, Steps: steps}
		}
	}
	return nil
}

// ---------------------------------------------------------------- others

func rails(root string) *Project {
	if !exists(root, "bin/rails") {
		return nil
	}
	railsCmd := func(a ...string) []string {
		if onWindows {
			return append([]string{"ruby", "bin/rails"}, a...)
		}
		return append([]string{"bin/rails"}, a...)
	}
	need := Need{Tool: "ruby", Label: "Ruby"}
	if v := strings.TrimPrefix(strings.TrimSpace(read(root, ".ruby-version")), "ruby-"); v != "" {
		need.Constraint, need.From = v, ".ruby-version"
	}
	return &Project{
		Name:  "Rails",
		Needs: []Need{need, {Tool: "bundle", Label: "Bundler"}},
		Steps: []Step{
			{Title: "install gems", Args: []string{"bundle", "install"}, Kind: Install},
			{Title: "set up the database", Args: railsCmd("db:prepare"), Kind: Setup, Ask: true},
			{Title: "start the app", Args: railsCmd("server"), Kind: Start,
				PortArgs: func(n int) []string { return railsCmd("server", "-p", itoa(n)) }},
		},
	}
}

func goProject(root string) *Project {
	mod := read(root, "go.mod")
	if mod == "" {
		return nil
	}
	target := ""
	if exists(root, "main.go") {
		target = "."
	} else if mains, _ := filepath.Glob(filepath.Join(root, "cmd", "*", "main.go")); len(mains) == 1 {
		rel, _ := filepath.Rel(root, filepath.Dir(mains[0]))
		target = "./" + filepath.ToSlash(rel)
	}
	if target == "" {
		return nil
	}
	need := Need{Tool: "go", Label: "Go"}
	if m := goVerRe.FindStringSubmatch(mod); m != nil {
		need.Constraint, need.From = ">="+m[1], "go.mod"
	}
	return &Project{Name: "Go", Needs: []Need{need}, Steps: []Step{{Title: "start the app", Args: []string{"go", "run", target}, Kind: Start}}}
}

func rust(root string) *Project {
	cargo := read(root, "Cargo.toml")
	if cargo == "" || !(exists(root, "src/main.rs") || strings.Contains(cargo, "[[bin]]")) {
		return nil
	}
	return &Project{Name: "Rust", Needs: []Need{{Tool: "cargo", Label: "Rust (cargo)"}},
		Steps: []Step{{Title: "start the app", Args: []string{"cargo", "run"}, Kind: Start}}}
}

func spring(root string) *Project {
	pom := read(root, "pom.xml")
	gradle := read(root, "build.gradle") + read(root, "build.gradle.kts")
	needs := []Need{{Tool: "java", Label: "Java"}}
	switch {
	case strings.Contains(pom, "spring-boot"):
		mvn := []string{"mvn"}
		if exists(root, "mvnw") {
			mvn = []string{"./mvnw"}
			if onWindows {
				mvn = []string{"mvnw.cmd"}
			}
		} else {
			needs = append(needs, Need{Tool: "mvn", Label: "Maven"})
		}
		cmd := func(extra ...string) []string {
			return append(append(append([]string{}, mvn...), "spring-boot:run"), extra...)
		}
		return &Project{Name: "Spring Boot", Needs: needs, Steps: []Step{{Title: "start the app", Args: cmd(), Kind: Start,
			PortArgs: func(n int) []string { return cmd("-Dspring-boot.run.arguments=--server.port=" + itoa(n)) }}}}
	case strings.Contains(gradle, "org.springframework.boot"):
		gw := []string{"gradle"}
		if exists(root, "gradlew") {
			gw = []string{"./gradlew"}
			if onWindows {
				gw = []string{"gradlew.bat"}
			}
		} else {
			needs = append(needs, Need{Tool: "gradle", Label: "Gradle"})
		}
		cmd := func(extra ...string) []string { return append(append(append([]string{}, gw...), "bootRun"), extra...) }
		return &Project{Name: "Spring Boot", Needs: needs, Steps: []Step{{Title: "start the app", Args: cmd(), Kind: Start,
			PortArgs: func(n int) []string { return cmd("--args=--server.port=" + itoa(n)) }}}}
	}
	return nil
}

func dotnet(root string) *Project {
	projects, _ := filepath.Glob(filepath.Join(root, "*.csproj"))
	if len(projects) != 1 {
		return nil
	}
	return &Project{Name: ".NET", Needs: []Need{{Tool: "dotnet", Label: ".NET SDK"}},
		Steps: []Step{{Title: "start the app", Args: []string{"dotnet", "run"}, Kind: Start,
			PortArgs: func(n int) []string { return []string{"dotnet", "run", "--urls", "http://localhost:" + itoa(n)} }}}}
}

func flutter(root string) *Project {
	if !strings.Contains(read(root, "pubspec.yaml"), "flutter:") {
		return nil
	}
	return &Project{Name: "Flutter", Needs: []Need{{Tool: "flutter", Label: "Flutter"}},
		Steps: []Step{
			{Title: "install packages", Args: []string{"flutter", "pub", "get"}, Kind: Install},
			{Title: "start the app", Args: []string{"flutter", "run"}, Kind: Start},
		}}
}

func compose(root string) *Project {
	for _, f := range []string{"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"} {
		if exists(root, f) {
			return &Project{Name: "Docker Compose", Needs: []Need{{Tool: "docker", Label: "Docker"}},
				Steps: []Step{{Title: "start the app", Args: []string{"docker", "compose", "up"}, Kind: Start}}}
		}
	}
	return nil
}

func static(root string) *Project {
	if !exists(root, "index.html") {
		return nil
	}
	py := "python3"
	if onWindows {
		py = "python"
	}
	serve := func(n int) []string { return []string{py, "-m", "http.server", itoa(n), "--bind", "127.0.0.1"} }
	return &Project{Name: "Static website", Needs: []Need{{Tool: py, Label: "Python"}},
		Steps: []Step{{Title: "start the app", Args: serve(8000), Kind: Start, PortArgs: serve}}}
}
