package compose

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
)

// Split9SliceConfig holds configuration for split 9-slice mode
type Split9SliceConfig struct {
	Enabled    string // "true" or "false"
	Margin     int
	Format     string // "single" or "split"
	CSSPrefix  string // Optional prefix for CSS class names (e.g., asset kind)
}

// CSSClass represents a CSS class with its properties
type CSSClass struct {
	Name       string                 `json:"name"`
	Properties map[string]string      `json:"properties"`
}

// CSSManifest represents the complete CSS output for asset sets
type CSSManifest struct {
	Classes []CSSClass `json:"classes"`
	Version string     `json:"version"`
}

// NewCSSManifest creates a new CSS manifest
func NewCSSManifest() CSSManifest {
	return CSSManifest{
		Classes: []CSSClass{},
		Version: "1.0",
	}
}

// AddCSSClass adds a CSS class to the manifest
func (m *CSSManifest) AddCSSClass(name string, properties map[string]string) {
	m.Classes = append(m.Classes, CSSClass{
		Name:       name,
		Properties: properties,
	})
}

// EncodeCSS encodes the CSS manifest as a byte slice
func (m CSSManifest) EncodeCSS() ([]byte, error) {
	return json.MarshalIndent(m, "", "  ")
}

// marginsJSON is the sidecar margins block shared by frame and bar files.
type marginsJSON struct {
	Top    int `json:"top"`
	Right  int `json:"right"`
	Bottom int `json:"bottom"`
	Left   int `json:"left"`
}

func uniformMargins(m int) marginsJSON {
	return marginsJSON{Top: m, Right: m, Bottom: m, Left: m}
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("compose: png encode: %v", err)
	}
	return buf.Bytes(), nil
}

// PunchNineSlice returns a copy with the interior rect [m, w-m)x[m, h-m)
// forced fully transparent. m must be 1..min(w,h)/2-1 — the same bounds the
// queue validates.
func PunchNineSlice(img image.Image, m int) (*image.NRGBA, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if m < 1 || 2*m >= w || 2*m >= h {
		return nil, fmt.Errorf("compose: nine-slice margin %d out of bounds for %dx%d", m, w, h)
	}
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(out, out.Bounds(), img, b.Min, draw.Src)
	for y := m; y < h-m; y++ {
		for x := m; x < w-m; x++ {
			out.SetNRGBA(x, y, color.NRGBA{})
		}
	}
	return out, nil
}

// ComposeFrame produces a nine-slice frame deliverable from the element
// render path: the border art PNG with the interior punched transparent,
// plus the margins sidecar.
func ComposeFrame(name string, white, black []byte, transparent bool, margins int, splitConfig Split9SliceConfig) ([]OutputFile, error) {
	// Load element as before
	el, err := ComposeElement(name, white, black, transparent)
	if err != nil {
		return nil, err
	}
	img, err := png.Decode(bytes.NewReader(el.Data))
	if err != nil {
		return nil, fmt.Errorf("compose: frame %q render is not a PNG: %v", name, err)
	}

	// Validate margins
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if margins < 1 || 2*margins >= w || 2*margins >= h {
		return nil, fmt.Errorf("compose: nine-slice margin %d out of bounds for %dx%d", margins, w, h)
	}

	// If split mode is enabled
	if splitConfig.Enabled == "true" {
		margin := margins

		// Encode a region to PNG
		encodeRegion := func(x0, y0, x1, y1 int) ([]byte, error) {
			bounds := image.Rect(x0, y0, x1, y1)
			cropped := image.NewNRGBA(bounds)
			draw.Draw(cropped, bounds, img, img.Bounds().Min, draw.Src)
			return encodePNG(cropped)
		}

		// Encode all regions
		topLeftData, err := encodeRegion(0, 0, margin, margin)
		if err != nil {
			return nil, err
		}
		topData, err := encodeRegion(margin, 0, w-margin, margin)
		if err != nil {
			return nil, err
		}
		topRightData, err := encodeRegion(w-margin, 0, w, margin)
		if err != nil {
			return nil, err
		}
		rightData, err := encodeRegion(w-margin, margin, w, h-margin)
		if err != nil {
			return nil, err
		}
		bottomRightData, err := encodeRegion(w-margin, h-margin, w, h)
		if err != nil {
			return nil, err
		}
		bottomData, err := encodeRegion(margin, h-margin, w-margin, h)
		if err != nil {
			return nil, err
		}
		bottomLeftData, err := encodeRegion(0, h-margin, margin, h)
		if err != nil {
			return nil, err
		}
		leftData, err := encodeRegion(0, margin, margin, h-margin)
		if err != nil {
			return nil, err
		}
		centerData, err := encodeRegion(margin, margin, w-margin, h-margin)
		if err != nil {
			return nil, err
		}

		// Create a map of region names to encoded data
		regionData := map[string][]byte{
			"top-left":     topLeftData,
			"top":          topData,
			"top-right":    topRightData,
			"right":        rightData,
			"bottom-right": bottomRightData,
			"bottom":       bottomData,
			"bottom-left":  bottomLeftData,
			"left":         leftData,
			"center":       centerData,
		}

		// Create output slice
		var outputs []OutputFile

		// Add each region as a separate PNG file
		for regionName, data := range regionData {
			outFile := OutputFile{
				Name: name + "-" + regionName + ".png",
				Data: data,
			}
			// Mark the center region as primary
			if regionName == "center" {
				outFile.Primary = true
			}
			outputs = append(outputs, outFile)
		}

		// Generate insets JSON
		type insetsJSON struct {
			Layout  string                     `json:"layout"`
			Margins marginsJSON                `json:"margins"`
			Insets  map[string]struct {
				X      int `json:"x"`
				Y      int `json:"y"`
				Width  int `json:"width"`
				Height int `json:"height"`
			} `json:"insets"`
		}
		insets := insetsJSON{
			Layout: "nine-slice",
			Margins: uniformMargins(margin),
			Insets: map[string]struct {
				X      int `json:"x"`
				Y      int `json:"y"`
				Width  int `json:"width"`
				Height int `json:"height"`
			}{
				"top-left":     {X: 0, Y: 0, Width: margin, Height: margin},
				"top":          {X: margin, Y: 0, Width: w - 2*margin, Height: margin},
				"top-right":    {X: w - margin, Y: 0, Width: margin, Height: margin},
				"right":        {X: w - margin, Y: margin, Width: margin, Height: h - 2*margin},
				"bottom-right": {X: w - margin, Y: h - margin, Width: margin, Height: margin},
				"bottom":       {X: margin, Y: h - margin, Width: w - 2*margin, Height: margin},
				"bottom-left":  {X: 0, Y: h - margin, Width: margin, Height: margin},
				"left":         {X: 0, Y: margin, Width: margin, Height: h - 2*margin},
			},
		}
		insetsBytes, err := json.Marshal(insets)
		if err != nil {
			return nil, err
		}
		insetsFile := OutputFile{
			Name: name + ".insets.json",
			Data: insetsBytes,
		}
		outputs = append(outputs, insetsFile)

		// CSS emission
		// Create a CSS manifest and add classes for each region
		cssMan := NewCSSManifest()
		for regionName := range regionData {
			className := regionName
			if splitConfig.CSSPrefix != "" {
				className = splitConfig.CSSPrefix + "-" + regionName
			}
			cssMan.AddCSSClass(className, map[string]string{
				"background-image": "url('" + name + "-" + regionName + ".png')",
				"width":           "auto",
				"height":          "auto",
			})
		}
		cssBytes, err := cssMan.EncodeCSS()
		if err != nil {
			return nil, err
		}
		cssFile := OutputFile{
			Name: "assets.css",
			Data: cssBytes,
		}
		outputs = append(outputs, cssFile)

		return outputs, nil
	}

	// Non-split (single) mode - keep existing behavior
	punched, err := PunchNineSlice(img, margins)
	if err != nil {
		return nil, err
	}
	data, err := encodePNG(punched)
	if err != nil {
		return nil, err
	}
	meta, err := json.Marshal(struct {
		Layout  string      `json:"layout"`
		Margins marginsJSON `json:"margins"`
	}{Layout: "nine-slice", Margins: uniformMargins(margins)})
	if err != nil {
		return nil, err
	}
	return []OutputFile{
		{Name: name + ".png", Data: data, Primary: true},
		{Name: name + ".frame.json", Data: meta},
	}, nil
}
