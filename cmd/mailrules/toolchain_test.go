package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// The Go toolchain is pinned in go.mod (CI and the release read it through setup-go) and
// again in deploy/Dockerfile, whose golang image cannot read go.mod. A security release
// bumped in one place and not the other ships the image with the old standard library.
func TestDockerfileBuildsWithGoModToolchain(t *testing.T) {
	read := func(parts ...string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, parts...)...))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	mod := regexp.MustCompile(`(?m)^toolchain go(\S+)$`).FindStringSubmatch(read("go.mod"))
	if mod == nil {
		t.Fatal("go.mod has no toolchain line; pin one (toolchain goX.Y.Z)")
	}
	image := regexp.MustCompile(`(?m)^FROM .*golang:(\S+) AS build$`).FindStringSubmatch(read("deploy", "Dockerfile"))
	if image == nil {
		t.Fatal("deploy/Dockerfile has no golang build stage")
	}
	if image[1] != mod[1] {
		t.Errorf("deploy/Dockerfile builds with golang:%s but go.mod pins toolchain go%s", image[1], mod[1])
	}
}
