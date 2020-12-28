package main

import (
	"bytes"
	"debug/elf"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/cavaliercoder/go-cpio"
	"github.com/google/renameio"
	"github.com/klauspost/compress/zstd"
)

type Image struct {
	file       *renameio.PendingFile
	compressor io.Closer
	out        *cpio.Writer
	contains   map[string]bool // whether image contains the file
}

func NewImage(path string) (*Image, error) {
	file, err := renameio.TempFile("", path)
	if err != nil {
		return nil, fmt.Errorf("new image: %v", err)
	}
	if err := file.Chmod(0644); err != nil {
		return nil, err
	}

	compressor, err := zstd.NewWriter(file)
	if err != nil {
		return nil, err
	}
	out := cpio.NewWriter(compressor)

	return &Image{
		file:       file,
		compressor: compressor,
		out:        out,
		contains:   make(map[string]bool),
	}, nil
}

func (img *Image) Cleanup() {
	_ = img.out.Close()
	_ = img.compressor.Close()
	_ = img.file.Cleanup()
}

func (img *Image) Close() error {
	if err := img.out.Close(); err != nil {
		return err
	}
	if err := img.compressor.Close(); err != nil {
		return err
	}
	return img.file.CloseAtomicallyReplace()
}

// AppendDir appends directory chain with its parents recursively
func (img *Image) AppendDir(dir string) error {
	if img.contains[dir] {
		return nil
	}

	if dir != "/" {
		parent := path.Dir(dir)
		if err := img.AppendDir(parent); err != nil {
			return err
		}
	}

	hdr := &cpio.Header{
		Name: strings.TrimPrefix(dir, "/"),
		Mode: cpio.FileMode(0755) | cpio.ModeDir,
	}
	if err := img.out.WriteHeader(hdr); err != nil {
		return fmt.Errorf("AppendDir: %v", err)
	}
	img.contains[dir] = true
	return nil
}

func (img *Image) AppendContent(content []byte, mode os.FileMode, dest string) error {
	if img.contains[dest] {
		return fmt.Errorf("Trying to add a file %s but it already been added to the image", dest)
	}

	// append parent dirs first
	if err := img.AppendDir(path.Dir(dest)); err != nil {
		return err
	}

	hdr := &cpio.Header{
		Name: strings.TrimPrefix(dest, "/"),
		Mode: cpio.FileMode(mode) | cpio.ModeRegular,
		Size: int64(len(content)),
	}
	if err := img.out.WriteHeader(hdr); err != nil {
		return fmt.Errorf("AppendFile: %v", err)
	}
	if _, err := img.out.Write(content); err != nil {
		return err
	}
	img.contains[dest] = true

	const minimalELFSize = 64 // 64 bytes is a size of 64bit ELF header
	if len(content) < minimalELFSize {
		return nil
	}
	// now check if the added file was ELF, then we scan the ELF dependencies and add them as well
	ef, err := elf.NewFile(bytes.NewReader(content))
	if err != nil {
		if _, ok := err.(*elf.FormatError); !ok || !strings.HasPrefix(err.Error(), "bad magic number") {
			// not an ELF
			return fmt.Errorf("cannot open ELF file: %v", err)
		} else {
			return nil
		}
	}
	defer ef.Close()

	if err := img.AppendElfDependencies(ef); err != nil {
		return fmt.Errorf("AppendFile: %v", err)
	}

	return nil
}

// AppendFile appends the file + its dependencies to the ramfs file
func (img *Image) AppendFile(fn string) error {
	fn = path.Clean(fn)

	if err := img.AppendDir(path.Dir(fn)); err != nil {
		return err
	}

	fi, err := os.Lstat(fn)
	if err != nil {
		return fmt.Errorf("AppendFile: %v", err)
	}

	if fi.Mode()&os.ModeSymlink == os.ModeSymlink {
		linkTarget, err := os.Readlink(fn)
		if err != nil {
			return fmt.Errorf("AppendFile: %v", err)
		}

		hdr := &cpio.Header{
			Name: strings.TrimPrefix(fn, "/"),
			Mode: cpio.FileMode(fi.Mode().Perm()) | cpio.ModeSymlink,
			Size: int64(len(linkTarget)),
		}
		if err := img.out.WriteHeader(hdr); err != nil {
			return fmt.Errorf("AppendFile: %v", err)
		}
		if _, err := img.out.Write([]byte(linkTarget)); err != nil {
			return fmt.Errorf("AppendFile: %v", err)
		}
		img.contains[fn] = true

		// now add the link target as well
		linkTarget, err = filepath.Abs(linkTarget)
		if err != nil {
			return fmt.Errorf("AppendFile: %v", err)
		}
		if err := img.AppendFile(linkTarget); err != nil {
			return fmt.Errorf("AppendFile: %v", err)
		}
	} else {
		// file
		content, err := ioutil.ReadFile(fn)
		if err != nil {
			return fmt.Errorf("AppendFile: %v", err)
		}

		if err := img.AppendContent(content, fi.Mode().Perm(), fn); err != nil {
			return fmt.Errorf("AppendFile: %v", err)
		}
	}

	return nil
}

func elfSectionContent(s *elf.Section) (string, error) {
	b, err := s.Data()
	if err != nil {
		return "", err
	}
	return string(b[:bytes.IndexByte(b, '\x00')]), nil
}

func (img *Image) AppendElfDependencies(ef *elf.File) error {
	// TODO: use ef.DynString(elf.DT_RPATH) to calculate path to the loaded library
	// or maybe we can parse /etc/ld.so.cache to get location for all libs?

	libs, err := ef.ImportedLibraries()
	if err != nil {
		return fmt.Errorf("AppendElfDependencies: %v", err)
	}

	is := ef.Section(".interp")
	if is != nil {
		interp, err := elfSectionContent(is)
		if err != nil {
			return err
		}
		libs = append(libs, interp)
	}

	for _, p := range libs {
		if !filepath.IsAbs(p) {
			p = filepath.Join("/usr/lib", p)
		}
		err := img.AppendFile(p)
		if err != nil {
			return fmt.Errorf("AppendElfDependencies: %v", err)
		}
	}
	return nil
}