// Package template verifies the materialized rootfs template (payload
// copied by the DaemonSet initContainer from the pw-rootfs image) against
// its SHA256SUMS, and GCs old template versions (keep-2).
package template

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Assets of one template tag.
type Assets struct {
	DesktopAvailable bool
	Dir              string
	RootfsPath       string
	KernelPath       string
	FcPath           string
}

// PayloadFile names inside a template dir.
const (
	rootfsName = "rootfs.ext4"
	kernelName = "vmlinux"
	fcName     = "firecracker"
	sumsName   = "SHA256SUMS"
)

// Resolve checks the template exists and matches SHA256SUMS.
func Resolve(templatesDir, tag string) (*Assets, error) {
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`).MatchString(tag) {
		return nil, fmt.Errorf("invalid template tag")
	}
	dir := filepath.Join(templatesDir, tag)
	a := &Assets{
		Dir:        dir,
		RootfsPath: filepath.Join(dir, rootfsName),
		KernelPath: filepath.Join(dir, kernelName),
		FcPath:     filepath.Join(dir, fcName),
	}
	sums, err := os.ReadFile(filepath.Join(dir, sumsName))
	if err != nil {
		return nil, fmt.Errorf("template %s: %w", tag, err)
	}
	expected := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 {
			expected[fields[1]] = fields[0]
		}
	}
	for _, name := range []string{rootfsName, kernelName, fcName} {
		want, ok := expected[name]
		if !ok {
			return nil, fmt.Errorf("template %s: %s missing from SHA256SUMS", tag, name)
		}
		got, err := fileSHA256(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", tag, err)
		}
		if got != want {
			return nil, fmt.Errorf("template %s: %s sha256 mismatch (want %s, got %s)", tag, name, want, got)
		}
	}
	// Legacy templates remain usable without a desktop. New templates declare
	// capabilities in checksum-covered metadata, not inferred from the tag name.
	if want, ok := expected["template.json"]; ok {
		path := filepath.Join(dir, "template.json")
		got, err := fileSHA256(path)
		if err != nil || got != want {
			return nil, fmt.Errorf("template metadata checksum mismatch")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var metadata struct {
			Tag     string `json:"tag"`
			Desktop bool   `json:"desktop"`
		}
		if err := json.Unmarshal(data, &metadata); err != nil {
			return nil, err
		}
		if metadata.Tag != tag {
			return nil, fmt.Errorf("template metadata tag mismatch")
		}
		a.DesktopAvailable = metadata.Desktop
	}
	return a, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// GC removes all but the newest keep template directories (by tag name
// sort — tags are semver-ish strings, newest sorts last lexically for
// same-major versions; callers pass keep=2).
func GC(templatesDir string, keep int) error {
	entries, err := os.ReadDir(templatesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var tags []string
	for _, e := range entries {
		if e.IsDir() {
			tags = append(tags, e.Name())
		}
	}
	sort.Strings(tags)
	for len(tags) > keep {
		oldest := tags[0]
		tags = tags[1:]
		if err := os.RemoveAll(filepath.Join(templatesDir, oldest)); err != nil {
			return err
		}
	}
	return nil
}
