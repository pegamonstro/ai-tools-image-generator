package compose

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
)

// SetPalette builds a set palette over imgs with the same deterministic
// median-cut the GIF encoder uses. Opaque colors only — the reserved
// transparent index-0 entry sharedPalette emits for the GIF encoder is
// dropped here, so a nearest-match tie on black cannot resolve to a
// transparent color and the marshaled file stays within 255 entries.
func SetPalette(imgs []image.Image, n int) color.Palette {
	pal := sharedPalette(imgs, n)
	out := make(color.Palette, 0, len(pal))
	for _, c := range pal {
		if _, _, _, a := c.RGBA(); a == 0 {
			continue
		}
		out = append(out, c)
	}
	return out
}

// PaletteHexes renders the palette as #rrggbb strings, in palette order
// (median-cut emission order is deterministic, so the file bytes are too).
func PaletteHexes(pal color.Palette) []string {
	hexes := make([]string, 0, len(pal))
	for _, c := range pal {
		n := color.NRGBAModel.Convert(c).(color.NRGBA)
		hexes = append(hexes, fmt.Sprintf("#%02x%02x%02x", n.R, n.G, n.B))
	}
	return hexes
}

// MarshalPalette writes {"colors": ["#rrggbb", ...]} — the set palette file.
func MarshalPalette(pal color.Palette) ([]byte, error) {
	type paletteFile struct {
		Colors []string `json:"colors"`
	}
	return json.Marshal(paletteFile{Colors: PaletteHexes(pal)})
}

// RemapImage returns a copy of img with every opaque pixel snapped to its
// CIEDE2000-nearest palette color. Pixels with alpha < 128 are left fully
// transparent — the 1-bit alpha convention shared with the GIF encoder.
func RemapImage(img image.Image, pal color.Palette) *image.NRGBA {
	labs := paletteLAB(pal)
	b := img.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			n := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			if n.A < 128 {
				out.SetNRGBA(x-b.Min.X, y-b.Min.Y, color.NRGBA{})
				continue
			}
			l, a, bb := rgbToLAB(float64(n.R), float64(n.G), float64(n.B))
			best := 0
			bestD := math.MaxFloat64
			for i, lp := range labs {
				d := cie2000(l, a, bb, lp[0], lp[1], lp[2])
				if d < bestD {
					bestD = d
					best = i
				}
			}
			out.SetNRGBA(x-b.Min.X, y-b.Min.Y, nrgbaAt(pal[best]))
		}
	}
	return out
}

// RemapPNG decodes a PNG, remaps it, and re-encodes. Data must be a
// decodable PNG; anything else is an error.
func RemapPNG(data []byte, pal color.Palette) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("compose: remap: not a PNG: %v", err)
	}
	out := RemapImage(img, pal)
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func nrgbaAt(c color.Color) color.NRGBA {
	return color.NRGBAModel.Convert(c).(color.NRGBA)
}

func paletteLAB(pal color.Palette) [][3]float64 {
	labs := make([][3]float64, len(pal))
	for i, c := range pal {
		r, g, b, _ := c.RGBA()
		l, a, bb := rgbToLAB(float64(r>>8), float64(g>>8), float64(b>>8))
		labs[i] = [3]float64{l, a, bb}
	}
	return labs
}

// rgbToLAB converts an sRGB color (8-bit components) to CIELAB (D65).
func rgbToLAB(r, g, b float64) (l, a, bb float64) {
	lin := func(v float64) float64 {
		v /= 255
		if v <= 0.04045 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	rl, gl, bl := lin(r), lin(g), lin(b)
	x := 0.4124564*rl + 0.3575761*gl + 0.1804375*bl
	y := 0.2126729*rl + 0.7151522*gl + 0.0721750*bl
	z := 0.0193339*rl + 0.1191920*gl + 0.9503041*bl
	f := func(t float64) float64 {
		if t > 216.0/24389.0 {
			return math.Cbrt(t)
		}
		return (24389.0/27.0*t + 16) / 116
	}
	fx, fy, fz := f(x/0.95047), f(y/1.0), f(z/1.08883)
	return 116*fy - 16, 500 * (fx - fy), 200 * (fy - fz)
}

// cie2000 is the full seven-term CIEDE2000 color difference between two
// CIELAB colors (hue terms work in degrees), per the Sharma/Wu/Dalal
// reference semantics: hue difference wraps at ±180, and when the adjusted
// chroma product is zero the hue-bar is the plain sum (not the mean) and the
// hue-difference term vanishes.
func cie2000(l1, a1, b1, l2, a2, b2 float64) float64 {
	rad := func(deg float64) float64 { return deg * math.Pi / 180 }
	c1 := math.Hypot(a1, b1)
	c2 := math.Hypot(a2, b2)
	cbar := (c1 + c2) / 2
	g := 0.5 * (1 - math.Sqrt(math.Pow(cbar, 7)/(math.Pow(cbar, 7)+math.Pow(25, 7))))
	a1p := (1 + g) * a1
	a2p := (1 + g) * a2
	c1p := math.Hypot(a1p, b1)
	c2p := math.Hypot(a2p, b2)
	h1p := huep(a1p, b1)
	h2p := huep(a2p, b2)
	dlp := l2 - l1
	dcp := c2p - c1p
	dhp := 0.0
	if c1p*c2p != 0 {
		dhp = h2p - h1p
		if dhp > 180 {
			dhp -= 360
		} else if dhp < -180 {
			dhp += 360
		}
	}
	dhp2 := 2 * math.Sqrt(c1p*c2p) * math.Sin(rad(dhp)/2)
	lbp := (l1 + l2) / 2
	cbp := (c1p + c2p) / 2
	hbp := h1p + h2p
	if c1p*c2p != 0 {
		if math.Abs(h1p-h2p) > 180 {
			if h1p+h2p < 360 {
				hbp += 360
			} else {
				hbp -= 360
			}
		}
		hbp /= 2
	}
	t := 1 - 0.17*math.Cos(rad(hbp-30)) + 0.24*math.Cos(rad(2*hbp)) +
		0.32*math.Cos(rad(3*hbp+6)) - 0.20*math.Cos(rad(4*hbp-63))
	dtheta := 30 * math.Exp(-math.Pow((hbp-275)/25, 2))
	rc := 2 * math.Sqrt(math.Pow(cbp, 7)/(math.Pow(cbp, 7)+math.Pow(25, 7)))
	sl := 1 + 0.015*(lbp-50)*(lbp-50)/math.Sqrt(20+(lbp-50)*(lbp-50))
	sc := 1 + 0.045*cbp
	sh := 1 + 0.015*cbp*t
	rt := -math.Sin(rad(2*dtheta)) * rc
	return math.Sqrt(
		(dlp/sl)*(dlp/sl) +
			(dcp/sc)*(dcp/sc) +
			(dhp2/sh)*(dhp2/sh) +
			rt*(dcp/sc)*(dhp2/sh))
}

func huep(a, b float64) float64 {
	h := math.Atan2(b, a) * 180 / math.Pi
	if h < 0 {
		h += 360
	}
	return h
}
