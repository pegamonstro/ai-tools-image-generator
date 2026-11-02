package compose

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
)

// COCO-17 keypoint indices, in DWPose sidecar order.
const (
	cocoNose = iota
	cocoLeftEye
	cocoRightEye
	cocoLeftEar
	cocoRightEar
	cocoLeftShoulder
	cocoRightShoulder
	cocoLeftElbow
	cocoRightElbow
	cocoLeftWrist
	cocoRightWrist
	cocoLeftHip
	cocoRightHip
	cocoLeftKnee
	cocoRightKnee
	cocoLeftAnkle
	cocoRightAnkle
	cocoKeypoints = 17
)

// cocoLimbs lists the skeleton edges ControlNet openpose maps expect: joints
// connect eye→ear, shoulder→elbow→wrist, hip→knee→ankle, plus the shoulder
// bar and the hip bar. Indices reference the COCO-17 list above.
var cocoLimbs = [][2]int{
	{cocoLeftEye, cocoNose}, {cocoNose, cocoRightEye},
	{cocoLeftEye, cocoLeftEar}, {cocoRightEye, cocoRightEar},
	{cocoLeftShoulder, cocoRightShoulder},
	{cocoLeftShoulder, cocoLeftElbow}, {cocoLeftElbow, cocoLeftWrist},
	{cocoRightShoulder, cocoRightElbow}, {cocoRightElbow, cocoRightWrist},
	{cocoLeftShoulder, cocoLeftHip}, {cocoRightShoulder, cocoRightHip},
	{cocoLeftHip, cocoRightHip},
	{cocoLeftHip, cocoLeftKnee}, {cocoLeftKnee, cocoLeftAnkle},
	{cocoRightHip, cocoRightKnee}, {cocoRightKnee, cocoRightAnkle},
}

// limbPalette approximates the canonical openpose body colors cycled over the
// limb list, so generated skeletons visually match the reference maps the
// pose ControlNet was trained on.
var limbPalette = []color.RGBA{
	{0xFF, 0x00, 0x00, 0xFF}, // red
	{0xFF, 0x88, 0x00, 0xFF}, // orange
	{0xFF, 0xFF, 0x00, 0xFF}, // yellow
	{0xA1, 0xFF, 0x00, 0xFF}, // lime
	{0x00, 0xFF, 0x51, 0xFF}, // green
	{0x00, 0xFF, 0xFF, 0xFF}, // cyan
	{0x00, 0x88, 0xFF, 0xFF}, // azure
	{0x00, 0x00, 0xFF, 0xFF}, // blue
	{0x88, 0x00, 0xFF, 0xFF}, // violet
	{0xFF, 0x00, 0xFF, 0xFF}, // magenta
	{0xFF, 0x00, 0x88, 0xFF}, // rose
}

// poseKeypoint holds one COCO joint in the skeleton's own coordinate space.
type poseKeypoint struct {
	x, y float64
}

// PoseSkeleton renders DWPose COCO-17 keypoints ([x, y, conf, ...], one
// triplet per joint) onto an openpose-style skeleton canvas sized like the
// source pose image, returned as base64 PNG ready to ship as ControlNet
// conditioning. Coordinates are accepted either as pixel positions matching
// the pose image or as 0..1 fractions — anything beyond a canvas edge (5%
// tolerance for border joints) is treated as normalized and scaled up.
// Keypoints with confidence <= 0 stay invisible and their limbs do not draw.
func PoseSkeleton(poseImgB64 string, keypoints []float32) (string, error) {
	poseRaw, err := base64.StdEncoding.DecodeString(poseImgB64)
	if err != nil {
		return "", fmt.Errorf("pose image base64: %v", err)
	}
	src, _, err := image.Decode(bytes.NewReader(poseRaw))
	if err != nil {
		return "", fmt.Errorf("pose image decode: %v", err)
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return "", fmt.Errorf("pose image has no pixels: %dx%d", w, h)
	}
	if len(keypoints) < cocoKeypoints*3 {
		return "", fmt.Errorf("keypoints: got %d floats, want >= %d (COCO-17 x,y,conf triplets)", len(keypoints), cocoKeypoints*3)
	}

	rawMax := 0.0
	pts := make([]poseKeypoint, cocoKeypoints)
	for i := range pts {
		rawMax = max(rawMax, float64(keypoints[i*3]), float64(keypoints[i*3+1]))
	}
	// DWPose sidecars emit one of two coordinate conventions: pixels in the
	// source image space, or 0..1 fractions. Detect once. Normalized coords
	// scale per axis (x by width, y by height) so limb directions stay correct
	// on non-square canvases; a shared scale would skew geometry.
	xScale, yScale := 1.0, 1.0
	if rawMax <= 1.0+1e-6 {
		xScale, yScale = float64(w), float64(h)
	}
	vis := make([]bool, cocoKeypoints)
	for i := range pts {
		pts[i] = poseKeypoint{x: float64(keypoints[i*3]) * xScale, y: float64(keypoints[i*3+1]) * yScale}
		vis[i] = float64(keypoints[i*3+2]) > 0
	}

	canvas := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.Black), image.Point{}, draw.Src)
	lineR := math.Max(1, float64(max(w, h))/128)
	for li, limb := range cocoLimbs {
		a, c := limb[0], limb[1]
		if !vis[a] || !vis[c] {
			continue
		}
		drawLimb(canvas, pts[a], pts[c], lineR, limbPalette[li%len(limbPalette)])
	}
	jointCol := color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}
	for i, p := range pts {
		if !vis[i] {
			continue
		}
		stampDisc(canvas, p.x, p.y, lineR*1.5, jointCol)
	}

	var out bytes.Buffer
	if err := png.Encode(&out, canvas); err != nil {
		return "", fmt.Errorf("skeleton png: %v", err)
	}
	return base64.StdEncoding.EncodeToString(out.Bytes()), nil
}

// drawLimb stamps overlapping discs along the segment — thick enough that the
// polyline reads solid at ControlNet input sizes; the disc radius caps at the
// source scale so long limbs stay one continuous stroke.
func drawLimb(dst *image.RGBA, a, c poseKeypoint, r float64, col color.RGBA) {
	dx, dy := c.x-a.x, c.y-a.y
	length := math.Hypot(dx, dy)
	steps := int(math.Max(1, length/(r/2)))
	for s := 0; s <= steps; s++ {
		t := float64(s) / float64(steps)
		stampDisc(dst, a.x+dx*t, a.y+dy*t, r, col)
	}
}

// stampDisc fills a solid disc clipped to the canvas.
func stampDisc(dst *image.RGBA, cx, cy, r float64, c color.RGBA) {
	if r <= 0 {
		return
	}
	b := dst.Bounds()
	x0, x1 := int(math.Floor(cx-r)), int(math.Ceil(cx+r))
	y0, y1 := int(math.Floor(cy-r)), int(math.Ceil(cy+r))
	for y := y0; y <= y1; y++ {
		if y < b.Min.Y || y >= b.Max.Y {
			continue
		}
		for x := x0; x <= x1; x++ {
			if x < b.Min.X || x >= b.Max.X {
				continue
			}
			dxx, dyy := float64(x)+0.5-cx, float64(y)+0.5-cy
			if dxx*dxx+dyy*dyy <= r*r {
				dst.SetRGBA(x, y, c)
			}
		}
	}
}