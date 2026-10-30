// Package install knows how to install developer tools and databases with
// the computer's own package manager. Nothing runs without the user's say-so:
// callers show the exact commands and ask first.
package install

import (
	"os/exec"
	"runtime"
	"strings"
)

// Method is one way to install something: commands to run in order.
type Method struct {
	Label string     // e.g. "Homebrew", "apt"
	Steps [][]string // argument lists; a single string with spaces runs through the shell
	Sudo  bool       // will ask for the computer password
	Note  string
}

func (m Method) Commands() []string {
	out := make([]string, len(m.Steps))
	for i, s := range m.Steps {
		out[i] = strings.Join(s, " ")
	}
	return out
}

type Plan struct {
	Tool    string
	Methods []Method // best first
	URL     string
}

func has(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}

// Manager reports this computer's package manager: brew, winget, apt-get, dnf, pacman, or "".
func Manager() string {
	switch runtime.GOOS {
	case "darwin":
		if has("brew") {
			return "brew"
		}
	case "windows":
		if has("winget") {
			return "winget"
		}
	case "linux":
		for _, pm := range []string{"apt-get", "dnf", "pacman"} {
			if has(pm) {
				return pm
			}
		}
	}
	return ""
}

// The official Homebrew installer. It asks for your password and to press Enter.
const homebrewInstall = `/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"`

// For returns the ways to install a developer tool on this computer.
func For(tool string) Plan {
	p := Plan{Tool: tool, URL: pages[tool]}
	if (tool == "pnpm" || tool == "yarn") && has("corepack") {
		p.Methods = append(p.Methods, Method{Label: "Corepack (comes with Node.js)", Steps: [][]string{{"corepack", "enable"}}})
	}
	switch pm := Manager(); pm {
	case "brew":
		if pkg, ok := brewPkgs[tool]; ok {
			p.Methods = append(p.Methods, Method{Label: "Homebrew", Steps: [][]string{brewArgs(pkg)}, Note: brewNotes[tool]})
		}
	case "winget":
		if id, ok := wingetIDs[tool]; ok {
			p.Methods = append(p.Methods, Method{Label: "winget", Steps: [][]string{wingetArgs(id)}, Note: wingetNotes[tool]})
		}
	case "apt-get", "dnf", "pacman":
		if pkgs, ok := linuxPkgs[pm][tool]; ok {
			p.Methods = append(p.Methods, linuxMethod(pm, strings.Fields(pkgs), linuxNotes[tool]))
		}
	case "":
		if pkg, ok := brewPkgs[tool]; ok && runtime.GOOS == "darwin" {
			p.Methods = append(p.Methods, Method{
				Label: "Homebrew (installs Homebrew first, the standard package manager for macOS)",
				Steps: [][]string{{homebrewInstall}, brewArgs(pkg)},
				Sudo:  true,
				Note:  brewNotes[tool],
			})
		}
	}
	return p
}

// ForService returns ways to install and start a database on this computer.
func ForService(kind string) Plan {
	p := Plan{Tool: kind, URL: pages[kind]}
	switch Manager() {
	case "brew":
		if f, ok := brewServices[kind]; ok {
			var steps [][]string
			if kind == "mongodb" {
				steps = append(steps, []string{"brew", "tap", "mongodb/brew"})
			}
			steps = append(steps, []string{"brew", "install", f}, []string{"brew", "services", "start", f})
			p.Methods = append(p.Methods, Method{Label: "Homebrew", Steps: steps})
		}
	case "apt-get":
		if pkg, ok := map[string]string{"postgres": "postgresql", "mysql": "mysql-server", "mariadb": "mariadb-server", "redis": "redis-server"}[kind]; ok {
			p.Methods = append(p.Methods, linuxMethod("apt-get", []string{pkg}, "It starts automatically after installing."))
		}
	case "dnf":
		switch kind {
		case "postgres":
			p.Methods = append(p.Methods, Method{Label: "dnf", Sudo: true, Steps: [][]string{
				{"sudo", "dnf", "install", "-y", "postgresql-server"},
				{"sudo", "postgresql-setup", "--initdb"},
				{"sudo", "systemctl", "enable", "--now", "postgresql"},
			}})
		case "mysql", "mariadb":
			p.Methods = append(p.Methods, Method{Label: "dnf", Sudo: true, Steps: [][]string{
				{"sudo", "dnf", "install", "-y", "mariadb-server"},
				{"sudo", "systemctl", "enable", "--now", "mariadb"},
			}})
		}
	}
	return p
}

func brewArgs(pkg string) []string {
	return append([]string{"brew", "install"}, strings.Fields(pkg)...)
}

func wingetArgs(id string) []string {
	return []string{"winget", "install", "-e", "--id", id, "--accept-package-agreements", "--accept-source-agreements"}
}

func linuxMethod(pm string, pkgs []string, note string) Method {
	switch pm {
	case "apt-get":
		return Method{Label: "apt", Sudo: true, Note: note, Steps: [][]string{
			{"sudo", "apt-get", "update"},
			append([]string{"sudo", "apt-get", "install", "-y"}, pkgs...),
		}}
	case "dnf":
		return Method{Label: "dnf", Sudo: true, Note: note, Steps: [][]string{append([]string{"sudo", "dnf", "install", "-y"}, pkgs...)}}
	}
	return Method{Label: "pacman", Sudo: true, Note: note, Steps: [][]string{append([]string{"sudo", "pacman", "-S", "--needed", "--noconfirm"}, pkgs...)}}
}

var pages = map[string]string{
	"node": "https://nodejs.org/en/download", "npm": "https://nodejs.org/en/download",
	"pnpm": "https://pnpm.io/installation", "yarn": "https://yarnpkg.com/getting-started/install", "bun": "https://bun.sh",
	"php": "https://www.php.net/downloads.php", "composer": "https://getcomposer.org/download/",
	"python": "https://www.python.org/downloads/", "python3": "https://www.python.org/downloads/",
	"uv": "https://docs.astral.sh/uv/getting-started/installation/", "poetry": "https://python-poetry.org/docs/#installation",
	"ruby": "https://www.ruby-lang.org/en/documentation/installation/", "bundle": "https://bundler.io",
	"go": "https://go.dev/dl/", "cargo": "https://rustup.rs", "rustc": "https://rustup.rs", "java": "https://adoptium.net",
	"mvn": "https://maven.apache.org/install.html", "gradle": "https://gradle.org/install/",
	"dotnet": "https://dotnet.microsoft.com/download", "flutter": "https://docs.flutter.dev/get-started/install",
	"docker": "https://docs.docker.com/get-docker/", "git": "https://git-scm.com/downloads",
	"postgres": "https://www.postgresql.org/download/", "mysql": "https://dev.mysql.com/downloads/mysql/",
	"mariadb": "https://mariadb.org/download/", "redis": "https://redis.io/downloads/",
	"mongodb": "https://www.mongodb.com/try/download/community",
}

var brewPkgs = map[string]string{
	"node": "node", "npm": "node", "pnpm": "pnpm", "yarn": "yarn", "bun": "oven-sh/bun/bun",
	"php": "php", "composer": "composer", "python3": "python", "uv": "uv", "poetry": "poetry",
	"ruby": "ruby", "bundle": "ruby", "go": "go", "cargo": "rust", "rustc": "rust",
	"java": "--cask temurin", "mvn": "maven", "gradle": "gradle", "dotnet": "--cask dotnet-sdk",
	"flutter": "--cask flutter", "docker": "--cask docker", "git": "git",
}

var brewNotes = map[string]string{
	"ruby":   "Homebrew's Ruby isn't put on your PATH automatically; add /opt/homebrew/opt/ruby/bin to PATH in ~/.zshrc.",
	"docker": "Open Docker from Applications once after installing to finish its setup.",
}

var brewServices = map[string]string{
	"postgres": "postgresql@16", "mysql": "mysql", "mariadb": "mariadb", "redis": "redis", "mongodb": "mongodb-community",
}

var wingetIDs = map[string]string{
	"node": "OpenJS.NodeJS.LTS", "npm": "OpenJS.NodeJS.LTS", "python": "Python.Python.3.12",
	"php": "PHP.PHP.8.3", "go": "GoLang.Go", "java": "EclipseAdoptium.Temurin.21.JDK",
	"dotnet": "Microsoft.DotNet.SDK.8", "docker": "Docker.DockerDesktop",
	"cargo": "Rustlang.Rustup", "rustc": "Rustlang.Rustup", "git": "Git.Git",
}

var wingetNotes = map[string]string{
	"php":    "After installing, copy php.ini-development to php.ini in PHP's folder and enable the extensions your project needs.",
	"cargo":  "Rust on Windows also needs the Visual Studio C++ build tools; rustup tells you if they're missing.",
	"docker": "Docker Desktop needs WSL 2 and may ask you to sign out or restart the first time.",
}

var linuxPkgs = map[string]map[string]string{
	"apt-get": {
		"node": "nodejs npm", "npm": "nodejs npm", "composer": "composer", "git": "git",
		"php":     "php-cli php-mbstring php-xml php-curl php-zip php-sqlite3 php-mysql php-pgsql",
		"python3": "python3 python3-venv python3-pip", "ruby": "ruby-full", "bundle": "ruby-bundler",
		"go": "golang-go", "cargo": "cargo", "rustc": "cargo", "java": "default-jdk", "mvn": "maven",
		"gradle": "gradle", "dotnet": "dotnet-sdk-8.0", "docker": "docker.io",
	},
	"dnf": {
		"node": "nodejs npm", "npm": "nodejs npm", "composer": "composer", "git": "git",
		"php":     "php-cli php-mbstring php-xml php-pdo php-mysqlnd php-pgsql",
		"python3": "python3 python3-pip", "ruby": "ruby", "go": "golang", "cargo": "cargo", "rustc": "cargo",
		"java": "java-21-openjdk-devel", "mvn": "maven", "dotnet": "dotnet-sdk-8.0",
	},
	"pacman": {
		"node": "nodejs npm", "npm": "nodejs npm", "php": "php", "composer": "composer", "git": "git",
		"python3": "python python-pip", "ruby": "ruby", "go": "go", "cargo": "rust", "rustc": "rust",
		"java": "jdk-openjdk", "mvn": "maven", "gradle": "gradle", "dotnet": "dotnet-sdk", "docker": "docker",
	},
}

var linuxNotes = map[string]string{
	"node":   "Linux distributions often ship an older Node.js. If this project needs a newer one, use the download page or nvm instead.",
	"npm":    "Linux distributions often ship an older Node.js. If this project needs a newer one, use the download page or nvm instead.",
	"docker": "To use Docker without sudo afterwards: sudo usermod -aG docker $USER, then log out and back in.",
}
