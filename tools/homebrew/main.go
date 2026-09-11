package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
)

var versionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var artifactVersionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-SNAPSHOT-[a-zA-Z0-9]+)?$`)
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func main() {
	version := flag.String("version", "", "release version")
	checksums := flag.String("checksums", "", "release checksum file")
	output := flag.String("output", "", "formula output path")
	verifyOnly := flag.Bool("verify-only", false, "check release archives without generating a formula")
	flag.Parse()
	if flag.NArg() != 0 || *checksums == "" || !*verifyOnly && *output == "" || !artifactVersionPattern.MatchString(*version) {
		fmt.Fprintln(os.Stderr, "usage: homebrew --version v1.0.0 --checksums checksums.txt --output tt.rb")
		os.Exit(2)
	}
	source, err := os.ReadFile(*checksums)
	if err == nil && *verifyOnly {
		err = verifyArchives(filepath.Dir(*checksums), strings.TrimPrefix(*version, "v"), string(source))
	} else if err == nil {
		var formula []byte
		formula, err = render(*version, string(source))
		if err == nil {
			err = verifyArchives(filepath.Dir(*checksums), strings.TrimPrefix(*version, "v"), string(source))
		}
		if err == nil {
			err = os.WriteFile(*output, formula, 0o644)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "homebrew: %q\n", err.Error())
		os.Exit(1)
	}
}

func render(version, checksums string) ([]byte, error) {
	if !versionPattern.MatchString(version) {
		return nil, errors.New("version must be a stable MAJOR.MINOR.PATCH release")
	}
	version = strings.TrimPrefix(version, "v")
	sums := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !digestPattern.MatchString(fields[0]) {
			return nil, errors.New("invalid checksum record")
		}
		if _, exists := sums[fields[1]]; exists {
			return nil, errors.New("duplicate checksum record")
		}
		sums[fields[1]] = fields[0]
	}
	data := map[string]string{"Version": version}
	for _, os := range []string{"darwin", "linux"} {
		for _, arch := range []string{"arm64", "amd64"} {
			key := os + "_" + arch
			name := "tt_" + version + "_" + key + ".tar.gz"
			if sums[name] == "" {
				return nil, fmt.Errorf("missing checksum for %s", name)
			}
			data[key] = sums[name]
		}
	}
	tmpl, err := template.New("formula").Option("missingkey=error").Parse(formulaTemplate)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	err = tmpl.Execute(&out, data)
	return out.Bytes(), err
}

const formulaTemplate = `class Tt < Formula
  desc "TickTick CLI and TUI"
  homepage "https://github.com/lamovs/tt"
  version "{{.Version}}"

  on_macos do
    depends_on macos: :ventura
    on_arm do
      url "https://github.com/lamovs/tt/releases/download/v{{.Version}}/tt_{{.Version}}_darwin_arm64.tar.gz"
      sha256 "{{.darwin_arm64}}"
    end
    on_intel do
      url "https://github.com/lamovs/tt/releases/download/v{{.Version}}/tt_{{.Version}}_darwin_amd64.tar.gz"
      sha256 "{{.darwin_amd64}}"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/lamovs/tt/releases/download/v{{.Version}}/tt_{{.Version}}_linux_arm64.tar.gz"
      sha256 "{{.linux_arm64}}"
    end
    on_intel do
      url "https://github.com/lamovs/tt/releases/download/v{{.Version}}/tt_{{.Version}}_linux_amd64.tar.gz"
      sha256 "{{.linux_amd64}}"
    end
  end

  def install
    bin.install "tt"
    pkgshare.install Dir["share/tt/*"]
    if OS.mac?
      bin.install_symlink pkgshare/"integrations/macos/tt-notify"
    end
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/tt version")
    assert_match "setup", shell_output("#{bin}/tt help setup")
  end
end
`
