// Command release-archive packages a built plugin library into the archive
// shape CLIProxyAPI's plugin store expects.
//
// The store's installer is strict, and every rule it enforces is enforced here
// first, so a broken archive fails the build instead of failing on a user's
// machine. See internal/pluginstore in CLIProxyAPI: ArchiveName builds
// `<id>_<version>_<goos>_<goarch>.zip`, SelectReleaseAssets looks that name up
// in the release, and readTargetLibrary requires the archive to hold exactly one
// dynamic library, at the archive root, named `<id><ext>` or
// `<id>-v<version><ext>`.
//
// Written in Go rather than shell so the release works on every runner in the
// matrix: the Windows runners have no `zip`.
package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yangshoulai/codex-turn-state-manager/internal/version"
)

// versionPattern mirrors the host's pluginVersionPattern: a leading digit, then
// alphanumerics, dots, plus and hyphen.
var versionPattern = regexp.MustCompile(`^[0-9][0-9A-Za-z.+-]*$`)

func main() {
	var (
		libPath = flag.String("lib", "", "path to the built shared library (required)")
		id      = flag.String("id", version.PluginName, "plugin id; also the library base name")
		ver     = flag.String("version", "", "plugin version, with or without a leading v (required)")
		goos    = flag.String("goos", "", "target GOOS (required)")
		goarch  = flag.String("goarch", "", "target GOARCH (required)")
		outDir  = flag.String("out", "dist/release", "directory to write the archive into")
	)
	flag.Parse()

	line, err := run(*libPath, *id, *ver, *goos, *goarch, *outDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "release-archive:", err)
		os.Exit(1)
	}
	fmt.Println(line)
}

// run writes the archive and returns the checksums.txt line for it.
func run(libPath, id, ver, goos, goarch, outDir string) (string, error) {
	// The store strips a leading "v" from the release tag before building the
	// artifact name, and the host rejects a version that still has one, so the
	// archive is always named from the bare version.
	version := strings.TrimPrefix(strings.TrimSpace(ver), "v")
	if !versionPattern.MatchString(version) {
		return "", fmt.Errorf("invalid version %q: must start with a digit and contain only [0-9A-Za-z.+-]", ver)
	}
	goos, goarch = strings.TrimSpace(goos), strings.TrimSpace(goarch)
	if goos == "" || goarch == "" {
		return "", fmt.Errorf("goos and goarch are required")
	}
	if strings.TrimSpace(libPath) == "" {
		return "", fmt.Errorf("-lib is required")
	}

	ext, err := libraryExtension(goos)
	if err != nil {
		return "", err
	}
	// Accept both the bare and the versioned name the host recognises, but
	// require the file on disk to already carry one of them: renaming here would
	// hide a build that produced something unexpected.
	base := filepath.Base(libPath)
	bare, versioned := id+ext, id+"-v"+version+ext
	if base != bare && base != versioned {
		return "", fmt.Errorf("library is named %q; the store requires %q or %q", base, bare, versioned)
	}

	data, err := os.ReadFile(libPath)
	if err != nil {
		return "", fmt.Errorf("read library: %w", err)
	}

	archiveName := fmt.Sprintf("%s_%s_%s_%s.zip", id, version, goos, goarch)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", fmt.Errorf("create output directory: %w", err)
	}
	archivePath := filepath.Join(outDir, archiveName)

	// The library sits at the archive root: a nested directory is rejected by
	// the installer.
	if err := writeZip(archivePath, base, data); err != nil {
		return "", err
	}

	// The checksum covers the archive, not the library: both install paths
	// verify the bytes they downloaded, and what they download is the .zip.
	sum, err := hashFile(archivePath)
	if err != nil {
		return "", err
	}
	// sha256sum format, which is what ParseChecksums reads.
	return fmt.Sprintf("%s  %s", sum, archiveName), nil
}

func libraryExtension(goos string) (string, error) {
	switch strings.ToLower(goos) {
	case "darwin":
		return ".dylib", nil
	case "windows":
		return ".dll", nil
	case "linux":
		return ".so", nil
	default:
		return "", fmt.Errorf("unsupported goos %q", goos)
	}
}

func writeZip(path, name string, data []byte) error {
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create archive: %w", err)
	}

	writer := zip.NewWriter(file)
	entry, err := writer.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
	if err != nil {
		file.Close()
		return fmt.Errorf("create archive entry: %w", err)
	}
	if _, err := entry.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("write archive entry: %w", err)
	}
	if err := writer.Close(); err != nil {
		file.Close()
		return fmt.Errorf("close archive: %w", err)
	}
	// The checksum is taken from this file, so it has to be flushed first.
	if err := file.Close(); err != nil {
		return fmt.Errorf("flush archive: %w", err)
	}
	return nil
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("hash archive: %w", err)
	}
	defer file.Close()

	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", fmt.Errorf("hash archive: %w", err)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
