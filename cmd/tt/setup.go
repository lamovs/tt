package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/install"
)

func init() {
	register(command{
		help: cli.Help{
			Verb: "setup", Summary: "install tt or configure optional integrations",
			Examples: []cli.Example{
				{Cmd: "tt setup", What: "show setup options without changing anything"},
				{Cmd: "./tt setup install", What: "install an extracted release into ~/.local/bin"},
				{Cmd: "tt setup notifications", What: "install the macOS helper; show Linux requirements"},
				{Cmd: "tt setup background", What: "enable automatic sync for the current user"},
				{Cmd: "tt setup background --remove", What: "stop background sync and retain the service files as backups"},
				{Cmd: "tt setup herdr", What: "print the Herdr configuration fragment"},
				{Cmd: "tt setup tmux", What: "print the tmux configuration fragment"},
			},
			Sections: []cli.HelpSection{
				{Title: "Installation", Items: []string{
					"Homebrew users do not need setup install. Use brew upgrade lamovs/tap/tt for updates.",
					"Archive installation keeps previous binaries and does not change shell startup files, credentials, configuration or task data. Add ~/.local/bin to PATH if needed.",
				}},
				{Title: "Notifications", Items: []string{
					"macOS 13 or newer uses the bundled TT Notifier app; Swift is not required. The app is ad-hoc signed, not notarized. macOS may require approval before opening it.",
					"Linux requires notify-send and a desktop session. After installing a notifier, run tt doctor --fix and tt notify test. Allow notifications in system settings.",
				}},
				{Title: "Background sync", Items: []string{
					"background explicitly enables a one-minute launchd or systemd user timer. It can immediately send queued changes using the saved login. System services, sudo and login are not configured.",
					"The service keeps the selected executable path and current XDG locations. Run setup again after moving tt or changing those locations. Removal keeps backups next to the old service files.",
				}},
				{Title: "Terminal indicators", Items: []string{
					"herdr and tmux only print fragments. Merge the required settings into your existing configuration; they do not rewrite it or reload a running terminal.",
				}},
			},
			SeeAlso: []string{"login", "auto", "doctor", "notify"},
		},
		run: cmdSetup,
	})
}

func cmdSetup(inv *invocation) int {
	if len(inv.args) == 0 {
		cli.WriteLines(inv.stdout, commands["setup"].help.Render(inv.outPalette()))
		return exitOK
	}
	if len(inv.args) > 2 || len(inv.args) == 2 && (inv.args[0] != "background" || inv.args[1] != "--remove") {
		return inv.misuse("use tt help setup for available options")
	}
	action := inv.args[0]
	switch action {
	case "install", "background", "notifications", "herdr", "tmux":
	default:
		return inv.misuse("use tt help setup for available options")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return inv.fail(errors.New("setup supports macOS and Linux only"))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return inv.fail(errors.New("cannot determine the home directory"))
	}
	executable, err := os.Executable()
	if err != nil {
		return inv.fail(errors.New("cannot determine the installed executable"))
	}
	executable = setupExecutable(executable)
	i := install.Installer{
		Platform: runtime.GOOS, Home: home, Executable: executable, Env: os.Environ(),
		Run: func(name string, args ...string) error {
			cmd := exec.CommandContext(inv.ctx, name, args...)
			return cmd.Run()
		},
	}
	if action == "background" {
		if err := i.Background(len(inv.args) == 2); err != nil {
			return setupFailure(inv, err)
		}
		if len(inv.args) == 2 {
			fmt.Fprintln(inv.stdout, "Background sync stopped. Service files were retained as backups.")
		} else {
			fmt.Fprintln(inv.stdout, "Background sync enabled. Check tt auto status for its result.")
		}
		return exitOK
	}
	if action == "notifications" && runtime.GOOS == "linux" {
		fmt.Fprintln(inv.stdout, "Install notify-send with your distribution's package manager.")
		fmt.Fprintln(inv.stdout, "In a desktop session, run tt doctor --fix, then tt notify test.")
		return exitOK
	}
	i.Resources, err = install.Resources(executable)
	if err != nil {
		return setupFailure(inv, err)
	}
	switch action {
	case "install":
		if _, err := i.Install(); err != nil {
			return setupFailure(inv, err)
		}
		fmt.Fprintln(inv.stdout, "Installed ~/.local/bin/tt. Previous installations were retained.")
		fmt.Fprintln(inv.stdout, "If tt is not found, add ~/.local/bin to PATH in your shell configuration.")
		fmt.Fprintln(inv.stdout, "Next: tt login token, then tt ui.")
	case "notifications":
		if err := i.Notifier(); err != nil {
			return setupFailure(inv, err)
		}
		fmt.Fprintln(inv.stdout, "Installed TT Notifier. Ensure ~/.local/bin is on PATH.")
		fmt.Fprintln(inv.stdout, "Run tt doctor --fix, then tt notify test. Allow notifications in macOS.")
	case "herdr", "tmux":
		file := "tt.toml"
		if action == "tmux" {
			file = "tt.conf"
		}
		content, err := os.ReadFile(filepath.Join(i.Resources, "integrations", action, file))
		if err != nil {
			return setupFailure(inv, err)
		}
		for _, line := range strings.Split(strings.TrimSuffix(string(content), "\n"), "\n") {
			fmt.Fprintln(inv.stdout, api.OneLine(line))
		}
	}
	return exitOK
}

func setupFailure(inv *invocation, err error) int {
	cli.WriteLines(inv.stderr, cli.Wrap("tt: setup: "+api.OneLine(err.Error()), cli.Width))
	return exitError
}

func setupExecutable(executable string) string {
	for _, name := range []string{"tt", os.Args[0]} {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		path, err = filepath.Abs(path)
		if err != nil {
			continue
		}
		a, errA := os.Stat(path)
		b, errB := os.Stat(executable)
		if errA == nil && errB == nil && os.SameFile(a, b) {
			return path
		}
	}
	return executable
}
