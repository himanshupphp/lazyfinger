package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/himanshupphp/lazyfinger/internal/config"
	"github.com/himanshupphp/lazyfinger/internal/console"
	"github.com/himanshupphp/lazyfinger/internal/heal"
	"github.com/himanshupphp/lazyfinger/internal/integrate"
	"github.com/himanshupphp/lazyfinger/internal/journal"
	"github.com/himanshupphp/lazyfinger/internal/migrate"

	"github.com/himanshupphp/lazyfinger/internal/provider"
	"github.com/himanshupphp/lazyfinger/internal/runapp"
	"github.com/himanshupphp/lazyfinger/internal/setup"
	"github.com/himanshupphp/lazyfinger/internal/snapshot"
	"github.com/himanshupphp/lazyfinger/internal/ui"
)

// version is set at release time with -ldflags "-X main.version=...".
var version = "0.1.0"

func main() { os.Exit(run()) }

type common struct {
	auto, review bool
	provider     string
	maxAttempts  int
}

func run() int {
	console.Init()

	fs := flag.NewFlagSet("lazy", flag.ContinueOnError)
	var c common
	fs.BoolVar(&c.auto, "auto", false, "apply fixes without asking (risky actions still ask)")
	fs.BoolVar(&c.review, "review", false, "ask before applying every fix")
	fs.StringVar(&c.provider, "provider", "", "provider to start with (a name from your config)")
	fs.IntVar(&c.maxAttempts, "max-attempts", 0, "maximum fix attempts per run")
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: lazy [flags] <command...>   run a command and fix it if it fails
       lazy run [folder]           install, set up, start, and watch a project
       lazy setup                  visual configurator
       lazy integrate [--remove]   add "Run with LazyFinger" to the right-click menu
       lazy history [--all]        what LazyFinger tried in this project
       lazy undo                   revert the latest fix
       lazy config

flags:`)
		fs.PrintDefaults()
	}
	// Flag parsing stops at the first non-flag, so `lazy npm run build --prod` works.
	if err := fs.Parse(os.Args[1:]); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Println("lazy", version)
		return 0
	}
	args := fs.Args()
	if len(args) == 0 {
		fs.Usage()
		return 2
	}

	root, err := os.Getwd()
	if err != nil {
		ui.Error(err.Error())
		return 1
	}
	migrate.Run(root)

	switch args[0] {
	case "setup":
		return setup.Run()
	case "integrate":
		return integrate.Run(args[1:])
	case "undo":
		summary, err := snapshot.Undo(root)
		if err != nil {
			ui.Error(err.Error())
			return 1
		}
		ui.Success("Reverted: " + summary)
		return 0
	case "config":
		return configCmd()
	case "history":
		return historyCmd(root, len(args) > 1 && args[1] == "--all")
	case "run":
		return runCmd(root, args[1:], c)
	}

	cfg, mode, chain, autoFallback, code := prepare(root, c)
	if code != 0 {
		return code
	}
	return heal.Run(context.Background(), heal.Options{
		Root:         root,
		Args:         args,
		Mode:         mode,
		MaxAttempts:  cfg.MaxAttempts,
		Provider:     chain,
		AutoFallback: autoFallback,
	})
}

func runCmd(root string, rest []string, c common) int {
	fs := flag.NewFlagSet("lazy run", flag.ContinueOnError)
	noOpen := fs.Bool("no-open", false, "don't open the browser when the app is ready")
	yes := fs.Bool("yes", false, "accept the plan without asking (steps that change data still ask)")
	pause := fs.Bool("pause", false, "wait for Enter before closing (used by the right-click menu)")
	fs.BoolVar(&c.auto, "auto", c.auto, "apply fixes without asking (risky actions still ask)")
	fs.StringVar(&c.provider, "provider", c.provider, "provider to start with")
	if err := fs.Parse(rest); err != nil {
		return 2
	}

	code := func() int {
		if fs.NArg() > 0 {
			dir, err := filepath.Abs(fs.Arg(0))
			if err != nil {
				ui.Error(err.Error())
				return 1
			}
			if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
				ui.Error(dir + " isn't a folder.")
				return 1
			}
			root = dir
			migrate.Run(root)
		}
		cfg, mode, chain, autoFallback, code := prepare(root, c)
		if code != 0 {
			return code
		}
		return runapp.Run(context.Background(), runapp.Options{
			Root:         root,
			Mode:         mode,
			MaxAttempts:  cfg.MaxAttempts,
			Provider:     chain,
			AutoFallback: autoFallback,
			OpenBrowser:  !*noOpen,
			Yes:          *yes,
		})
	}()

	if *pause {
		ui.Ask("Press Enter to close this window")
	}
	return code
}

// prepare loads the config, resolves the mode, and builds the provider chain.
func prepare(root string, c common) (config.Config, config.Mode, *provider.Chain, bool, int) {
	if !config.Exists() {
		ui.Muted("Tip: run `lazy setup` to choose your model in a visual configurator.")
	}
	cfg, err := config.Load(root)
	if err != nil {
		ui.Error("config: " + err.Error())
		return cfg, "", nil, false, 1
	}
	mode := cfg.Mode
	if c.auto {
		mode = config.Autofix
	}
	if c.review {
		mode = config.Analyze
	}
	if mode != config.Analyze && mode != config.Autofix {
		ui.Error(fmt.Sprintf("unknown mode %q (use \"analyze\" or \"autofix\")", mode))
		return cfg, mode, nil, false, 1
	}
	if c.maxAttempts > 0 {
		cfg.MaxAttempts = c.maxAttempts
	}
	primary := cfg.Default
	if c.provider != "" {
		primary = c.provider
	}
	if _, ok := cfg.Providers[primary]; !ok {
		ui.Error(fmt.Sprintf("no provider named %q in your config (run `lazy setup`)", primary))
		return cfg, mode, nil, false, 1
	}
	chain, autoFallback, err := buildChain(cfg, primary)
	if err != nil {
		ui.Error(err.Error())
		return cfg, mode, nil, false, 1
	}
	if skipped := chain.SkipCooling(); len(skipped) > 0 {
		ui.Muted("Paused: " + strings.Join(skipped, ", ") + ". Starting with " + chain.Name() + ".")
	}
	return cfg, mode, chain, autoFallback, 0
}

// buildChain orders providers: the chosen one first, then the configured
// fallbacks (switched to automatically), or else every other provider
// (switched to only after asking).
func buildChain(cfg config.Config, primary string) (*provider.Chain, bool, error) {
	auto := len(cfg.Fallback) > 0
	var rest []string
	if auto {
		rest = cfg.Fallback
	} else {
		for n := range cfg.Providers {
			rest = append(rest, n)
		}
		sort.Strings(rest)
	}
	order := []string{primary}
	for _, n := range rest {
		if _, ok := cfg.Providers[n]; ok && !slices.Contains(order, n) {
			order = append(order, n)
		}
	}
	var ps []provider.Provider
	for i, n := range order {
		p, err := provider.New(n, cfg.Providers[n])
		if err != nil {
			if i == 0 {
				return nil, auto, err
			}
			continue // a fallback without a key just isn't used
		}
		ps = append(ps, p)
	}
	return provider.NewChain(ps, provider.LoadState()), auto, nil
}

func historyCmd(root string, all bool) int {
	n := 5
	if all {
		n = 1 << 30
	}
	tree := journal.Load(root).Tree(n)
	if tree == "" {
		ui.Muted("No history yet in this project. It starts the first time LazyFinger fixes something here.")
	} else {
		fmt.Print(tree)
	}
	for name, cd := range provider.LoadState().Active() {
		ui.Muted(fmt.Sprintf("%s is paused until %s (%s).", name, cd.Until.Local().Format("15:04"), cd.Reason))
	}
	return 0
}

func configCmd() int {
	path, err := config.Path()
	if err != nil {
		ui.Error(err.Error())
		return 1
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			ui.Error(err.Error())
			return 1
		}
		if err := os.WriteFile(path, []byte(config.Example), 0o600); err != nil {
			ui.Error(err.Error())
			return 1
		}
		ui.Success("Created " + path)
		ui.Muted("Run `lazy setup` to edit it visually.")
		return 0
	}
	ui.Muted("Config file: " + path)
	ui.Muted("Run `lazy setup` to edit it visually.")
	return 0
}
