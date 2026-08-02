//go:build !linux

package fileio

import (
	"errors"
	"io"
	"os"
)

var (
	ErrInvalidPath       = errors.New("invalid file path")
	ErrUnsafePath        = errors.New("unsafe file path")
	ErrUnsafeFileType    = errors.New("unsafe file type")
	ErrTooLarge          = errors.New("file too large")
	ErrChanged           = errors.New("file changed")
	ErrDestinationExists = errors.New("destination exists")
	ErrUnsupported       = errors.New("unsupported file I/O primitive")
	ErrInvalidTemporary  = errors.New("unsafe temporary file")
)

// Source is unavailable off Linux because this package deliberately has no
// check-then-follow fallback for its filesystem security contract.
type Source struct {
	Size   int64
	SHA256 string
}

func (s *Source) Path() string                  { return "" }
func (s *Source) Device() uint64                { return 0 }
func (s *Source) Inode() uint64                 { return 0 }
func (s *Source) MtimeNS() int64                { return 0 }
func (s *Source) Close() error                  { return nil }
func (s *Source) Revalidate() error             { return ErrUnsupported }
func (s *Source) StreamTo(io.Writer) error      { return ErrUnsupported }
func OpenSource(string, int64) (*Source, error) { return nil, ErrUnsupported }

type TempDir struct{}

func (d *TempDir) Root() string            { return "" }
func (d *TempDir) Close() error            { return nil }
func OpenTempDir(string) (*TempDir, error) { return nil, ErrUnsupported }

type TempFile struct {
	File *os.File
	Name string
	Path string
}

func (d *TempDir) Create() (*TempFile, error)    { return nil, ErrUnsupported }
func (d *TempDir) Open(string) (*os.File, error) { return nil, ErrUnsupported }
func (d *TempDir) Remove(string) error           { return ErrUnsupported }

type ReconcileReport struct {
	Removed int
	Live    int
	Missing int
	Unsafe  int
	Unknown int
}

func (d *TempDir) Reconcile(map[string]struct{}) (ReconcileReport, error) {
	return ReconcileReport{}, ErrUnsupported
}
func IsGeneratedName(string) bool                           { return false }
func PlaceOutput(string, string, int64, string, bool) error { return ErrUnsupported }
