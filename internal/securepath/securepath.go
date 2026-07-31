// Package securepath validates and creates Mosscap's owner-private paths.
package securepath

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// UID is the effective user ID used for ownership checks.
func UID() int { return os.Geteuid() }

func stat(path string) (fs.FileInfo, *syscall.Stat_t, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("unsafe path %q: symlink", path)
	}
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, nil, fmt.Errorf("cannot verify ownership of %q", path)
	}
	return info, s, nil
}

func checkOwnerMode(path string, info fs.FileInfo, st *syscall.Stat_t, want fs.FileMode, dir bool) error {
	if st.Uid != uint32(UID()) {
		return fmt.Errorf("unsafe path %q: not owned by effective user", path)
	}
	if info.IsDir() != dir {
		return fmt.Errorf("unsafe path %q: wrong file type", path)
	}
	if info.Mode().Perm() != want {
		return fmt.Errorf("unsafe path %q: mode must be %04o", path, want)
	}
	return nil
}

// CheckDir requires an owner-owned directory with exactly mode 0700.
func CheckDir(path string) error {
	info, st, err := stat(path)
	if err != nil {
		return err
	}
	return checkOwnerMode(path, info, st, 0700, true)
}

// CheckFile requires an owner-owned, singly linked regular file with mode 0600.
func CheckFile(path string) error {
	info, st, err := stat(path)
	if err != nil {
		return err
	}
	if err := checkOwnerMode(path, info, st, 0600, false); err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("unsafe path %q: not a regular file", path)
	}
	if st.Nlink != 1 {
		return fmt.Errorf("unsafe path %q: hard-linked file", path)
	}
	return nil
}

// CheckExistingComponents rejects symlinks and group/world writable components
// from home (exclusive) down to path's existing ancestor.
func CheckExistingComponents(home, path string) error {
	home = filepath.Clean(home)
	path = filepath.Clean(path)
	if !filepath.IsAbs(home) || !filepath.IsAbs(path) {
		return errors.New("paths must be absolute")
	}
	rel, err := filepath.Rel(home, path)
	if err != nil || rel == ".." || (len(rel) > 3 && rel[:3] == ".."+string(os.PathSeparator)) {
		return fmt.Errorf("path %q is outside home %q", path, home)
	}
	current := home
	if _, _, err := stat(current); err != nil {
		return err
	}
	for _, part := range split(rel) {
		current = filepath.Join(current, part)
		info, _, err := stat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("unsafe path component %q: group/world writable", current)
		}
	}
	return nil
}

func split(rel string) []string {
	if rel == "." || rel == "" {
		return nil
	}
	return strings.Split(rel, string(os.PathSeparator))
}

// EnsurePrivateDir creates path, after checking existing components, and
// requires the result to be exactly owner mode 0700.
func EnsurePrivateDir(home, path string) error {
	if err := CheckExistingComponents(home, path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return CheckDir(path)
}

// Descendant reports whether child is lexically below root after cleaning.
func Descendant(root, child string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(child))
	return err == nil && rel != "." && rel != ".." && !(len(rel) > 3 && rel[:3] == ".."+string(os.PathSeparator))
}
