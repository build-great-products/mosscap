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
