package compose

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"time"
)

// OutputFile is one deliverable the compose layer hands to storage. Primary
// marks the file Job.ImagePath points at and the MCP preview serves.
type OutputFile struct {
	Name    string
	Data    []byte
	Primary bool
}

// ComposeElement produces an element's deliverable. transparent=false uses
// the single render as-is; transparent=true recovers RGBA from the
// white-background and black-background sibling renders.
func ComposeElement(name string, white, black []byte, transparent bool) (OutputFile, error) {
	if transparent {
		un, err := UnmixAlpha(white, black)
		if err != nil {
			return OutputFile{}, err
		}
		return OutputFile{Name: name + ".png", Data: un, Primary: true}, nil
	}
	if white == nil {
		return OutputFile{}, fmt.Errorf("compose: element %q render missing", name)
	}
	return OutputFile{Name: name + ".png", Data: white, Primary: true}, nil
}

// ComposeKit turns per-class renders into one deliverable per class. imgs is
// parallel to KitClasses. A kit with any failed class is wholly failed: the
// point of a kit is the 20 pieces that tile together.
func ComposeKit(name string, imgs [][]byte) ([]OutputFile, error) {
	if len(imgs) != len(KitClasses) {
		return nil, fmt.Errorf("compose: kit %q needs %d renders, got %d", name, len(KitClasses), len(imgs))
	}
	files := make([]OutputFile, 0, len(imgs))
	for i, img := range imgs {
		if img == nil {
			return nil, fmt.Errorf("compose: kit %q class %q render failed", name, KitClasses[i].Key)
		}
		files = append(files, OutputFile{
			Name:    fmt.Sprintf("%s-%s.png", name, KitClasses[i].Key),
			Data:    img,
			Primary: i == 0,
		})
	}
	return files, nil
}

// ComposeAtlas packs frame renders into a sheet plus TexturePacker metadata.
func ComposeAtlas(name string, framePNGs [][]byte, columns int) ([]OutputFile, error) {
	frames := make([]image.Image, 0, len(framePNGs))
	for i, fb := range framePNGs {
		img, err := png.Decode(bytes.NewReader(fb))
		if err != nil {
			return nil, fmt.Errorf("compose: atlas %q frame %d is not a PNG: %v", name, i, err)
		}
		frames = append(frames, img)
	}
	sheet, metas, err := PackGrid(name, frames, columns)
	if err != nil {
		return nil, err
	}
	metaJSON, err := MarshalAtlas(metas)
	if err != nil {
		return nil, err
	}
	return []OutputFile{
		{Name: name + "-sheet.png", Data: sheet, Primary: true},
		{Name: name + "-atlas.json", Data: metaJSON},
	}, nil
}

// ComposeGIF encodes the animation and optional WebP variant. webpStatus is
// empty on success and records the skip reason when gif2webp is absent or
// fails — WebP is an extra, never a failure of the GIF.
func ComposeGIF(name string, frames []image.Image, delayMS, loop int, formats []string) (files []OutputFile, encoder, webpStatus string, err error) {
	if len(frames) == 0 {
		return nil, "", "", fmt.Errorf("compose: gif %q needs at least one frame", name)
	}
	gifBytes, err := EncodeGIF(frames, delayMS, loop)
	if err != nil {
		return nil, "", "", err
	}
	files = []OutputFile{{Name: name + ".gif", Data: gifBytes, Primary: true}}
	encoder = "stdlib shared median-cut palette"
	if len(formats) == 0 {
		formats = []string{"gif"}
	}
	for _, f := range formats {
		switch f {
		case "gif":
			// primary already emitted
		case "webp":
			if !WebPAvailable() {
				webpStatus = "gif2webp not found on PATH; webp variant skipped"
				continue
			}
			wb, werr := EncodeWebP(gifBytes)
			if werr != nil {
				webpStatus = fmt.Sprintf("gif2webp failed: %v", werr)
				continue
			}
			files = append(files, OutputFile{Name: name + ".webp", Data: wb})
		default:
			return nil, "", "", fmt.Errorf("compose: unknown output format %q (want \"gif\" or \"webp\")", f)
		}
	}
	return files, encoder, webpStatus, nil
}

// WebPAvailable reports whether gif2webp (libwebp) is installed. It is
// consulted at compose time, not build time, so the WebP extra is optional
// machinery with zero dependency cost.
func WebPAvailable() bool {
	_, err := exec.LookPath("gif2webp")
	return err == nil
}

// EncodeWebP converts an animated GIF into an animated WebP via gif2webp.
func EncodeWebP(gifBytes []byte) ([]byte, error) {
	if len(gifBytes) == 0 {
		return nil, fmt.Errorf("compose: empty gif")
	}
	dir, err := os.MkdirTemp("", "compose-webp-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	in := dir + "/in.gif"
	out := dir + "/out.webp"
	if err := os.WriteFile(in, gifBytes, 0o600); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gif2webp", "-mixed", in, "-o", out)
	if ob, err := cmd.CombinedOutput(); err != nil {
		msg := string(ob)
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, fmt.Errorf("gif2webp: %v: %s", err, msg)
	}
	return os.ReadFile(out)
}
