// Package securepath validates and creates Mosscap's owner-private paths.
package securepath

import (
	"errors"
	"fmt"
	"io"
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
	return checkFileInfo(path, info, st)
}

func checkFileInfo(path string, info fs.FileInfo, st *syscall.Stat_t) error {
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

// OpenFileNoFollow opens path without following a final symlink. Callers must
// subsequently use CheckOpenFile to verify that the pathname was not replaced.
func OpenFileNoFollow(path string, flag int, perm fs.FileMode) (*os.File, error) {
	fd, err := syscall.Open(path, flag|syscall.O_NOFOLLOW, uint32(perm))
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

// CheckOpenFile verifies an already-open private file and confirms that path
// still names that same inode after it was opened.
func CheckOpenFile(path string, f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot verify ownership of %q", path)
	}
	if err := checkFileInfo(path, info, st); err != nil {
		return err
	}
	current, currentST, err := stat(path)
	if err != nil {
		return err
	}
	if uint64(st.Dev) != uint64(currentST.Dev) || st.Ino != currentST.Ino || current.Mode() != info.Mode() {
		return fmt.Errorf("unsafe path %q: changed while opening", path)
	}
	return nil
}

// ReadRegularFile reads at most limit bytes from a non-symlink regular file.
// It rejects pathname replacement and size changes during the read.
func ReadRegularFile(path string, limit int64) ([]byte, error) {
	return readRegularFile(path, limit, nil)
}

// readRegularFile has a test seam at the post-read revalidation boundary.
func readRegularFile(path string, limit int64, afterRead func()) ([]byte, error) {
	if limit < 0 {
		return nil, errors.New("invalid file size limit")
	}
	f, err := OpenFileNoFollow(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, err
	}
	beforeST, ok := before.Sys().(*syscall.Stat_t)
	if !ok || !before.Mode().IsRegular() {
		return nil, fmt.Errorf("unsafe input file %q: not a regular file", path)
	}
	if before.Size() > limit {
		return nil, fmt.Errorf("input file %q exceeds size limit", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("input file %q exceeds size limit", path)
	}
	if afterRead != nil {
		afterRead()
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	afterST, ok := after.Sys().(*syscall.Stat_t)
	if !ok || beforeST.Dev != afterST.Dev || beforeST.Ino != afterST.Ino || before.Size() != after.Size() || int64(len(b)) != after.Size() {
		return nil, fmt.Errorf("unsafe input file %q: changed while reading", path)
	}
	current, currentST, err := stat(path)
	if err != nil {
		return nil, err
	}
	if uint64(beforeST.Dev) != uint64(currentST.Dev) || beforeST.Ino != currentST.Ino || current.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("unsafe input file %q: replaced while reading", path)
	}
	return b, nil
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

// CheckAbsoluteComponents rejects symlinks and group/world writable components
// from the filesystem root down to path's existing ancestor. Unlike
// CheckExistingComponents it permits system-owned root components, making it
// suitable for configured absolute paths that are not beneath the home dir.
func CheckAbsoluteComponents(path string) error {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return errors.New("path must be absolute")
	}
	current := string(os.PathSeparator)
	for _, part := range split(strings.TrimPrefix(path, string(os.PathSeparator))) {
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
