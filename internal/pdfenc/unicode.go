// unicode.go 实现 PDF 文本层的纯 Go Unicode 字符编码恢复：ToUnicode
// CMap 解析、标准 CJK 命名编码、自定义 Encoding CMap 以及 CIDToGIDMap 与
// 嵌入 TrueType/OpenType cmap 反推。本包只做字节级字符映射，不感知版面。
//
// ledongthuc/pdf 能读取常见 ToUnicode，但对缺失 ToUnicode 的 Type0
// Identity-H 字体只能按单字节 PDFDocEncoding 降级，中文会变成替换符；其
// bfrange 连续映射也无法跨字节进位。本文件在原坐标提取质量不足时执行第二条
// 只读路径：优先解析完整 ToUnicode CMap，缺失时通过 CIDToGIDMap 与嵌入
// TrueType/OpenType cmap 反推 Unicode。恢复提取同时跟踪文本矩阵与 CTM 产出
// 定位 run（TextRun），上层可重建带坐标的版面行；坐标依赖文本矩阵而字符宽
// 度为估算值，只在乱码显著减少时替换原坐标提取结果。
package pdfenc

import (
	"encoding/binary"
	"io"
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

const (
	// pdfUnicodeMaxMappings 限制单字体构造的字符映射数量，防止畸形范围放大内存。
	pdfUnicodeMaxMappings = 200000
	// pdfUnicodeMaxStreamBytes 限制 CMap 或嵌入字体流的读取大小。
	pdfUnicodeMaxStreamBytes = 64 << 20
	// pdfUnicodeMaxTokens 限制文本 CMap 词法单元数量。
	pdfUnicodeMaxTokens = 1000000
	// pdfUnicodeMaxFormDepth 限制 Form XObject 文本递归深度。
	pdfUnicodeMaxFormDepth = 8
	// pdfUnicodeMaxFormVisits 限制单页解释的 Form XObject 次数。
	pdfUnicodeMaxFormVisits = 256
)

// pdfUnicodeCodeRange 描述 CMap 某一编码长度的合法大端范围。
type pdfUnicodeCodeRange struct {
	low  uint32
	high uint32
	size int
}

// pdfUnicodeCMap 保存原始字符码到 Unicode 文本的映射。
type pdfUnicodeCMap struct {
	mappings map[string]string
	ranges   []pdfUnicodeCodeRange
	lengths  []int
	fallback pdf.TextEncoding
	whole    func([]byte) string
}

// pdfCIDCMap 保存 Type0 字体 Encoding CMap 的字符码到 CID 映射。
// 它是缺失 ToUnicode 时连接原始内容流与 CIDToGIDMap 的必要中间层。
type pdfCIDCMap struct {
	mappings map[string]uint16
	ranges   []pdfUnicodeCodeRange
}

// decode 按 codespace 切分字节并映射为 Unicode；未知字符保留替换符，避免
// 把不可证明的字体码伪装成正确正文。
func (cmap *pdfUnicodeCMap) Decode(raw []byte) string {
	if cmap == nil || len(raw) == 0 {
		return ""
	}
	if cmap.whole != nil {
		return cmap.whole(raw)
	}
	var out strings.Builder
	for offset := 0; offset < len(raw); {
		size := cmap.codeSize(raw[offset:])
		if size <= 0 || offset+size > len(raw) {
			size = 1
		}
		code := string(raw[offset : offset+size])
		if text, ok := cmap.mappings[code]; ok && text != "" {
			out.WriteString(text)
		} else if cmap.fallback != nil {
			fallback := cmap.fallback.Decode(code)
			if fallback == "" {
				out.WriteRune(unicode.ReplacementChar)
			} else {
				out.WriteString(fallback)
			}
		} else {
			out.WriteRune(unicode.ReplacementChar)
		}
		offset += size
	}
	return out.String()
}

// codeSize 返回当前位置匹配的编码字节数；优先使用显式 codespace，否则使用
// 映射键推导出的长度，并优先较长编码避免前缀误匹配。
func (cmap *pdfUnicodeCMap) codeSize(raw []byte) int {
	for _, size := range cmap.lengths {
		if size <= 0 || size > len(raw) {
			continue
		}
		value := pdfBigEndianUint(raw[:size])
		for _, codeRange := range cmap.ranges {
			if codeRange.size == size && value >= codeRange.low && value <= codeRange.high {
				return size
			}
		}
	}
	for _, size := range cmap.lengths {
		if size <= len(raw) {
			return size
		}
	}
	return 0
}

// pdfCMapToken 是 ToUnicode CMap 的最小词法单元。
type pdfCMapToken struct {
	kind byte // h=hex string，n=整数，w=关键字，[/]=数组界定符。
	text string
	raw  []byte
	num  int64
}

// parsePDFToUnicodeCMap 解析 codespacerange、bfchar 和 bfrange；连续目标
// 使用完整大端加法，支持目标数组及 UTF-16 代理对。
func parsePDFToUnicodeCMap(data []byte) (*pdfUnicodeCMap, bool) {
	tokens := lexPDFCMap(data)
	if len(tokens) == 0 {
		return nil, false
	}
	cmap := &pdfUnicodeCMap{mappings: map[string]string{}}
	for index := 0; index < len(tokens); index++ {
		token := tokens[index]
		if token.kind != 'w' {
			continue
		}
		count := pdfCMapDeclaredCount(tokens, index)
		switch token.text {
		case "begincodespacerange":
			index = parsePDFCodeSpaceTokens(tokens, index+1, count, cmap)
		case "beginbfchar":
			index = parsePDFBFCharTokens(tokens, index+1, count, cmap)
		case "beginbfrange":
			index = parsePDFBFRangeTokens(tokens, index+1, count, cmap)
		}
		if len(cmap.mappings) >= pdfUnicodeMaxMappings {
			break
		}
	}
	if len(cmap.mappings) == 0 {
		return nil, false
	}
	cmap.finalizeLengths()
	return cmap, len(cmap.lengths) > 0
}

// parsePDFCIDCMap 解析 Encoding CMap 的 codespacerange、cidchar 和
// cidrange。CID 按规范限制为 16 位，畸形或超大范围被跳过。
// Lookup 返回字符码映射的 CID;未命中时返回 0。cidrange 在解析期已
// 展开为逐字符码映射，这里无需再做范围运算。
func (cmap *pdfCIDCMap) Lookup(code string) uint16 {
	if value, ok := cmap.mappings[code]; ok {
		return value
	}
	return 0
}

func ParseCIDCMap(data []byte) (*pdfCIDCMap, bool) {
	tokens := lexPDFCMap(data)
	if len(tokens) == 0 {
		return nil, false
	}
	cmap := &pdfCIDCMap{mappings: map[string]uint16{}}
	for index := 0; index < len(tokens); index++ {
		token := tokens[index]
		if token.kind != 'w' {
			continue
		}
		count := pdfCMapDeclaredCount(tokens, index)
		switch token.text {
		case "begincodespacerange":
			unicodeMap := &pdfUnicodeCMap{}
			index = parsePDFCodeSpaceTokens(tokens, index+1, count, unicodeMap)
			cmap.ranges = append(cmap.ranges, unicodeMap.ranges...)
		case "begincidchar":
			index = parsePDFCIDCharTokens(tokens, index+1, count, cmap)
		case "begincidrange":
			index = parsePDFCIDRangeTokens(tokens, index+1, count, cmap)
		}
		if len(cmap.mappings) >= pdfUnicodeMaxMappings {
			break
		}
	}
	return cmap, len(cmap.mappings) > 0
}

// parsePDFCIDCharTokens 读取逐字符 CID 映射。
func parsePDFCIDCharTokens(tokens []pdfCMapToken, index, count int, cmap *pdfCIDCMap) int {
	read := 0
	for index < len(tokens) && read < count {
		if tokens[index].kind == 'w' && tokens[index].text == "endcidchar" {
			return index
		}
		if index+1 >= len(tokens) || tokens[index].kind != 'h' || tokens[index+1].kind != 'n' {
			index++
			continue
		}
		if source, cid := tokens[index].raw, tokens[index+1].num; len(source) > 0 && len(source) <= 4 && cid >= 0 && cid <= 0xffff {
			cmap.mappings[string(source)] = uint16(cid)
		}
		read++
		index += 2
	}
	return index - 1
}

// parsePDFCIDRangeTokens 展开连续字符码到连续 CID 的映射。
func parsePDFCIDRangeTokens(tokens []pdfCMapToken, index, count int, cmap *pdfCIDCMap) int {
	read := 0
	for index < len(tokens) && read < count {
		if tokens[index].kind == 'w' && tokens[index].text == "endcidrange" {
			return index
		}
		if index+2 >= len(tokens) || tokens[index].kind != 'h' || tokens[index+1].kind != 'h' || tokens[index+2].kind != 'n' {
			index++
			continue
		}
		low, high, firstCID := tokens[index].raw, tokens[index+1].raw, tokens[index+2].num
		if len(low) > 0 && len(low) == len(high) && len(low) <= 4 && firstCID >= 0 && firstCID <= 0xffff {
			start, end := pdfBigEndianUint(low), pdfBigEndianUint(high)
			if end >= start && uint64(end-start)+1 <= pdfUnicodeMaxMappings && uint64(firstCID)+uint64(end-start) <= 0xffff {
				for delta := uint32(0); delta <= end-start && len(cmap.mappings) < pdfUnicodeMaxMappings; delta++ {
					cmap.mappings[string(pdfBigEndianBytes(start+delta, len(low)))] = uint16(firstCID + int64(delta))
				}
			}
		}
		read++
		index += 3
	}
	return index - 1
}

// pdfCMapDeclaredCount 读取 begin* 操作符前的声明数量；缺失时由 end* 边界控制。
func pdfCMapDeclaredCount(tokens []pdfCMapToken, index int) int {
	if index > 0 && tokens[index-1].kind == 'n' && tokens[index-1].num > 0 && tokens[index-1].num <= pdfUnicodeMaxMappings {
		return int(tokens[index-1].num)
	}
	return pdfUnicodeMaxMappings
}

// parsePDFCodeSpaceTokens 读取 CMap 编码空间。
func parsePDFCodeSpaceTokens(tokens []pdfCMapToken, index, count int, cmap *pdfUnicodeCMap) int {
	read := 0
	for index < len(tokens) && read < count {
		if tokens[index].kind == 'w' && tokens[index].text == "endcodespacerange" {
			return index
		}
		if index+1 >= len(tokens) || tokens[index].kind != 'h' || tokens[index+1].kind != 'h' {
			index++
			continue
		}
		low, high := tokens[index].raw, tokens[index+1].raw
		if len(low) > 0 && len(low) == len(high) && len(low) <= 4 {
			cmap.ranges = append(cmap.ranges, pdfUnicodeCodeRange{
				low: pdfBigEndianUint(low), high: pdfBigEndianUint(high), size: len(low),
			})
		}
		read++
		index += 2
	}
	return index - 1
}

// parsePDFBFCharTokens 读取逐字符映射。
func parsePDFBFCharTokens(tokens []pdfCMapToken, index, count int, cmap *pdfUnicodeCMap) int {
	read := 0
	for index < len(tokens) && read < count {
		if tokens[index].kind == 'w' && tokens[index].text == "endbfchar" {
			return index
		}
		if index+1 >= len(tokens) || tokens[index].kind != 'h' || tokens[index+1].kind != 'h' {
			index++
			continue
		}
		cmap.addMapping(tokens[index].raw, decodePDFUnicodeBytes(tokens[index+1].raw))
		read++
		index += 2
	}
	return index - 1
}

// parsePDFBFRangeTokens 读取连续范围或显式目标数组。
func parsePDFBFRangeTokens(tokens []pdfCMapToken, index, count int, cmap *pdfUnicodeCMap) int {
	read := 0
	for index < len(tokens) && read < count {
		if tokens[index].kind == 'w' && tokens[index].text == "endbfrange" {
			return index
		}
		if index+2 >= len(tokens) || tokens[index].kind != 'h' || tokens[index+1].kind != 'h' {
			index++
			continue
		}
		low, high := tokens[index].raw, tokens[index+1].raw
		if len(low) == 0 || len(low) != len(high) || len(low) > 4 {
			index += 2
			continue
		}
		start, end := pdfBigEndianUint(low), pdfBigEndianUint(high)
		if end < start || uint64(end-start)+1 > pdfUnicodeMaxMappings {
			index += 2
			continue
		}
		destination := tokens[index+2]
		rangeCount := end - start + 1
		switch destination.kind {
		case 'h':
			for delta := uint32(0); delta < rangeCount && len(cmap.mappings) < pdfUnicodeMaxMappings; delta++ {
				source := pdfBigEndianBytes(start+delta, len(low))
				target := addPDFBigEndian(destination.raw, delta)
				cmap.addMapping(source, decodePDFUnicodeBytes(target))
			}
			index += 3
		case '[':
			index += 3
			for delta := uint32(0); delta < rangeCount && index < len(tokens) && len(cmap.mappings) < pdfUnicodeMaxMappings; index++ {
				if tokens[index].kind == ']' {
					break
				}
				if tokens[index].kind != 'h' {
					continue
				}
				cmap.addMapping(pdfBigEndianBytes(start+delta, len(low)), decodePDFUnicodeBytes(tokens[index].raw))
				delta++
			}
		default:
			index += 3
		}
		read++
	}
	return index - 1
}

// addMapping 添加非空、长度合法的映射。
func (cmap *pdfUnicodeCMap) addMapping(source []byte, target string) {
	if len(source) == 0 || len(source) > 4 || target == "" || len(cmap.mappings) >= pdfUnicodeMaxMappings {
		return
	}
	cmap.mappings[string(source)] = target
}

// finalizeLengths 汇总可用编码长度并按从长到短排序。
func (cmap *pdfUnicodeCMap) finalizeLengths() {
	seen := map[int]bool{}
	for _, codeRange := range cmap.ranges {
		seen[codeRange.size] = true
	}
	for source := range cmap.mappings {
		seen[len(source)] = true
	}
	cmap.lengths = cmap.lengths[:0]
	for size := range seen {
		if size > 0 && size <= 4 {
			cmap.lengths = append(cmap.lengths, size)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(cmap.lengths)))
}

// lexPDFCMap 对 CMap 的相关字面量做有界词法分析；字典、名称和无关
// PostScript 操作符作为普通单词保留，解析阶段会跳过。
func lexPDFCMap(data []byte) []pdfCMapToken {
	tokens := make([]pdfCMapToken, 0, 256)
	for index := 0; index < len(data) && len(tokens) < pdfUnicodeMaxTokens; {
		char := data[index]
		if isPDFCMapSpace(char) {
			index++
			continue
		}
		if char == '%' {
			for index < len(data) && data[index] != '\n' && data[index] != '\r' {
				index++
			}
			continue
		}
		if char == '[' || char == ']' {
			tokens = append(tokens, pdfCMapToken{kind: char})
			index++
			continue
		}
		if char == '<' && index+1 < len(data) && data[index+1] != '<' {
			end := index + 1
			for end < len(data) && data[end] != '>' {
				end++
			}
			if end >= len(data) {
				break
			}
			if raw, ok := decodePDFHex(data[index+1 : end]); ok {
				tokens = append(tokens, pdfCMapToken{kind: 'h', raw: raw})
			}
			index = end + 1
			continue
		}
		end := index
		for end < len(data) && !isPDFCMapSpace(data[end]) && !strings.ContainsRune("[]<>", rune(data[end])) {
			end++
		}
		if end == index {
			index++
			continue
		}
		word := string(data[index:end])
		if number, ok := parsePDFCMapInteger(word); ok {
			tokens = append(tokens, pdfCMapToken{kind: 'n', num: number})
		} else {
			tokens = append(tokens, pdfCMapToken{kind: 'w', text: word})
		}
		index = end
	}
	return tokens
}

// parsePDFCMapInteger 解析非负十进制计数。
func parsePDFCMapInteger(value string) (int64, bool) {
	if value == "" {
		return 0, false
	}
	var number int64
	for _, char := range value {
		if char < '0' || char > '9' {
			return 0, false
		}
		number = number*10 + int64(char-'0')
		if number > pdfUnicodeMaxTokens {
			return 0, false
		}
	}
	return number, true
}

// decodePDFHex 解码允许空白和奇数位补零的 PDF 十六进制字符串。
func decodePDFHex(value []byte) ([]byte, bool) {
	nibbles := make([]byte, 0, len(value))
	for _, char := range value {
		if isPDFCMapSpace(char) {
			continue
		}
		nibble, ok := pdfHexNibble(char)
		if !ok {
			return nil, false
		}
		nibbles = append(nibbles, nibble)
	}
	if len(nibbles)%2 != 0 {
		nibbles = append(nibbles, 0)
	}
	out := make([]byte, len(nibbles)/2)
	for index := range out {
		out[index] = nibbles[index*2]<<4 | nibbles[index*2+1]
	}
	return out, len(out) > 0
}

// pdfHexNibble 返回十六进制字符数值。
func pdfHexNibble(char byte) (byte, bool) {
	switch {
	case char >= '0' && char <= '9':
		return char - '0', true
	case char >= 'A' && char <= 'F':
		return char - 'A' + 10, true
	case char >= 'a' && char <= 'f':
		return char - 'a' + 10, true
	default:
		return 0, false
	}
}

// isPDFCMapSpace 判断 PDF/PostScript 空白字符。
func isPDFCMapSpace(char byte) bool {
	return char == 0 || char == '\t' || char == '\n' || char == '\f' || char == '\r' || char == ' '
}

// decodePDFUnicodeBytes 按 UTF-16BE 解码 ToUnicode 目标；单字节目标按
// Unicode 标量值处理，以兼容少量非标准生成器。
func decodePDFUnicodeBytes(value []byte) string {
	if len(value) == 0 {
		return ""
	}
	if len(value) == 1 {
		return string(rune(value[0]))
	}
	if len(value)%2 != 0 {
		value = append([]byte{0}, value...)
	}
	units := make([]uint16, 0, len(value)/2)
	for index := 0; index+1 < len(value); index += 2 {
		units = append(units, binary.BigEndian.Uint16(value[index:index+2]))
	}
	if len(units) > 0 && units[0] == 0xfeff {
		units = units[1:]
	}
	return string(utf16.Decode(units))
}

// addPDFBigEndian 返回大端字节串加 delta 后的同宽结果，完整处理跨字节进位。
func addPDFBigEndian(value []byte, delta uint32) []byte {
	result := append([]byte(nil), value...)
	carry := uint64(delta)
	for index := len(result) - 1; index >= 0 && carry > 0; index-- {
		sum := uint64(result[index]) + carry
		result[index] = byte(sum)
		carry = sum >> 8
	}
	return result
}

// pdfBigEndianUint 把最多四字节编码转换为整数。
func pdfBigEndianUint(value []byte) uint32 {
	var result uint32
	for _, char := range value {
		result = result<<8 | uint32(char)
	}
	return result
}

// pdfBigEndianBytes 把整数编码为固定宽度大端字节。
func pdfBigEndianBytes(value uint32, size int) []byte {
	result := make([]byte, size)
	for index := size - 1; index >= 0; index-- {
		result[index] = byte(value)
		value >>= 8
	}
	return result
}

// hasPDFUnicodeRecoveryCandidate 判断页面是否含官方 ToUnicode、标准 CJK
// 命名 CMap、可通过嵌入字体恢复的 Identity Type0 字体，或主库不会递归的
// Form XObject。
func HasUnicodeRecoveryCandidate(page pdf.Page) bool {
	defer func() { _ = recover() }()
	resources := page.Resources()
	fonts := resources.Key("Font")
	for _, name := range fonts.Keys() {
		font := pdf.Font{V: fonts.Key(name)}
		if font.V.Key("ToUnicode").Kind() == pdf.Stream {
			return true
		}
		encodingValue := font.V.Key("Encoding")
		encodingName := encodingValue.Name()
		if NamedCMapDecoder(encodingName) != nil {
			return true
		}
		if encodingValue.Kind() != pdf.Stream && encodingName != "Identity-H" && encodingName != "Identity-V" {
			continue
		}
		descendant := font.V.Key("DescendantFonts").Index(0)
		descriptor := descendant.Key("FontDescriptor")
		if descriptor.Key("FontFile2").Kind() == pdf.Stream || descriptor.Key("FontFile3").Kind() == pdf.Stream {
			return true
		}
	}
	xObjects := resources.Key("XObject")
	for _, name := range xObjects.Keys() {
		if xObjects.Key(name).Key("Subtype").Name() == "Form" {
			return true
		}
	}
	return false
}

// extractPDFUnicodeText 读取页面内容流并按字体解码文本操作符。函数对第三方
// PDF 解释器 panic 做流级隔离，单个畸形 Form 不影响页面其他内容。
func ExtractUnicodeText(page pdf.Page) (string, bool) {
	extractor := newPDFUnicodeTextExtractor()
	extractor.extract(page.V.Key("Contents"), page.Resources(), 0)
	result := strings.TrimSpace(extractor.out.String())
	return result, result != ""
}

// TextRun 一段定位文本：内容流中一次文本绘制算子的解码结果，
// X/Y 为默认用户空间（y 向上，pt）内的绘制起点，FontSize 为包含文本矩阵
// 与 CTM 缩放后的有效字号，WidthEstimate 为按字形类别的经验宽度估算。
type TextRun struct {
	Text          string
	X, Y          float64
	FontSize      float64
	WidthEstimate float64
}

// ExtractUnicodeTextRuns 带定位提取页面文本：在解码文本的同时跟踪 CTM 与
// 文本矩阵（Tm/Td/TD/T*/TJ 等），把每次绘制算子的文本记录为定位 run，
// 供上层把恢复文本升级为带坐标的版面行。提取失败或无文本时返回 false。
func ExtractUnicodeTextRuns(page pdf.Page) ([]TextRun, bool) {
	extractor := newPDFUnicodeTextExtractor()
	extractor.extract(page.V.Key("Contents"), page.Resources(), 0)
	return extractor.runs, len(extractor.runs) > 0
}

// pdfUnicodeMatrix 是 2D 仿射矩阵的行向量表示（a b c d e f）：
// 点 p 经矩阵映射为 p·M，x' = x·a + y·c + e，y' = x·b + y·d + f。
type pdfUnicodeMatrix struct{ a, b, c, d, e, f float64 }

var pdfUnicodeIdentityMatrix = pdfUnicodeMatrix{a: 1, d: 1}

// compose 返回先作用 outer、再作用 inner 的复合矩阵（p·outer·inner）。
func (outer pdfUnicodeMatrix) compose(inner pdfUnicodeMatrix) pdfUnicodeMatrix {
	return pdfUnicodeMatrix{
		a: outer.a*inner.a + outer.b*inner.c,
		b: outer.a*inner.b + outer.b*inner.d,
		c: outer.c*inner.a + outer.d*inner.c,
		d: outer.c*inner.b + outer.d*inner.d,
		e: outer.e*inner.a + outer.f*inner.c + inner.e,
		f: outer.e*inner.b + outer.f*inner.d + inner.f,
	}
}

// translate 返回平移矩阵。
func pdfUnicodeTranslate(tx, ty float64) pdfUnicodeMatrix {
	return pdfUnicodeMatrix{a: 1, d: 1, e: tx, f: ty}
}

// scale 取矩阵线性部分的均匀缩放近似（行列式绝对值的平方根），
// 旋转/翻转矩阵返回 1。
func (m pdfUnicodeMatrix) scale() float64 {
	det := m.a*m.d - m.b*m.c
	return math.Sqrt(math.Abs(det))
}

// transform 把点映射到矩阵目标空间。
func (m pdfUnicodeMatrix) transform(x, y float64) (float64, float64) {
	return x*m.a + y*m.c + m.e, x*m.b + y*m.d + m.f
}

// pdfUnicodeRunWidth 按字形类别估算文本渲染宽度：CJK 全角字形按 1em，
// 其他按 0.55em。用于 run 间水平间隙判断与词盒宽度，无需字体度量。
func pdfUnicodeRunWidth(text string, fontSize float64) float64 {
	width := 0.0
	for _, r := range text {
		switch {
		case r == ' ':
			width += 0.35 * fontSize
		case unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hangul, r) ||
			unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) ||
			(r >= 0x3000 && r <= 0x303F) || (r >= 0xFF00 && r <= 0xFFEF):
			width += fontSize
		default:
			width += 0.55 * fontSize
		}
	}
	return width
}

// pdfUnicodeTextExtractor 保存页面及嵌套 Form XObject 的有界文本提取状态，
// 同时跟踪图形状态（CTM/q/Q/cm）与文本矩阵（Tm/Td/TD/T*/TL/Tf/TJ），
// 把每次文本绘制记录为定位 run。
// newPDFUnicodeTextExtractor 构造提取器：CTM 与文本矩阵初始化为单位矩阵
// （内容流无 cm/Tm 时坐标即原始用户空间坐标）。
func newPDFUnicodeTextExtractor() *pdfUnicodeTextExtractor {
	return &pdfUnicodeTextExtractor{
		ctm: pdfUnicodeIdentityMatrix,
		tm:  pdfUnicodeIdentityMatrix,
		tlm: pdfUnicodeIdentityMatrix,
	}
}

type pdfUnicodeTextExtractor struct {
	out        strings.Builder
	lineEnded  bool
	formVisits int
	runs       []TextRun
	ctm        pdfUnicodeMatrix
	tm         pdfUnicodeMatrix
	tlm        pdfUnicodeMatrix
	leading    float64
	fontSize   float64
	ctmStack   []pdfUnicodeMatrix
}

// lineBreak 在已有非换行内容后追加一次换行。
func (extractor *pdfUnicodeTextExtractor) lineBreak() {
	if extractor.out.Len() > 0 && !extractor.lineEnded {
		extractor.out.WriteByte('\n')
		extractor.lineEnded = true
	}
}

// appendRaw 使用当前字体解码并追加原始 PDF 字符串；同时按当前文本矩阵
// 记录定位 run，并按估算字宽推进 Tm（ successive Tj 无显式定位时避免
// 多个 run 叠在同一坐标）。
func (extractor *pdfUnicodeTextExtractor) appendRaw(current *pdfUnicodeCMap, raw string) {
	if current == nil {
		return
	}
	decoded := current.Decode([]byte(raw))
	extractor.out.WriteString(decoded)
	if decoded != "" {
		extractor.lineEnded = strings.HasSuffix(decoded, "\n")
	}
	if strings.TrimSpace(decoded) == "" {
		return
	}
	fontSize := extractor.fontSize * extractor.tm.scale() * extractor.ctm.scale()
	x, y := extractor.ctm.transform(extractor.tm.e, extractor.tm.f)
	extractor.runs = append(extractor.runs, TextRun{
		Text:          decoded,
		X:             x,
		Y:             y,
		FontSize:      fontSize,
		WidthEstimate: pdfUnicodeRunWidth(decoded, fontSize),
	})
	// 把估算渲染宽度换算回文本空间并推进 Tm（Tj 的规范隐式推进）
	scale := extractor.tm.scale() * extractor.ctm.scale()
	if scale > 1e-6 {
		advance := extractor.runs[len(extractor.runs)-1].WidthEstimate / scale
		extractor.tm = pdfUnicodeTranslate(advance, 0).compose(extractor.tm)
	}
}

// extract 按绘制顺序解释一个页面或 Form 内容流；Form 自带 Resources 时
// 使用自身资源，否则继承调用方资源。深度、次数和 panic 均局部隔离。
func (extractor *pdfUnicodeTextExtractor) extract(stream, resources pdf.Value, depth int) (ok bool) {
	if stream.IsNull() || depth > pdfUnicodeMaxFormDepth || extractor.formVisits > pdfUnicodeMaxFormVisits {
		return false
	}
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	fonts := map[string]*pdfUnicodeCMap{}
	fontResources := resources.Key("Font")
	for _, name := range fontResources.Keys() {
		fonts[name] = NewUnicodeFont(pdf.Font{V: fontResources.Key(name)})
	}
	var current *pdfUnicodeCMap
	pdf.Interpret(stream, func(stack *pdf.Stack, operator string) {
		count := stack.Len()
		arguments := make([]pdf.Value, count)
		for index := count - 1; index >= 0; index-- {
			arguments[index] = stack.Pop()
		}
		switch operator {
		case "q":
			extractor.ctmStack = append(extractor.ctmStack, extractor.ctm)
		case "Q":
			if len(extractor.ctmStack) > 0 {
				extractor.ctm = extractor.ctmStack[len(extractor.ctmStack)-1]
				extractor.ctmStack = extractor.ctmStack[:len(extractor.ctmStack)-1]
			}
		case "cm":
			if matrix, ok := pdfUnicodeMatrixFromValues(arguments); ok {
				extractor.ctm = matrix.compose(extractor.ctm)
			}
		case "Tf":
			if len(arguments) == 2 {
				current = fonts[arguments[0].Name()]
				extractor.fontSize = arguments[1].Float64()
			}
		case "TL":
			if len(arguments) == 1 {
				extractor.leading = arguments[0].Float64()
			}
		case "BT":
			extractor.tm, extractor.tlm = pdfUnicodeIdentityMatrix, pdfUnicodeIdentityMatrix
		case "Tm":
			if matrix, ok := pdfUnicodeMatrixFromValues(arguments); ok {
				extractor.tm, extractor.tlm = matrix, matrix
			}
		case "Td", "TD":
			if len(arguments) != 2 {
				return
			}
			tx, ty := arguments[0].Float64(), arguments[1].Float64()
			if operator == "TD" {
				extractor.leading = -ty
			}
			if ty != 0 {
				extractor.lineBreak()
			}
			extractor.tlm = pdfUnicodeTranslate(tx, ty).compose(extractor.tlm)
			extractor.tm = extractor.tlm
		case "T*":
			extractor.tlm = pdfUnicodeTranslate(0, -extractor.leading).compose(extractor.tlm)
			extractor.tm = extractor.tlm
			extractor.lineBreak()
		case "Tj":
			if len(arguments) == 1 {
				extractor.appendRaw(current, arguments[0].RawString())
			}
		case "TJ":
			if len(arguments) != 1 {
				return
			}
			array := arguments[0]
			for index := 0; index < array.Len(); index++ {
				value := array.Index(index)
				if value.Kind() == pdf.String {
					extractor.appendRaw(current, value.RawString())
				} else if value.Float64() < -120 && extractor.out.Len() > 0 {
					extractor.out.WriteByte(' ')
					extractor.lineEnded = false
				}
				if value.Kind() != pdf.String {
					// TJ 数字参数按规范水平调整 Tm（−num/1000×Tfs）
					extractor.tm = pdfUnicodeTranslate(
						-value.Float64()/1000*extractor.fontSize, 0,
					).compose(extractor.tm)
				}
			}
		case "'", "\"":
			extractor.lineBreak()
			extractor.tlm = pdfUnicodeTranslate(0, -extractor.leading).compose(extractor.tlm)
			extractor.tm = extractor.tlm
			if len(arguments) > 0 {
				extractor.appendRaw(current, arguments[len(arguments)-1].RawString())
			}
		case "ET":
			extractor.lineBreak()
		case "Do":
			if len(arguments) != 1 || depth >= pdfUnicodeMaxFormDepth || extractor.formVisits >= pdfUnicodeMaxFormVisits {
				return
			}
			form := resources.Key("XObject").Key(arguments[0].Name())
			if form.Kind() != pdf.Stream || form.Key("Subtype").Name() != "Form" {
				return
			}
			formResources := form.Key("Resources")
			if formResources.IsNull() {
				formResources = resources
			}
			// Form 有自带 Matrix 时先作用其变换再递归（结束后恢复外层状态）
			savedCTM, savedTM, savedTLM := extractor.ctm, extractor.tm, extractor.tlm
			if raw := form.Key("Matrix"); raw.Len() == 6 {
				matrixValues := make([]pdf.Value, 6)
				for i := range matrixValues {
					matrixValues[i] = raw.Index(i)
				}
				if matrix, ok := pdfUnicodeMatrixFromValues(matrixValues); ok {
					extractor.ctm = matrix.compose(extractor.ctm)
				}
			}
			extractor.formVisits++
			extractor.lineBreak()
			extractor.extract(form, formResources, depth+1)
			extractor.lineBreak()
			extractor.ctm, extractor.tm, extractor.tlm = savedCTM, savedTM, savedTLM
		}
	})
	return true
}

// pdfUnicodeMatrixFromValues 按 a b c d e f 顺序构造矩阵；参数不足或非法时失败。
func pdfUnicodeMatrixFromValues(values []pdf.Value) (pdfUnicodeMatrix, bool) {
	if len(values) != 6 {
		return pdfUnicodeMatrix{}, false
	}
	for _, value := range values {
		if value.Kind() != pdf.Integer && value.Kind() != pdf.Real {
			return pdfUnicodeMatrix{}, false
		}
	}
	return pdfUnicodeMatrix{
		a: values[0].Float64(), b: values[1].Float64(),
		c: values[2].Float64(), d: values[3].Float64(),
		e: values[4].Float64(), f: values[5].Float64(),
	}, true
}

// newPDFUnicodeFont 构造字体解码器：自带 ToUnicode 时使用更完整的 CMap
// 解析；缺失时对 Identity-H/V 或自定义 Encoding CMap 的 Type0 字体尝试
// 经 CIDToGIDMap 与嵌入 sfnt 反向映射。
func NewUnicodeFont(font pdf.Font) *pdfUnicodeCMap {
	if data, ok := ReadUnicodeStream(font.V.Key("ToUnicode")); ok {
		if cmap, parsed := parsePDFToUnicodeCMap(data); parsed {
			cmap.fallback = safePDFFontEncoder(font)
			return cmap
		}
	}
	encodingValue := font.V.Key("Encoding")
	encoding := encodingValue.Name()
	if decoder := NamedCMapDecoder(encoding); decoder != nil {
		return decoder
	}
	fallback := safePDFFontEncoder(font)
	if encodingValue.Kind() != pdf.Stream && encoding != "Identity-H" && encoding != "Identity-V" {
		return &pdfUnicodeCMap{fallback: fallback, lengths: []int{1}}
	}
	if cmap, ok := embeddedPDFUnicodeCMap(font.V, fallback); ok {
		return cmap
	}
	return &pdfUnicodeCMap{fallback: fallback, lengths: []int{2}}
}

// namedPDFCMapDecoder 为 Adobe 标准 CJK 命名 CMap 选择纯 Go 字符集。
// Unicode CMap 的字符码直接是 UTF-16BE；区域编码 CMap 分别复用 x/text
// 的 GBK/GB18030、Big5、Shift-JIS、EUC-JP 和 EUC-KR 解码器。
func NamedCMapDecoder(name string) *pdfUnicodeCMap {
	upper := strings.ToUpper(strings.TrimSpace(name))
	if upper == "" || upper == "IDENTITY-H" || upper == "IDENTITY-V" {
		return nil
	}
	unicodePrefixes := []string{"UNIGB-", "UNICNS-", "UNIJIS-", "UNIKS-"}
	for _, prefix := range unicodePrefixes {
		if strings.HasPrefix(upper, prefix) && (strings.Contains(upper, "-UCS2-") || strings.Contains(upper, "-UTF16-")) {
			return &pdfUnicodeCMap{whole: func(raw []byte) string { return decodePDFUnicodeBytes(raw) }, lengths: []int{4, 2}}
		}
	}
	var textEncoding encoding.Encoding
	switch {
	case strings.HasPrefix(upper, "GBK2K-"):
		textEncoding = simplifiedchinese.GB18030
	case strings.HasPrefix(upper, "GB-") || strings.HasPrefix(upper, "GBPC-") ||
		strings.HasPrefix(upper, "GBK-") || strings.HasPrefix(upper, "GBKP-"):
		textEncoding = simplifiedchinese.GBK
	case strings.Contains(upper, "-B5-") || strings.HasPrefix(upper, "B5PC-") || strings.HasPrefix(upper, "ETEN-B5-"):
		textEncoding = traditionalchinese.Big5
	case strings.Contains(upper, "RKSJ-"):
		textEncoding = japanese.ShiftJIS
	case upper == "EUC-H" || upper == "EUC-V":
		textEncoding = japanese.EUCJP
	case strings.HasPrefix(upper, "KSC-") || strings.HasPrefix(upper, "KSCMS-") || strings.HasPrefix(upper, "KSCPC-"):
		textEncoding = korean.EUCKR
	default:
		return nil
	}
	return &pdfUnicodeCMap{whole: func(raw []byte) string {
		decoded, err := textEncoding.NewDecoder().Bytes(raw)
		if err != nil {
			return ""
		}
		return string(decoded)
	}, lengths: []int{1}}
}

// safePDFFontEncoder 隔离第三方 CMap 解释器对畸形 begin/end 块的 panic。
func safePDFFontEncoder(font pdf.Font) (encoder pdf.TextEncoding) {
	defer func() {
		if recover() != nil {
			encoder = nil
		}
	}()
	return font.Encoder()
}

// embeddedPDFUnicodeCMap 通过 CIDToGIDMap 和嵌入 TrueType/OpenType cmap
// 建立双字节 Identity 字体映射。
func embeddedPDFUnicodeCMap(fontValue pdf.Value, fallback pdf.TextEncoding) (*pdfUnicodeCMap, bool) {
	descendant := fontValue.Key("DescendantFonts").Index(0)
	if descendant.IsNull() {
		return nil, false
	}
	descriptor := descendant.Key("FontDescriptor")
	fontStream := descriptor.Key("FontFile2")
	if fontStream.IsNull() {
		fontStream = descriptor.Key("FontFile3")
	}
	fontData, ok := ReadUnicodeStream(fontStream)
	if !ok {
		return nil, false
	}
	glyphs := ParseSFNTGlyphUnicode(fontData)
	if len(glyphs) == 0 {
		return nil, false
	}
	cidToGID := ParseCIDToGID(descendant.Key("CIDToGIDMap"))
	cmap := &pdfUnicodeCMap{
		mappings: map[string]string{},
		ranges:   []pdfUnicodeCodeRange{{low: 0, high: 0xffff, size: 2}},
		lengths:  []int{2}, fallback: fallback,
	}
	if encodingData, read := ReadUnicodeStream(fontValue.Key("Encoding")); read {
		if encodingCMap, parsed := ParseCIDCMap(encodingData); parsed {
			cmap.ranges = append([]pdfUnicodeCodeRange(nil), encodingCMap.ranges...)
			for source, cid := range encodingCMap.mappings {
				glyphID := cid
				if cidToGID != nil {
					mapped, found := cidToGID[cid]
					if !found {
						continue
					}
					glyphID = mapped
				}
				if char, found := glyphs[glyphID]; found {
					cmap.addMapping([]byte(source), string(char))
				}
			}
			cmap.finalizeLengths()
			return cmap, len(cmap.mappings) > 0
		}
	}
	if cidToGID == nil {
		for glyphID, char := range glyphs {
			if glyphID <= 0xffff {
				cmap.addMapping(pdfBigEndianBytes(uint32(glyphID), 2), string(char))
			}
		}
	} else {
		for cid, glyphID := range cidToGID {
			if char, found := glyphs[glyphID]; found {
				cmap.addMapping(pdfBigEndianBytes(uint32(cid), 2), string(char))
			}
		}
	}
	return cmap, len(cmap.mappings) > 0
}

// parsePDFCIDToGID 返回显式 CIDToGIDMap；nil 表示 Identity 或缺省映射。
func ParseCIDToGID(value pdf.Value) map[uint16]uint16 {
	if value.IsNull() || value.Name() == "Identity" {
		return nil
	}
	data, ok := ReadUnicodeStream(value)
	if !ok || len(data) < 2 {
		return nil
	}
	result := make(map[uint16]uint16, len(data)/2)
	limit := len(data) / 2
	if limit > 65536 {
		limit = 65536
	}
	for cid := 0; cid < limit; cid++ {
		glyphID := binary.BigEndian.Uint16(data[cid*2 : cid*2+2])
		if glyphID != 0 {
			result[uint16(cid)] = glyphID
		}
	}
	return result
}

// readPDFUnicodeStream 有界读取 PDF stream，并隔离不支持过滤器造成的 panic。
func ReadUnicodeStream(value pdf.Value) (data []byte, ok bool) {
	if value.Kind() != pdf.Stream {
		return nil, false
	}
	defer func() {
		if recover() != nil {
			data, ok = nil, false
		}
	}()
	reader := value.Reader()
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, pdfUnicodeMaxStreamBytes+1))
	if err != nil || len(data) == 0 || len(data) > pdfUnicodeMaxStreamBytes {
		return nil, false
	}
	return data, true
}

// parseSFNTGlyphUnicode 从 sfnt/TrueType collection 的 Unicode cmap
// 反向构造 glyph ID 到字符映射，支持 format 0/4/6/12。
func ParseSFNTGlyphUnicode(data []byte) map[uint16]rune {
	fontOffset := 0
	if len(data) >= 16 && string(data[:4]) == "ttcf" {
		fontOffset = int(binary.BigEndian.Uint32(data[12:16]))
	}
	if fontOffset < 0 || fontOffset+12 > len(data) {
		return nil
	}
	numTables := int(binary.BigEndian.Uint16(data[fontOffset+4 : fontOffset+6]))
	if numTables <= 0 || numTables > 4096 || fontOffset+12+numTables*16 > len(data) {
		return nil
	}
	var cmapData []byte
	for index := 0; index < numTables; index++ {
		offset := fontOffset + 12 + index*16
		if string(data[offset:offset+4]) != "cmap" {
			continue
		}
		tableOffset := int(binary.BigEndian.Uint32(data[offset+8 : offset+12]))
		tableLength := int(binary.BigEndian.Uint32(data[offset+12 : offset+16]))
		if tableOffset >= 0 && tableLength >= 4 && tableOffset+tableLength <= len(data) {
			cmapData = data[tableOffset : tableOffset+tableLength]
		}
		break
	}
	if len(cmapData) < 4 {
		return nil
	}
	numSubtables := int(binary.BigEndian.Uint16(cmapData[2:4]))
	if numSubtables <= 0 || numSubtables > 4096 || 4+numSubtables*8 > len(cmapData) {
		return nil
	}
	type sfntCmapRecord struct {
		offset   int
		priority int
	}
	records := make([]sfntCmapRecord, 0, numSubtables)
	for index := 0; index < numSubtables; index++ {
		offset := 4 + index*8
		platform := binary.BigEndian.Uint16(cmapData[offset : offset+2])
		encoding := binary.BigEndian.Uint16(cmapData[offset+2 : offset+4])
		subtableOffset := int(binary.BigEndian.Uint32(cmapData[offset+4 : offset+8]))
		if subtableOffset < 0 || subtableOffset+2 > len(cmapData) {
			continue
		}
		priority := sfntCmapPriority(platform, encoding)
		if priority > 0 {
			records = append(records, sfntCmapRecord{offset: subtableOffset, priority: priority})
		}
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].priority > records[j].priority })
	result := map[uint16]rune{}
	for _, record := range records {
		parseSFNTCmapSubtable(cmapData[record.offset:], result)
		if len(result) >= pdfUnicodeMaxMappings {
			break
		}
	}
	return result
}

// sfntCmapPriority 优先选择完整 Unicode 表，其次 Windows BMP 和平台 0。
func sfntCmapPriority(platform, encoding uint16) int {
	switch {
	case platform == 3 && encoding == 10:
		return 4
	case platform == 0:
		return 3
	case platform == 3 && encoding == 1:
		return 2
	case platform == 3 && encoding == 0:
		return 1
	default:
		return 0
	}
}

// parseSFNTCmapSubtable 分发常见 cmap 子表格式。
func parseSFNTCmapSubtable(data []byte, result map[uint16]rune) {
	if len(data) < 2 {
		return
	}
	switch binary.BigEndian.Uint16(data[:2]) {
	case 0:
		parseSFNTCmapFormat0(data, result)
	case 4:
		parseSFNTCmapFormat4(data, result)
	case 6:
		parseSFNTCmapFormat6(data, result)
	case 12:
		parseSFNTCmapFormat12(data, result)
	}
}

// parseSFNTCmapFormat0 解析 8 位字形表。
func parseSFNTCmapFormat0(data []byte, result map[uint16]rune) {
	if len(data) < 262 {
		return
	}
	for char := 0; char < 256; char++ {
		addSFNTGlyphRune(result, uint16(data[6+char]), rune(char))
	}
}

// parseSFNTCmapFormat6 解析连续 BMP 字符表。
func parseSFNTCmapFormat6(data []byte, result map[uint16]rune) {
	if len(data) < 10 {
		return
	}
	length := int(binary.BigEndian.Uint16(data[2:4]))
	first := int(binary.BigEndian.Uint16(data[6:8]))
	count := int(binary.BigEndian.Uint16(data[8:10]))
	if length > len(data) || count > (length-10)/2 {
		return
	}
	for index := 0; index < count; index++ {
		glyphID := binary.BigEndian.Uint16(data[10+index*2 : 12+index*2])
		addSFNTGlyphRune(result, glyphID, rune(first+index))
	}
}

// parseSFNTCmapFormat12 解析完整 Unicode 分组表。
func parseSFNTCmapFormat12(data []byte, result map[uint16]rune) {
	if len(data) < 16 {
		return
	}
	length := int(binary.BigEndian.Uint32(data[4:8]))
	groups := int(binary.BigEndian.Uint32(data[12:16]))
	if length > len(data) || groups < 0 || groups > (length-16)/12 {
		return
	}
	for index := 0; index < groups && len(result) < pdfUnicodeMaxMappings; index++ {
		offset := 16 + index*12
		start := binary.BigEndian.Uint32(data[offset : offset+4])
		end := binary.BigEndian.Uint32(data[offset+4 : offset+8])
		glyphStart := binary.BigEndian.Uint32(data[offset+8 : offset+12])
		if end < start || uint64(end-start)+1 > pdfUnicodeMaxMappings {
			continue
		}
		rangeCount := end - start + 1
		for delta := uint32(0); delta < rangeCount && len(result) < pdfUnicodeMaxMappings; delta++ {
			char := start + delta
			glyphID := glyphStart + delta
			if glyphID <= 0xffff && utf8.ValidRune(rune(char)) {
				addSFNTGlyphRune(result, uint16(glyphID), rune(char))
			}
		}
	}
}

// parseSFNTCmapFormat4 解析分段 BMP 字符表。
func parseSFNTCmapFormat4(data []byte, result map[uint16]rune) {
	if len(data) < 16 {
		return
	}
	length := int(binary.BigEndian.Uint16(data[2:4]))
	segments := int(binary.BigEndian.Uint16(data[6:8])) / 2
	if length > len(data) || segments <= 0 || segments > 32768 {
		return
	}
	endOffset := 14
	startOffset := endOffset + segments*2 + 2
	deltaOffset := startOffset + segments*2
	rangeOffset := deltaOffset + segments*2
	if rangeOffset+segments*2 > length {
		return
	}
	for segment := 0; segment < segments && len(result) < pdfUnicodeMaxMappings; segment++ {
		start := binary.BigEndian.Uint16(data[startOffset+segment*2 : startOffset+segment*2+2])
		end := binary.BigEndian.Uint16(data[endOffset+segment*2 : endOffset+segment*2+2])
		delta := binary.BigEndian.Uint16(data[deltaOffset+segment*2 : deltaOffset+segment*2+2])
		rangeValue := binary.BigEndian.Uint16(data[rangeOffset+segment*2 : rangeOffset+segment*2+2])
		if end < start || start == 0xffff {
			continue
		}
		for char := uint32(start); char <= uint32(end) && len(result) < pdfUnicodeMaxMappings; char++ {
			var glyphID uint16
			if rangeValue == 0 {
				glyphID = uint16(char + uint32(delta))
			} else {
				glyphOffset := rangeOffset + segment*2 + int(rangeValue) + int(char-uint32(start))*2
				if glyphOffset+2 > length {
					continue
				}
				glyphID = binary.BigEndian.Uint16(data[glyphOffset : glyphOffset+2])
				if glyphID != 0 {
					glyphID += delta
				}
			}
			addSFNTGlyphRune(result, glyphID, rune(char))
		}
	}
}

// addSFNTGlyphRune 为字形选择稳定 Unicode；优先非私用区字符，避免符号字体
// 的私有码覆盖可检索正文。
func addSFNTGlyphRune(result map[uint16]rune, glyphID uint16, char rune) {
	if glyphID == 0 || !utf8.ValidRune(char) || char == unicode.ReplacementChar {
		return
	}
	existing, found := result[glyphID]
	if !found || (IsUnicodePrivateUse(existing) && !IsUnicodePrivateUse(char)) {
		result[glyphID] = char
	}
}

// isUnicodePrivateUse 判断 Unicode 私用区，避免依赖未导出的标准库属性表。
func IsUnicodePrivateUse(char rune) bool {
	return char >= 0xE000 && char <= 0xF8FF ||
		char >= 0xF0000 && char <= 0xFFFFD ||
		char >= 0x100000 && char <= 0x10FFFD
}
