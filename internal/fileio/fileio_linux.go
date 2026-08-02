//go:build linux

// Package fileio provides the Linux-only, race-safe filesystem primitives used
// by file transfer. It intentionally does not interpret file names or content.
package fileio

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

var (
	// ErrInvalidPath means a caller supplied a non-absolute or non-lexical path.
	ErrInvalidPath = errors.New("invalid file path")
	// ErrUnsafePath means a path contains or was replaced by a symlink.
	ErrUnsafePath = errors.New("unsafe file path")
	// ErrUnsafeFileType means a path does not name a regular file where one is required.
	ErrUnsafeFileType = errors.New("unsafe file type")
	// ErrTooLarge means the regular file's logical size exceeds the supplied bound.
	ErrTooLarge = errors.New("file too large")
	// ErrChanged means a file changed or was replaced while it was being used.
	ErrChanged = errors.New("file changed")
	// ErrDestinationExists means exclusive destination installation found an existing name.
	ErrDestinationExists = errors.New("destination exists")
	// ErrUnsupported means the kernel lacks an atomic primitive required to fail closed.
	ErrUnsupported = errors.New("unsupported file I/O primitive")
	// ErrInvalidTemporary means an entry is not a Mosscap private generated file.
	ErrInvalidTemporary = errors.New("unsafe temporary file")
)

const generatedPrefix = "mosscap-xfr-"

// testHook is deliberately package-private. Tests use it for deterministic
// replacement/truncation boundaries instead of relying on scheduler races.
var testHook func(phase string)

func runTestHook(phase string) {
	if testHook != nil {
		testHook(phase)
	}
}

// Source is one opened outgoing source. Its descriptor remains bound to the
// file opened during preparation; callers must Close it when the attempt ends.
type Source struct {
	path string
	file *os.File
	stat unix.Stat_t

	// Size is the prepared logical byte count.
	Size int64
	// SHA256 is the lowercase hexadecimal digest calculated during preparation.
	SHA256 string
}

// Path is the lexical absolute source path recorded as upload intent.
func (s *Source) Path() string { return s.path }

// Device and Inode are diagnostic prepare-time identity evidence.
func (s *Source) Device() uint64 { return uint64(s.stat.Dev) }
func (s *Source) Inode() uint64  { return s.stat.Ino }
func (s *Source) MtimeNS() int64 { return s.stat.Mtim.Sec*1_000_000_000 + s.stat.Mtim.Nsec }

// Close closes the retained source descriptor.
func (s *Source) Close() error {
	if s == nil || s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

// OpenSource securely opens and hashes one absolute regular source file. It
// never follows a symlink in the source path and rejects sparse files by their
// logical size before reading their holes into the page cache.
func OpenSource(path string, limit int64) (*Source, error) {
	if err := absoluteLexical(path); err != nil {
		return nil, err
	}
	if limit < 0 {
		return nil, fmt.Errorf("%w: negative limit", ErrTooLarge)
	}
	parent, name := filepath.Dir(path), filepath.Base(path)
	dirfd, err := openDirectory(parent)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dirfd)
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, classifyOpen(err)
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		f.Close()
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		f.Close()
		return nil, ErrUnsafeFileType
	}
	if st.Size > limit {
		f.Close()
		return nil, ErrTooLarge
	}
	s := &Source{path: path, file: f, stat: st, Size: st.Size}
	digest, err := s.hashExact()
	if err != nil {
		s.Close()
		return nil, err
	}
	s.SHA256 = digest
	runTestHook("source-open-after-hash")
	if err := s.Revalidate(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Revalidate proves that the retained descriptor and the current source path
// still name the prepared regular file with the prepared logical size. Content
// is re-hashed by StreamTo immediately before upload completion.
func (s *Source) Revalidate() error {
	if s == nil || s.file == nil {
		return os.ErrClosed
	}
	var now unix.Stat_t
	if err := unix.Fstat(int(s.file.Fd()), &now); err != nil {
		return err
	}
	if !sameIdentity(s.stat, now) || now.Size != s.Size {
		return ErrChanged
	}
	pathStat, err := statNoFollow(s.path)
	if err != nil {
		return err
	}
	if !sameIdentity(s.stat, pathStat) || pathStat.Size != s.Size {
		return ErrChanged
	}
	return nil
}

// StreamTo seeks the retained descriptor to zero, copies exactly the prepared
// bytes, computes a second digest, and revalidates descriptor and pathname
// identity. It detects both early EOF and appended bytes without buffering the
// source in memory.
func (s *Source) StreamTo(dst io.Writer) error {
	if s == nil || s.file == nil {
		return os.ErrClosed
	}
	if err := s.Revalidate(); err != nil {
		return err
	}
	runTestHook("source-stream-after-revalidate")
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	h := sha256.New()
	written, err := io.CopyN(io.MultiWriter(dst, h), s.file, s.Size)
	if err != nil {
		return fmt.Errorf("%w: expected %d bytes, copied %d: %v", ErrChanged, s.Size, written, err)
	}
	var one [1]byte
	n, err := s.file.Read(one[:])
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if n != 0 {
		return ErrChanged
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != s.SHA256 {
		return ErrChanged
	}
	runTestHook("source-stream-before-final-revalidate")
	return s.Revalidate()
}

func (s *Source) hashExact() (string, error) {
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.CopyN(h, s.file, s.Size)
	if err != nil {
		return "", fmt.Errorf("%w: expected %d bytes, read %d: %v", ErrChanged, s.Size, n, err)
	}
	var one [1]byte
	read, err := s.file.Read(one[:])
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if read != 0 {
		return "", ErrChanged
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// TempDir is an opened, owner-private 0700 temporary directory. The directory
// descriptor, not an untrusted path lookup, is used for all children.
type TempDir struct {
	fd   int
	root string
}

// Root is the configured absolute temporary root.
func (d *TempDir) Root() string { return d.root }

// Close closes the directory descriptor.
func (d *TempDir) Close() error {
	if d == nil || d.fd < 0 {
		return nil
	}
	err := unix.Close(d.fd)
	d.fd = -1
	return err
}

// OpenTempDir opens an existing owner-owned directory with exactly mode 0700,
// rejecting symlinks in every component.
func OpenTempDir(path string) (*TempDir, error) {
	if err := absoluteLexical(path); err != nil {
		return nil, err
	}
	fd, err := openDirectory(path)
	if err != nil {
		return nil, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != uint32(os.Geteuid()) || st.Mode&0777 != 0700 {
		unix.Close(fd)
		return nil, ErrUnsafePath
	}
	return &TempDir{fd: fd, root: path}, nil
}

// TempFile is an unpublished generated 0600 temporary file. Callers write and
// Sync File before publishing any durable row, then Close it.
type TempFile struct {
	File *os.File
	Name string
	Path string
}

// Create creates a random, exclusive generated child with 128 bits of entropy.
func (d *TempDir) Create() (*TempFile, error) {
	if d == nil || d.fd < 0 {
		return nil, os.ErrClosed
	}
	for range 16 {
		name, err := generatedName()
		if err != nil {
			return nil, err
		}
		fd, err := unix.Openat(d.fd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return nil, err
		}
		f := os.NewFile(uintptr(fd), filepath.Join(d.root, name))
		if err := verifyPrivateFD(fd); err != nil {
			f.Close()
			unix.Unlinkat(d.fd, name, 0)
			return nil, err
		}
		return &TempFile{File: f, Name: name, Path: filepath.Join(d.root, name)}, nil
	}
	return nil, fmt.Errorf("temporary name collision")
}

// Open verifies and opens a generated child without following it.
func (d *TempDir) Open(name string) (*os.File, error) {
	if d == nil || d.fd < 0 {
		return nil, os.ErrClosed
	}
	if !IsGeneratedName(name) {
		return nil, ErrInvalidTemporary
	}
	fd, err := unix.Openat(d.fd, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, classifyOpen(err)
	}
	f := os.NewFile(uintptr(fd), filepath.Join(d.root, name))
	if err := verifyPrivateFD(fd); err != nil {
		f.Close()
		return nil, err
	}
	current, err := statAtNoFollow(d.fd, name)
	if err != nil {
		f.Close()
		return nil, err
	}
	var held unix.Stat_t
	if err := unix.Fstat(fd, &held); err != nil || !sameIdentity(held, current) {
		f.Close()
		return nil, ErrChanged
	}
	return f, nil
}

// Remove unlinks only a currently verified generated private child. It never
// follows a final symlink and refuses a pathname replaced after opening.
func (d *TempDir) Remove(name string) error {
	f, err := d.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	var held unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &held); err != nil {
		return err
	}
	current, err := statAtNoFollow(d.fd, name)
	if err != nil {
		return err
	}
	if !sameIdentity(held, current) {
		return ErrChanged
	}
	return unix.Unlinkat(d.fd, name, 0)
}

// ReconcileReport is a path-free summary of one temporary directory sweep.
type ReconcileReport struct {
	Removed int
	Live    int
	Missing int
	Unsafe  int
	Unknown int
}

// Reconcile removes only recognized generated orphan files. known contains the
// generated names represented by live durable transfer rows. Unknown entries
// and malformed/private-unsafe entries are reported, never removed.
func (d *TempDir) Reconcile(known map[string]struct{}) (ReconcileReport, error) {
	var report ReconcileReport
	if d == nil || d.fd < 0 {
		return report, os.ErrClosed
	}
	dup, err := unix.Dup(d.fd)
	if err != nil {
		return report, err
	}
	listing := os.NewFile(uintptr(dup), d.root)
	entries, err := listing.ReadDir(-1)
	closeErr := listing.Close()
	if err != nil {
		return report, err
	}
	if closeErr != nil {
		return report, closeErr
	}
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !IsGeneratedName(name) {
			report.Unknown++
			continue
		}
		seen[name] = struct{}{}
		if _, live := known[name]; live {
			if f, err := d.Open(name); err != nil {
				report.Unsafe++
			} else {
				f.Close()
				report.Live++
			}
			continue
		}
		if err := d.Remove(name); err != nil {
			report.Unsafe++
		} else {
			report.Removed++
		}
	}
	for name := range known {
		if !IsGeneratedName(name) {
			report.Unsafe++
			continue
		}
		if _, ok := seen[name]; !ok {
			report.Missing++
		}
	}
	return report, nil
}

// IsGeneratedName reports whether name is exactly a generated Mosscap temp
// basename. It deliberately accepts no separators or external filenames.
func IsGeneratedName(name string) bool {
	if !strings.HasPrefix(name, generatedPrefix) || len(name) != len(generatedPrefix)+32 {
		return false
	}
	for i := len(generatedPrefix); i < len(name); i++ {
		if (name[i] < '0' || name[i] > '9') && (name[i] < 'a' || name[i] > 'f') {
			return false
		}
	}
	return true
}

// PlaceOutput copies a verified generated temporary source into a 0600 random
// sibling staging file and atomically installs it at destination. It never
// follows destination parent/final symlinks. force may replace only an observed
// regular destination; it is not needed to create a new destination.
func PlaceOutput(sourcePath, destination string, size int64, sha256 string, force bool) error {
	if err := absoluteLexical(sourcePath); err != nil {
		return err
	}
	if err := absoluteLexical(destination); err != nil {
		return err
	}
	if size < 0 || !validDigest(sha256) {
		return ErrInvalidTemporary
	}
	tmp, err := OpenTempDir(filepath.Dir(sourcePath))
	if err != nil {
		return err
	}
	defer tmp.Close()
	name := filepath.Base(sourcePath)
	if !IsGeneratedName(name) {
		return ErrInvalidTemporary
	}
	src, err := tmp.Open(name)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := checkExactTemp(src, size, sha256); err != nil {
		return err
	}

	parent, base := filepath.Dir(destination), filepath.Base(destination)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return ErrInvalidPath
	}
	dirfd, err := openDirectory(parent)
	if err != nil {
		return err
	}
	defer unix.Close(dirfd)
	stageName, stage, err := createOutputStage(dirfd)
	if err != nil {
		return err
	}
	installed := false
	defer func() {
		stage.Close()
		if !installed {
			_ = unix.Unlinkat(dirfd, stageName, 0)
		}
	}()
	if err := copyExact(src, stage, size, sha256); err != nil {
		return err
	}
	if err := checkTempPath(tmp, name, src); err != nil {
		return err
	}
	if err := stage.Sync(); err != nil {
		return err
	}
	runTestHook("output-before-install")
	if err := installOutput(dirfd, stageName, base, force); err != nil {
		return err
	}
	installed = true
	if err := syncDir(dirfd); err != nil {
		return err
	}
	return nil
}

func installOutput(dirfd int, stage, destination string, force bool) error {
	if !force {
		return renameNoReplace(dirfd, stage, destination)
	}
	var old unix.Stat_t
	err := unix.Fstatat(dirfd, destination, &old, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return renameNoReplace(dirfd, stage, destination)
	}
	if err != nil {
		return err
	}
	if old.Mode&unix.S_IFMT != unix.S_IFREG {
		return ErrUnsafeFileType
	}
	runTestHook("output-force-after-observe")
	if err := unix.Renameat2(dirfd, stage, dirfd, destination, unix.RENAME_EXCHANGE); err != nil {
		return renameError(err)
	}
	// The old destination is now at stage. Comparing it after exchange detects
	// a final-component rename race without ever opening/following that entry.
	var moved unix.Stat_t
	if err := unix.Fstatat(dirfd, stage, &moved, unix.AT_SYMLINK_NOFOLLOW); err != nil || !sameIdentity(old, moved) {
		// Best-effort rollback is still an atomic name exchange. If a same-UID
		// actor races it, return an error rather than claiming safe replacement.
		_ = unix.Renameat2(dirfd, stage, dirfd, destination, unix.RENAME_EXCHANGE)
		return ErrChanged
	}
	if err := unix.Unlinkat(dirfd, stage, 0); err != nil {
		return err
	}
	return nil
}

func renameNoReplace(dirfd int, source, target string) error {
	err := unix.Renameat2(dirfd, source, dirfd, target, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EEXIST) {
		return ErrDestinationExists
	}
	return renameError(err)
}

func renameError(err error) error {
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
		return fmt.Errorf("%w: renameat2", ErrUnsupported)
	}
	return err
}

func createOutputStage(dirfd int) (string, *os.File, error) {
	for range 16 {
		name, err := outputName()
		if err != nil {
			return "", nil, err
		}
		fd, err := unix.Openat(dirfd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		return name, os.NewFile(uintptr(fd), name), nil
	}
	return "", nil, errors.New("output staging name collision")
}

func copyExact(src *os.File, dst *os.File, size int64, want string) error {
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.CopyN(io.MultiWriter(dst, h), src, size)
	if err != nil {
		return fmt.Errorf("%w: expected %d bytes, copied %d: %v", ErrChanged, size, n, err)
	}
	var one [1]byte
	read, err := src.Read(one[:])
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if read != 0 || hex.EncodeToString(h.Sum(nil)) != want {
		return ErrChanged
	}
	return checkExactTemp(src, size, want)
}

func checkTempPath(d *TempDir, name string, f *os.File) error {
	var held unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &held); err != nil {
		return err
	}
	current, err := statAtNoFollow(d.fd, name)
	if err != nil {
		return err
	}
	if !sameIdentity(held, current) {
		return ErrChanged
	}
	return nil
}

func checkExactTemp(f *os.File, size int64, want string) error {
	var before unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &before); err != nil {
		return err
	}
	if err := verifyPrivateStat(before); err != nil || before.Size != size {
		return ErrInvalidTemporary
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.CopyN(h, f, size)
	if err != nil || n != size {
		return ErrChanged
	}
	var one [1]byte
	read, err := f.Read(one[:])
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if read != 0 || hex.EncodeToString(h.Sum(nil)) != want {
		return ErrChanged
	}
	var after unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &after); err != nil || !sameIdentity(before, after) || before.Size != after.Size {
		return ErrChanged
	}
	return nil
}

func openDirectory(path string) (int, error) {
	if err := absoluteLexical(path); err != nil {
		return -1, err
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	for _, component := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if component == "" {
			continue
		}
		next, err := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		unix.Close(fd)
		if err != nil {
			return -1, classifyOpen(err)
		}
		fd = next
	}
	return fd, nil
}

func statNoFollow(path string) (unix.Stat_t, error) {
	parent, name := filepath.Dir(path), filepath.Base(path)
	fd, err := openDirectory(parent)
	if err != nil {
		return unix.Stat_t{}, err
	}
	defer unix.Close(fd)
	return statAtNoFollow(fd, name)
}

func statAtNoFollow(dirfd int, name string) (unix.Stat_t, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return st, classifyOpen(err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return st, ErrUnsafeFileType
	}
	return st, nil
}

func verifyPrivateFD(fd int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	return verifyPrivateStat(st)
}

func verifyPrivateStat(st unix.Stat_t) error {
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != uint32(os.Geteuid()) || st.Mode&0777 != 0600 || st.Nlink != 1 {
		return ErrInvalidTemporary
	}
	return nil
}

func sameIdentity(a, b unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode == b.Mode
}

func absoluteLexical(path string) error {
	if path == "" || strings.IndexByte(path, 0) >= 0 || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrInvalidPath
	}
	return nil
}

func classifyOpen(err error) error {
	if errors.Is(err, unix.ELOOP) {
		return ErrUnsafePath
	}
	if errors.Is(err, unix.ENOTDIR) {
		return ErrUnsafePath
	}
	if errors.Is(err, unix.ENXIO) {
		return ErrUnsafeFileType
	}
	return err
}

func generatedName() (string, error) { return randomName(generatedPrefix) }
func outputName() (string, error)    { return randomName(".mosscap-output-") }

func randomName(prefix string) (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(entropy[:]), nil
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func syncDir(fd int) error {
	err := unix.Fsync(fd)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EROFS) {
		return nil
	}
	return err
}
