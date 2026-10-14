package compose

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
)

var ErrNotPixelGrid = errors.New("not a pixel-art grid")

// Repair contract constants shared by the compose post-op and the
// /api/v1/repair endpoint.
const (
	RepairMinCell       = 2
	RepairMaxCell       = 64
	RepairMaxColors     = 64
	RepairDefaultColors = 32
	RepairMaxDim        = 1024
)

// DetectGridCell estimates the pixel-art cell size of img by
// autocorrelating its edge-energy signal: a pixel-art grid spikes at every
// cell boundary (≈1/cell of the interior lines) and stays quiet inside
// cells, so the raw signal self-aligns at lag = cell. Horizontal and
// vertical lags must agree, else the image is refused (ErrNotPixelGrid)
// rather than repaired with a guessed cell. The image must span at least
// ~2 cells (plus 4 px) per axis.
func DetectGridCell(img image.Image) (int, error) {
	colEdges, rowEdges := edgesAlong(img)
	colLag, err := detectCellAxis(colEdges)
	if err != nil {
		return 0, err
	}
	rowLag, err := detectCellAxis(rowEdges)
	if err != nil {
		return 0, err
	}
	if colLag != rowLag {
		return 0, ErrNotPixelGrid
	}
	return colLag, nil
}

// edgesAlong sums absolute channel differences across each interior line:
// colEdges[x] is the boundary between columns x and x+1 (a vertical-line
// sum over every row), rowEdges[y] the same for horizontal boundaries.
func edgesAlong(img image.Image) (colEdges, rowEdges []int) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > 1 {
		colEdges = make([]int, w-1)
		for x := 0; x < w-1; x++ {
			s := 0
			for y := 0; y < h; y++ {
				s += diff3(nrgbaAt(img.At(x+b.Min.X, y+b.Min.Y)), nrgbaAt(img.At(x+1+b.Min.X, y+b.Min.Y)))
			}
			colEdges[x] = s
		}
	}
	if h > 1 {
		rowEdges = make([]int, h-1)
		for y := 0; y < h-1; y++ {
			s := 0
			for x := 0; x < w; x++ {
				s += diff3(nrgbaAt(img.At(x+b.Min.X, y+b.Min.Y)), nrgbaAt(img.At(x+b.Min.X, y+1+b.Min.Y)))
			}
			rowEdges[y] = s
		}
	}
	return colEdges, rowEdges
}

func diff3(a, c color.NRGBA) int {
	return intAbs(int(a.R)-int(c.R)) + intAbs(int(a.G)-int(c.G)) + intAbs(int(a.B)-int(c.B))
}

func intAbs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// detectCellAxis returns the smallest lag whose raw edge signal
// self-aligns (cosine similarity ≥ 0.85). The spread guard first rejects
// diffuse signals: a true grid concentrates its edge energy in ≈1/cell of
// the lines, so when more than 55% of lines carry ≥ 0.2·max the signal
// cannot be a grid (noise, ramps, smooth gradients — all measured ≥ 0.57
// here, grids ≤ 0.50). All comparisons run on the raw sums: smoothing
// would fuse dense grids' boundary spikes into a near-constant signal that
// self-aligns at every lag. The lag window (2 .. min(64, n/2−4)) requires
// roughly two cells per axis.
func detectCellAxis(edges []int) (int, error) {
	n := len(edges)
	if n < 8 {
		return 0, ErrNotPixelGrid
	}
	maxEdge := 0
	for _, e := range edges {
		if e > maxEdge {
			maxEdge = e
		}
	}
	if maxEdge <= 0 {
		return 0, ErrNotPixelGrid
	}
	significant := 0
	for _, e := range edges {
		if float64(e) >= 0.2*float64(maxEdge) {
			significant++
		}
	}
	if float64(significant)/float64(n) > 0.55 {
		return 0, ErrNotPixelGrid
	}
	maxLag := minInt(RepairMaxCell, n/2-4)
	for lag := RepairMinCell; lag <= maxLag; lag++ {
		num, la, lb := 0.0, 0.0, 0.0
		for i := 0; i+lag < n; i++ {
			num += float64(edges[i]) * float64(edges[i+lag])
			la += float64(edges[i]) * float64(edges[i])
			lb += float64(edges[i+lag]) * float64(edges[i+lag])
		}
		if la > 0 && lb > 0 && num/math.Sqrt(la*lb) >= 0.85 {
			return lag, nil
		}
	}
	return 0, ErrNotPixelGrid
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
// DownscaleByCell shrinks src to floor(w/cell)×floor(h/cell) by sampling
// each cell's center pixel; a partial trailing cell (bounds not a multiple
// of cell) is dropped. RepairImage rejects non-multiples before this ever
// sees one.
func DownscaleByCell(src image.Image, cell int) *image.NRGBA {
	b := src.Bounds()
	w, h := b.Dx()/cell, b.Dy()/cell
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			out.SetNRGBA(x, y, nrgbaAt(src.At(b.Min.X+x*cell+cell/2, b.Min.Y+y*cell+cell/2)))
		}
	}
	return out
}

// RepairImage quantizes src onto its dominant palette (at most colors
// colors, the ONE palette implementation from remap.go) and downscales by
// its pixel-art cell. cell 0 means auto-detect; explicit cells must be
// inside 2..64. Transparency is preserved: pixels with alpha < 128 snap to
// fully transparent, the 1-bit convention RemapImage shares with the GIF
// encoder.
func RepairImage(src image.Image, cell, colors int) (*image.NRGBA, error) {
	if colors < 1 || colors > RepairMaxColors {
		return nil, fmt.Errorf("compose: repair: colors %d out of range 1..%d", colors, RepairMaxColors)
	}
	switch {
	case cell == 0:
		detected, err := DetectGridCell(src)
		if err != nil {
			return nil, err
		}
		cell = detected
	case cell < RepairMinCell || cell > RepairMaxCell:
		return nil, fmt.Errorf("compose: repair: cell %d out of range %d..%d", cell, RepairMinCell, RepairMaxCell)
	}
	b := src.Bounds()
	if b.Dx()%cell != 0 || b.Dy()%cell != 0 {
		return nil, fmt.Errorf("compose: repair: %dx%d is not a multiple of cell %d", b.Dx(), b.Dy(), cell)
	}
	pal := SetPalette([]image.Image{src}, colors)
	quantized := RemapImage(src, pal)
	return DownscaleByCell(quantized, cell), nil
}

// RepairPNG is RepairImage over raw PNG bytes: "compose: repair: ..." errors.
func RepairPNG(data []byte, cell, colors int) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("compose: repair: not a PNG: %v", err)
	}
	out, err := RepairImage(img, cell, colors)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, fmt.Errorf("compose: repair: encode: %v", err)
	}
	return buf.Bytes(), nil
}
