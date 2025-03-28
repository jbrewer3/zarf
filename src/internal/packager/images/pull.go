// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: 2021-Present The Zarf Authors

// Package images provides functions for building and pushing images.
package images

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/context/docker"
	"github.com/docker/cli/cli/flags"
	"github.com/zarf-dev/zarf/src/pkg/logger"

	"github.com/avast/retry-go/v4"
	"github.com/defenseunicorns/pkg/helpers/v2"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/logs"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/cache"
	"github.com/google/go-containerregistry/pkg/v1/daemon"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	clayout "github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/zarf-dev/zarf/src/pkg/message"
	"github.com/zarf-dev/zarf/src/pkg/transform"
	"github.com/zarf-dev/zarf/src/pkg/utils"
	"golang.org/x/sync/errgroup"
)

func checkForIndex(refInfo transform.Image, desc *remote.Descriptor) error {
	if refInfo.Digest != "" && desc != nil && types.MediaType(desc.MediaType).IsIndex() {
		var idx v1.IndexManifest
		if err := json.Unmarshal(desc.Manifest, &idx); err != nil {
			return fmt.Errorf("unable to unmarshal index.json: %w", err)
		}
		lines := []string{"The following images are available in the index:"}
		name := refInfo.Name
		if refInfo.Tag != "" {
			name += ":" + refInfo.Tag
		}
		for _, desc := range idx.Manifests {
			lines = append(lines, fmt.Sprintf("image - %s@%s with platform %s", name, desc.Digest.String(), desc.Platform.String()))
		}
		imageOptions := strings.Join(lines, "\n")
		return fmt.Errorf("%s resolved to an OCI image index which is not supported by Zarf, select a specific platform to use: %s", refInfo.Reference, imageOptions)
	}
	return nil
}

func getDockerEndpointHost() (string, error) {
	dockerCli, err := command.NewDockerCli(command.WithStandardStreams())
	if err != nil {
		return "", err
	}
	newClientOpts := flags.NewClientOptions()
	err = dockerCli.Initialize(newClientOpts)
	if err != nil {
		return "", err
	}
	store := dockerCli.ContextStore()
	metadata, err := store.GetMetadata(dockerCli.CurrentContext())
	if err != nil {
		return "", err
	}
	endpoint, err := docker.EndpointFromContext(metadata)
	if err != nil {
		return "", err
	}
	return endpoint.Host, nil
}

// Pull pulls all images from the given config.
func Pull(ctx context.Context, cfg PullConfig) (map[transform.Image]v1.Image, error) {
	l := logger.From(ctx)
	var longer string
	pullStart := time.Now()

	imageCount := len(cfg.ImageList)
	// Give some additional user feedback on larger image sets
	if imageCount > 15 {
		longer = "This step may take a couple of minutes to complete."
	} else if imageCount > 5 {
		longer = "This step may take several seconds to complete."
	}

	if err := helpers.CreateDirectory(cfg.DestinationDirectory, helpers.ReadExecuteAllWriteUser); err != nil {
		return nil, fmt.Errorf("failed to create image path %s: %w", cfg.DestinationDirectory, err)
	}

	cranePath, err := clayout.Write(cfg.DestinationDirectory, empty.Index)
	if err != nil {
		return nil, err
	}

	// Give some additional user feedback on larger image sets
	imageFetchStart := time.Now()
	// TODO(mkcp): Remove message on logger release
	spinner := message.NewProgressSpinner("Fetching info for %d images. %s", imageCount, longer)
	defer spinner.Stop()
	l.Info("fetching info for images", "count", imageCount, "destination", cfg.DestinationDirectory)

	logs.Warn.SetOutput(&message.DebugWriter{})
	logs.Progress.SetOutput(&message.DebugWriter{})

	eg, ectx := errgroup.WithContext(ctx)
	eg.SetLimit(10)

	var shaLock sync.Mutex
	shas := map[string]bool{}
	opts := CommonOpts(cfg.Arch)

	fetched := map[transform.Image]v1.Image{}

	var counter, totalBytes atomic.Int64
	var dockerEndPointHost string

	// Spawn a goroutine for each
	for _, refInfo := range cfg.ImageList {
		refInfo := refInfo
		eg.Go(func() error {
			idx := counter.Add(1)
			// TODO(mkcp): Remove message on logger release
			spinner.Updatef("Fetching image info (%d of %d)", idx, imageCount)
			l.Debug("fetching image info", "name", refInfo.Name)

			ref := refInfo.Reference
			for k, v := range cfg.RegistryOverrides {
				if strings.HasPrefix(refInfo.Reference, k) {
					ref = strings.Replace(refInfo.Reference, k, v, 1)
				}
			}

			var img v1.Image
			var desc *remote.Descriptor

			// load from local fs if it's a tarball
			if strings.HasSuffix(ref, ".tar") || strings.HasSuffix(ref, ".tar.gz") || strings.HasSuffix(ref, ".tgz") {
				img, err = crane.Load(ref, opts...)
				if err != nil {
					return fmt.Errorf("unable to load %s: %w", refInfo.Reference, err)
				}
			} else {
				reference, err := name.ParseReference(ref)
				if err != nil {
					return fmt.Errorf("failed to parse reference: %w", err)
				}
				desc, err = crane.Get(ref, opts...)
				if err != nil {
					if strings.Contains(err.Error(), "unexpected status code 429 Too Many Requests") {
						return fmt.Errorf("rate limited by registry: %w", err)
					}

					// TODO(mkcp): Remove message on logger release
					message.Warnf("Falling back to local 'docker', failed to find the manifest on a remote: %s", err.Error())
					l.Warn("Falling back to local 'docker', failed to find the manifest on a remote", "error", err.Error())

					if dockerEndPointHost == "" {
						dockerEndPointHost, err = getDockerEndpointHost()
						if err != nil {
							return err
						}
					}
					// Attempt to connect to the local docker daemon.
					cli, err := client.NewClientWithOpts(
						client.WithHost(dockerEndPointHost),
						client.WithTLSClientConfigFromEnv(),
						client.WithVersionFromEnv(),
					)
					if err != nil {
						return fmt.Errorf("docker not available: %w", err)
					}
					cli.NegotiateAPIVersion(ectx)

					// Inspect the image to get the size.
					rawImg, _, err := cli.ImageInspectWithRaw(ectx, ref)
					if err != nil {
						return err
					}

					// Warn the user if the image is large.
					if rawImg.Size > 750*1000*1000 {
						message.Warnf("%s is %s and may take a very long time to load via docker. "+
							"See https://docs.zarf.dev/faq for suggestions on how to improve large local image loading operations.",
							ref, utils.ByteFormat(float64(rawImg.Size), 2))
					}

					// Use unbuffered opener to avoid OOM Kill issues https://github.com/zarf-dev/zarf/issues/1214.
					// This will also take forever to load large images.
					img, err = daemon.Image(reference, daemon.WithUnbufferedOpener(), daemon.WithClient(cli))
					if err != nil {
						return fmt.Errorf("failed to load from docker daemon: %w", err)
					}
				} else {
					img, err = crane.Pull(ref, opts...)
					if err != nil {
						return fmt.Errorf("unable to pull image %s: %w", refInfo.Reference, err)
					}
				}
			}

			if err := checkForIndex(refInfo, desc); err != nil {
				return err
			}

			cacheImg, err := utils.OnlyHasImageLayers(img)
			if err != nil {
				return err
			}
			if cacheImg && cfg.CacheDirectory != "" {
				img = cache.Image(img, cache.NewFilesystemCache(cfg.CacheDirectory))
			}

			size, err := getSizeOfImage(img)
			if err != nil {
				return fmt.Errorf("failed to get size of image: %w", err)
			}
			totalBytes.Add(size)

			layers, err := img.Layers()
			if err != nil {
				return fmt.Errorf("unable to get layers for %s: %w", refInfo.Reference, err)
			}

			shaLock.Lock()
			defer shaLock.Unlock()
			for _, layer := range layers {
				digest, err := layer.Digest()
				if err != nil {
					return fmt.Errorf("unable to get digest for image layer: %w", err)
				}
				if _, ok := shas[digest.Hex]; !ok {
					shas[digest.Hex] = true
				}
			}

			if img == nil {
				return fmt.Errorf("failed to fetch image %s", refInfo.Reference)
			}

			fetched[refInfo] = img

			return nil
		})
	}

	// Wait until we're done fetching images
	if err := eg.Wait(); err != nil {
		return nil, err
	}

	// TODO(mkcp): Remove message on logger release
	spinner.Successf("Fetched info for %d images", imageCount)
	l.Debug("done fetching info for images", "count", len(cfg.ImageList), "duration", time.Since(imageFetchStart))

	doneSaving := make(chan error)
	updateText := fmt.Sprintf("Pulling %d images", imageCount)
	// TODO(mkcp): Remove progress bar on logger release
	go utils.RenderProgressBarForLocalDirWrite(cfg.DestinationDirectory, totalBytes.Load(), doneSaving, updateText, updateText)
	l.Info("pulling images", "count", len(cfg.ImageList))

	toPull := maps.Clone(fetched)

	err = retry.Do(func() error {
		saved, err := SaveConcurrent(ctx, cranePath, toPull, cfg.CacheDirectory)
		// Done save, remove from download list.
		for k := range saved {
			delete(toPull, k)
		}
		return err
	},
		retry.Context(ctx),
		retry.Attempts(2),
	)
	if err != nil {
		// TODO(mkcp): Remove message on logger release
		message.Warnf("Failed to save images in parallel, falling back to sequential save: %s", err.Error())
		l.Warn("failed to save images in parallel, falling back to sequential save", "error", err.Error())
		err = retry.Do(func() error {
			saved, err := SaveSequential(ctx, cranePath, toPull, cfg.CacheDirectory)
			for k := range saved {
				delete(toPull, k)
			}
			return err
		},
			retry.Context(ctx),
			retry.Attempts(2),
		)
		if err != nil {
			return nil, err
		}
	}

	// Send a signal to the progress bar that we're done and wait for the thread to finish
	// TODO(mkcp): Remove progress bar on logger release
	doneSaving <- nil
	<-doneSaving

	// Needed because when pulling from the local docker daemon, while using the docker containerd runtime
	// Crane incorrectly names the blob of the docker image config to a sha that does not match the contents
	// https://github.com/zarf-dev/zarf/issues/2584
	// This is a band aid fix while we wait for crane and or docker to create the permanent fix
	blobDir := filepath.Join(cfg.DestinationDirectory, "blobs", "sha256")
	err = filepath.Walk(blobDir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if fi.IsDir() {
			return nil
		}

		hash, err := helpers.GetSHA256OfFile(path)
		if err != nil {
			return err
		}
		newFile := filepath.Join(blobDir, hash)
		return os.Rename(path, newFile)
	})
	if err != nil {
		return nil, err
	}

	l.Debug("done pulling images", "count", len(cfg.ImageList), "duration", time.Since(pullStart))

	return fetched, nil
}

// from https://github.com/google/go-containerregistry/blob/6bce25ecf0297c1aa9072bc665b5cf58d53e1c54/pkg/v1/cache/fs.go#L143
func layerCachePath(path string, h v1.Hash) string {
	var file string
	if runtime.GOOS == "windows" {
		file = fmt.Sprintf("%s-%s", h.Algorithm, h.Hex)
	} else {
		file = h.String()
	}
	return filepath.Join(path, file)
}

// CleanupInProgressLayers removes incomplete layers from the cache.
func CleanupInProgressLayers(ctx context.Context, img v1.Image, cacheDirectory string) error {
	layers, err := img.Layers()
	if err != nil {
		return err
	}
	eg, _ := errgroup.WithContext(ctx)
	for _, layer := range layers {
		layer := layer
		eg.Go(func() error {
			digest, err := layer.Digest()
			if err != nil {
				return err
			}
			size, err := layer.Size()
			if err != nil {
				return err
			}
			location := layerCachePath(cacheDirectory, digest)
			info, err := os.Stat(location)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if info.Size() != size {
				if err := os.Remove(location); err != nil {
					return fmt.Errorf("failed to remove incomplete layer %s: %w", digest.Hex, err)
				}
			}
			return nil
		})
	}
	return eg.Wait()
}

func getSizeOfImage(img v1.Image) (int64, error) {
	var totalSize int64
	manifestSize, err := img.Size()
	if err != nil {
		return 0, err
	}
	totalSize += manifestSize
	manifest, err := img.Manifest()
	if err != nil {
		return 0, err
	}
	totalSize += manifest.Config.Size
	layers, err := img.Layers()
	if err != nil {
		return 0, err
	}
	for _, layer := range layers {
		size, err := layer.Size()
		if err != nil {
			return 0, err
		}
		totalSize += size
	}
	return totalSize, nil
}

// SaveSequential saves images sequentially.
func SaveSequential(ctx context.Context, cl clayout.Path, m map[transform.Image]v1.Image, cacheDirectory string) (map[transform.Image]v1.Image, error) {
	l := logger.From(ctx)
	saved := map[transform.Image]v1.Image{}
	for info, img := range m {
		annotations := map[string]string{
			ocispec.AnnotationBaseImageName: info.Reference,
		}
		wStart := time.Now()
		size, err := getSizeOfImage(img)
		if err != nil {
			return saved, fmt.Errorf("failed to get size of image: %w", err)
		}
		byteSize := utils.ByteFormat(float64(size), 2)
		l.Info("saving image", "ref", info.Reference, "size", byteSize, "method", "sequential")
		if err := cl.AppendImage(img, clayout.WithAnnotations(annotations)); err != nil {
			if err := CleanupInProgressLayers(ctx, img, cacheDirectory); err != nil {
				message.WarnErr(err, "failed to clean up in-progress layers, please run `zarf tools clear-cache`")
				l.Error("failed to clean up in-progress layers. please run `zarf tools clear-cache`")
			}
			return saved, err
		}
		saved[info] = img
		l.Debug("done saving image",
			"ref", info.Reference,
			"bytes", size,
			"method", "sequential",
			"duration", time.Since(wStart),
		)
	}
	return saved, nil
}

// SaveConcurrent saves images in a concurrent, bounded manner.
func SaveConcurrent(ctx context.Context, cl clayout.Path, m map[transform.Image]v1.Image, cacheDirectory string) (map[transform.Image]v1.Image, error) {
	l := logger.From(ctx)
	saved := map[transform.Image]v1.Image{}

	var mu sync.Mutex

	eg, ectx := errgroup.WithContext(ctx)
	eg.SetLimit(10)

	for info, img := range m {
		info, img := info, img
		eg.Go(func() error {
			select {
			case <-ectx.Done():
				return ectx.Err()
			default:
				desc, err := partial.Descriptor(img)
				if err != nil {
					return err
				}
				size, err := getSizeOfImage(img)
				if err != nil {
					return err
				}
				byteSize := utils.ByteFormat(float64(size), 2)
				wStart := time.Now()
				l.Info("saving image", "ref", info.Reference, "size", byteSize, "method", "concurrent")
				if err := cl.WriteImage(img); err != nil {
					if err := CleanupInProgressLayers(ectx, img, cacheDirectory); err != nil {
						message.WarnErr(err, "failed to clean up in-progress layers, please run `zarf tools clear-cache`")
						l.Error("failed to clean up in-progress layers. please run `zarf tools clear-cache`")
					}
					return err
				}
				l.Debug("done saving image",
					"ref", info.Reference,
					"bytes", size,
					"method", "concurrent",
					"duration", time.Since(wStart),
				)

				mu.Lock()
				defer mu.Unlock()
				annotations := map[string]string{
					ocispec.AnnotationBaseImageName: info.Reference,
				}
				desc.Annotations = annotations
				if err := cl.AppendDescriptor(*desc); err != nil {
					return err
				}

				saved[info] = img
				return nil
			}
		})
	}

	return saved, eg.Wait()
}
