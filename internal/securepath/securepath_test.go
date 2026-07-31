package securepath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateDirectoryAndFileChecks(t *testing.T) {
	home := t.TempDir()
	d := filepath.Join(home, "private")
	if err := EnsurePrivateDir(home, d); err != nil {
		t.Fatal(err)
	}
	if err := CheckDir(d); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(d, "config")
	if err := os.WriteFile(f, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckFile(f); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f, 0644); err != nil {
		t.Fatal(err)
	}
	if err := CheckFile(f); err == nil {
		t.Fatal("accepted public file")
	}
}
func TestReadRegularFileRejectsSymlinkAndOversize(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "text")
	if err := os.WriteFile(file, []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRegularFile(file, 4); err == nil {
		t.Fatal("accepted oversized input")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRegularFile(link, 10); err == nil {
		t.Fatal("accepted symlink input")
	}
	got, err := ReadRegularFile(file, 5)
	if err != nil || string(got) != "hello" {
		t.Fatalf("ReadRegularFile = %q, %v", got, err)
	}
	replacement := filepath.Join(dir, "replacement")
	if err := os.WriteFile(replacement, []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegularFile(file, 5, func() {
		if err := os.Rename(replacement, file); err != nil {
			t.Fatal(err)
		}
	}); err == nil {
		t.Fatal("accepted a file replaced during read")
	}
	if _, err := readRegularFile(file, 5, func() {
		if err := os.Truncate(file, 4); err != nil {
			t.Fatal(err)
		}
	}); err == nil {
		t.Fatal("accepted a file whose size changed during read")
	}
}

func TestRejectsSymlinkAndEscapingDescendant(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDir(home, filepath.Join(link, "child")); err == nil {
		t.Fatal("accepted symlink component")
	}
	if Descendant(filepath.Join(home, "state"), filepath.Join(home, "other", "socket")) {
		t.Fatal("accepted escaped descendant")
	}
}
