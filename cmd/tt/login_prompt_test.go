package main

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

func TestSecretPromptWaitsForEchoOff(t *testing.T) {
	saved := toggleEcho
	t.Cleanup(func() { toggleEcho = saved })
	echoOn, shown := true, false
	toggleEcho = func(_ *os.File, on bool, _ pendingInput) error {
		echoOn = on
		return nil
	}
	var stdout, stderr bytes.Buffer
	value, err := readSecretNoEcho(context.Background(), charDevice(t),
		bufio.NewReader(strings.NewReader("synthetic-secret\n")), &stdout, &stderr, func() {
			if echoOn {
				t.Fatal("input was invited while terminal echo was enabled")
			}
			shown = true
		})
	if err != nil || value != "synthetic-secret" || !shown || !echoOn {
		t.Fatalf("prompt lifecycle: err=%v shown=%v restored=%v", err, shown, echoOn)
	}
	if strings.Contains(stdout.String()+stderr.String(), value) {
		t.Fatal("secret reached output")
	}
}
