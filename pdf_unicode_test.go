// pdf_unicode_test.go 验证 PDF 字体 Unicode 恢复路径：覆盖多字节
// ToUnicode CMap 范围进位，以及缺失 ToUnicode 时从嵌入 TrueType cmap
// 和 Identity CIDToGIDMap 恢复中文文本。
package docling

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"github.com/ledongthuc/pdf"

	"github.com/unitedrhino/docling/internal/pdfenc"
)

// TestParsePDFKeepsCoordinatesWhenTJAddsSyntheticReplacement 验证坐标解析器
// 在 Type0 字体的 TJ 末尾误解码时，独立解码可确认并移除伪字形。
func TestParsePDFKeepsCoordinatesWhenTJAddsSyntheticReplacement(t *testing.T) {
	toUnicode := []byte(`1 begincodespacerange
<0000> <FFFF>
endcodespacerange
2 beginbfchar
<0001> <4E2D>
<0002> <6587>
endbfchar`)
	objects := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [4 0 R] /Count 1 >>"),
		[]byte("<< /Type /Font /Subtype /Type0 /BaseFont /TestSubset /Encoding /Identity-H /DescendantFonts [6 0 R] /ToUnicode 7 0 R >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 5 0 R /Resources << /Font << /F1 3 0 R >> >> >>"),
		pdfTestStream([]byte("BT /F1 24 Tf 72 700 Td [<0001> -500 <0002>] TJ ET")),
		[]byte("<< /Type /Font /Subtype /CIDFontType2 /BaseFont /TestSubset /CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> /DW 1000 >>"),
		pdfTestStream(toUnicode),
	}
	data := buildPDFBinaryObjects(t, objects)
	doc, err := ParsePDFWithOptions(data, PDFOptions{DisablePopplerFallback: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Texts) != 1 || doc.Texts[0].Text != "中文" || doc.Texts[0].Label != LabelTitle ||
		len(doc.Texts[0].Prov) != 1 || doc.Texts[0].Prov[0].BBox == nil {
		t.Fatalf("预期保留标题、坐标与连续中文，实际为 %+v", doc.Texts)
	}
}

// TestParsePDFRecoversIdentityHFromEmbeddedTrueType 验证 Type0/Identity-H
// 字体缺少 ToUnicode 时，从 CIDToGIDMap 与嵌入 TrueType cmap 纯 Go 恢复中文；
// 测试显式关闭 Poppler，确保成功并非来自外部 CLI。
func TestParsePDFRecoversIdentityHFromEmbeddedTrueType(t *testing.T) {
	font := buildTestSFNTUnicodeCmap(t, map[rune]uint32{'中': 1, '文': 2})
	content := []byte("BT /F1 12 Tf 72 700 Td <00010002> Tj ET")
	objects := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [4 0 R] /Count 1 >>"),
		[]byte("<< /Type /Font /Subtype /Type0 /BaseFont /TestSubset /Encoding /Identity-H /DescendantFonts [6 0 R] >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 5 0 R /Resources << /Font << /F1 3 0 R >> >> >>"),
		pdfTestStream(content),
		[]byte("<< /Type /Font /Subtype /CIDFontType2 /BaseFont /TestSubset /CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> /FontDescriptor 7 0 R /CIDToGIDMap /Identity /DW 1000 >>"),
		[]byte("<< /Type /FontDescriptor /FontName /TestSubset /Flags 4 /FontBBox [0 -200 1000 900] /ItalicAngle 0 /Ascent 800 /Descent -200 /CapHeight 700 /StemV 80 /FontFile2 8 0 R >>"),
		pdfTestStream(font),
	}
	data := buildPDFBinaryObjects(t, objects)
	doc, err := ParsePDFWithOptions(data, PDFOptions{DisablePopplerFallback: true})
	if err != nil {
		t.Fatalf("ParsePDFWithOptions: %v", err)
	}
	text := doc.Text()
	if !strings.Contains(text, "中文") || strings.ContainsRune(text, '\uFFFD') {
		t.Fatalf("纯 Go Unicode 恢复失败: %q", text)
	}
}

// TestParsePDFRecoversCustomEncodingCMap 验证 Type0 字体通过自定义 Encoding
// CMap 把字符码映射为 CID 后，继续经 CIDToGIDMap 与嵌入字体 cmap 恢复
// Unicode；字符码与 CID 不相等，防止 Identity 假设掩盖问题。
func TestParsePDFRecoversCustomEncodingCMap(t *testing.T) {
	font := buildTestSFNTUnicodeCmap(t, map[rune]uint32{'中': 1, '文': 2})
	content := []byte("BT /F1 12 Tf 72 700 Td <4142> Tj ET")
	encodingCMap := []byte(`/CIDInit /ProcSet findresource begin
12 dict begin begincmap
1 begincodespacerange
<00> <FF>
endcodespacerange
1 begincidrange
<41> <42> 5
endcidrange
endcmap end end`)
	cidToGID := make([]byte, 14)
	binary.BigEndian.PutUint16(cidToGID[10:12], 1)
	binary.BigEndian.PutUint16(cidToGID[12:14], 2)
	objects := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [4 0 R] /Count 1 >>"),
		[]byte("<< /Type /Font /Subtype /Type0 /BaseFont /CustomEncoding /Encoding 7 0 R /DescendantFonts [6 0 R] >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 5 0 R /Resources << /Font << /F1 3 0 R >> >> >>"),
		pdfTestStream(content),
		[]byte("<< /Type /Font /Subtype /CIDFontType2 /BaseFont /CustomEncoding /CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> /FontDescriptor 8 0 R /CIDToGIDMap 9 0 R /DW 1000 >>"),
		pdfTestStream(encodingCMap),
		[]byte("<< /Type /FontDescriptor /FontName /CustomEncoding /Flags 4 /FontBBox [0 -200 1000 900] /ItalicAngle 0 /Ascent 800 /Descent -200 /CapHeight 700 /StemV 80 /FontFile2 10 0 R >>"),
		pdfTestStream(cidToGID),
		pdfTestStream(font),
	}
	data := buildPDFBinaryObjects(t, objects)
	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open custom Encoding PDF: %v", err)
	}
	page := reader.Page(1)
	fontValue := page.Resources().Key("Font").Key("F1")
	encodingData, readEncoding := pdfenc.ReadUnicodeStream(fontValue.Key("Encoding"))
	encodingMap, parsedEncoding := pdfenc.ParseCIDCMap(encodingData)
	if !readEncoding || !parsedEncoding || encodingMap.Lookup(string([]byte{0x41})) != 5 {
		t.Fatalf("custom Encoding stream read=%v parsed=%v map=%+v", readEncoding, parsedEncoding, encodingMap)
	}
	descendant := fontValue.Key("DescendantFonts").Index(0)
	if got := pdfenc.ParseCIDToGID(descendant.Key("CIDToGIDMap")); got[5] != 1 || got[6] != 2 {
		t.Fatalf("CIDToGIDMap=%+v", got)
	}
	fontData, readFont := pdfenc.ReadUnicodeStream(descendant.Key("FontDescriptor").Key("FontFile2"))
	glyphs := pdfenc.ParseSFNTGlyphUnicode(fontData)
	if !readFont || glyphs[1] != '中' || glyphs[2] != '文' {
		t.Fatalf("embedded font read=%v glyphs=%+v", readFont, glyphs)
	}
	decoder := pdfenc.NewUnicodeFont(pdf.Font{V: fontValue})
	if got := decoder.Decode([]byte{0x41, 0x42}); got != "中文" {
		t.Fatalf("custom Encoding font decoder=%q encodingKind=%v", got, fontValue.Key("Encoding").Kind())
	}
	if !pdfenc.HasUnicodeRecoveryCandidate(page) {
		t.Fatal("custom Encoding font should trigger Unicode recovery")
	}
	doc, err := ParsePDFWithOptions(data, PDFOptions{DisablePopplerFallback: true})
	if err != nil {
		t.Fatalf("ParsePDFWithOptions: %v", err)
	}
	if text := doc.Text(); !strings.Contains(text, "中文") || strings.ContainsRune(text, '\uFFFD') {
		t.Fatalf("自定义 Encoding CMap 恢复失败: %q", text)
	}
}

// TestParsePDFRecoversAfterMalformedLibraryCMap 验证第三方 CMap 解释器因多余
// endbfchar panic 时，主解析隔离异常并继续使用自有有界解析器恢复正文。
func TestParsePDFRecoversAfterMalformedLibraryCMap(t *testing.T) {
	content := []byte("BT /F1 12 Tf 72 700 Td <00010002> Tj ET")
	toUnicode := []byte(`endbfchar
1 begincodespacerange
<0000> <FFFF>
endcodespacerange
2 beginbfchar
<0001> <4E2D>
<0002> <6587>
endbfchar`)
	objects := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [4 0 R] /Count 1 >>"),
		[]byte("<< /Type /Font /Subtype /Type0 /BaseFont /BrokenCMap /Encoding /Identity-H /DescendantFonts [6 0 R] /ToUnicode 7 0 R >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 5 0 R /Resources << /Font << /F1 3 0 R >> >> >>"),
		pdfTestStream(content),
		[]byte("<< /Type /Font /Subtype /CIDFontType2 /BaseFont /BrokenCMap /CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> /CIDToGIDMap /Identity /DW 1000 >>"),
		pdfTestStream(toUnicode),
	}
	doc, err := ParsePDFWithOptions(buildPDFBinaryObjects(t, objects), PDFOptions{DisablePopplerFallback: true})
	if err != nil {
		t.Fatalf("ParsePDFWithOptions 不应传播第三方 CMap panic: %v", err)
	}
	if text := doc.Text(); !strings.Contains(text, "中文") || strings.ContainsRune(text, '\uFFFD') {
		t.Fatalf("畸形 CMap 恢复失败: %q", text)
	}
}

// TestEvaluatePDFPageQualityRoutesCoordinateLessRecovery 验证字体恢复文本因缺少
// 可靠 bbox 自动进入结构化视觉，由模型补回阅读顺序而非误判为高质量文本层。
func TestEvaluatePDFPageQualityRoutesCoordinateLessRecovery(t *testing.T) {
	quality := evaluatePDFPageQuality([]pdfLine{{
		Text: "纯 Go 字体恢复后的中文标准正文", PageIdx: 0, Fallback: true,
	}})
	if !quality.NeedsVisual || !containsString(quality.Reasons, "coordinate_less_text") {
		t.Fatalf("无坐标恢复文本未触发视觉路由: %+v", quality)
	}
}

// TestParsePDFRecoversNamedCJKEncodings 验证缺少 ToUnicode 和嵌入字体时，
// 常见标准 CJK CMap 名称仍可用纯 Go 字符集解码恢复正文。
func TestParsePDFRecoversNamedCJKEncodings(t *testing.T) {
	tests := []struct {
		name     string
		encoding string
		hexText  string
	}{
		{name: "UniGB UCS2", encoding: "UniGB-UCS2-H", hexText: "4E2D6587"},
		{name: "GBK EUC", encoding: "GBK-EUC-H", hexText: "D6D0CEC4"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := []byte(fmt.Sprintf("BT /F1 12 Tf 72 700 Td <%s> Tj ET", test.hexText))
			objects := [][]byte{
				[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
				[]byte("<< /Type /Pages /Kids [4 0 R] /Count 1 >>"),
				[]byte(fmt.Sprintf("<< /Type /Font /Subtype /Type0 /BaseFont /NamedCMap /Encoding /%s /DescendantFonts [6 0 R] >>", test.encoding)),
				[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 5 0 R /Resources << /Font << /F1 3 0 R >> >> >>"),
				pdfTestStream(content),
				[]byte("<< /Type /Font /Subtype /CIDFontType2 /BaseFont /NamedCMap /CIDSystemInfo << /Registry (Adobe) /Ordering (GB1) /Supplement 5 >> /CIDToGIDMap /Identity /DW 1000 >>"),
			}
			doc, err := ParsePDFWithOptions(buildPDFBinaryObjects(t, objects), PDFOptions{DisablePopplerFallback: true})
			if err != nil {
				t.Fatalf("ParsePDFWithOptions: %v", err)
			}
			if text := doc.Text(); !strings.Contains(text, "中文") || strings.ContainsRune(text, '\uFFFD') {
				t.Fatalf("%s 恢复失败: %q", test.encoding, text)
			}
		})
	}
}

// TestParsePDFRecoversTextInsideFormXObject 验证页面主内容流中的 Do 操作会
// 递归读取 Form XObject 自有字体资源，且恢复文本与页面原有文本保持绘制顺序。
func TestParsePDFRecoversTextInsideFormXObject(t *testing.T) {
	pageContent := []byte("BT /F1 12 Tf 72 720 Td (prefix) Tj ET\n/Fm0 Do")
	formContent := []byte("BT /F2 12 Tf 10 50 Td <4E2D6587> Tj ET")
	fontWidths := strings.TrimRight(strings.Repeat("500 ", 95), " ")
	formObject := bytes.NewBuffer(nil)
	fmt.Fprintf(formObject, "<< /Type /XObject /Subtype /Form /BBox [0 0 200 100] /Resources << /Font << /F2 7 0 R >> >> /Length %d >>\nstream\n", len(formContent))
	formObject.Write(formContent)
	formObject.WriteString("\nendstream")
	objects := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [4 0 R] /Count 1 >>"),
		[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding /FirstChar 32 /LastChar 126 /Widths [" + fontWidths + "] >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 5 0 R /Resources << /Font << /F1 3 0 R >> /XObject << /Fm0 6 0 R >> >> >>"),
		pdfTestStream(pageContent),
		formObject.Bytes(),
		[]byte("<< /Type /Font /Subtype /Type0 /BaseFont /FormCJK /Encoding /UniGB-UCS2-H /DescendantFonts [8 0 R] >>"),
		[]byte("<< /Type /Font /Subtype /CIDFontType2 /BaseFont /FormCJK /CIDSystemInfo << /Registry (Adobe) /Ordering (GB1) /Supplement 5 >> /CIDToGIDMap /Identity /DW 1000 >>"),
	}
	doc, err := ParsePDFWithOptions(buildPDFBinaryObjects(t, objects), PDFOptions{DisablePopplerFallback: true})
	if err != nil {
		t.Fatalf("ParsePDFWithOptions: %v", err)
	}
	text := doc.Text()
	if !strings.Contains(text, "prefix") || !strings.Contains(text, "中文") || strings.Index(text, "prefix") > strings.Index(text, "中文") {
		t.Fatalf("Form XObject 文本恢复或顺序错误: %q", text)
	}
}

// buildTestSFNTUnicodeCmap 构造只含 cmap format 12 的最小 sfnt 字节；解析器
// 只读取字体目录和 cmap，因此无需在 fixture 中携带轮廓、度量等无关表。
func buildTestSFNTUnicodeCmap(t *testing.T, glyphs map[rune]uint32) []byte {
	t.Helper()
	ordered := []rune{'中', '文'}
	if len(glyphs) != len(ordered) {
		t.Fatalf("测试 cmap 字符数量=%d", len(glyphs))
	}
	subtable := make([]byte, 16+12*len(ordered))
	binary.BigEndian.PutUint16(subtable[0:2], 12)
	binary.BigEndian.PutUint32(subtable[4:8], uint32(len(subtable)))
	binary.BigEndian.PutUint32(subtable[12:16], uint32(len(ordered)))
	for index, char := range ordered {
		offset := 16 + index*12
		binary.BigEndian.PutUint32(subtable[offset:offset+4], uint32(char))
		binary.BigEndian.PutUint32(subtable[offset+4:offset+8], uint32(char))
		binary.BigEndian.PutUint32(subtable[offset+8:offset+12], glyphs[char])
	}
	cmap := make([]byte, 12+len(subtable))
	binary.BigEndian.PutUint16(cmap[2:4], 1)
	binary.BigEndian.PutUint16(cmap[4:6], 3)
	binary.BigEndian.PutUint16(cmap[6:8], 10)
	binary.BigEndian.PutUint32(cmap[8:12], 12)
	copy(cmap[12:], subtable)

	font := make([]byte, 28+len(cmap))
	binary.BigEndian.PutUint32(font[0:4], 0x00010000)
	binary.BigEndian.PutUint16(font[4:6], 1)
	copy(font[12:16], "cmap")
	binary.BigEndian.PutUint32(font[20:24], 28)
	binary.BigEndian.PutUint32(font[24:28], uint32(len(cmap)))
	copy(font[28:], cmap)
	return font
}

// pdfTestStream 把任意二进制负载包装为无过滤器 PDF stream 对象。
func pdfTestStream(data []byte) []byte {
	var out bytes.Buffer
	fmt.Fprintf(&out, "<< /Length %d >>\nstream\n", len(data))
	out.Write(data)
	out.WriteString("\nendstream")
	return out.Bytes()
}

// buildPDFBinaryObjects 用显式对象数组生成最小 PDF，允许 fixture 包含二进制
// 字体流，避免字符串转换破坏字体字节。
func buildPDFBinaryObjects(t *testing.T, objects [][]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for index, object := range objects {
		offsets[index] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n", index+1)
		out.Write(object)
		out.WriteString("\nendobj\n")
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return out.Bytes()
}
