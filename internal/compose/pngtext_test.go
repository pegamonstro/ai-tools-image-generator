package compose

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

// renderTestPNG returns a tiny PNG of the given size (solid white).
func renderTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	c := color.RGBA{255, 255, 255, 255}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: c.R, G: c.G, B: c.B, A: c.A})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// pngDecode decodes a PNG byte slice.
func pngDecode(p []byte) (image.Image, error) {
	return png.Decode(bytes.NewReader(p))
}

// scanText walks every PNG chunk of p and returns the tEXt key/value pairs
// (order preserved) after verifying each chunk's CRC.
func scanText(t *testing.T, p []byte) map[string]string {
	t.Helper()
	if bytes.Equal(p[12:16], []byte("IHDR")) == false {
		t.Fatalf("not a png chunk stream: % x", p[:8])
	}
	off := 8
	out := map[string]string{}
	for off < len(p) {
		l := int(binary.BigEndian.Uint32(p[off : off+4]))
		typ := string(p[off+4 : off+8])
		data := p[off+8 : off+8+l]
		sum := binary.BigEndian.Uint32(p[off+8+l : off+12+l])
		if crc32.ChecksumIEEE(p[off+4:off+8+l]) != sum {
			t.Fatalf("chunk %s crc mismatch", typ)
		}
		if typ == "tEXt" {
			i := bytes.IndexByte(data, 0)
			out[string(data[:i])] = string(data[i+1:])
		}
		off += 12 + l
		if typ == "IEND" {
			break
		}
	}
	return out
}

func TestEmbedTextPNG(t *testing.T) {
	src := renderTestPNG(t, 16, 16) // tiny png via png.Encode
	out, err := EmbedTextPNG(src, map[string]string{"software": "img-gen", "seed": "42"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(out) <= len(src) {
		t.Fatal("expected output larger than input")
	}
	// Output must still decode as a PNG (stdlib ignores tEXt).
	if _, err := pngDecode(out); err != nil {
		t.Fatalf("embedded png undecodable: %v", err)
	}
	got := scanText(t, out)
	if got["software"] != "img-gen" || got["seed"] != "42" {
		t.Fatalf("tEXt mismatch: %v", got)
	}
}

func TestEmbedTextPNGRejectsAndTruncates(t *testing.T) {
	if _, err := EmbedTextPNG([]byte("not a png"), map[string]string{"a": "b"}); err == nil {
		t.Fatal("want error for non-png input")
	}
	// IEND tamper: truncate past last chunk
	full := renderTestPNG(t, 16, 16)
	bad := full[:len(full)-12]
	if _, err := EmbedTextPNG(bad, nil); err == nil {
		t.Fatal("want error for missing IEND")
	}
	long := strings.Repeat("x", 5000)
	out, err := EmbedTextPNG(renderTestPNG(t, 16, 16), map[string]string{"parameters": long})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if got := scanText(t, out)["parameters"]; len(got) != 4096 {
		t.Fatalf("want value truncated to 4096, got %d", len(got))
	}
}

func TestScanTextChunksRejectsMangledCRC(t *testing.T) {
	src, err := EmbedTextPNG(renderTestPNG(t, 16, 16), map[string]string{"k": "v"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	bad := append([]byte(nil), src...)
	// the embedded tEXt chunk's CRC is the 4 bytes just before IEND's
	// 12-byte trailer — flip one to prove verification is real (the spec's
	// "hand-mangled chunk is rejected" test)
	bad[len(bad)-16] ^= 0xFF
	if _, err := ScanTextChunks(bad); err == nil {
		t.Fatal("want CRC mismatch error")
	}
}

