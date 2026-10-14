package compose

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"sort"
)

// pngTextMax caps one tEXt value; keeps history.jsonl and PNG params alike
// in size. PNG allows far more, this keeps embeds practical.
const pngTextMax = 4096

// EmbedTextPNG splices ASCII tEXt chunks (one per key, sorted by key for
// determinism) before the IEND chunk of a PNG. Returns the input unchanged
// when fields is empty. Errors on non-PNG input or a missing IEND trailer;
// it never "fixes" anything else about the stream.
func EmbedTextPNG(p []byte, fields map[string]string) ([]byte, error) {
	if len(p) < 45 || !bytes.HasPrefix(p, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}) {
		return nil, errors.New("pngtext: not a png")
	}
	const iendLen = 12
	tail := p[len(p)-iendLen:]
	if binary.BigEndian.Uint32(tail[0:4]) != 0 || string(tail[4:8]) != "IEND" {
		return nil, errors.New("pngtext: missing IEND trailer")
	}
	if len(fields) == 0 {
		return p, nil
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		if k == "" || len(k) > 79 || bytes.ContainsAny([]byte(k), "\x00 ") {
			return nil, errors.New("pngtext: invalid keyword")
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ins := make([]byte, 0, 64)
	for _, k := range keys {
		v := fields[k]
		if len(v) > pngTextMax {
			v = v[:pngTextMax]
		}
		ins = appendChunk(ins, "tEXt", append(append([]byte(k), 0), v...))
	}
	out := make([]byte, 0, len(p)+len(ins))
	out = append(out, p[:len(p)-iendLen]...)
	out = append(out, ins...)
	return append(out, tail...), nil
}

func appendChunk(dst []byte, typ string, data []byte) []byte {
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(len(data)))
	copy(hdr[4:8], typ)
	dst = append(dst, hdr[:]...)
	dst = append(dst, data...)
	binary.BigEndian.PutUint32(hdr[0:4], crc32.ChecksumIEEE(append([]byte(typ), data...)))
	return append(dst, hdr[0:4]...)
}

// ScanTextChunks walks every PNG chunk, verifies each CRC, and returns the
// tEXt key/value pairs. Errors on non-PNG input, truncated chunk framing, or
// a CRC mismatch — it is the reader half of EmbedTextPNG and the delivery
// path tests' verifier.
func ScanTextChunks(p []byte) (map[string]string, error) {
	if len(p) < 8 || !bytes.HasPrefix(p, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}) {
		return nil, errors.New("pngtext: not a png")
	}
	out := map[string]string{}
	off := 8
	for off < len(p) {
		if off+12 > len(p) {
			return nil, errors.New("pngtext: truncated chunk header")
		}
		ll := int64(binary.BigEndian.Uint32(p[off : off+4]))
		if ll < 0 {
			return nil, errors.New("pngtext: negative chunk length")
		}
		if int64(off)+12+ll > int64(len(p)) {
			return nil, errors.New("pngtext: truncated chunk body")
		}
		l := int(ll)
		typ := string(p[off+4 : off+8])
		data := p[off+8 : off+8+l]
		if crc32.ChecksumIEEE(p[off+4:off+8+l]) != binary.BigEndian.Uint32(p[off+8+l:off+12+l]) {
			return nil, fmt.Errorf("pngtext: chunk %s crc mismatch", typ)
		}
		if typ == "tEXt" {
			i := bytes.IndexByte(data, 0)
			if i < 0 {
				return nil, errors.New("pngtext: tEXt missing keyword separator")
			}
			out[string(data[:i])] = string(data[i+1:])
		}
		off += 12 + l
		if typ == "IEND" {
			break
		}
	}
	return out, nil
}
