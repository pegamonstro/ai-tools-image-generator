package compose

import (
	"errors"
	"image"
	"image/color"
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
	max := 0
	for _, e := range edges {
		if e > max {
			max = e
		}
	}
	if max <= 0 {
		return 0, ErrNotPixelGrid
	}
	significant := 0
	for _, e := range edges {
		if float64(e) >= 0.2*float64(max) {
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