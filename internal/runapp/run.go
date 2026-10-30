// Package runapp implements `lazy run`: work out how to run a project, check
// what it needs, fill in its environment, then install, set up, start, and
// watch it, healing failures along the way.
package runapp

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/himanshupphp/lazyfinger/internal/browser"
	"github.com/himanshupphp/lazyfinger/internal/config"
	"github.com/himanshupphp/lazyfinger/internal/envfile"
	"github.com/himanshupphp/lazyfinger/internal/heal"
	"github.com/himanshupphp/lazyfinger/internal/install"
	"github.com/himanshupphp/lazyfinger/internal/project"
	"github.com/himanshupphp/lazyfinger/internal/provider"
	"github.com/himanshupphp/lazyfinger/internal/runner"
	"github.com/himanshupphp/lazyfinger/internal/services"
	"github.com/himanshupphp/lazyfinger/internal/ui"
)

type Options struct {
	Root         string
	Mode         config.Mode
	MaxAttempts  int
	Provider     provider.Provider
	AutoFallback bool
	OpenBrowser  bool
	Yes          bool // accept the plan without asking (steps that change data still ask)
}

func Run(ctx context.Context, o Options) int {
	if ch, ok := o.Provider.(*provider.Chain); ok {
		ch.OnSwitch = func(from, to provider.Provider, reason string) bool {
			ui.Warn(fmt.Sprintf("%s %s.", from.Name(), reason))
			if !o.AutoFallback && !ui.Confirm("Continue with "+to.Name()+"?") {
				return false
			}
			return !to.Remote() || heal.ConfirmRemote(to)
		}
	}

	p := project.Detect(o.Root)
	if p == nil {
		ui.Muted("LazyFinger doesn't recognize this kind of project yet, so it will ask " + o.Provider.Name() + " to read it.")
		if o.Provider.Remote() && !heal.ConfirmRemote(o.Provider) {
			return 1
		}
		stop := ui.Spinner("Reading the project...")
		var err error
		p, err = project.Suggest(ctx, o.Provider, o.Root)
		stop()
		if err != nil {
			ui.Error("Couldn't work out how to run this project: " + err.Error())
			return 1
		}
	}

	statuses := project.Check(p.Needs)
	envPlan, err := envfile.Inspect(o.Root, p.EnvSkip)
	if err != nil {
		ui.Warn("Couldn't read the example environment file: " + err.Error())
	}
	showPlan(p, statuses, envPlan, services.Detect(o.Root))
	if !o.Yes && !ui.Confirm("Run this plan?") {
		ui.Muted("Nothing was run.")
		return 0
	}
	if !ensureTools(statuses) {
		return 1
	}
	if envPlan != nil {
		if err := fillEnv(o.Root, envPlan); err != nil {
			ui.Error("Couldn't write .env: " + err.Error())
			return 1
		}
	}
	// Re-read now that .env is complete, so the real host, port and credentials are used.
	if !ensureServices(o.Root, services.Detect(o.Root)) {
		return 1
	}

	hopts := heal.Options{Root: o.Root, Mode: o.Mode, MaxAttempts: o.MaxAttempts, Provider: o.Provider, AutoFallback: o.AutoFallback}
	for _, s := range p.Steps {
		if s.Kind == project.Start {
			continue
		}
		if s.Skip != nil && s.Skip(o.Root) {
			ui.Muted("Skipping: " + s.Title + " (already done).")
			continue
		}
		if s.Ask && !ui.Confirm(fmt.Sprintf("Run %q to %s?", s.Command(), s.Title)) {
			ui.Muted("Skipped: " + s.Title + ".")
			continue
		}
		ui.Header(sentence(s.Title) + " · " + s.Command())
		opts := hopts
		opts.Args = s.Args
		if code := heal.Run(ctx, opts); code != 0 {
			ui.Error(fmt.Sprintf("Couldn't %s. Fix the problem above, then run `lazy run` again.", s.Title))
			return code
		}
	}

	start := p.StartStep()
	if start == nil {
		ui.Success("Done. This project has no start command.")
		return 0
	}
	return serve(ctx, o, hopts, *start, strings.HasPrefix(p.Name, "Laravel"))
}

func showPlan(p *project.Project, statuses []project.Status, env *envfile.Plan, svcs []services.Service) {
	var rows []string
	if env != nil {
		switch {
		case !env.EnvExists:
			rows = append(rows, fmt.Sprintf("create .env from %s%s", env.Example, askNote(env)))
		case len(env.Copy) > 0 || len(env.Ask) > 0:
			rows = append(rows, fmt.Sprintf("complete .env (%d missing%s)", len(env.Copy), askNote(env)))
		}
	}
	for _, s := range svcs {
		rows = append(rows, fmt.Sprintf("make sure %s is running at %s (from %s)", s.Label(), s.Addr(), s.From))
	}
	for _, s := range p.Steps {
		row := fmt.Sprintf("%s  — %s", s.Command(), s.Title)
		if s.Ask {
			row += " (asks first)"
		}
		rows = append(rows, row)
	}
	ui.Plan(p.Name+" project", rows)
	for _, s := range statuses {
		switch {
		case !s.Found:
			ui.Check(false, s.Label+" isn't installed")
		case !s.OK:
			ui.Check(false, fmt.Sprintf("%s %s is installed, but this project asks for %s (%s)", s.Label, s.Have, s.Constraint, s.From))
		default:
			ui.Check(true, strings.TrimSpace(s.Label+" "+s.Have))
		}
	}
	ui.Muted("LazyFinger will run these commands on your computer. Only continue if you trust where this project came from.")
}

func askNote(env *envfile.Plan) string {
	if len(env.Ask) == 0 {
		return ""
	}
	return fmt.Sprintf(", %d value(s) for you to fill in", len(env.Ask))
}

// ---------------------------------------------------------------- tools

func ensureTools(statuses []project.Status) bool {
	for _, s := range statuses {
		switch {
		case s.Found && s.OK:
			continue
		case s.Found:
			ui.Warn(fmt.Sprintf("%s %s may not work with this project (it asks for %s).", s.Label, s.Have, s.Constraint))
			if hint := project.VersionHint(s.Tool); hint != "" {
				ui.Muted(hint)
			}
			if !ui.Confirm("Continue anyway?") {
				return false
			}
		default:
			if !installTool(s) {
				return false
			}
		}
	}
	return true
}

func installTool(s project.Status) bool {
	ui.Warn(s.Label + " is needed but isn't installed.")
	ok := installLoop(s.Label, install.For(s.Tool), func() bool {
		project.RefreshPath()
		_, err := exec.LookPath(s.Tool)
		return err == nil
	})
	if !ok {
		return false
	}
	if st := project.Check([]project.Need{s.Need}); len(st) == 1 && st[0].Found && !st[0].OK {
		ui.Warn(fmt.Sprintf("The installed %s is %s, but this project asks for %s.", s.Label, st[0].Have, s.Constraint))
		if hint := project.VersionHint(s.Tool); hint != "" {
			ui.Muted(hint)
		}
		return ui.Confirm("Continue anyway?")
	}
	return true
}

// installLoop offers the ways to install something, runs the chosen one after
// confirmation, and checks with ready() until it works or the user stops.
func installLoop(label string, plan install.Plan, ready func() bool) bool {
	for {
		var opts []ui.Option
		for i, m := range plan.Methods {
			if i < 9 {
				opts = append(opts, ui.Option{Key: rune('1' + i), Label: "install with " + m.Label})
			}
		}
		if plan.URL != "" {
			opts = append(opts, ui.Option{Key: 'o', Label: "open the download page"})
		}
		opts = append(opts, ui.Option{Key: 'r', Label: "check again"}, ui.Option{Key: 's', Label: "stop"})

		c := ui.Choose("How do you want to get "+label+"?", opts...)
		switch {
		case c == 's':
			return false
		case c == 'o':
			_ = browser.Open(plan.URL)
			ui.Muted("Install it from the page that opened, then choose \"check again\".")
			continue
		case c >= '1' && c <= '9':
			if !runMethod(plan.Methods[c-'1']) {
				continue
			}
		}
		if ready() {
			ui.Success(label + " is ready.")
			return true
		}
		ui.Warn(label + " still isn't found. If you just installed it, you may need to open a new terminal.")
	}
}

func runMethod(m install.Method) bool {
	ui.Muted("This will run:")
	for _, c := range m.Commands() {
		ui.Command(c)
	}
	if m.Sudo {
		ui.Muted("You'll be asked for your computer password.")
	}
	if m.Note != "" {
		ui.Muted(m.Note)
	}
	if !ui.Confirm("Run these commands?") {
		return false
	}
	for _, step := range m.Steps {
		ui.Header("Running · " + strings.Join(step, " "))
		r, err := runner.Run("", step)
		project.RefreshPath() // e.g. Homebrew was just installed
		if err != nil || r.Failed() {
			ui.Error("That command didn't finish. You can try another option or the download page.")
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- services

func ensureServices(root string, svcs []services.Service) bool {
	if len(svcs) == 0 {
		return true
	}
	composeNames := services.ComposeNames(root)
	for _, s := range svcs {
		if s.Reachable() {
			ui.Check(true, fmt.Sprintf("%s is running at %s", s.Label(), s.Addr()))
			continue
		}
		if !s.Local() {
			if composeNames[s.Host] {
				ui.Warn(fmt.Sprintf("%s's host is %q, a Docker Compose service, so this project is set up to run inside Docker (for example Laravel Sail).", s.Label(), s.Host))
				ui.Muted("Run it with `./vendor/bin/sail up` or `docker compose up` instead, or set the host to 127.0.0.1 in .env to use a database on this computer.")
			} else {
				ui.Warn(fmt.Sprintf("%s at %s isn't reachable. It isn't on this computer, so check the address and your network.", s.Label(), s.Addr()))
			}
			if !ui.Confirm("Continue anyway?") {
				return false
			}
			continue
		}
		if !startService(root, s) && !ui.Confirm(fmt.Sprintf("Continue without %s? The app may fail to start.", s.Label())) {
			return false
		}
	}
	return true
}

func startService(root string, s services.Service) bool {
	ui.Warn(fmt.Sprintf("%s isn't running at %s (needed by %s).", s.Label(), s.Addr(), s.From))
	native := install.ForService(s.Kind)
	for {
		var opts []ui.Option
		if s.Compose != "" {
			opts = append(opts, ui.Option{Key: 'c', Label: "start the project's own \"" + s.Compose + "\" service with Docker Compose"})
		}
		opts = append(opts, ui.Option{Key: 'd', Label: "run it in Docker using your .env settings (recommended)"})
		if len(native.Methods) > 0 {
			opts = append(opts, ui.Option{Key: 'i', Label: "install it on this computer with " + native.Methods[0].Label})
		}
		if native.URL != "" {
			opts = append(opts, ui.Option{Key: 'o', Label: "open the download page"})
		}
		opts = append(opts, ui.Option{Key: 'r', Label: "check again"}, ui.Option{Key: 's', Label: "skip"})

		ok := false
		switch ui.Choose("How do you want to start "+s.Label()+"?", opts...) {
		case 'c':
			ok = ensureDocker() && runConfirmed(root, []string{"docker", "compose", "up", "-d", s.Compose}, "")
		case 'd':
			ok = ensureDocker() && startInDocker(root, s)
		case 'i':
			if ok = runMethod(native.Methods[0]); ok {
				ui.Muted("Native installs come with their own default users. If the app can't log in, set the database user and password in .env to match, or use Docker, which follows your .env.")
			}
		case 'o':
			_ = browser.Open(native.URL)
			continue
		case 'r':
			ok = true
		case 's':
			return false
		}
		if !ok {
			continue
		}
		stop := ui.Spinner(fmt.Sprintf("Waiting for %s at %s...", s.Label(), s.Addr()))
		ready := services.WaitReachable(s, 90*time.Second)
		stop()
		if ready {
			ui.Success(s.Label() + " is running.")
			return true
		}
		ui.Warn(s.Label() + " still isn't answering at " + s.Addr() + ".")
	}
}

func ensureDocker() bool {
	for {
		switch services.DockerStatus() {
		case "ready":
			return true
		case "missing":
			ui.Warn("Docker isn't installed.")
			if !installLoop("Docker", install.For("docker"), func() bool {
				project.RefreshPath()
				return services.DockerStatus() != "missing"
			}) {
				return false
			}
		case "permission":
			ui.Warn("Docker is installed, but your user can't use it yet.")
			ui.Command("sudo usermod -aG docker $USER")
			ui.Muted("Run that, log out and back in, then run `lazy run` again.")
			return false
		case "stopped":
			ui.Warn("Docker is installed but not running.")
			if !ui.Confirm("Start Docker now?") {
				return false
			}
			args, background := services.StartDockerCommand()
			if background {
				if err := exec.Command(args[0], args[1:]...).Start(); err != nil {
					ui.Error("Couldn't start Docker: " + err.Error() + ". Open Docker Desktop yourself, then try again.")
					return false
				}
			} else if r, err := runner.Run("", args); err != nil || r.Failed() {
				ui.Error("Couldn't start Docker.")
				return false
			}
			stop := ui.Spinner("Waiting for Docker to start (this can take a minute)...")
			ok := services.WaitDocker(150 * time.Second)
			stop()
			if !ok {
				ui.Warn("Docker didn't start in time. Open Docker Desktop yourself, then choose \"check again\".")
				return false
			}
		}
	}
}

func startInDocker(root string, s services.Service) bool {
	name := s.ContainerName(root)
	if services.ContainerExists(name) {
		ui.Muted("A container from an earlier run exists; starting it again.")
		return runConfirmed(root, []string{"docker", "start", name}, "")
	}
	args, err := s.DockerArgs(root)
	if err != nil {
		ui.Warn(err.Error())
		return false
	}
	if strings.EqualFold(s.Host, "localhost") && (s.Kind == "mysql" || s.Kind == "mariadb") {
		ui.Muted("Note: with the host set to localhost, PHP connects through a socket file instead of the network. Set DB_HOST=127.0.0.1 in .env so it can reach the container.")
	}
	ui.Muted(fmt.Sprintf("This creates a %s container that only this computer can reach, with its data kept in a Docker volume.", s.Label()))
	return runConfirmed(root, args, s.Password)
}

// runConfirmed shows a command (with any secret masked), asks, then runs it.
func runConfirmed(dir string, args []string, secret string) bool {
	shown := strings.Join(args, " ")
	if secret != "" {
		shown = strings.ReplaceAll(shown, secret, "••••••")
	}
	ui.Command(shown)
	if !ui.Confirm("Run this?") {
		return false
	}
	r, err := runner.Run(dir, args)
	if err != nil || r.Failed() {
		ui.Error("That didn't work; see the output above.")
		return false
	}
	return true
}

// ---------------------------------------------------------------- .env

func fillEnv(root string, plan *envfile.Plan) error {
	filled := map[string]string{}
	if len(plan.Ask) > 0 {
		ui.Header(fmt.Sprintf(".env · %d value(s) to fill in", len(plan.Ask)))
		ui.Muted("Values stay on this computer, in .env. Press Enter to leave one empty; you can edit .env later.")
		if ui.Confirm("Fill them in now?") {
			for _, v := range plan.Ask {
				if v.Comment != "" {
					ui.Muted(v.Comment)
				}
				if v.Secret {
					filled[v.Key] = ui.AskSecret(v.Key)
				} else {
					filled[v.Key] = ui.AskDefault(v.Key, v.Default)
				}
			}
		}
	}
	if plan.EnvExists && len(plan.Copy) == 0 && len(filled) == 0 {
		return nil
	}
	if err := envfile.Write(root, plan, filled); err != nil {
		return err
	}
	ui.Success(".env is ready.")
	if envfile.NeedsIgnore(root) && ui.Confirm(".env isn't in .gitignore, so your secrets could be committed. Add it?") {
		if err := envfile.AddIgnore(root); err == nil {
			ui.Success("Added .env to .gitignore.")
		}
	}
	return nil
}

// ---------------------------------------------------------------- serving

// serve starts the app and watches it. When it crashes, or errors were seen
// while it ran, LazyFinger offers a fix and restarts it.
func serve(ctx context.Context, o Options, hopts heal.Options, s project.Step, laravel bool) int {
	args := s.Args
	opened := false
	for restarts := 0; restarts <= 5; restarts++ {
		ui.Header("Starting · " + strings.Join(args, " "))
		ui.Muted("Press Ctrl-C to stop.")

		w := newWatcher()
		w.onError = func(h string) {
			ui.Notice(false, "Error noticed: "+h+" · Press Ctrl-C to stop the app and LazyFinger will offer a fix.")
		}
		stopCh := make(chan struct{})
		go func() {
			select {
			case u := <-w.ready:
				if waitReady(u, stopCh) {
					msg := "Ready → " + u
					if o.OpenBrowser && !opened {
						if browser.Open(u) == nil {
							msg += "  (opened in your browser)"
							opened = true
						}
					}
					ui.Notice(true, msg)
				}
			case <-stopCh:
			}
		}()
		if laravel {
			go tailLaravelLog(o.Root, w, stopCh)
		}

		res, err := runner.RunWith(o.Root, args, w)
		close(stopCh)
		w.stop()
		if err != nil {
			ui.Error("Couldn't start the app: " + err.Error())
			return 127
		}

		if w.portLine != "" && res.Failed() {
			next, ok := otherPort(w.portLine, s)
			if !ok {
				return res.ExitCode
			}
			args = next
			continue
		}

		errs := w.errorList()
		var code int
		switch {
		case res.Interrupted && len(errs) == 0:
			ui.Muted("Stopped.")
			return 0
		case res.Interrupted:
			if !ui.Confirm(fmt.Sprintf("LazyFinger noticed %d error(s) while the app was running. Look at them now?", len(errs))) {
				return 0
			}
			out := "Errors seen while the app was running:\n\n" + strings.Join(errs, "\n\n---\n\n")
			code = fixWith(ctx, hopts, args, out, 1, "Errors while the app was running")
		case !res.Failed():
			ui.Muted("The app exited.")
			return 0
		default:
			code = fixWith(ctx, hopts, args, string(res.Output), res.ExitCode, fmt.Sprintf("The app stopped (exit %d)", res.ExitCode))
		}
		if code != 0 {
			return code
		}
		ui.Muted("Restarting with the fix...")
	}
	ui.Warn("Restarted several times without success; stopping here. Run `lazy history` to see what was tried.")
	return 1
}

func fixWith(ctx context.Context, hopts heal.Options, args []string, output string, exit int, title string) int {
	opts := hopts
	opts.Args = args
	opts.NoRerun = true // the caller restarts the app with its watcher
	opts.Headline = title
	return heal.Fix(ctx, opts, &runner.Result{ExitCode: exit, Output: []byte(output)})
}

var numRe = regexp.MustCompile(`\d{2,5}`)

// otherPort explains a "port already in use" error and offers the next free port.
func otherPort(line string, s project.Step) ([]string, bool) {
	port := 0
	for _, m := range numRe.FindAllString(line, -1) {
		if n, _ := strconv.Atoi(m); n >= 80 && n <= 65535 {
			port = n
		}
	}
	msg := "The port is already in use"
	if port > 0 {
		msg = fmt.Sprintf("Port %d is already in use", port)
	}
	ui.Warn(msg + ".")
	if holder := portHolder(port); holder != "" {
		ui.Muted("It's being used by " + holder + ".")
	}
	if s.PortArgs == nil || port == 0 {
		ui.Muted("Stop the other app, then run `lazy run` again.")
		return nil, false
	}
	free := freePort(port + 1)
	if free == 0 || !ui.Confirm(fmt.Sprintf("Start on port %d instead?", free)) {
		return nil, false
	}
	return s.PortArgs(free), true
}

func freePort(from int) int {
	for p := from; p < from+50 && p <= 65535; p++ {
		if l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p)); err == nil {
			l.Close()
			return p
		}
	}
	return 0
}

func portHolder(port int) string {
	if port == 0 {
		return ""
	}
	if runtime.GOOS == "windows" {
		out, err := exec.Command("netstat", "-ano", "-p", "tcp").Output()
		if err != nil {
			return ""
		}
		for _, l := range strings.Split(string(out), "\n") {
			f := strings.Fields(l)
			if len(f) >= 5 && strings.HasSuffix(f[1], ":"+strconv.Itoa(port)) && strings.EqualFold(f[3], "LISTENING") {
				name := "PID " + f[4]
				if o, err := exec.Command("tasklist", "/FI", "PID eq "+f[4], "/FO", "CSV", "/NH").Output(); err == nil {
					if parts := strings.Split(strings.TrimSpace(string(o)), ","); len(parts) > 0 && parts[0] != "" {
						name = strings.Trim(parts[0], `"`) + " (PID " + f[4] + ")"
					}
				}
				return name
			}
		}
		return ""
	}
	out, err := exec.Command("lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN").Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return ""
	}
	f := strings.Fields(lines[1])
	if len(f) < 2 {
		return ""
	}
	return f[0] + " (PID " + f[1] + ")"
}

func sentence(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
