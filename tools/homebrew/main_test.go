package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFormulaRubySyntax(t *testing.T) {
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Skip("ruby is not installed")
	}
	formula, err := render("1.2.3", checksumFixture())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tt.rb")
	if err := os.WriteFile(path, formula, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(ruby, "-c", path).CombinedOutput(); err != nil {
		t.Fatalf("ruby syntax: %v: %s", err, out)
	}
}

func checksumFixture() string {
	var rows []string
	for _, platform := range []string{"darwin_arm64", "darwin_amd64", "linux_arm64", "linux_amd64"} {
		rows = append(rows, strings.Repeat("a", 64)+"  tt_1.2.3_"+platform+".tar.gz")
	}
	return strings.Join(rows, "\n")
}

func TestRender(t *testing.T) {
	for _, version := range []string{"1.2.3", "v1.2.3"} {
		got, err := render(version, checksumFixture())
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"class Tt < Formula", `version "1.2.3"`, "darwin_arm64.tar.gz", "linux_amd64.tar.gz", `pkgshare.install Dir["share/tt/*"]`} {
			if !strings.Contains(string(got), want) {
				t.Errorf("formula missing %q", want)
			}
		}
		if strings.Count(string(got), `sha256 "`+strings.Repeat("a", 64)+`"`) != 4 {
			t.Fatal("formula must pin all four archives")
		}
	}
}

func TestRenderRefusesInvalidInput(t *testing.T) {
	for _, version := range []string{"", "v1", "v01.2.3", "1.2.3-beta", `1.2.3"`, "1.2.3\n"} {
		if _, err := render(version, checksumFixture()); err == nil {
			t.Errorf("accepted version %q", version)
		}
	}
	for _, source := range []string{"", "not a digest", checksumFixture() + "\n" + checksumFixture(), strings.Replace(checksumFixture(), "darwin_arm64", "wrong", 1)} {
		if _, err := render("1.2.3", source); err == nil {
			t.Errorf("accepted invalid checksums %q", source)
		}
	}
}
