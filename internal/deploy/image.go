package deploy

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/template"
)

// imageSchemaVersion folds into every tag; the rendered expression is hashed
// too, so this changes only when the hash's own input set changes.
const imageSchemaVersion = "1"

// DefaultPin is the nixpkgs snapshot the gate image is built over unless a
// deploy config names its own.
var DefaultPin = Pin{
	Rev:    "b6018f87da91d19d0ab4cf979885689b469cdd41",
	SHA256: "0ln4yw7z3g9lb0x081hc0pd2j1wsx2qqf6bgmwwvdbkcl4bcy1dp",
}

//go:embed image.nix.tmpl
var imageTemplate string

//go:embed entrypoint.sh
var entrypointScript []byte

// nixSystem maps a Go arch to the Nix system the image is built for.
var nixSystem = map[string]string{"amd64": "x86_64-linux", "arm64": "aarch64-linux"}

// renderExpr renders the image expression; tag stays a function argument so
// the expression's bytes do not depend on the hash they feed.
func renderExpr(name, arch string, pin Pin, packages []string, overlay bool) ([]byte, error) {
	system, ok := nixSystem[arch]
	if !ok {
		return nil, fmt.Errorf("unsupported architecture %q", arch)
	}
	attrs := make([]string, 0, len(packages))
	for _, p := range slices.Sorted(slices.Values(packages)) {
		var sb strings.Builder
		sb.WriteString("pkgs")
		for _, seg := range strings.Split(p, ".") {
			fmt.Fprintf(&sb, ".%q", seg)
		}
		attrs = append(attrs, sb.String())
	}
	t, err := template.New("image").Parse(imageTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse image template: %w", err)
	}
	var out bytes.Buffer
	err = t.Execute(&out, struct {
		Name, System, Rev, SHA256 string
		Packages                  []string
		Overlay                   bool
	}{name, system, pin.Rev, pin.SHA256, attrs, overlay})
	if err != nil {
		return nil, fmt.Errorf("render image template: %w", err)
	}
	return out.Bytes(), nil
}

// stage writes every image input into dir — the expression, the gate binary,
// the entrypoint, the overlay and the normalized config tree — and returns the
// image tag: a hash over exactly the staged bytes, paths and modes.
func stage(dir string, c *Config, arch, gateBin string) (string, error) {
	pin := DefaultPin
	if c.Image.Pin != nil {
		pin = *c.Image.Pin
	}
	expr, err := renderExpr(c.Name, arch, pin, c.Image.Packages, c.Image.Overlay != "")
	if err != nil {
		return "", err
	}
	for name, f := range map[string]struct {
		b    []byte
		mode os.FileMode
	}{"image.nix": {expr, 0o644}, "entrypoint.sh": {entrypointScript, 0o755}} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, f.b, f.mode); err != nil {
			return "", err
		}
		if err := os.Chmod(p, f.mode); err != nil {
			return "", err
		}
	}
	if err := copyFile(gateBin, filepath.Join(dir, "portitor-gate"), 0o755); err != nil {
		return "", fmt.Errorf("stage gate binary: %w", err)
	}
	if c.Image.Overlay != "" {
		if err := copyFile(c.Image.Overlay, filepath.Join(dir, "overlay.nix"), 0o644); err != nil {
			return "", fmt.Errorf("stage overlay: %w", err)
		}
	}
	if err := copyConfigTree(c.ConfigDir, filepath.Join(dir, "config"), 0); err != nil {
		return "", fmt.Errorf("stage config dir: %w", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config", "repos.d")); err != nil {
		return "", fmt.Errorf("config dir %s has no repos.d/", c.ConfigDir)
	}
	return hashTree(dir)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

// maxConfigDepth bounds the symlink-following copy, so a symlink cycle fails
// instead of recursing forever.
const maxConfigDepth = 32

// copyConfigTree copies src into dst following symlinks, leaving out
// dot-prefixed and .lock entries, with modes normalized to 0755 for directories
// and executables and 0644 for everything else.
func copyConfigTree(src, dst string, depth int) error {
	if depth > maxConfigDepth {
		return fmt.Errorf("%s: deeper than %d levels (a symlink cycle?)", src, maxConfigDepth)
	}
	if err := os.Mkdir(dst, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(dst, 0o755); err != nil {
		return err
	}
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".lock") {
			continue
		}
		s, d := filepath.Join(src, name), filepath.Join(dst, name)
		fi, err := os.Stat(s)
		if err != nil {
			return err
		}
		switch {
		case fi.IsDir():
			if err := copyConfigTree(s, d, depth+1); err != nil {
				return err
			}
		case fi.Mode().IsRegular():
			mode := os.FileMode(0o644)
			if fi.Mode()&0o111 != 0 {
				mode = 0o755
			}
			if err := copyFile(s, d, mode); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s is neither a file nor a directory", s)
		}
	}
	return nil
}

// hashTree hashes every regular file under dir by relative path, mode and
// bytes, in path order.
func hashTree(dir string) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "portitor-gate image %s\n", imageSchemaVersion)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s %o %d\n", filepath.ToSlash(rel), fi.Mode().Perm(), len(b))
		h.Write(b)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
