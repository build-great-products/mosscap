package filetransfer

import "github.com/stayradiated/mosscap/internal/fileio"

// FileIOFiles adapts the reviewed descriptor-based fileio primitives to the
// service lifecycle interfaces. It owns neither the directory nor its Close;
// server composition closes the underlying TempDir after Service.Close.
type FileIOFiles struct{ dir *fileio.TempDir }

// NewFileIOFiles adapts an already-open owner-private temporary directory.
func NewFileIOFiles(dir *fileio.TempDir) *FileIOFiles { return &FileIOFiles{dir: dir} }

func (f *FileIOFiles) Root() string { return f.dir.Root() }
func (f *FileIOFiles) Create() (TemporaryFile, error) {
	tmp, err := f.dir.Create()
	if err != nil {
		return nil, err
	}
	return fileIOTemporaryFile{tmp: tmp}, nil
}
func (f *FileIOFiles) Remove(name string) error { return f.dir.Remove(name) }
func (f *FileIOFiles) Exists(name string) error {
	file, err := f.dir.Open(name)
	if err != nil {
		return err
	}
	return file.Close()
}
func (f *FileIOFiles) Reconcile(known map[string]struct{}) (fileio.ReconcileReport, error) {
	return f.dir.Reconcile(known)
}

type fileIOTemporaryFile struct{ tmp *fileio.TempFile }

func (f fileIOTemporaryFile) Write(p []byte) (int, error) { return f.tmp.File.Write(p) }
func (f fileIOTemporaryFile) Sync() error                 { return f.tmp.File.Sync() }
func (f fileIOTemporaryFile) Close() error                { return f.tmp.File.Close() }
func (f fileIOTemporaryFile) Name() string                { return f.tmp.Name }
func (f fileIOTemporaryFile) Path() string                { return f.tmp.Path }

var _ TemporaryFiles = (*FileIOFiles)(nil)
var _ TemporaryFile = fileIOTemporaryFile{}
