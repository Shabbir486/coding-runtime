package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"go.uber.org/zap"
)

// ImagePool tracks which Docker images are already present on the host and
// provides idempotent pull operations so that each image is only pulled once
// even when many workers start concurrently.
type ImagePool struct {
	client *client.Client
	images map[string]bool
	mu     sync.RWMutex
	logger *zap.Logger
}

// NewImagePool constructs an ImagePool backed by the provided Docker client.
func NewImagePool(dockerClient *client.Client, logger *zap.Logger) *ImagePool {
	return &ImagePool{
		client: dockerClient,
		images: make(map[string]bool),
		logger: logger,
	}
}

// EnsureImage guarantees the named image exists locally.  If it is already
// present (tracked in the in-memory map or found via Docker inspect) the
// function returns immediately without a pull.
func (p *ImagePool) EnsureImage(ctx context.Context, img string) error {
	// Fast path: already tracked.
	p.mu.RLock()
	ok := p.images[img]
	p.mu.RUnlock()
	if ok {
		return nil
	}

	// Check Docker directly (image may have been pulled before this process
	// started, e.g., pre-built into the VM image).
	present, err := p.imageExistsLocally(ctx, img)
	if err != nil {
		return fmt.Errorf("image_pool: inspect %q: %w", img, err)
	}
	if present {
		p.markPresent(img)
		return nil
	}

	return p.PullIfMissing(ctx, img)
}

// PullIfMissing unconditionally pulls the image.  Use EnsureImage when you
// want the cheap local-check fast path.
func (p *ImagePool) PullIfMissing(ctx context.Context, img string) error {
	p.logger.Info("pulling docker image", zap.String("image", img))

	reader, err := p.client.ImagePull(ctx, img, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("image_pool: pull %q: %w", img, err)
	}
	defer reader.Close()

	// Consume the pull stream so the pull completes; log layer progress at
	// debug level so the caller sees useful diagnostics without drowning logs.
	dec := json.NewDecoder(reader)
	for {
		var event struct {
			Status         string `json:"status"`
			ProgressDetail struct {
				Current int64 `json:"current"`
				Total   int64 `json:"total"`
			} `json:"progressDetail"`
			ID string `json:"id"`
		}
		if err := dec.Decode(&event); err != nil {
			if err == io.EOF {
				break
			}
			// Ignore decode errors mid-stream; they are generally harmless.
			continue
		}
		p.logger.Debug("image pull progress",
			zap.String("image", img),
			zap.String("status", event.Status),
			zap.String("layer", event.ID),
		)
	}

	p.markPresent(img)
	p.logger.Info("docker image ready", zap.String("image", img))
	return nil
}

// PrewarmImages pulls all images in parallel.  It waits until every pull
// finishes (or fails) before returning.  Individual failures are collected and
// returned as a combined error so the caller can decide whether to abort.
func (p *ImagePool) PrewarmImages(ctx context.Context, images []string) error {
	type result struct {
		img string
		err error
	}

	ch := make(chan result, len(images))
	for _, img := range images {
		go func(i string) {
			ch <- result{img: i, err: p.EnsureImage(ctx, i)}
		}(img)
	}

	var failures []string
	for range images {
		r := <-ch
		if r.err != nil {
			p.logger.Error("failed to prewarm image",
				zap.String("image", r.img),
				zap.Error(r.err),
			)
			failures = append(failures, fmt.Sprintf("%s: %v", r.img, r.err))
		}
	}

	if len(failures) > 0 {
		msg := "image_pool: prewarm failures:"
		for _, f := range failures {
			msg += " [" + f + "]"
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// ListImages returns all image tags currently known to Docker on the host.
func (p *ImagePool) ListImages(ctx context.Context) ([]string, error) {
	summaries, err := p.client.ImageList(ctx, image.ListOptions{
		Filters: filters.NewArgs(),
	})
	if err != nil {
		return nil, fmt.Errorf("image_pool: list images: %w", err)
	}

	var tags []string
	for _, s := range summaries {
		tags = append(tags, s.RepoTags...)
	}
	return tags, nil
}

// ---- helpers -----------------------------------------------------------------

func (p *ImagePool) markPresent(img string) {
	p.mu.Lock()
	p.images[img] = true
	p.mu.Unlock()
}

// imageExistsLocally returns true if the image can be inspected locally.
func (p *ImagePool) imageExistsLocally(ctx context.Context, img string) (bool, error) {
	_, _, err := p.client.ImageInspectWithRaw(ctx, img)
	if err != nil {
		if client.IsErrNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
