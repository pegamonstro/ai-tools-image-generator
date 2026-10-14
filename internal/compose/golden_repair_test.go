package compose

import (
	"bytes"
	"errors"
	"image"
	"image/png"
	"os"
	"testing"
)

// TestRepairGoldenCircuitCap recomputes the repair stages for the committed
// real artifact and asserts byte equality with the committed goldens.
// Provenance: input.png is circuit-cap-n.png from the mock-site render run
// (see the task report that committed it).
//
// The committed input is a soft SDXL render: its edge energy is diffuse, so
// DetectGridCell refuses it — that verdict is part of what this test pins.
// The repair stages run with the explicit cell 4 the goldens were generated
// with (the same argument values; if detection calibration changes and the
// refusal flips, that is a threshold recalibration to rule on, not a silent
// test mutation).
func TestRepairGoldenCircuitCap(t *testing.T) {
	raw, err := os.ReadFile("testdata/repair/input.png")
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, derr := DetectGridCell(img); !errors.Is(derr, ErrNotPixelGrid) {
		t.Fatalf("real render now auto-detects (calibration moved): %v", derr)
	}
	const cell = 4
	pal := SetPalette([]image.Image{img}, 32)

	// Stage 1 golden: quantized buffer (pre-downscale).
	var qbuf bytes.Buffer
	if err := png.Encode(&qbuf, RemapImage(img, pal)); err != nil {
		t.Fatal(err)
	}
	qc, err := os.ReadFile("testdata/repair/quantized.png")
	if err != nil {
		t.Fatalf("quantized golden missing: %v", err)
	}
	if !bytes.Equal(qbuf.Bytes(), qc) {
		t.Fatal("quantized golden drifted")
	}

	// Stage 2 golden: palette envelope.
	pj, err := MarshalPalette(pal)
	if err != nil {
		t.Fatal(err)
	}
	pc, err := os.ReadFile("testdata/repair/palette.json")
	if err != nil {
		t.Fatalf("palette golden missing: %v", err)
	}
	if !bytes.Equal(pj, pc) {
		t.Fatal("palette golden drifted")
	}

	// Stage 3 golden: full repair (quantize + downscale by the pinned cell).
	qfull, err := RepairPNG(raw, cell, 32)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	dc, err := os.ReadFile("testdata/repair/downscaled.png")
	if err != nil {
		t.Fatalf("downscaled golden missing: %v", err)
	}
	if !bytes.Equal(qfull, dc) {
		t.Fatal("downscaled golden drifted")
	}
}
