package queue

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/png"
	"strconv"
	"strings"

	"img-gen/internal/compose"
	"img-gen/internal/storage"
)

// regionalOccDilation is how far the exclude-occupancy silhouette is grown
// (px) so a region's repaint never starts right at figure A's contour.
const regionalOccDilation = 14

// regionalPipeline renders one regional job: stage 1 is the base figure
// (regions[0].Prompt) as a plain text2img render; stages 2..N repaint each
// region's rect over the running image with the region's own prompt (masked
// img2img through Ops.Fill). Each figure is therefore prompted in visual
// context of the previous stage — regional prompting without engine support,
// one GPU-lane job, all stages inside one queued job.
//
// sp (sampling params) rides every stage so critic-loop retries keep the
// attempt's seed policy; the returned seed is the last stage's.
func (m *Manager) regionalPipeline(ctx context.Context, id string, job *storage.Job, inputs imageInputs, spec storage.ModelSpec, sp storage.SamplingParams) ([]byte, *int64, error) {
	// Submit validation guarantees >= 2 regions with a non-empty base prompt;
	// the guard keeps a corrupted persisted spec from panicking the worker.
	if len(job.Regions) < 1 {
		return nil, nil, errors.New("regional job has no regions (corrupt spec)")
	}
	m.log(id, "regional: base render (%d region(s))", len(job.Regions))
	png, seed, err := m.dispatch(ctx, id, job, inputs, job.Regions[0].Prompt, spec, sp)
	if err != nil {
		return nil, seed, err
	}
	cur := png
	for i, r := range job.Regions[1:] {
		if ctx.Err() != nil || m.isCancelled(id) {
			return nil, seed, fmt.Errorf("cancelled before region %d", i+1)
		}
		if strings.TrimSpace(r.Prompt) == "" {
			return nil, seed, fmt.Errorf("region %d has an empty prompt (corrupt spec)", i+1)
		}
			maskB64, err := m.regionalMask(ctx, id, job, cur, r)
		if err != nil {
			return nil, seed, fmt.Errorf("region %d mask: %v", i+2, err)
		}
		m.log(id, "region %d/%d: fill %s (strength=%.2f occupancy=%t)",
			i+2, len(job.Regions), rectString(r.Rect), r.Strength, r.ExcludeOccupancy)
		strength := r.Strength
		if strength == 0 {
			strength = 1.0
		}
		filled, fillSeed, err := m.opts.Ops.Fill(ctx, r.Prompt, job.Size,
			base64.StdEncoding.EncodeToString(cur), maskB64, strength, spec, sp)
		if err != nil {
			return nil, seed, fmt.Errorf("region %d fill: %v", i+2, err)
		}
		if fillSeed != nil {
			seed = fillSeed
		}
		cur = filled
	}
	return cur, seed, nil
}

// regionalMask builds the fill mask for one region: a white rect (and, with
// ExcludeOccupancy, minus the running image's dilated subject silhouette).
func (m *Manager) regionalMask(ctx context.Context, id string, job *storage.Job, cur []byte, r storage.RegionSpec) (string, error) {
	w, h, ok := strings.Cut(job.Size, "x")
	if !ok {
		return "", fmt.Errorf("job size %q is not WxH", job.Size)
	}
	canvasW, err1 := strconv.Atoi(w)
	canvasH, err2 := strconv.Atoi(h)
	if err1 != nil || err2 != nil || canvasW <= 0 || canvasH <= 0 {
		return "", fmt.Errorf("job size %q is invalid", job.Size)
	}
	rects := []image.Rectangle{
		image.Rect(r.Rect[0], r.Rect[1], r.Rect[0]+r.Rect[2], r.Rect[1]+r.Rect[3]),
	}
	if !r.ExcludeOccupancy {
		return compose.RegionMask(canvasW, canvasH, rects...)
	}
	if m.opts.Ops.Rembg == nil {
		// Submit validation refuses exclude_occupancy without the rembg
		// sidecar; this backstop keeps an unwired-after-submit run honest.
		return "", errors.New("exclude_occupancy needs the rembg sidecar (REMBG_URL)")
	}
	alphaPNG, err := m.opts.Ops.Rembg(ctx, base64.StdEncoding.EncodeToString(cur))
	if err != nil {
		return "", fmt.Errorf("occupancy segment: %v", err)
	}
	alphaImg, err := png.Decode(bytes.NewReader(alphaPNG))
	if err != nil {
		return "", fmt.Errorf("occupancy segment: %v", err)
	}
	return compose.RegionMaskExcluding(canvasW, canvasH, regionalOccDilation, alphaImg, rects...)
}

func rectString(rect [4]int) string {
	return fmt.Sprintf("[%d,%d %dx%d]", rect[0], rect[1], rect[2], rect[3])
}
