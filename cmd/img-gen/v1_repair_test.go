package main

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testPixelGrid builds an exact cell-grid PNG of the requested size.
func testPixelGrid(t *testing.T, w, h, cell int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	colors := [][3]uint8{{20, 24, 31}, {232, 93, 4}, {251, 242, 54}}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := colors[((x/cell)+(y/cell)*2)%len(colors)]
			img.Set(x, y, color.RGBA{R: c[0], G: c[1], B: c[2], A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// postV1Repair posts raw JSON and returns status, Content-Type, and body.
func postV1Repair(t *testing.T, srv *httptest.Server, body string) (int, string, []byte) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/api/v1/repair", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), raw
}

func mustB64(t *testing.T, data []byte) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString(data)
}

func TestV1RepairExplicitCell(t *testing.T) {
	srv, _ := newV1ComposeServer(t)
	defer srv.Close()
	in := testPixelGrid(t, 64, 64, 8)
	status, ctype, raw := postV1Repair(t, srv, `{"image":"`+mustB64(t, in)+`","cell":8}`)
	if status != 200 || !strings.HasPrefix(ctype, "image/png") {
		t.Fatalf("status %d ctype %q body %.200s", status, ctype, raw)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("not a PNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 8 || b.Dy() != 8 {
		t.Fatalf("want 8x8, got %dx%d", b.Dx(), b.Dy())
	}
}

func TestV1RepairAutoDetect(t *testing.T) {
	srv, _ := newV1ComposeServer(t)
	defer srv.Close()
	in := testPixelGrid(t, 64, 64, 8)
	status, _, raw := postV1Repair(t, srv, `{"image":"`+mustB64(t, in)+`"}`)
	if status != 200 {
		t.Fatalf("status %d body %.200s", status, raw)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 8 || b.Dy() != 8 {
		t.Fatalf("auto-detect want 8x8, got %dx%d", b.Dx(), b.Dy())
	}
}

func TestV1RepairErrors(t *testing.T) {
	srv, _ := newV1ComposeServer(t)
	defer srv.Close()
	in := testPixelGrid(t, 64, 64, 8)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing image", `{}`, "image is required"},
		{"bad base64", `{"image":"@@@not-b64@@@"}` , "base64"},
		{"not a PNG", `{"image":"` + mustB64(t, []byte("junk")) + `"}`, "not a PNG"},
		{"too large", `{"image":"` + mustB64(t, testPixelGrid(t, 1056, 16, 4)) + `","cell":4}`, "repair supports up to 1024x1024"},
		{"cell high", `{"image":"` + mustB64(t, in) + `","cell":65}`, "cell must be between 2 and 64"},
		{"cell low", `{"image":"` + mustB64(t, in) + `","cell":1}`, "cell must be between 2 and 64"},
		{"colors high", `{"image":"` + mustB64(t, in) + `","cell":8,"colors":65}`, "colors must be between 1 and 64"},
		{"colors low", `{"image":"` + mustB64(t, in) + `","cell":8,"colors":-1}`, "colors must be between 1 and 64"},
		{"not a grid", `{"image":"` + mustB64(t, v1NoisePNG(t)) + `"}`, "not a pixel-art grid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, _, raw := postV1Repair(t, srv, tc.body)
			if status != 400 {
				t.Fatalf("want 400, got %d (%.200s)", status, raw)
			}
			if !strings.Contains(string(raw), tc.want) {
				t.Fatalf("want %q in %s", tc.want, raw)
			}
			if !strings.Contains(string(raw), `"validation_error"`) {
				t.Fatalf("want validation_error code in %s", raw)
			}
		})
	}
}

// v1NoisePNG builds a deterministic non-grid noise PNG for the negative
// detection case.
func v1NoisePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	seed := uint32(1)
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			seed = seed*1664525 + 1013904223
			r := uint8((seed >> 16) & 0xff)
			seed = seed*1664525 + 1013904223
			g := uint8((seed >> 16) & 0xff)
			seed = seed*1664525 + 1013904223
			b := uint8((seed >> 16) & 0xff)
			img.Set(x, y, color.RGBA{R: r, G: g, B: b, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}