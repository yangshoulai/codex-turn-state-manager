package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The rules asserted here are the host's, not ours: CLIProxyAPI's
// internal/pluginstore selects the release asset by exact name and then refuses
// any archive whose single dynamic library is misnamed or nested. Getting one
// wrong produces a plugin that publishes cleanly and then fails to install.
const id = "codex-turn-state-manager"

func writeLib(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("not really a shared library"), 0o755); err != nil {
		t.Fatalf("write library: %v", err)
	}
	return path
}

func TestArchiveNameMatchesTheStoreContract(t *testing.T) {
	dir := t.TempDir()
	lib := writeLib(t, dir, id+".so")

	line, err := run(lib, id, "1.2.3", "linux", "arm64", filepath.Join(dir, "out"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	want := id + "_1.2.3_linux_arm64.zip"
	if !strings.HasSuffix(line, want) {
		t.Errorf("checksums line %q does not name %q", line, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "out", want)); err != nil {
		t.Errorf("archive was not written as %s: %v", want, err)
	}
}

// A release tag carries a leading "v"; the artifact name must not.
func TestLeadingVIsStrippedFromTheVersion(t *testing.T) {
	dir := t.TempDir()
	lib := writeLib(t, dir, id+".so")

	line, err := run(lib, id, "v0.1.0", "linux", "amd64", filepath.Join(dir, "out"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.HasSuffix(line, id+"_0.1.0_linux_amd64.zip") {
		t.Errorf("checksums line %q kept the tag's v prefix", line)
	}
}

// The versioned filename is the shape the installer actually writes, so an
// archive built from it has to be accepted too.
func TestVersionedLibraryNameIsAccepted(t *testing.T) {
	dir := t.TempDir()
	lib := writeLib(t, dir, id+"-v1.2.3.so")

	if _, err := run(lib, id, "1.2.3", "linux", "amd64", filepath.Join(dir, "out")); err != nil {
		t.Fatalf("run with a versioned library name: %v", err)
	}
}

func TestArchiveHoldsExactlyOneLibraryAtTheRoot(t *testing.T) {
	dir := t.TempDir()
	lib := writeLib(t, dir, id+".so")

	_, err := run(lib, id, "1.2.3", "linux", "amd64", filepath.Join(dir, "out"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	reader, err := zip.OpenReader(filepath.Join(dir, "out", id+"_1.2.3_linux_amd64.zip"))
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer reader.Close()

	if len(reader.File) != 1 {
		t.Fatalf("archive holds %d entries, want 1", len(reader.File))
	}
	// A nested path is rejected by the installer even when the base name is
	// right, so assert the entry is exactly the bare filename.
	if name := reader.File[0].Name; name != id+".so" {
		t.Errorf("archive entry is %q, want %q at the root", name, id+".so")
	}
}

// checksums.txt is verified against the downloaded archive, not the library
// inside it, so the digest has to be the archive's.
func TestChecksumCoversTheArchiveNotTheLibrary(t *testing.T) {
	dir := t.TempDir()
	lib := writeLib(t, dir, id+".so")
	out := filepath.Join(dir, "out")

	line, err := run(lib, id, "1.2.3", "linux", "amd64", out)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	archive, err := os.ReadFile(filepath.Join(out, id+"_1.2.3_linux_amd64.zip"))
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	sum := sha256.Sum256(archive)
	want := hex.EncodeToString(sum[:])

	if !strings.HasPrefix(line, want) {
		t.Errorf("checksums line starts with %q, want the archive digest %q", strings.SplitN(line, " ", 2)[0], want)
	}

	// Guard the other direction: hashing the library would be the natural
	// mistake, so make sure that value is not what got written.
	libData, err := os.ReadFile(lib)
	if err != nil {
		t.Fatalf("read library: %v", err)
	}
	libSum := sha256.Sum256(libData)
	if strings.HasPrefix(line, hex.EncodeToString(libSum[:])) {
		t.Error("checksums line is the library's digest, not the archive's")
	}
}

func TestRejectsAMisnamedLibrary(t *testing.T) {
	dir := t.TempDir()
	lib := writeLib(t, dir, "something-else.so")

	_, err := run(lib, id, "1.2.3", "linux", "amd64", filepath.Join(dir, "out"))
	if err == nil {
		t.Fatal("a library the installer would reject was packaged anyway")
	}
	if !strings.Contains(err.Error(), "something-else.so") {
		t.Errorf("error does not name the offending file: %v", err)
	}
}

func TestRejectsAnInvalidVersion(t *testing.T) {
	dir := t.TempDir()
	lib := writeLib(t, dir, id+".so")

	// The host's pattern requires a leading digit.
	if _, err := run(lib, id, "nightly", "linux", "amd64", filepath.Join(dir, "out")); err == nil {
		t.Error("a non-numeric-leading version was accepted")
	}
}

func TestExtensionFollowsTheTargetOS(t *testing.T) {
	cases := map[string]string{"linux": ".so", "darwin": ".dylib", "windows": ".dll"}
	for goos, ext := range cases {
		dir := t.TempDir()
		lib := writeLib(t, dir, id+ext)
		if _, err := run(lib, id, "1.2.3", goos, "amd64", filepath.Join(dir, "out")); err != nil {
			t.Errorf("%s: %v", goos, err)
		}
	}
	if _, err := run("x", id, "1.2.3", "plan9", "amd64", t.TempDir()); err == nil {
		t.Error("an unsupported GOOS was accepted")
	}
}
