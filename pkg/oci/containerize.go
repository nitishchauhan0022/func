package oci

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// languageLayerBuilder builds the layer for the given language whuch may
// be different from one platform to another.  For example, this is the
// layer in the image which contains the Go cross-compiled binary.
type languageLayerBuilder interface {
	Build(*buildConfig, v1.Platform) (v1.Descriptor, v1.Layer, error)
}

func newLanguageLayerBuilder(cfg *buildConfig) (l languageLayerBuilder, err error) {
	switch cfg.f.Runtime {
	case "go":
		l = goLayerBuilder{}
	case "python":
		// Likely the next to be supported after Go
		err = errors.New("functions written in Python are not yet supported by the host builder")
	case "node":
		// Likely the next to be supported after Python
		err = errors.New("functions written in Node are not yet supported by the host builder")
	case "rust":
		// Likely the next to be supprted after Node
		err = errors.New("functions written in Rust are not yet supported by the host builder")
	default:
		// Others are not likely to be supported in the near future without
		// increased contributions.
		err = fmt.Errorf("the language runtime '%v' is not a recognized language by the host builder", cfg.f.Runtime)
	}
	return
}

// containerize the scaffolded project by creating and writing an OCI
// conformant directory structure into the functions .func/builds directory.
// The source code to be containerized is indicated by cfg.dir
func containerize(cfg *buildConfig) (err error) {
	// Create the required directories: oci/blobs/sha256
	if err = os.MkdirAll(cfg.blobsDir(), os.ModePerm); err != nil {
		return
	}

	// Create the static, required oci-layout metadata file
	if err = os.WriteFile(path(cfg.ociDir(), "oci-layout"),
		[]byte(`{ "imageLayoutVersion": "1.0.0" }`), os.ModePerm); err != nil {
		return
	}

	// Create the data layer and its descriptor
	dataDesc, dataLayer, err := newDataLayer(cfg) // shared
	if err != nil {
		return
	}

	// TODO: if the base image is not provided, create a certificates layer
	// which includes root certificates such that the resultant container
	// can validate SSL (make HTTPS requests)
	/*
		certsDesc, certsLayer, err := newCerts(cfg) // shared
		if err != nil {
			return
		}
	*/

	// Create an image for each platform consisting of the shared data layer
	// and an os/platform specific layer.
	imageDescs := []v1.Descriptor{}
	for _, p := range defaultPlatforms { // TODO: Configurable additions.
		imageDesc, err := newImage(cfg, dataDesc, dataLayer, p, cfg.verbose)
		if err != nil {
			return err
		}
		imageDescs = append(imageDescs, imageDesc)
	}

	// Create the Image Index which enumerates all images contained within
	// the container.
	_, err = newImageIndex(cfg, imageDescs)
	return
}

// newDataLayer creates the shared data layer in the container file hierarchy and
// returns both its descriptor and layer metadata.
func newDataLayer(cfg *buildConfig) (desc v1.Descriptor, layer v1.Layer, err error) {

	// Create the data tarball
	// TODO: try WithCompressedCaching?
	source := cfg.f.Root // The source is the function's entire filesystem
	target := path(cfg.buildDir(), "datalayer.tar.gz")

	if err = newDataTarball(source, target, defaultIgnored, cfg.verbose); err != nil {
		return
	}

	// Layer
	if layer, err = tarball.LayerFromFile(target); err != nil {
		return
	}

	// Descriptor
	if desc, err = newDescriptor(layer); err != nil {
		return
	}

	// Blob
	blob := path(cfg.blobsDir(), desc.Digest.Hex)
	if cfg.verbose {
		fmt.Printf("mv %v %v\n", rel(cfg.buildDir(), target), rel(cfg.buildDir(), blob))
	}
	err = os.Rename(target, blob)
	return
}

func newDataTarball(source, target string, ignored []string, verbose bool) error {
	targetFile, err := os.Create(target)
	if err != nil {
		return err
	}
	defer targetFile.Close()

	gw := gzip.NewWriter(targetFile)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	return filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		for _, v := range ignored {
			if info.Name() == v {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}

		header, err := tar.FileInfoHeader(info, info.Name())
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}

		header.Name = filepath.Join("/func", relPath)
		// TODO: should we set file timestamps to the build start time of cfg.t?
		// header.ModTime = timestampArgument

		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if verbose {
			fmt.Printf("→ %v \n", header.Name)
		}
		if info.IsDir() {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		_, err = io.Copy(tw, file)
		return err
	})
}

func newDescriptor(layer v1.Layer) (desc v1.Descriptor, err error) {
	size, err := layer.Size()
	if err != nil {
		return
	}
	digest, err := layer.Digest()
	if err != nil {
		return
	}
	return v1.Descriptor{
		MediaType: types.OCILayer,
		Size:      size,
		Digest:    digest,
	}, nil
}

// newImage creates an image for the given platform.
// The image consists of the shared data layer which is provided
func newImage(cfg *buildConfig, dataDesc v1.Descriptor, dataLayer v1.Layer, p v1.Platform, verbose bool) (imageDesc v1.Descriptor, err error) {
	b, err := newLanguageLayerBuilder(cfg)
	if err != nil {
		return
	}

	// Write Exec Layer as Blob -> Layer
	execDesc, execLayer, err := b.Build(cfg, p)
	if err != nil {
		return
	}

	// Write Config Layer as Blob -> Layer
	configDesc, _, err := newConfig(cfg, p, dataLayer, execLayer)
	if err != nil {
		return
	}

	// Image Manifest
	image := v1.Manifest{
		SchemaVersion: 2,
		MediaType:     types.OCIManifestSchema1,
		Config:        configDesc,
		Layers:        []v1.Descriptor{dataDesc, execDesc},
	}

	// Write image manifest out as json to a tempfile
	filePath := fmt.Sprintf("image.%v.%v.json", p.OS, p.Architecture)
	file, err := os.Create(filePath)
	if err != nil {
		return
	}
	enc := json.NewEncoder(file)
	enc.SetIndent("", "  ")
	if err = enc.Encode(image); err != nil {
		return
	}
	if err = file.Close(); err != nil {
		return
	}

	// Create a descriptor from hash and size
	file, err = os.Open(filePath)
	if err != nil {
		return
	}
	hash, size, err := v1.SHA256(file)
	if err != nil {
		return
	}
	imageDesc = v1.Descriptor{
		MediaType: types.OCIManifestSchema1,
		Digest:    hash,
		Size:      size,
		Platform:  &p,
	}
	if err = file.Close(); err != nil {
		return
	}

	// move image into blobs
	blob := path(cfg.blobsDir(), hash.Hex)
	if cfg.verbose {
		fmt.Printf("mv %v %v\n", rel(cfg.buildDir(), filePath), rel(cfg.buildDir(), blob))
	}
	err = os.Rename(filePath, blob)
	return
}

func newConfig(cfg *buildConfig, p v1.Platform, layers ...v1.Layer) (desc v1.Descriptor, config v1.ConfigFile, err error) {
	volumes := make(map[string]struct{}) // Volumes are odd, see spec.
	for _, v := range cfg.f.Run.Volumes {
		if v.Path == nil {
			continue // TODO: remove pointers from Volume and Env struct members
		}
		volumes[*v.Path] = struct{}{}
	}

	rootfs := v1.RootFS{
		Type: "layers",
	}
	var diff v1.Hash
	for _, v := range layers {
		if diff, err = v.DiffID(); err != nil {
			return
		}
		rootfs.DiffIDs = append(rootfs.DiffIDs, diff)
	}

	config = v1.ConfigFile{
		Created:      v1.Time{Time: cfg.t},
		Architecture: p.Architecture,
		OS:           p.OS,
		OSVersion:    p.OSVersion,
		// OSFeatures:   p.OSFeatures, // TODO: need to update dep to get this
		Variant: p.Variant,
		Config: v1.Config{
			ExposedPorts: map[string]struct{}{"8080/tcp": {}},
			Env:          cfg.f.Run.Envs.Slice(),
			Cmd:          []string{"/func/f"}, // NOTE: Using Cmd because Entrypoint can not be overridden
			WorkingDir:   "/func/",
			StopSignal:   "SIGKILL",
			Volumes:      volumes,
			// Labels
			// History
		},
		RootFS: rootfs,
	}

	// Write the config out as json to a tempfile
	filePath := path(cfg.buildDir(), "config.json")
	file, err := os.Create(filePath)
	if err != nil {
		return
	}
	enc := json.NewEncoder(file)
	enc.SetIndent("", "  ")
	if err = enc.Encode(config); err != nil {
		return
	}
	if err = file.Close(); err != nil {
		return
	}

	// Create a descriptor using hash and size
	file, err = os.Open(filePath)
	if err != nil {
		return
	}
	hash, size, err := v1.SHA256(file)
	if err != nil {
		return
	}
	desc = v1.Descriptor{
		MediaType: types.OCIConfigJSON,
		Digest:    hash,
		Size:      size,
	}
	if err = file.Close(); err != nil {
		return
	}

	// move config into blobs
	blobPath := path(cfg.blobsDir(), hash.Hex)
	if cfg.verbose {
		fmt.Printf("mv %v %v\n", rel(cfg.buildDir(), filePath), rel(cfg.buildDir(), blobPath))
	}
	err = os.Rename(filePath, blobPath)
	return
}

func newImageIndex(cfg *buildConfig, imageDescs []v1.Descriptor) (index v1.IndexManifest, err error) {
	index = v1.IndexManifest{
		SchemaVersion: 2,
		MediaType:     types.OCIImageIndex,
		Manifests:     imageDescs,
	}

	filePath := path(cfg.ociDir(), "index.json")
	file, err := os.Create(filePath)
	if err != nil {
		return
	}
	defer file.Close()

	enc := json.NewEncoder(file)
	enc.SetIndent("", "  ")
	err = enc.Encode(index)
	return
}

// rel is a simple prefix trim used exclusively for verbose debugging
// statements to print paths as relative to the current build directory
// rather than absolute. Returns the path relative to the current working
// build directory.  If it is not a subpath, the full path is returned
// unchanged.
func rel(base, path string) string {
	if strings.HasPrefix(path, base) {
		return "." + strings.TrimPrefix(path, base)
	}
	return path
}
