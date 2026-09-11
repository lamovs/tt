package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/movsar/tt/internal/config"
)

var notifyHints = map[string]string{
	"macos":       config.NotifyMacOS,
	"notify-send": config.NotifyLinux,
}

var notifyVarNames = func() []string {
	names := make([]string, len(notifyPlaceholders))
	for i, p := range notifyPlaceholders {
		names[i] = notifyVarName(p)
	}
	return names
}()

func systemLabel(sys string) string {
	switch sys {
	case "macos":
		return "macOS"
	case "notify-send":
		return "Linux (notify-send)"
	case "wsl":
		return "WSL"
	default:
		return "this system"
	}
}

func detectSystem() string {
	return classifySystem(runtime.GOOS, wslDetected(), notifySendAvailable())
}

func classifySystem(goos string, wsl, notifySend bool) string {
	switch {
	case goos == "darwin":
		return "macos"
	case goos == "linux" && wsl:
		return "wsl"
	case goos == "linux" && notifySend:
		return "notify-send"
	default:
		return "unknown"
	}
}

func wslDetected() bool {
	return os.Getenv("WSL_DISTRO_NAME") != "" || os.Getenv("WSL_INTEROP") != ""
}

func notifySendAvailable() bool {
	_, err := exec.LookPath("notify-send")
	return err == nil
}

type notifyMachine struct {
	system string
	goos   string
}

func thisMachine() notifyMachine {
	return notifyMachine{system: detectSystem(), goos: runtime.GOOS}
}

func shellAvailable() bool {
	_, err := exec.LookPath("sh")
	return err == nil
}

var notifyProbeTimeout = 2 * time.Second

type probeAnswer int

const (
	wordResolves probeAnswer = iota
	wordDoesNotResolve
	probeUnanswered
)

func probeWord(word string) probeAnswer {
	ctx, cancel := context.WithTimeout(context.Background(), notifyProbeTimeout)
	defer cancel()
	err := exec.CommandContext(ctx, "sh", "-c", `command -v -- "$1" >/dev/null 2>&1`, "sh", word).Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return wordResolves
	case ctx.Err() != nil:
		return probeUnanswered
	case errors.As(err, &exit) && exit.ExitCode() >= 0:
		return wordDoesNotResolve
	default:
		return probeUnanswered
	}
}

const (
	busAddressVar = "DBUS_SESSION_BUS_ADDRESS"
	runtimeDirVar = "XDG_RUNTIME_DIR"
)

func sessionBusPresent() bool {
	if os.Getenv(busAddressVar) != "" {
		return true
	}
	dir := os.Getenv(runtimeDirVar)
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "bus"))
	return err == nil
}
