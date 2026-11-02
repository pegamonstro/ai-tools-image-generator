package compose

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// posePNG builds a 2x1-keypoint-friendly source image (WxH solid gray) and
// returns its base64 PNG.
func posePNG(t *testing.T, w, h int, c color.RGBA) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func testKeypoints() []float32 {
	// Confident standing figure: head at top, shoulders mid, legs down.
	base := []float32{
		0.5, 0.1, 0.9, // nose
		0.48, 0.09, 0.9, // left eye
		0.52, 0.09, 0.9, // right eye
		0.47, 0.10, 0.9, // left ear
		0.53, 0.10, 0.9, // right ear
		0.4, 0.3, 0.9, // left shoulder
		0.6, 0.3, 0.9, // right shoulder
		0.35, 0.5, 0.9, // left elbow
		0.65, 0.5, 0.9, // right elbow
		0.33, 0.7, 0.9, // left wrist
		0.67, 0.7, 0.9, // right wrist
		0.45, 0.6, 0.9, // left hip
		0.55, 0.6, 0.9, // right hip
		0.43, 0.8, 0.9, // left knee
		0.57, 0.8, 0.9, // right knee
		0.42, 0.98, 0.9, // left ankle
		0.58, 0.98, 0.9, // right ankle
	}
	return base
}

func TestPoseSkeletonPixelCoordinates(t *testing.T) {
	const w, h = 256, 512
	kps := testKeypoints()
	// Scale the normalized figure into source pixel space.
	for i := 0; i < len(kps); i += 3 {
		kps[i] *= w
		kps[i+1] *= h
	}
	out, err := PoseSkeleton(posePNG(t, w, h, color.RGBA{100, 100, 100, 255}), kps)
	if err != nil {
		t.Fatalf("PoseSkeleton: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(out)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("skeleton is not a PNG: %v", err)
	}
	if got := img.Bounds().Dx(); got != w {
		t.Fatalf("canvas width = %d, want %d (skeleton inherits pose image size)", got, w)
	}
	// The torso bar (shoulder→shoulder, palette red) must have red pixels.
	g := img.(*image.RGBA) // canvas is RGBA by construction
	found := false
	for y := 0; y < h && !found; y++ {
		for x := 0; x < w; x++ {
			r, gc, b, _ := g.At(x, y).RGBA()
			if r > 0xC000 && gc < 0x4000 && b < 0x4000 { // strong red
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatal("no red (limb#0) pixels found — limbs are not drawn")
	}
	// Joints are white discs.
	whiteFound := false
	for y := 0; y < h && !whiteFound; y++ {
		for x := 0; x < w; x++ {
			r, g2, b2, _ := g.At(x, y).RGBA()
			if r > 0xC000 && g2 > 0xC000 && b2 > 0xC000 {
				whiteFound = true
				break
			}
		}
	}
	if !whiteFound {
		t.Fatal("no white joint discs found")
	}
}

func TestPoseSkeletonNormalizedCoordinates(t *testing.T) {
	const w, h = 256, 512
	out, err := PoseSkeleton(posePNG(t, w, h, color.RGBA{0, 0, 0, 255}), testKeypoints())
	if err != nil {
		t.Fatalf("PoseSkeleton with 0..1 coords: %v", err)
	}
	raw, _ := base64.StdEncoding.DecodeString(out)
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != w || img.Bounds().Dy() != h {
		t.Fatalf("canvas = %dx%d, want %dx%d", img.Bounds().Dx(), img.Bounds().Dy(), w, h)
	}
}

func TestPoseSkeletonLowConfidenceInvisible(t *testing.T) {
	// Assert structurally: render twice — once with every joint confident, once
	// with the left wrist zeroed — and expect strictly fewer drawn pixels in a
	// 64px box around the wrist: its joint disc and the elbow→wrist limb both
	// disappear, and nothing else redraws there.
	const w, h = 256, 512
	full, err := PoseSkeleton(posePNG(t, w, h, color.RGBA{0, 0, 0, 255}), testKeypoints())
	if err != nil {
		t.Fatal(err)
	}
	wristOff := testKeypoints()
	wristOff[9*3+2] = 0
	absent, err := PoseSkeleton(posePNG(t, w, h, color.RGBA{0, 0, 0, 255}), wristOff)
	if err != nil {
		t.Fatal(err)
	}
	countDrawn := func(b64 string) int {
		raw, _ := base64.StdEncoding.DecodeString(b64)
		img, _ := png.Decode(bytes.NewReader(raw))
		n := 0
		cx, cy := w*33/100, h*7/10
		for y := cy - 32; y <= cy+32; y++ {
			for x := cx - 32; x <= cx+32; x++ {
				r, g, b, _ := img.At(x, y).RGBA()
				if r|g|b > 0 {
					n++
				}
			}
		}
		return n
	}
	if countDrawn(absent) >= countDrawn(full) {
		t.Fatalf("zero-confidence wrist did not hide geometry (full=%d absent=%d)", countDrawn(full), countDrawn(absent))
	}
}

func TestPoseSkeletonBadInput(t *testing.T) {
	if _, err := PoseSkeleton("not-base64!", testKeypoints()); err == nil {
		t.Fatal("bad base64 accepted")
	}
	if _, err := PoseSkeleton(posePNG(t, 64, 64, color.RGBA{1, 1, 1, 255}), []float32{1, 2, 3}); err == nil {
		t.Fatal("short keypoint array accepted")
	}
}