package project

import (
	"context"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Need is a tool the project requires, optionally with a version constraint.
type Need struct {
	Tool       string // command name: node, php, python3, composer...
	Label      string // what people call it: Node.js, PHP...
	Constraint string // e.g. "^8.2", ">=20", "3.12"; empty means any version
	From       string // where the constraint came from
}

type Status struct {
	Need
	Have  string // installed version, if found
	Found bool
	OK    bool // satisfies the constraint (or there isn't one)
}

// Dedupe merges needs for the same tool, keeping the first constraint seen.
func Dedupe(needs []Need) []Need {
	var out []Need
	idx := map[string]int{}
	for _, n := range needs {
		if n.Label == "" {
			n.Label = n.Tool
		}
		if i, ok := idx[n.Tool]; ok {
			if out[i].Constraint == "" {
				out[i].Constraint, out[i].From = n.Constraint, n.From
			}
			continue
		}
		idx[n.Tool] = len(out)
		out = append(out, n)
	}
	return out
}

func Check(needs []Need) []Status {
	var out []Status
	for _, n := range Dedupe(needs) {
		s := Status{Need: n}
		if _, err := exec.LookPath(n.Tool); err == nil {
			s.Found = true
			s.Have = version(n.Tool)
			// Windows ships a "python" stub that only opens the Store.
			if runtime.GOOS == "windows" && strings.HasPrefix(n.Tool, "python") && s.Have == "" {
				s.Found = false
			}
		}
		s.OK = s.Found && (n.Constraint == "" || s.Have == "" || Satisfies(s.Have, n.Constraint))
		out = append(out, s)
	}
	return out
}

var verRe = regexp.MustCompile(`\d+\.\d+(?:\.\d+)?`)

func version(tool string) string {
	args := []string{tool, "--version"}
	switch tool {
	case "php":
		args = []string{"php", "-r", "echo PHP_VERSION;"}
	case "go":
		args = []string{"go", "version"}
	case "java":
		args = []string{"java", "-version"}
	case "ruby":
		args = []string{"ruby", "-v"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
	return verRe.FindString(string(out))
}

var (
	altSplitRe = regexp.MustCompile(`\s*\|\|?\s*`)
	opSpaceRe  = regexp.MustCompile(`([<>=~^!]+)\s+`)
)

// Satisfies checks a version against common constraint styles from npm,
// Composer, and Python: ^8.2, ~8.2, >=18, <21, 20.x, 8.2.*, 3.12, a || b.
// Anything it can't parse counts as satisfied, so it never blocks wrongly.
func Satisfies(have, constraint string) bool {
	h := parseVersion(have)
	if h == nil {
		return true
	}
	constraint = opSpaceRe.ReplaceAllString(constraint, "$1")
	for _, alt := range altSplitRe.Split(constraint, -1) {
		if alt = strings.TrimSpace(alt); alt == "" {
			continue
		}
		ok := true
		for _, term := range strings.FieldsFunc(alt, func(r rune) bool { return r == ' ' || r == ',' }) {
			if !satisfiesTerm(h, term) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func satisfiesTerm(h []int, term string) bool {
	op := ""
	for _, o := range []string{">=", "<=", "==", "~=", "!=", ">", "<", "=", "^", "~"} {
		if strings.HasPrefix(term, o) {
			op = o
			break
		}
	}
	v := parseVersion(strings.TrimPrefix(term, op))
	if v == nil {
		return true
	}
	switch op {
	case ">=":
		return compare(h, v) >= 0
	case ">":
		return compare(h, v) > 0
	case "<=":
		return compare(h, v) <= 0
	case "<":
		return compare(h, v) < 0
	case "!=":
		return compare(h, v) != 0
	case "^":
		return at(h, 0) == v[0] && compare(h, v) >= 0
	case "~", "~=":
		if len(v) >= 3 && v[2] >= 0 {
			return at(h, 0) == v[0] && at(h, 1) == at(v, 1) && compare(h, v) >= 0
		}
		return at(h, 0) == v[0] && compare(h, v) >= 0
	}
	// "=", "==" or a bare version / wildcard: match the parts that were given.
	for i, x := range v {
		if x < 0 {
			break
		}
		if at(h, i) != x {
			return false
		}
	}
	return true
}

// parseVersion turns "v8.2.x" into [8 2 -1]; -1 marks a wildcard.
func parseVersion(s string) []int {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if s == "" {
		return nil
	}
	var out []int
	for _, part := range strings.Split(s, ".") {
		if part == "x" || part == "X" || part == "*" {
			out = append(out, -1)
			continue
		}
		digits := firstNum.FindString(part)
		if digits == "" || !strings.HasPrefix(part, digits) {
			break
		}
		n, _ := strconv.Atoi(digits)
		out = append(out, n)
		if digits != part {
			break // e.g. "0rc1"
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func at(v []int, i int) int {
	if i < len(v) && v[i] >= 0 {
		return v[i]
	}
	return 0
}

func compare(a, b []int) int {
	for i := 0; i < 3; i++ {
		if x, y := at(a, i), at(b, i); x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Help says how to install a tool on this computer.
type Help struct {
	Command string // only set when its installer (brew, winget...) is available
	URL     string
}

var downloadPages = map[string]string{
	"node": "https://nodejs.org/en/download", "npm": "https://nodejs.org/en/download",
	"pnpm": "https://pnpm.io/installation", "yarn": "https://yarnpkg.com/getting-started/install", "bun": "https://bun.sh",
	"php": "https://www.php.net/downloads.php", "composer": "https://getcomposer.org/download/",
	"python": "https://www.python.org/downloads/", "python3": "https://www.python.org/downloads/",
	"uv": "https://docs.astral.sh/uv/getting-started/installation/", "poetry": "https://python-poetry.org/docs/#installation",
	"ruby": "https://www.ruby-lang.org/en/documentation/installation/", "bundle": "https://bundler.io",
	"go": "https://go.dev/dl/", "cargo": "https://rustup.rs", "java": "https://adoptium.net",
	"mvn": "https://maven.apache.org/install.html", "gradle": "https://gradle.org/install/",
	"dotnet": "https://dotnet.microsoft.com/download", "flutter": "https://docs.flutter.dev/get-started/install",
	"docker": "https://docs.docker.com/get-docker/",
}

var brewInstall = map[string]string{
	"node": "brew install node", "npm": "brew install node", "pnpm": "brew install pnpm", "yarn": "brew install yarn",
	"bun": "brew install oven-sh/bun/bun", "php": "brew install php", "composer": "brew install composer",
	"python3": "brew install python", "uv": "brew install uv", "poetry": "brew install poetry",
	"ruby": "brew install ruby", "go": "brew install go", "java": "brew install --cask temurin",
	"mvn": "brew install maven", "gradle": "brew install gradle", "dotnet": "brew install --cask dotnet-sdk",
	"flutter": "brew install --cask flutter", "docker": "brew install --cask docker",
}

var wingetInstall = map[string]string{
	"node": "winget install -e --id OpenJS.NodeJS.LTS", "npm": "winget install -e --id OpenJS.NodeJS.LTS",
	"python": "winget install -e --id Python.Python.3.12", "go": "winget install -e --id GoLang.Go",
	"java": "winget install -e --id EclipseAdoptium.Temurin.21.JDK", "dotnet": "winget install -e --id Microsoft.DotNet.SDK.8",
	"docker": "winget install -e --id Docker.DockerDesktop", "cargo": "winget install -e --id Rustlang.Rustup",
}

func InstallHelp(tool string) Help {
	h := Help{URL: downloadPages[tool]}
	cmd := ""
	switch runtime.GOOS {
	case "darwin":
		cmd = brewInstall[tool]
	case "windows":
		cmd = wingetInstall[tool]
	}
	if tool == "pnpm" || tool == "yarn" {
		if _, err := exec.LookPath("corepack"); err == nil {
			cmd = "corepack enable"
		}
	}
	if cmd != "" {
		if _, err := exec.LookPath(strings.Fields(cmd)[0]); err == nil {
			h.Command = cmd
		}
	}
	return h
}

// VersionHint suggests the usual way to switch versions of a tool.
func VersionHint(tool string) string {
	switch tool {
	case "node":
		return "If you use nvm or fnm, run `nvm use` or `fnm use` in this folder, then `lazy run` again."
	case "python", "python3":
		return "If you use pyenv or uv, install the version this project asks for, then `lazy run` again."
	case "ruby":
		return "If you use rbenv or rvm, install the version in .ruby-version, then `lazy run` again."
	case "php":
		return "On macOS with Homebrew you can install a specific version, e.g. `brew install php@8.3`."
	}
	return ""
}
