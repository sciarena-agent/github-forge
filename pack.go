// pack.go — pkt-line framing, packfile generation and parsing.
//
// Faithful port of git.ts (pack section). Packfiles are v2, plain objects,
// no deltas on generation; parsing resolves OFS_DELTA and REF_DELTA entries
// (including thin packs via resolveBase) and verifies the trailer SHA-1 and
// the declared entry count. Objects are zlib-deflated with compress/zlib at
// DefaultCompression.
//
// Parsing does ONE inflate per object, in a single forward pass over the
// pack: each zlib stream is inflated exactly once, starting at its own
// offset, and the stream end is recovered from the underlying bytes.Reader
// position (flate reads byte-at-a-time from a *bytes.Reader, so it cannot
// consume bytes past the end of the stream). This is the v3 lesson —
// never inflate the same object twice (the TS inflateEnd binary search
// re-inflated each entry O(log n) times).
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

const ZeroOID = "0000000000000000000000000000000000000000"

// ------------------------------------------------------------------
// pkt-line
// ------------------------------------------------------------------

type PktKind string

const (
	PktData        PktKind = "data"
	PktFlush       PktKind = "flush"
	PktDelim       PktKind = "delim"
	PktResponseEnd PktKind = "response-end"
)

type Pkt struct {
	Kind PktKind
	Data []byte
}

var (
	FlushPacket       = []byte("0000")
	DelimPacket       = []byte("0001")
	ResponseEndPacket = []byte("0002")
)

// EncodePktLine encodes one pkt-line. Payload must be <= 65516 bytes.
func EncodePktLine(data []byte) ([]byte, error) {
	if len(data) > 65516 {
		return nil, errors.New("pkt-line payload too large")
	}
	out := make([]byte, 4, 4+len(data))
	copy(out, []byte(fmt.Sprintf("%04x", len(data)+4)))
	return append(out, data...), nil
}

func mustPktLine(data []byte) []byte {
	b, err := EncodePktLine(data)
	if err != nil {
		panic(err)
	}
	return b
}

func mustPktLineString(s string) []byte { return mustPktLine([]byte(s)) }

// EncodePktLines encodes several payloads as concatenated pkt-lines.
func EncodePktLines(lines [][]byte) []byte {
	var out []byte
	for _, l := range lines {
		out = append(out, mustPktLine(l)...)
	}
	return out
}

func encodePktLineStrings(lines []string) []byte {
	var out []byte
	for _, l := range lines {
		out = append(out, mustPktLineString(l)...)
	}
	return out
}

// DecodePktLines decodes a whole buffer of pkt-lines.
func DecodePktLines(buf []byte) ([]Pkt, error) {
	var out []Pkt
	offset := 0
	for offset < len(buf) {
		pkt, next, err := decodeOnePkt(buf, offset)
		if err != nil {
			return nil, err
		}
		out = append(out, pkt)
		offset = next
	}
	return out, nil
}

func decodeOnePkt(buf []byte, offset int) (Pkt, int, error) {
	if offset+4 > len(buf) {
		return Pkt{}, 0, errors.New("truncated pkt-line length")
	}
	hexStr := string(buf[offset : offset+4])
	n, err := parseHex4(hexStr)
	if err != nil {
		return Pkt{}, 0, fmt.Errorf("invalid pkt-line length %q", hexStr)
	}
	switch n {
	case 0:
		return Pkt{Kind: PktFlush}, offset + 4, nil
	case 1:
		return Pkt{Kind: PktDelim}, offset + 4, nil
	case 2:
		return Pkt{Kind: PktResponseEnd}, offset + 4, nil
	default:
		if offset+n > len(buf) {
			return Pkt{}, 0, errors.New("truncated pkt-line payload")
		}
		data := make([]byte, n-4)
		copy(data, buf[offset+4:offset+n])
		return Pkt{Kind: PktData, Data: data}, offset + n, nil
	}
}

func parseHex4(s string) (int, error) {
	if len(s) != 4 {
		return 0, errors.New("bad hex length")
	}
	v := 0
	for i := 0; i < 4; i++ {
		c := s[i]
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int(c-'A') + 10
		default:
			return 0, errors.New("bad hex digit")
		}
		v = v*16 + d
	}
	return v, nil
}

// SplitPktAndRest splits a body into pkt-line packets up to and including
// the first flush, plus the trailing raw bytes (the pack after the flush
// in a receive-pack request).
func SplitPktAndRest(buf []byte) ([]Pkt, []byte, error) {
	var packets []Pkt
	offset := 0
	for offset < len(buf) {
		pkt, next, err := decodeOnePkt(buf, offset)
		if err != nil {
			return nil, nil, err
		}
		packets = append(packets, pkt)
		offset = next
		if pkt.Kind == PktFlush {
			return packets, buf[offset:], nil
		}
	}
	return packets, []byte{}, nil
}

// ------------------------------------------------------------------
// SHA-1 helpers (same bytes as store.go Sha1 / ObjHash)
// ------------------------------------------------------------------

func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

func objectSha(typ string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// ------------------------------------------------------------------
// packfiles
// ------------------------------------------------------------------

var typeCode = map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var codeType = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

type PackEntry struct {
	Type string
	Data []byte
}

type PackObject struct {
	Oid  string
	Type string
	Data []byte
}

// readVarint reads a git object header: first byte low 4 bits + type.
func readVarint(buf []byte, offset int) (typ int, size int, next int, err error) {
	if offset >= len(buf) {
		return 0, 0, 0, errors.New("truncated object header")
	}
	b := buf[offset]
	typ = int(b>>4) & 0x7
	size = int(b & 0x0f)
	shift := uint(4)
	i := offset + 1
	for b&0x80 != 0 {
		if i >= len(buf) {
			return 0, 0, 0, errors.New("truncated object header")
		}
		b = buf[i]
		size |= int(b&0x7f) << shift
		shift += 7
		i++
	}
	return typ, size, i, nil
}

// ApplyDelta applies a binary delta (git delta format) to a base buffer.
func ApplyDelta(base, delta []byte) ([]byte, error) {
	offset := 0
	readVar := func() (int, error) {
		value := 0
		shift := uint(0)
		for {
			if offset >= len(delta) {
				return 0, errors.New("truncated delta varint")
			}
			b := delta[offset]
			offset++
			value |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value, nil
	}
	sourceSize, err := readVar()
	if err != nil {
		return nil, err
	}
	targetSize, err := readVar()
	if err != nil {
		return nil, err
	}
	if sourceSize != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	var chunks [][]byte
	total := 0
	for offset < len(delta) {
		opcode := delta[offset]
		offset++
		if opcode&0x80 != 0 {
			copyOffset := 0
			copySize := 0
			for i := 0; i < 4; i++ {
				if opcode&(1<<uint(i)) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("truncated delta copy")
					}
					copyOffset |= int(delta[offset]) << (8 * uint(i))
					offset++
				}
			}
			for i := 0; i < 3; i++ {
				if opcode&(0x10<<uint(i)) != 0 {
					if offset >= len(delta) {
						return nil, errors.New("truncated delta copy")
					}
					copySize |= int(delta[offset]) << (8 * uint(i))
					offset++
				}
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOffset < 0 || copyOffset+copySize > len(base) {
				return nil, errors.New("delta copy out of range")
			}
			chunk := base[copyOffset : copyOffset+copySize]
			chunks = append(chunks, chunk)
			total += len(chunk)
		} else if opcode != 0 {
			if offset+int(opcode) > len(delta) {
				return nil, errors.New("truncated delta insert")
			}
			chunk := delta[offset : offset+int(opcode)]
			offset += int(opcode)
			chunks = append(chunks, chunk)
			total += len(chunk)
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	out := make([]byte, 0, total)
	for _, c := range chunks {
		out = append(out, c...)
	}
	if len(out) != targetSize {
		return nil, errors.New("delta target size mismatch")
	}
	return out, nil
}

// inflateAt inflates the single zlib stream starting at offset in pack and
// returns the inflated bytes and the offset just past the stream. Exactly
// one inflate pass is performed over these bytes.
func inflateAt(pack []byte, offset int) ([]byte, int, error) {
	br := bytes.NewReader(pack[offset:])
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, err
	}
	defer zr.Close()
	data, err := io.ReadAll(zr)
	if err != nil {
		return nil, 0, err
	}
	if err := zr.Close(); err != nil {
		return nil, 0, err
	}
	consumed, err := br.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, 0, err
	}
	return data, offset + int(consumed), nil
}

// ReadPack parses a packfile, resolving OFS_DELTA and REF_DELTA entries
// (against earlier pack entries, or against resolveBase for thin packs).
// It verifies the trailer SHA-1 and the declared entry count.
func ReadPack(pack []byte, resolveBase func(oid string) (*GitObject, bool)) ([]PackObject, error) {
	if len(pack) < 32 {
		return nil, errors.New("pack too small")
	}
	if string(pack[0:4]) != "PACK" {
		return nil, errors.New("bad pack magic")
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	count := binary.BigEndian.Uint32(pack[8:12])
	trailerStart := len(pack) - 20
	if sha1Hex(pack[:trailerStart]) != hex.EncodeToString(pack[trailerStart:]) {
		return nil, errors.New("pack checksum mismatch")
	}
	entries := make([]PackObject, 0, count)
	byOid := make(map[string]*PackObject, count)
	byOffset := make(map[int]*PackObject, count)
	offset := 12
	for n := uint32(0); n < count; n++ {
		entryOffset := offset
		typ, size, next, err := readVarint(pack, offset)
		if err != nil {
			return nil, err
		}
		offset = next
		var entryType string
		var data []byte
		switch typ {
		case 6: // OFS_DELTA
			if offset >= len(pack) {
				return nil, errors.New("truncated ofs-delta")
			}
			b := pack[offset]
			offset++
			baseOffset := int(b & 0x7f)
			for b&0x80 != 0 {
				if offset >= len(pack) {
					return nil, errors.New("truncated ofs-delta")
				}
				b = pack[offset]
				offset++
				baseOffset = ((baseOffset + 1) << 7) | int(b&0x7f)
			}
			base := byOffset[entryOffset-baseOffset]
			if base == nil {
				return nil, errors.New("ofs-delta base not found")
			}
			inflated, end, err := inflateAt(pack, offset)
			if err != nil {
				return nil, err
			}
			offset = end
			data, err = ApplyDelta(base.Data, inflated)
			if err != nil {
				return nil, err
			}
			entryType = base.Type
		case 7: // REF_DELTA
			if offset+20 > len(pack) {
				return nil, errors.New("truncated ref-delta")
			}
			baseOid := hex.EncodeToString(pack[offset : offset+20])
			offset += 20
			inflated, end, err := inflateAt(pack, offset)
			if err != nil {
				return nil, err
			}
			offset = end
			var base *PackObject
			if e, ok := byOid[baseOid]; ok {
				base = e
			} else if resolveBase != nil {
				if obj, ok := resolveBase(baseOid); ok {
					base = &PackObject{Oid: baseOid, Type: obj.Type, Data: obj.Data}
				}
			}
			if base == nil {
				return nil, fmt.Errorf("ref-delta base %s not found", baseOid)
			}
			data, err = ApplyDelta(base.Data, inflated)
			if err != nil {
				return nil, err
			}
			entryType = base.Type
		default:
			var ok bool
			entryType, ok = codeType[typ]
			if !ok {
				return nil, fmt.Errorf("unknown pack object type %d", typ)
			}
			inflated, end, err := inflateAt(pack, offset)
			if err != nil {
				return nil, err
			}
			offset = end
			data = inflated
			if len(data) != size {
				return nil, errors.New("pack entry size mismatch")
			}
		}
		oid := objectSha(entryType, data)
		e := PackObject{Oid: oid, Type: entryType, Data: data}
		entries = append(entries, e)
		byOid[oid] = &entries[len(entries)-1]
		byOffset[entryOffset] = &entries[len(entries)-1]
	}
	if uint32(len(entries)) != count {
		return nil, errors.New("pack entry count mismatch")
	}
	if offset != trailerStart {
		return nil, errors.New("pack length mismatch")
	}
	return entries, nil
}

// WritePack builds a packfile (v2, plain objects, no deltas) from entries.
func WritePack(entries []PackEntry) []byte {
	var body bytes.Buffer
	head := make([]byte, 12)
	copy(head, "PACK")
	binary.BigEndian.PutUint32(head[4:8], 2)
	binary.BigEndian.PutUint32(head[8:12], uint32(len(entries)))
	body.Write(head)
	for _, entry := range entries {
		typeCode := typeCode[entry.Type]
		size := len(entry.Data)
		remaining := size >> 4
		first := byte((typeCode << 4) | (size & 0x0f))
		if remaining > 0 {
			first |= 0x80
		}
		body.WriteByte(first)
		for remaining > 0 {
			b := byte(remaining & 0x7f)
			remaining >>= 7
			if remaining > 0 {
				b |= 0x80
			}
			body.WriteByte(b)
		}
		var zb bytes.Buffer
		zw, err := zlib.NewWriterLevel(&zb, zlib.DefaultCompression)
		if err != nil {
			panic(err)
		}
		if _, err := zw.Write(entry.Data); err != nil {
			panic(err)
		}
		if err := zw.Close(); err != nil {
			panic(err)
		}
		body.Write(zb.Bytes())
	}
	out := body.Bytes()
	trailer := sha1.Sum(out)
	return append(out, trailer[:]...)
}

// Sideband wraps payload chunks into sideband packets (band byte + data),
// pkt-line framed. Empty data produces no output.
func Sideband(data []byte, band byte, chunkSize int) []byte {
	if len(data) == 0 {
		return nil
	}
	if chunkSize <= 0 {
		chunkSize = 32768
	}
	var out []byte
	for i := 0; i < len(data); i += chunkSize {
		end := i + chunkSize
		if end > len(data) {
			end = len(data)
		}
		payload := make([]byte, 0, 1+end-i)
		payload = append(payload, band)
		payload = append(payload, data[i:end]...)
		out = append(out, mustPktLine(payload)...)
	}
	return out
}
