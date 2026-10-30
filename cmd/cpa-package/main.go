// Command cpa-package wraps a built plugin library in the archive layout the
// CLIProxyAPI plugin store installs: <id>_<version>_<goos>_<goarch>.zip
// containing <id>-v<version>.<ext>, plus a sha256 line in checksums.txt.
package main

import (
	"archive/zip"
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

var (
	idPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	versionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*([-+][0-9A-Za-z.-]+)?$`)
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "cpa-package:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("cpa-package", flag.ContinueOnError)
	id := flags.String("id", "", "plugin ID (required)")
	version := flags.String("version", "", "version without leading v (required)")
	goos := flags.String("goos", runtime.GOOS, "target OS")
	goarch := flags.String("goarch", runtime.GOARCH, "target architecture")
	lib := flags.String("lib", "", "built plugin library (required)")
	out := flags.String("out", "dist/release", "output directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !idPattern.MatchString(*id) {
		return fmt.Errorf("-id must match %s", idPattern)
	}
	if !versionPattern.MatchString(*version) {
		return fmt.Errorf("-version %q must be a dotted number without a leading v", *version)
	}
	if *lib == "" {
		return fmt.Errorf("-lib is required")
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	archiveName := fmt.Sprintf("%s_%s_%s_%s.zip", *id, *version, *goos, *goarch)
	entryName := fmt.Sprintf("%s-v%s%s", *id, *version, filepath.Ext(*lib))
	archivePath := filepath.Join(*out, archiveName)
	if err := writeZip(archivePath, *lib, entryName); err != nil {
		return err
	}
	sum, err := fileSHA256(archivePath)
	if err != nil {
		return err
	}
	if err := updateChecksums(filepath.Join(*out, "checksums.txt"), archiveName, sum); err != nil {
		return err
	}
	fmt.Printf("%s  %s\n", sum, archivePath)
	return nil
}

func writeZip(archivePath, libPath, entryName string) error {
	src, err := os.Open(libPath)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.Create(archivePath)
	if err != nil {
		return err
	}
	writer := zip.NewWriter(dst)
	// Fixed timestamp (DOS epoch) keeps archives reproducible.
	header := &zip.FileHeader{Name: entryName, Method: zip.Deflate, Modified: time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)}
	header.SetMode(0o755)
	entry, err := writer.CreateHeader(header)
	if err == nil {
		_, err = io.Copy(entry, src)
	}
	if errClose := writer.Close(); err == nil {
		err = errClose
	}
	if errClose := dst.Close(); err == nil {
		err = errClose
	}
	return err
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// updateChecksums replaces or adds the line for name, keeping other archives' lines.
func updateChecksums(path, name, sum string) error {
	lines := map[string]string{}
	if f, err := os.Open(path); err == nil {
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) == 2 {
				lines[strings.TrimPrefix(fields[1], "*")] = fields[0]
			}
		}
		f.Close()
	}
	lines[name] = sum
	names := make([]string, 0, len(lines))
	for n := range lines {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "%s  %s\n", lines[n], n)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
