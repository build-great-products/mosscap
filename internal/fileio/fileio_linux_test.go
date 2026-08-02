//go:build linux

package fileio

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOpenSourceRegularBinaryZeroAndSparse(t *testing.T) {
	dir := privateDir(t)
	cases := []struct {
		name string
		data []byte
	}{
		{"text", []byte("hello\n")},
		{"binary", []byte{0, 1, 2, 255, 0, 4}},
		{"zero", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name)
			if err := os.WriteFile(path, tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			src, err := OpenSource(path, 64)
			if err != nil {
				t.Fatal(err)
			}
			defer src.Close()
			var got bytes.Buffer
			if err := src.StreamTo(&got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), tc.data) || src.Size != int64(len(tc.data)) || src.SHA256 != digest(tc.data) {
				t.Fatalf("got=%x size=%d hash=%q", got.Bytes(), src.Size, src.SHA256)
			}
		})
	}

	sparse := filepath.Join(dir, "sparse")
	f, err := os.OpenFile(sparse, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(8192); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("end"), 8189); err != nil {
		t.Fatal(err)
	}
	f.Close()
	src, err := OpenSource(sparse, 8192)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	var got bytes.Buffer
	if err := src.StreamTo(&got); err != nil {
		t.Fatal(err)
	}
	if got.Len() != 8192 || !bytes.Equal(got.Bytes()[8189:], []byte("end")) {
		t.Fatal("sparse source was not streamed exactly")
	}
	if _, err := OpenSource(sparse, 8191); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized sparse error = %v", err)
	}
}

func TestOpenSourceRejectsUnsafePathsAndTypes(t *testing.T) {
	dir := privateDir(t)
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(dir, "final-link")
	if err := os.Symlink(file, final); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSource(final, 10); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("final symlink = %v", err)
	}
	parent := filepath.Join(dir, "parent")
	if err := os.Symlink(dir, parent); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSource(filepath.Join(parent, "file"), 10); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("parent symlink = %v", err)
	}
	if _, err := OpenSource(dir, 10); !errors.Is(err, ErrUnsafeFileType) {
		t.Fatalf("directory = %v", err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSource(fifo, 10); !errors.Is(err, ErrUnsafeFileType) {
		t.Fatalf("fifo = %v", err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "socket"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if _, err := OpenSource(filepath.Join(dir, "socket"), 10); err == nil {
		t.Fatal("accepted socket")
	}
	if _, err := OpenSource("relative", 10); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("relative = %v", err)
	}
}

func TestSourceDeterministicMutationRaces(t *testing.T) {
	dir := privateDir(t)
	path := filepath.Join(dir, "source")
	if err := os.WriteFile(path, []byte("abcdef"), 0600); err != nil {
		t.Fatal(err)
	}
	withHook(t, func(phase string) {
		if phase == "source-open-after-hash" {
			if err := os.Truncate(path, 2); err != nil {
				t.Fatal(err)
			}
		}
	})
	if _, err := OpenSource(path, 10); !errors.Is(err, ErrChanged) {
		t.Fatalf("truncate while opening = %v", err)
	}
	testHook = nil
	if err := os.WriteFile(path, []byte("abcdef"), 0600); err != nil {
		t.Fatal(err)
	}
	src, err := OpenSource(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	withHook(t, func(phase string) {
		if phase == "source-stream-after-revalidate" {
			if err := os.Truncate(path, 2); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err := src.StreamTo(io.Discard); !errors.Is(err, ErrChanged) {
		t.Fatalf("early EOF = %v", err)
	}
	testHook = nil
	if err := os.WriteFile(path, []byte("abcdef"), 0600); err != nil {
		t.Fatal(err)
	}
	src.Close()
	src, err = OpenSource(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	withHook(t, func(phase string) {
		if phase == "source-stream-after-revalidate" {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write([]byte("g")); err != nil {
				f.Close()
				t.Fatal(err)
			}
			f.Close()
		}
	})
	if err := src.StreamTo(io.Discard); !errors.Is(err, ErrChanged) {
		t.Fatalf("extra byte = %v", err)
	}
	testHook = nil
	if err := os.WriteFile(path, []byte("abcdef"), 0600); err != nil {
		t.Fatal(err)
	}
	src.Close()
	src, err = OpenSource(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	withHook(t, func(phase string) {
		if phase == "source-stream-before-final-revalidate" {
			replacement := filepath.Join(dir, "replacement")
			if err := os.WriteFile(replacement, []byte("abcdef"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacement, path); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err := src.StreamTo(io.Discard); !errors.Is(err, ErrChanged) {
		t.Fatalf("rename before completion = %v", err)
	}
}

func TestTempDirCreationVerificationAndReconciliation(t *testing.T) {
	root := privateDir(t)
	dir, err := OpenTempDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	tmp, err := dir.Create()
	if err != nil {
		t.Fatal(err)
	}
	if !IsGeneratedName(tmp.Name) || filepath.Dir(tmp.Path) != root {
		t.Fatal("bad generated temporary name")
	}
	if err := tmp.File.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(tmp.Path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("temp permissions = %v, %v", info, err)
	}
	if _, err := dir.Open(tmp.Name); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp.Path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := dir.Open(tmp.Name); !errors.Is(err, ErrInvalidTemporary) {
		t.Fatalf("wrong mode = %v", err)
	}
	if err := os.Chmod(tmp.Path, 0600); err != nil {
		t.Fatal(err)
	}
	orphan, err := dir.Create()
	if err != nil {
		t.Fatal(err)
	}
	orphan.File.Close()
	unsafe, err := generatedName()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(tmp.Path, filepath.Join(root, unsafe)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "unrecognized"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := dir.Reconcile(map[string]struct{}{tmp.Name: {}})
	if err != nil {
		t.Fatal(err)
	}
	if report.Live != 1 || report.Removed != 1 || report.Unsafe != 1 || report.Unknown != 1 {
		t.Fatalf("report = %+v", report)
	}
	if _, err := os.Lstat(orphan.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan exists: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, unsafe)); err != nil {
		t.Fatalf("unsafe symlink was removed: %v", err)
	}
}

func TestPlaceOutputExclusiveForceAndCleanup(t *testing.T) {
	root := privateDir(t)
	tmpDir, err := OpenTempDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer tmpDir.Close()
	contents := []byte("validated source")
	tmp, err := tmpDir.Create()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.File.Write(contents); err != nil {
		t.Fatal(err)
	}
	if err := tmp.File.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := tmp.File.Close(); err != nil {
		t.Fatal(err)
	}
	outDir := privateDir(t)
	dest := filepath.Join(outDir, "result")
	if err := PlaceOutput(tmp.Path, dest, int64(len(contents)), digest(contents), false); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, contents) {
		t.Fatalf("destination = %q, %v", got, err)
	}
	if info, err := os.Stat(dest); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("destination mode = %v, %v", info, err)
	}
	if err := PlaceOutput(tmp.Path, dest, int64(len(contents)), digest(contents), false); !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("exclusive conflict = %v", err)
	}
	if err := os.WriteFile(dest, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := PlaceOutput(tmp.Path, dest, int64(len(contents)), digest(contents), true); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(dest)
	if !bytes.Equal(got, contents) {
		t.Fatal("force did not atomically replace regular destination")
	}
	target := filepath.Join(outDir, "target")
	if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(outDir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := PlaceOutput(tmp.Path, link, int64(len(contents)), digest(contents), true); !errors.Is(err, ErrUnsafeFileType) {
		t.Fatalf("force symlink = %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "unchanged" {
		t.Fatal("symlink target changed")
	}
	folder := filepath.Join(outDir, "folder")
	if err := os.Mkdir(folder, 0700); err != nil {
		t.Fatal(err)
	}
	if err := PlaceOutput(tmp.Path, folder, int64(len(contents)), digest(contents), true); !errors.Is(err, ErrUnsafeFileType) {
		t.Fatalf("force directory = %v", err)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".mosscap-output-") {
			t.Fatalf("staging residue %q", entry.Name())
		}
	}
}

func TestPlaceOutputRejectsTempModeAndDestinationRace(t *testing.T) {
	root := privateDir(t)
	dir, err := OpenTempDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	tmp, err := dir.Create()
	if err != nil {
		t.Fatal(err)
	}
	tmp.File.Write([]byte("x"))
	tmp.File.Close()
	if err := os.Chmod(tmp.Path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := PlaceOutput(tmp.Path, filepath.Join(privateDir(t), "out"), 1, digest([]byte("x")), false); !errors.Is(err, ErrInvalidTemporary) {
		t.Fatalf("wrong temp mode = %v", err)
	}
	if err := os.Chmod(tmp.Path, 0600); err != nil {
		t.Fatal(err)
	}
	outDir := privateDir(t)
	dest := filepath.Join(outDir, "out")
	withHook(t, func(phase string) {
		if phase == "output-before-install" {
			if err := os.WriteFile(dest, []byte("racer"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err := PlaceOutput(tmp.Path, dest, 1, digest([]byte("x")), false); !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("destination race = %v", err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "racer" {
		t.Fatal("exclusive install replaced racing destination")
	}
	// Exchange-based force installation must roll back if the final component
	// changes after it was observed, rather than replacing a symlink race.
	if err := os.WriteFile(dest, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outDir, "target")
	if err := os.WriteFile(target, []byte("target"), 0600); err != nil {
		t.Fatal(err)
	}
	withHook(t, func(phase string) {
		if phase == "output-force-after-observe" {
			if err := os.Remove(dest); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, dest); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err := PlaceOutput(tmp.Path, dest, 1, digest([]byte("x")), true); !errors.Is(err, ErrChanged) {
		t.Fatalf("force replacement race = %v", err)
	}
	info, err := os.Lstat(dest)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("force race destination = %v, %v", info, err)
	}
	if got, _ := os.ReadFile(target); string(got) != "target" {
		t.Fatal("force race changed symlink target")
	}
}

func privateDir(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func withHook(t *testing.T, hook func(string)) {
	t.Helper()
	old := testHook
	testHook = hook
	t.Cleanup(func() { testHook = old })
}
