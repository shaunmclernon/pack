package pack

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"

	"github.com/buildpack/packs"
	"github.com/buildpack/packs/img"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func readImage(repoName string, useDaemon bool) (v1.Image, error) {
	repoStore, err := repoStore(repoName, useDaemon)
	if err != nil {
		return nil, err
	}

	origImage, err := repoStore.Image()
	if err != nil {
		// Assume error is due to non-existent image
		return nil, nil
	}
	if _, err := origImage.RawManifest(); err != nil {
		// Assume error is due to non-existent image
		// This is necessary for registries
		return nil, nil
	}

	return origImage, nil
}

func repoStore(repoName string, useDaemon bool) (img.Store, error) {
	newRepoStore := img.NewRegistry
	if useDaemon {
		newRepoStore = img.NewDaemon
	}
	repoStore, err := newRepoStore(repoName)
	if err != nil {
		return nil, packs.FailErr(err, "access", repoName)
	}
	return repoStore, nil
}

func createTarReader(fsDir, tarDir string) (io.Reader, chan error) {
	r, w := io.Pipe()
	errChan := make(chan error, 1)

	go func() {
		defer w.Close()
		tw := tar.NewWriter(w)
		defer tw.Close()

		err := filepath.Walk(fsDir, func(file string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if fi.Mode().IsDir() {
				return nil
			}
			relPath, err := filepath.Rel(fsDir, file)
			if err != nil {
				return err
			}

			var header *tar.Header
			if fi.Mode()&os.ModeSymlink != 0 {
				target, err := os.Readlink(file)
				if err != nil {
					return err
				}
				header, err = tar.FileInfoHeader(fi, target)
				if err != nil {
					return err
				}
			} else {
				header, err = tar.FileInfoHeader(fi, fi.Name())
				if err != nil {
					return err
				}
			}
			header.Name = filepath.Join(tarDir, relPath)

			if err := tw.WriteHeader(header); err != nil {
				return err
			}
			if fi.Mode().IsRegular() {
				f, err := os.Open(file)
				if err != nil {
					return err
				}
				defer f.Close()
				if _, err := io.Copy(tw, f); err != nil {
					return err
				}
			}
			return nil
		})
		tw.Close()
		w.Close()
		errChan <- err
	}()

	return r, errChan
}
