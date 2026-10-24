package ansible

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Limits of an extracted project archive.
const (
	maxArchiveBytes = 512 << 20
	maxArchiveFiles = 20000
)

// extract unpacks a .tgz project archive into dir. Absolute paths, "..", links and special files
// are refused: the archive comes from an artifact, but nothing in it may write outside dir.
func extract(archive, dir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("ansible plugin: open artifact: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("ansible plugin: the artifact is not a .tgz archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	for files := 0; ; files++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("ansible plugin: read artifact: %w", err)
		}
		if files >= maxArchiveFiles {
			return fmt.Errorf("ansible plugin: the artifact has more than %d entries", maxArchiveFiles)
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if name == "." {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("ansible plugin: unsafe path %q in the artifact", h.Name)
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			total += h.Size
			if total > maxArchiveBytes {
				return fmt.Errorf("ansible plugin: the artifact is larger than %d MiB once extracted", maxArchiveBytes>>20)
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return err
			}
			out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return fmt.Errorf("ansible plugin: extract %s: %w", name, err)
			}
			_, err = io.Copy(out, io.LimitReader(tr, h.Size))
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return fmt.Errorf("ansible plugin: extract %s: %w", name, err)
			}
		case tar.TypeXGlobalHeader:
		default:
			return fmt.Errorf("ansible plugin: %q is a link or a special file: not allowed in the artifact", h.Name)
		}
	}
}

// projectRoot returns the directory of the project: the archive root, or its only top-level
// directory when the playbook is not at the root (archives made with tar -C .. dir).
func projectRoot(dir, playbook string) (string, error) {
	if isFile(filepath.Join(dir, filepath.FromSlash(playbook))) {
		return dir, nil
	}
	entries, err := os.ReadDir(dir)
	if err == nil && len(entries) == 1 && entries[0].IsDir() {
		sub := filepath.Join(dir, entries[0].Name())
		if isFile(filepath.Join(sub, filepath.FromSlash(playbook))) {
			return sub, nil
		}
	}
	return "", fmt.Errorf("ansible plugin: playbook %s not found in the artifact", playbook)
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}
