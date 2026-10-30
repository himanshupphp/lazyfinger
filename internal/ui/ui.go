package ui

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"

	"github.com/himanshupphp/lazyfinger/internal/console"
)

var (
	purple = lipgloss.Color("#8B7CF6")
	green  = lipgloss.Color("#3FB950")
	red    = lipgloss.Color("#F85149")
	yellow = lipgloss.Color("#D29922")
	gray   = lipgloss.Color("#8B949E")

	badge = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Background(purple).Padding(0, 1)
	bold  = lipgloss.NewStyle().Bold(true)
	muted = lipgloss.NewStyle().Foreground(gray)
	keyS  = lipgloss.NewStyle().Foreground(purple).Bold(true)
	okS   = lipgloss.NewStyle().Foreground(green).Bold(true)
	badS  = lipgloss.NewStyle().Foreground(red).Bold(true)
	warnS = lipgloss.NewStyle().Foreground(yellow).Bold(true)
	addS  = lipgloss.NewStyle().Foreground(green)
	delS  = lipgloss.NewStyle().Foreground(red)
	panel = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(purple).Padding(0, 1)

	in = bufio.NewReader(console.Stdin)

	spinMu   sync.Mutex
	spinStop func()
)

// haltSpinner stops any running spinner so other output doesn't collide with it.
func haltSpinner() {
	spinMu.Lock()
	stop := spinStop
	spinStop = nil
	spinMu.Unlock()
	if stop != nil {
		stop()
	}
}

func width() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 20 {
		return w
	}
	return 80
}

func Header(s string)  { haltSpinner(); fmt.Println("\n" + badge.Render("lazy") + " " + bold.Render(s)) }
func Success(s string) { haltSpinner(); fmt.Println(okS.Render("✓ ") + s) }
func Error(s string)   { haltSpinner(); fmt.Println(badS.Render("✗ ") + s) }
func Warn(s string)    { haltSpinner(); fmt.Println(warnS.Render("! ") + s) }
func Muted(s string)   { haltSpinner(); fmt.Println(muted.Render(s)) }

func Check(ok bool, s string) {
	haltSpinner()
	if ok {
		fmt.Println(okS.Render("  ✓ ") + muted.Render(s))
		return
	}
	fmt.Println(warnS.Render("  ! ") + s)
}

func Explanation(s string) {
	haltSpinner()
	fmt.Println(panel.Width(min(width()-2, 88)).Render(keyS.Render("Diagnosis") + "\n" + s))
}

func Command(c string) {
	haltSpinner()
	fmt.Println(warnS.Render("  $ ") + bold.Render(c) + muted.Render("   (command to run)"))
}

func Diff(d string) {
	haltSpinner()
	for _, l := range strings.Split(strings.TrimRight(d, "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
			fmt.Println(bold.Render(l))
		case strings.HasPrefix(l, "@@"):
			fmt.Println(keyS.Render(l))
		case strings.HasPrefix(l, "+"):
			fmt.Println(addS.Render(l))
		case strings.HasPrefix(l, "-"):
			fmt.Println(delS.Render(l))
		default:
			fmt.Println(muted.Render(l))
		}
	}
}

// Spinner shows an animated status line; call the returned func to stop it.
// Any other output from this package also stops it.
func Spinner(msg string) func() {
	haltSpinner()
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(80 * time.Millisecond)
		defer t.Stop()
		for i := 0; ; i++ {
			fmt.Printf("\r%s %s", keyS.Render(frames[i%len(frames)]), muted.Render(msg))
			select {
			case <-done:
				fmt.Print("\r\033[K")
				return
			case <-t.C:
			}
		}
	}()
	var once sync.Once
	stop := func() { once.Do(func() { close(done); wg.Wait() }) }
	spinMu.Lock()
	spinStop = stop
	spinMu.Unlock()
	return func() { haltSpinner(); stop() }
}

type Option struct {
	Key   rune
	Label string
}

// Choose returns the chosen key. On EOF (no terminal) it returns the last
// option, so always put the safe choice last.
func Choose(question string, opts ...Option) rune {
	haltSpinner()
	parts := make([]string, len(opts))
	for i, o := range opts {
		parts[i] = keyS.Render("["+string(o.Key)+"]") + " " + o.Label
	}
	for {
		fmt.Printf("\n%s  %s › ", bold.Render(question), strings.Join(parts, "  "))
		line, err := in.ReadString('\n')
		if err != nil {
			fmt.Println()
			return opts[len(opts)-1].Key
		}
		line = strings.ToLower(strings.TrimSpace(line))
		if line == "" {
			continue
		}
		r := []rune(line)[0]
		for _, o := range opts {
			if o.Key == r {
				return r
			}
		}
	}
}

func Confirm(q string) bool {
	return Choose(q, Option{Key: 'y', Label: "yes"}, Option{Key: 'n', Label: "no"}) == 'y'
}

func Ask(q string) string {
	haltSpinner()
	fmt.Print(bold.Render(q) + " › ")
	line, _ := in.ReadString('\n')
	return strings.TrimSpace(line)
}

// Plan shows a numbered list of steps in a panel.
func Plan(title string, rows []string) {
	haltSpinner()
	body := keyS.Render(title)
	for i, r := range rows {
		body += fmt.Sprintf("\n%s %s", muted.Render(fmt.Sprintf("%d.", i+1)), r)
	}
	fmt.Println(panel.Width(min(width()-2, 100)).Render(body))
}

// Notice prints while a command runs in raw mode, where "\n" alone doesn't
// return the cursor to the start of the line.
func Notice(ok bool, s string) {
	mark := warnS.Render("! ")
	if ok {
		mark = okS.Render("✓ ")
	}
	fmt.Print("\r\n" + badge.Render("lazy") + " " + mark + s + "\r\n")
}

// AskDefault asks for a value; pressing Enter keeps the default.
func AskDefault(q, def string) string {
	haltSpinner()
	if def != "" {
		fmt.Print(bold.Render(q) + muted.Render(" ["+def+"]") + " › ")
	} else {
		fmt.Print(bold.Render(q) + " › ")
	}
	line, _ := in.ReadString('\n')
	if line = strings.TrimSpace(line); line == "" {
		return def
	}
	return line
}

// AskSecret reads a value without showing it; dots appear instead.
func AskSecret(q string) string {
	haltSpinner()
	fmt.Print(bold.Render(q) + muted.Render(" (hidden)") + " › ")
	restore := console.MakeRaw()
	defer restore()
	var buf []rune
	for {
		r, _, err := in.ReadRune()
		if err != nil {
			break
		}
		switch r {
		case '\r', '\n':
			fmt.Print("\r\n")
			return string(buf)
		case 3: // Ctrl-C: leave it empty
			fmt.Print("\r\n")
			return ""
		case 127, 8: // backspace
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
				fmt.Print("\b \b")
			}
		case 21: // Ctrl-U clears
			for range buf {
				fmt.Print("\b \b")
			}
			buf = nil
		default:
			if r >= 32 {
				buf = append(buf, r)
				fmt.Print("•")
			}
		}
	}
	fmt.Print("\r\n")
	return string(buf)
}
