package main

import (
	"crypto/md5"
	"embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Font hash map is inherited from the GPL-licensed upstream project.
//
//go:embed resource/font_map_table.json
var fontAssets embed.FS
var fontBase64RE = regexp.MustCompile(`base64,([A-Za-z0-9+/=\r\n]+)`)

type fontTable struct{ offset, length int }

func u16(b []byte, i int) int { return int(binary.BigEndian.Uint16(b[i : i+2])) }
func i16(b []byte, i int) int { return int(int16(binary.BigEndian.Uint16(b[i : i+2]))) }
func u32(b []byte, i int) int { return int(binary.BigEndian.Uint32(b[i : i+4])) }
func fontMap(page []byte) (mapping map[rune]rune, err error) {
	mapping = map[rune]rune{}
	if !strings.Contains(string(page), "cxSecretStyle") {
		return mapping, nil
	}
	defer func() {
		if recover() != nil {
			mapping = nil
			err = fail("format", "加密字体文件格式无效", 0)
		}
	}()
	m := fontBase64RE.FindSubmatch(page)
	if len(m) < 2 {
		return nil, fail("format", "加密字体数据缺失", 0)
	}
	font, e := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.ReplaceAll(string(m[1]), "\n", ""), "\r", ""))
	if e != nil {
		return nil, fail("format", "加密字体 Base64 无效", 0)
	}
	if len(font) < 12 {
		return nil, fail("format", "字体文件过短", 0)
	}
	tables := map[string]fontTable{}
	for i := 0; i < u16(font, 4); i++ {
		p := 12 + 16*i
		name := string(font[p : p+4])
		t := fontTable{u32(font, p+8), u32(font, p+12)}
		if t.offset < 0 || t.length < 0 || t.offset+t.length > len(font) {
			return nil, fail("format", "字体表越界", 0)
		}
		tables[name] = t
	}
	for _, name := range []string{"head", "maxp", "loca", "glyf", "cmap"} {
		if tables[name].length == 0 {
			return nil, fail("format", "字体缺少必要数据表", 0)
		}
	}
	assets, _ := fontAssets.ReadFile("resource/font_map_table.json")
	var hashes map[string]string
	if json.Unmarshal(assets, &hashes) != nil {
		return nil, fail("format", "字体映射资源无效", 0)
	}
	reverse := map[string]rune{}
	for name, hash := range hashes {
		if strings.HasPrefix(name, "uni") {
			if code, e := strconv.ParseInt(name[3:], 16, 32); e == nil {
				reverse[hash] = rune(code)
			}
		}
	}
	glyphs := cmapGlyphs(font, tables["cmap"].offset)
	count := u16(font, tables["maxp"].offset+4)
	longLoca := i16(font, tables["head"].offset+50) == 1
	loca := tables["loca"].offset
	glyf := tables["glyf"].offset
	for code, index := range glyphs {
		if index <= 0 || index >= count {
			continue
		}
		off, next := 0, 0
		if longLoca {
			off = u32(font, loca+index*4)
			next = u32(font, loca+(index+1)*4)
		} else {
			off = u16(font, loca+index*2) * 2
			next = u16(font, loca+(index+1)*2) * 2
		}
		if next <= off {
			continue
		}
		if next > tables["glyf"].length {
			return nil, fail("format", "字形数据越界", 0)
		}
		hash := glyphHash(font[glyf+off : glyf+next])
		if original, ok := reverse[hash]; ok {
			mapping[code] = original
		}
	}
	if len(mapping) == 0 {
		return nil, fail("format", "无法解码加密字体，停止自动答题", 0)
	}
	return mapping, nil
}
func cmapGlyphs(font []byte, offset int) map[rune]int {
	out := map[rune]int{}
	num := u16(font, offset+2)
	for i := 0; i < num; i++ {
		p := offset + 4 + i*8
		platform := u16(font, p)
		encoding := u16(font, p+2)
		if platform != 0 && !(platform == 3 && (encoding == 1 || encoding == 10)) {
			continue
		}
		sub := offset + u32(font, p+4)
		switch u16(font, sub) {
		case 4:
			segments := u16(font, sub+6) / 2
			ends := sub + 14
			starts := ends + segments*2 + 2
			deltas := starts + segments*2
			rangeOffsets := deltas + segments*2
			for s := 0; s < segments; s++ {
				start, end := u16(font, starts+s*2), u16(font, ends+s*2)
				delta := i16(font, deltas+s*2)
				ro := u16(font, rangeOffsets+s*2)
				for code := start; code <= end && code < 65535; code++ {
					gid := 0
					if ro == 0 {
						gid = (code + delta) & 65535
					} else {
						gid = u16(font, rangeOffsets+s*2+ro+(code-start)*2)
						if gid != 0 {
							gid = (gid + delta) & 65535
						}
					}
					if gid > 0 {
						out[rune(code)] = gid
					}
				}
			}
		case 12:
			groups := u32(font, sub+12)
			if groups > 100000 {
				panic("cmap too large")
			}
			for g := 0; g < groups; g++ {
				p := sub + 16 + 12*g
				start, end, gid := u32(font, p), u32(font, p+4), u32(font, p+8)
				if end-start > 100000 || end > 0x10ffff {
					panic("invalid cmap")
				}
				for code := start; code <= end; code++ {
					out[rune(code)] = gid + code - start
				}
			}
		}
	}
	return out
}
func glyphHash(g []byte) string {
	contours := i16(g, 0)
	if contours <= 0 {
		return ""
	}
	last := u16(g, 10+(contours-1)*2)
	points := last + 1
	p := 10 + contours*2
	instructions := u16(g, p)
	p += 2 + instructions
	flags := make([]byte, 0, points)
	for len(flags) < points {
		f := g[p]
		p++
		flags = append(flags, f)
		if f&8 != 0 {
			n := int(g[p])
			p++
			if len(flags)+n > points {
				panic("invalid glyph flags")
			}
			for i := 0; i < n; i++ {
				flags = append(flags, f)
			}
		}
	}
	xs := make([]int, points)
	ys := make([]int, points)
	x, y := 0, 0
	for i, f := range flags {
		d := 0
		if f&2 != 0 {
			d = int(g[p])
			p++
			if f&16 == 0 {
				d = -d
			}
		} else if f&16 == 0 {
			d = i16(g, p)
			p += 2
		}
		x += d
		xs[i] = x
	}
	for i, f := range flags {
		d := 0
		if f&4 != 0 {
			d = int(g[p])
			p++
			if f&32 == 0 {
				d = -d
			}
		} else if f&32 == 0 {
			d = i16(g, p)
			p += 2
		}
		y += d
		ys[i] = y
	}
	var data strings.Builder
	for i, f := range flags {
		fmt.Fprintf(&data, "%d%d%d", xs[i], ys[i], f&1)
	}
	h := md5.Sum([]byte(data.String()))
	return hex.EncodeToString(h[:])
}
func decodeFontText(text string, mapping map[rune]rune) string {
	return strings.Map(func(r rune) rune {
		if mapped, ok := mapping[r]; ok {
			return mapped
		}
		return r
	}, text)
}
