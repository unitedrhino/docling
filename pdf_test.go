// pdf_test.go 验证 PDF 解析主路径（ParsePDF → DoclingDocument）：
// 手写最小文本型 PDF fixture（PDF 1.4 语法：catalog/pages/font/page/stream/xref），
// 覆盖标题启发式与层级父挂接、正文元素 prov（页号/BOTTOMLEFT bbox/charspan）、
// 页面尺寸登记（含负偏移 MediaBox）、扫描件兜底（无 prov text、全空文档报错）、
// 单页超长正文分段与 prov bbox 并集。
package docling

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/ledongthuc/pdf"
)

// testPDFPage 合成 PDF 的单页描述：内容流、页面框、旋转与用户单位。
type testPDFPage struct {
	content  string  // 页内容流（BT/ET 文本操作符）
	mediaBox string  // MediaBox 数组文本，如 "[0 0 612 792]"
	cropBox  string  // CropBox 数组文本；空表示不声明。
	rotate   int     // Rotate 页面顺时针旋转角度；0 表示不声明。
	userUnit float64 // UserUnit 用户空间缩放；0 表示默认 1。
}

// buildTestPDF 按最小 PDF 1.4 语法拼出多页文本型 PDF 字节串：
// 对象布局为 1=Catalog、2=Pages、3=Font、每页依次 Page 字典与 Contents 流；
// 字体为 Helvetica + WinAnsiEncoding（ASCII 字节即字符码）并给全 500 字宽
// （字符宽 = 0.5×字号），保证 Content() 可解码且字符坐标/宽度可精确预测。
func buildTestPDF(t *testing.T, pages []testPDFPage) []byte {
	t.Helper()
	fontWidths := strings.TrimRight(strings.Repeat("500 ", 95), " ")
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
	}
	kids := make([]string, 0, len(pages))
	for i := range pages {
		kids = append(kids, fmt.Sprintf("%d 0 R", 4+2*i))
	}
	objs = append(objs, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages)))
	objs = append(objs, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding /FirstChar 32 /LastChar 126 /Widths ["+fontWidths+"] >>")
	for i, p := range pages {
		mediaBox := p.mediaBox
		if mediaBox == "" {
			mediaBox = "[0 0 612 792]"
		}
		var pageOptions strings.Builder
		if p.cropBox != "" {
			fmt.Fprintf(&pageOptions, " /CropBox %s", p.cropBox)
		}
		if p.rotate != 0 {
			fmt.Fprintf(&pageOptions, " /Rotate %d", p.rotate)
		}
		if p.userUnit != 0 {
			fmt.Fprintf(&pageOptions, " /UserUnit %g", p.userUnit)
		}
		objs = append(objs, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox %s%s /Contents %d 0 R /Resources << /Font << /F1 3 0 R >> >> >>", mediaBox, pageOptions.String(), 4+2*i+1))
		objs = append(objs, fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(p.content), p.content))
	}
	var b strings.Builder
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xrefOffset := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n", len(objs)+1)
	b.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xrefOffset)
	return []byte(b.String())
}

// TestPDFPageGeometryNormalizesCropRotationAndUserUnit 验证可见 CropBox、
// 非零原点、UserUnit 与页旋转统一映射到从零开始的显示坐标。
func TestPDFPageGeometryNormalizesCropRotationAndUserUnit(t *testing.T) {
	data := buildTestPDF(t, []testPDFPage{{
		content:  "BT /F1 10 Tf 150 250 Td (rotated) Tj ET",
		mediaBox: "[-50 -100 550 700]", cropBox: "[100 200 500 700]", rotate: 90, userUnit: 2,
	}})
	reader, err := pdf.NewReader(strings.NewReader(string(data)), int64(len(data)))
	if err != nil {
		t.Fatalf("open geometry fixture: %v", err)
	}
	geometry := pdfPageGeometryForPage(reader.Page(1))
	if geometry.rotation != 90 || geometry.width != 1000 || geometry.height != 800 ||
		geometry.minX != 100 || geometry.minY != 200 || geometry.userUnit != 2 {
		t.Fatalf("page geometry=%+v", geometry)
	}
	bbox := geometry.transformBBox(150, 250, 250, 300)
	if bbox == nil || bbox.L != 100 || bbox.B != 500 || bbox.R != 200 || bbox.T != 700 {
		t.Fatalf("rotated crop bbox=%+v", bbox)
	}
}

// TestPDFPageGeometryAllQuarterTurns 验证四个直角旋转的页尺寸与
// 矩形四角映射，同时覆盖负角度归一化。
func TestPDFPageGeometryAllQuarterTurns(t *testing.T) {
	tests := []struct {
		name         string
		rotation     int
		wantRotation int
		wantWidth    float64
		wantHeight   float64
		wantLeft     float64
		wantBottom   float64
		wantRight    float64
		wantTop      float64
	}{
		{name: "zero", rotation: 0, wantRotation: 0, wantWidth: 400, wantHeight: 500, wantLeft: 50, wantBottom: 50, wantRight: 150, wantTop: 100},
		{name: "ninety", rotation: 90, wantRotation: 90, wantWidth: 500, wantHeight: 400, wantLeft: 50, wantBottom: 250, wantRight: 100, wantTop: 350},
		{name: "one-eighty", rotation: 180, wantRotation: 180, wantWidth: 400, wantHeight: 500, wantLeft: 250, wantBottom: 400, wantRight: 350, wantTop: 450},
		{name: "negative-ninety", rotation: -90, wantRotation: 270, wantWidth: 500, wantHeight: 400, wantLeft: 400, wantBottom: 50, wantRight: 450, wantTop: 150},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := buildTestPDF(t, []testPDFPage{{
				content:  "BT /F1 10 Tf 150 250 Td (geometry) Tj ET",
				mediaBox: "[0 0 600 800]", cropBox: "[100 200 500 700]", rotate: test.rotation,
			}})
			reader, err := pdf.NewReader(strings.NewReader(string(data)), int64(len(data)))
			if err != nil {
				t.Fatalf("open quarter-turn fixture: %v", err)
			}
			geometry := pdfPageGeometryForPage(reader.Page(1))
			bbox := geometry.transformBBox(150, 250, 250, 300)
			if geometry.rotation != test.wantRotation || geometry.width != test.wantWidth ||
				geometry.height != test.wantHeight || bbox == nil || bbox.L != test.wantLeft ||
				bbox.B != test.wantBottom || bbox.R != test.wantRight || bbox.T != test.wantTop {
				t.Fatalf("quarter-turn geometry=%+v bbox=%+v", geometry, bbox)
			}
		})
	}
}

// TestParsePDFCropBoxDropsInvisibleTextAndNormalizesProvenance 验证裁剪框外
// 的文本不进入知识库，框内文本坐标应用非零原点与 UserUnit。
func TestParsePDFCropBoxDropsInvisibleTextAndNormalizesProvenance(t *testing.T) {
	data := buildTestPDF(t, []testPDFPage{{
		content: "BT /F1 10 Tf 50 100 Td (hidden crop text) Tj ET\n" +
			"BT /F1 10 Tf 150 250 Td (visible text) Tj ET",
		mediaBox: "[0 0 600 800]", cropBox: "[100 200 500 700]", userUnit: 2,
	}})
	doc, err := ParsePDFWithOptions(data, PDFOptions{DisablePopplerFallback: true})
	if err != nil {
		t.Fatalf("parse crop text fixture: %v", err)
	}
	if strings.Contains(doc.Text(), "hidden crop text") || !strings.Contains(doc.Text(), "visible text") {
		t.Fatalf("cropped document text=%q", doc.Text())
	}
	page := doc.Pages["1"]
	if page.Size == nil || page.Size.Width != 800 || page.Size.Height != 1000 {
		t.Fatalf("scaled crop page=%+v", page)
	}
	if len(doc.Texts) != 1 || len(doc.Texts[0].Prov) != 1 || doc.Texts[0].Prov[0].BBox == nil {
		t.Fatalf("visible text provenance=%+v", doc.Texts)
	}
	bbox := doc.Texts[0].Prov[0].BBox
	if bbox.L != 100 || bbox.B != 100 || bbox.R <= bbox.L || bbox.T != bbox.B {
		t.Fatalf("normalized visible bbox=%+v", bbox)
	}
}

// TestAssemblePDFLinesPreservesWordCoordinates 验证字符路径在保持既有行文本和
// 行 bbox 的同时，把由水平间隙分隔的字符聚合为携带 bbox/字号的词。
func TestAssemblePDFLinesPreservesWordCoordinates(t *testing.T) {
	chars := []pdf.Text{
		{S: "i", X: 15, Y: 100.5, W: 5, FontSize: 11},
		{S: "G", X: 30, Y: 99.5, W: 5, FontSize: 12},
		{S: "H", X: 10, Y: 100, W: 5, FontSize: 10},
		{S: "o", X: 35, Y: 100.2, W: 5, FontSize: 12},
	}

	lines := assemblePDFLines(chars, 2)
	if len(lines) != 1 {
		t.Fatalf("lines = %+v, want one line", lines)
	}
	line := lines[0]
	if line.Text != "Hi Go" || line.PageIdx != 2 || line.MinX != 10 || line.MaxX != 40 ||
		line.MinY != 99.5 || line.MaxY != 100.5 || line.MaxFontSize != 12 {
		t.Fatalf("line = %+v, want unchanged text/page/bbox/font", line)
	}
	if len(line.Words) != 2 {
		t.Fatalf("line words = %+v, want two words", line.Words)
	}
	want := []pdfWord{
		{Text: "Hi", MinX: 10, MaxX: 20, MinY: 100, MaxY: 100.5, FontSize: 11},
		{Text: "Go", MinX: 30, MaxX: 40, MinY: 99.5, MaxY: 100.2, FontSize: 12},
	}
	for i := range want {
		if line.Words[i] != want[i] {
			t.Fatalf("words[%d] = %+v, want %+v", i, line.Words[i], want[i])
		}
	}
}

// TestParsePDFDropsCoincidentGlyphs 验证重绘字符只提取一次，邻位重复字母仍保留。
func TestParsePDFDropsCoincidentGlyphs(t *testing.T) {
	data := buildTestPDF(t, []testPDFPage{{content: "BT /F1 12 Tf 72 700 Td (LETTER) Tj ET\n" +
		"BT /F1 12 Tf 72 700 Td (LETTER) Tj ET"}})
	doc, err := ParsePDF(data)
	if err != nil {
		t.Fatal(err)
	}
	if text := strings.TrimSpace(doc.Text()); text != "LETTER" {
		t.Fatalf("预期提取一次 LETTER，实际为 %q", text)
	}
}

// TestPDFHeadingRankChecksEveryLine 验证同字号的坏字、碎行与长正文无需
// 进入章节路径，同时保留同页的有效标题。
func TestPDFHeadingRankChecksEveryLine(t *testing.T) {
	lines := []pdfLine{
		{PageIdx: 1, Text: "有效章节", MaxFontSize: 16},
		{PageIdx: 1, Text: "p", MaxFontSize: 16},
		{PageIdx: 1, Text: "坏字�标题", MaxFontSize: 16},
		{PageIdx: 1, Text: "“ G o { 4 ^ E ä à { k _ p Ê Ä", MaxFontSize: 16},
		{PageIdx: 1, Text: strings.Repeat("同字号正文", 20), MaxFontSize: 16},
	}
	for range 6 {
		lines = append(lines, pdfLine{PageIdx: 1, Text: "较小字号的正文行。", MaxFontSize: 10})
	}
	elements := buildPDFElements(lines)
	for i, element := range elements {
		if (element.headingLevel > 0) != (i == 0) {
			t.Fatalf("正文或标题分类错误：%q level=%d", element.line.Text, element.headingLevel)
		}
	}
}

// TestParsePDFDoclingStructure 验证主路径：标题启发式识别与层级父挂接、
// 正文元素 prov（页号/BOTTOMLEFT bbox/charspan）、页面尺寸登记，
// 以及树结构可被简化器消费（正文携带祖先标题章节路径）。
// fixture 坐标约定：正文 10pt/标题 18pt、14pt，字体全 500 字宽（字符宽 0.5×字号）。
// 首页正文凑足 15 行（不小于 knowledgePDFCoverMaxHomeLines），
// 避免首页命中封面主标题识别（其要求首页行数 <15）。
func TestParsePDFDoclingStructure(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("BT /F1 18 Tf 72 720 Td (Introduction) Tj ET\n")
	sb.WriteString("BT /F1 10 Tf 72 690 Td (This is the first body line.) Tj ET\n")
	sb.WriteString("BT /F1 10 Tf 72 670 Td (Second body line.) Tj ET\n")
	for i := 1; i <= 12; i++ {
		fmt.Fprintf(&sb, "BT /F1 10 Tf 72 %g Td (Extra body line %02d.) Tj ET\n", float64(670-20*i), i)
	}
	data := buildTestPDF(t, []testPDFPage{
		{content: sb.String()},
		{
			content: "BT /F1 14 Tf 72 700 Td (Details) Tj ET\n" +
				"BT /F1 10 Tf 72 670 Td (Body under details.) Tj ET\n",
			// 负偏移 MediaBox：宽高须按右上-左下差值计算
			mediaBox: "[-20 -30 592 762]",
		},
	})
	doc, err := ParsePDF(data)
	if err != nil {
		t.Fatalf("ParsePDF: %v", err)
	}
	if len(doc.Texts) != 4 {
		t.Fatalf("expected 4 texts (2 headings + 2 body blocks), got %d: %+v", len(doc.Texts), doc.Texts)
	}

	// 标题：字号 18/14 降序映射层级 1/2，label=section_header
	h1 := doc.Texts[0]
	if h1.Label != LabelSectionHeader || h1.TextLevel != 1 || h1.Text != "Introduction" {
		t.Fatalf("texts[0] = %+v, want Introduction level 1", h1)
	}
	h2 := doc.Texts[2]
	if h2.Label != LabelSectionHeader || h2.TextLevel != 2 || h2.Text != "Details" {
		t.Fatalf("texts[2] = %+v, want Details level 2", h2)
	}
	// 一级标题挂 body；二级标题挂最近一级标题（跳级不补分组）
	if h1.Parent == nil || h1.Parent.String() != "#/body" {
		t.Fatalf("h1 parent = %+v, want #/body", h1.Parent)
	}
	if h2.Parent == nil || h2.Parent.String() != "#/texts/0" {
		t.Fatalf("h2 parent = %+v, want #/texts/0", h2.Parent)
	}

	// 标题 prov：单行 bbox 上下边重合于基线（Top=Bottom=720），charspan 覆盖整行
	if len(h1.Prov) != 1 {
		t.Fatalf("h1 prov = %+v, want single prov", h1.Prov)
	}
	hp := h1.Prov[0]
	if hp.PageNo != 1 || hp.BBox.L != 72 || hp.BBox.T != 720 || hp.BBox.B != 720 ||
		hp.BBox.CoordOrigin != CoordOriginBottomLeft || hp.CharSpan != [2]int64{0, 12} {
		t.Fatalf("h1 prov[0] = %+v, want page 1 bbox l=72 t=b=720 BOTTOMLEFT charspan [0,12]", hp)
	}

	// 正文块：页内 14 行正文合并为一个 text 元素，prov bbox 为各行并集（Top>Bottom）。
	// 行文本无空格是既有启发式行为：char 级 Content() 下每个空格独立成 Text，
	// 被 assemblePDFLines 的空白过滤剔除（保持旧实现口径，不在本次改造范围）。
	body1 := doc.Texts[1]
	wantLines := []string{"This is the first body line.", "Second body line."}
	for i := 1; i <= 12; i++ {
		wantLines = append(wantLines, fmt.Sprintf("Extra body line %02d.", i))
	}
	wantText := strings.Join(wantLines, "\n")
	if body1.Label != LabelText || body1.Text != wantText {
		t.Fatalf("texts[1] = %+v, want %q", body1, wantText)
	}
	if body1.Parent == nil || body1.Parent.String() != "#/texts/0" {
		t.Fatalf("body1 parent = %+v, want #/texts/0", body1.Parent)
	}
	if len(body1.Prov) != 1 {
		t.Fatalf("body1 prov = %+v, want single prov", body1.Prov)
	}
	bp := body1.Prov[0]
	if bp.PageNo != 1 || bp.BBox.L != 72 || bp.BBox.T != 690 || bp.BBox.B != 430 {
		t.Fatalf("body1 prov[0] = %+v, want page 1 bbox l=72 t=690 b=430", bp)
	}
	if !(bp.BBox.T > bp.BBox.B) || !(bp.BBox.R > bp.BBox.L) {
		t.Fatalf("body1 bbox degenerate: %+v", bp.BBox)
	}
	// 右边缘 = X + 末字符宽度（28 字符 × 0.5×10pt = 140 → 72+140）
	if math.Abs(bp.BBox.R-212) > 0.01 {
		t.Fatalf("body1 bbox right = %v, want 212", bp.BBox.R)
	}
	if bp.CharSpan != [2]int64{0, int64(len([]rune(wantText)))} {
		t.Fatalf("body1 charspan = %+v, want [0 %d]", bp.CharSpan, len([]rune(wantText)))
	}

	// 第二页正文挂 Details（#texts/2），prov 页号为 2
	body2 := doc.Texts[3]
	if body2.Text != "Body under details." {
		t.Fatalf("texts[3] text = %q, want 'Body under details.'", body2.Text)
	}
	if body2.Parent == nil || body2.Parent.String() != "#/texts/2" {
		t.Fatalf("body2 parent = %+v, want #/texts/2", body2.Parent)
	}
	if len(body2.Prov) != 1 || body2.Prov[0].PageNo != 2 {
		t.Fatalf("body2 prov = %+v, want page 2", body2.Prov)
	}

	// 页面尺寸：MediaBox 右上-左下差值（页 2 为负偏移 MediaBox）
	p1, ok := doc.Pages["1"]
	if !ok || p1.Size == nil || p1.Size.Width != 612 || p1.Size.Height != 792 {
		t.Fatalf("page 1 = %+v, want 612x792", p1)
	}
	p2 := doc.Pages["2"]
	if p2.Size == nil || p2.Size.Width != 612 || p2.Size.Height != 792 {
		t.Fatalf("page 2 = %+v, want 612x792 (from negative-offset MediaBox)", p2)
	}

	// 树结构可被简化器消费：正文块携带祖先标题章节路径
	var body2Path string
	for _, it := range ToContentList(doc, SourceGolight) {
		if it.Text == "Body under details." {
			body2Path = strings.Join(it.SectionPath, ">")
		}
	}
	if body2Path != "Introduction>Details" {
		t.Fatalf("body2 section path = %q, want Introduction>Details", body2Path)
	}
}

// TestParsePDFBodyChunkSegments 单页正文超长按 knowledgePDFBodyChunkRunes 分段：
// 每段为独立 text 元素，prov bbox 为段内行并集——相邻两段的上/下边缘差恰为一行距。
func TestParsePDFBodyChunkSegments(t *testing.T) {
	const lineHeight = 20.0
	var sb strings.Builder
	for i, y := 0, 700.0; i < 60; i, y = i+1, y-lineHeight {
		// 每行 26 rune，60 行约 1.6k rune > 900 → 必然分段
		fmt.Fprintf(&sb, "BT /F1 10 Tf 72 %g Td (Body line padding text %03d) Tj ET\n", y, i)
	}
	data := buildTestPDF(t, []testPDFPage{{content: sb.String()}})
	doc, err := ParsePDF(data)
	if err != nil {
		t.Fatalf("ParsePDF: %v", err)
	}
	if len(doc.Texts) < 2 {
		t.Fatalf("expected long page split into >=2 texts, got %d", len(doc.Texts))
	}
	for i, tx := range doc.Texts {
		if tx.Label != LabelText {
			t.Fatalf("texts[%d] label = %v, want text", i, tx.Label)
		}
		if len(tx.Prov) != 1 {
			t.Fatalf("texts[%d] prov = %+v, want single prov", i, tx.Prov)
		}
		p := tx.Prov[0]
		if p.PageNo != 1 || p.BBox.L != 72 || !(p.BBox.T > p.BBox.B) || !(p.BBox.R > p.BBox.L) {
			t.Fatalf("texts[%d] prov = %+v, want page 1 non-degenerate bbox at x=72", i, p)
		}
		if p.CharSpan != [2]int64{0, int64(len([]rune(tx.Text)))} {
			t.Fatalf("texts[%d] charspan = %+v, want [0 %d]", i, p.CharSpan, len([]rune(tx.Text)))
		}
	}
	// 首段上边缘 = 首行基线
	if doc.Texts[0].Prov[0].BBox.T != 700 {
		t.Fatalf("first segment top = %v, want 700", doc.Texts[0].Prov[0].BBox.T)
	}
	// 相邻段边界：上一段下边缘与下一段上边缘差一行距（并集边界无重叠无缝隙）
	gap := doc.Texts[0].Prov[0].BBox.B - doc.Texts[1].Prov[0].BBox.T
	if math.Abs(gap-lineHeight) > 0.01 {
		t.Fatalf("segment gap = %v, want %v", gap, lineHeight)
	}
}

// TestParsePDFFallbackBlocks 扫描件兜底行（无坐标/字号信息）走正文通道：
// 产出无 prov 的 text 元素，且即使字号大也不参与标题判定。
func TestParsePDFFallbackBlocks(t *testing.T) {
	blocks := buildPDFBlocks([]pdfElement{
		{headingLevel: 1, line: pdfLine{Text: "Real Title", MaxFontSize: 16, PageIdx: 0, MinX: 70, MaxX: 200, MinY: 700, MaxY: 716}},
		{line: pdfLine{Text: "OCR fallback page one", PageIdx: 1, Fallback: true}},
		{line: pdfLine{Text: "big fake heading", MaxFontSize: 30, PageIdx: 2, Fallback: true}},
	})
	doc := NewDoclingDocument("pdf")
	buildPDFDoclingDocument(blocks, doc)
	if len(doc.Texts) != 3 {
		t.Fatalf("expected 3 texts (1 heading + 2 fallback blocks), got %d: %+v", len(doc.Texts), doc.Texts)
	}
	for i, want := range []string{"OCR fallback page one", "big fake heading"} {
		tx := doc.Texts[i+1]
		if tx.Label != LabelText || tx.Text != want {
			t.Fatalf("texts[%d] = %+v, want text %q", i+1, tx, want)
		}
		if len(tx.Prov) != 0 {
			t.Fatalf("texts[%d] prov = %+v, want empty (fallback has no coordinates)", i+1, tx.Prov)
		}
		if tx.Parent == nil || tx.Parent.String() != "#/texts/0" {
			t.Fatalf("texts[%d] parent = %+v, want #/texts/0", i+1, tx.Parent)
		}
	}
}

// TestParsePDFEmptyDocument 全文档无文本（扫描件无文字层）时返回错误且不产文档。
func TestParsePDFEmptyDocument(t *testing.T) {
	data := buildTestPDF(t, []testPDFPage{{content: ""}})
	doc, err := ParsePDF(data)
	if err == nil {
		t.Fatalf("expected error for text-free pdf, got doc %+v", doc)
	}
	if doc != nil {
		t.Fatalf("doc should be nil on error, got %+v", doc)
	}
}

// TestParsePDFPageHeaderFooter 验证页眉/页脚跨页指纹识别（规则 A1）：
// 同一归一化文本出现在全部 3 页顶部/底部区域的行为 page_header/page_footer，
// 页眉大字号不参与标题判定（不产生 section_header）、不进章节路径，且
// 节点挂 furniture 根并固定使用 furniture 内容层；
// 页数 <3 时不启用（两页文档同构重复文本不误判）；简化器透传 label。
func TestParsePDFPageHeaderFooter(t *testing.T) {
	pages := make([]testPDFPage, 0, 3)
	for i := 1; i <= 3; i++ {
		content := fmt.Sprintf("BT /F1 20 Tf 72 760 Td (Quarterly Protocol Header) Tj ET\n"+
			"BT /F1 10 Tf 72 600 Td (Body content of page %02d.) Tj ET\n"+
			"BT /F1 10 Tf 72 80 Td (Fixed Footer Notice) Tj ET\n", i)
		if i == 2 {
			// 页 2 追加一个真实标题（全文档最大字号在非首页，不触发封面判定）
			content += "BT /F1 24 Tf 72 700 Td (Overview) Tj ET\n"
		}
		pages = append(pages, testPDFPage{content: content})
	}
	doc, err := ParsePDF(buildTestPDF(t, pages))
	if err != nil {
		t.Fatalf("ParsePDF: %v", err)
	}
	if len(doc.Texts) != 10 {
		t.Fatalf("expected 10 texts (3 header + 3 body + 3 footer + 1 heading), got %d: %+v", len(doc.Texts), doc.Texts)
	}
	// 页眉页脚 label、挂 furniture、prov 定位页顶/页底
	for _, idx := range []int{0, 3, 7} {
		tx := doc.Texts[idx]
		if tx.Label != LabelPageHeader || tx.Text != "Quarterly Protocol Header" {
			t.Fatalf("texts[%d] = %+v, want page_header 'Quarterly Protocol Header'", idx, tx)
		}
		if tx.Parent == nil || tx.Parent.String() != "#/furniture" || tx.ContentLayer != LayerFurniture {
			t.Fatalf("texts[%d] parent/layer = %+v/%s, want furniture", idx, tx.Parent, tx.ContentLayer)
		}
	}
	for _, idx := range []int{2, 6, 9} {
		tx := doc.Texts[idx]
		if tx.Label != LabelPageFooter || tx.Text != "Fixed Footer Notice" {
			t.Fatalf("texts[%d] = %+v, want page_footer 'Fixed Footer Notice'", idx, tx)
		}
		if tx.Parent == nil || tx.Parent.String() != "#/furniture" || tx.ContentLayer != LayerFurniture {
			t.Fatalf("texts[%d] parent/layer = %+v/%s, want furniture", idx, tx.Parent, tx.ContentLayer)
		}
	}
	if p := doc.Texts[0].Prov; len(p) != 1 || p[0].PageNo != 1 || p[0].BBox.T != 760 {
		t.Fatalf("header prov = %+v, want page 1 top=760", doc.Texts[0].Prov)
	}
	if p := doc.Texts[2].Prov; len(p) != 1 || p[0].PageNo != 1 || p[0].BBox.B != 80 {
		t.Fatalf("footer prov = %+v, want page 1 bottom=80", doc.Texts[2].Prov)
	}
	// 页眉大字号（20pt > 正文 10pt+1.5）不参与标题判定：唯一 section_header 是 Overview
	for _, tx := range doc.Texts {
		if tx.Label != LabelSectionHeader {
			continue
		}
		if tx.Text != "Overview" || tx.TextLevel != 1 {
			t.Fatalf("unexpected section_header %+v, want Overview level 1 only", tx)
		}
	}
	// 页眉页脚不进章节路径：页 2 正文路径只有 Overview，且挂 Overview 下
	if doc.Texts[5].Parent == nil || doc.Texts[5].Parent.String() != "#/texts/4" {
		t.Fatalf("page2 body parent = %+v, want #/texts/4 (Overview)", doc.Texts[5].Parent)
	}
	var headerItemOK, footerItemOK bool
	var bodyPath string
	for _, it := range ToContentList(doc, SourceGolight) {
		switch it.Text {
		case "Quarterly Protocol Header":
			headerItemOK = it.Type == ItemTypeText && it.Label == "page_header"
		case "Fixed Footer Notice":
			footerItemOK = it.Type == ItemTypeText && it.Label == "page_footer"
		case "Body content of page 02.":
			bodyPath = strings.Join(it.SectionPath, ">")
		}
	}
	if !headerItemOK || !footerItemOK {
		t.Fatalf("page_header/page_footer label not passed through: header=%v footer=%v", headerItemOK, footerItemOK)
	}
	if bodyPath != "Overview" {
		t.Fatalf("page2 body section path = %q, want Overview (no header/footer)", bodyPath)
	}
}

// TestParsePDFTableMixedWithBody 验证坐标表格在正文流中的原位结构化：连续三行
// 三列生成一个 TableItem，被消费的单元格文字不再重复生成 TextItem，表格前后
// 正文仍按页面阅读顺序保留。
func TestParsePDFTableMixedWithBody(t *testing.T) {
	content := "BT /F1 10 Tf 50 740 Td (Report introduction.) Tj ET\n" +
		"BT /F1 10 Tf 50 700 Td (Name) Tj ET\n" +
		"BT /F1 10 Tf 200 700 Td (Region) Tj ET\n" +
		"BT /F1 10 Tf 350 700 Td (Score) Tj ET\n" +
		"BT /F1 10 Tf 50 680 Td (Alice) Tj ET\n" +
		"BT /F1 10 Tf 200 680 Td (East) Tj ET\n" +
		"BT /F1 10 Tf 350 680 Td (95) Tj ET\n" +
		"BT /F1 10 Tf 50 660 Td (Bob) Tj ET\n" +
		"BT /F1 10 Tf 200 660 Td (West) Tj ET\n" +
		"BT /F1 10 Tf 350 660 Td (88) Tj ET\n" +
		"BT /F1 10 Tf 50 620 Td (Report conclusion.) Tj ET\n"
	doc, err := ParsePDF(buildTestPDF(t, []testPDFPage{{content: content}}))
	if err != nil {
		t.Fatalf("ParsePDF: %v", err)
	}
	if len(doc.Tables) != 1 || doc.Tables[0].Data == nil {
		t.Fatalf("tables = %+v, want one structured table", doc.Tables)
	}
	data := doc.Tables[0].Data
	if data.NumRows != 3 || data.NumCols != 3 || len(data.TableCells) != 9 {
		t.Fatalf("table data = %+v, want 3x3 with nine cells", data)
	}
	if len(doc.Tables[0].Prov) != 1 || doc.Tables[0].Prov[0].BBox == nil ||
		doc.Tables[0].Prov[0].BBox.CoordOrigin != CoordOriginBottomLeft {
		t.Fatalf("table prov = %+v, want BOTTOMLEFT bbox", doc.Tables[0].Prov)
	}
	for _, text := range doc.Texts {
		if strings.Contains(text.Text, "Alice") || strings.Contains(text.Text, "Score") {
			t.Fatalf("table text duplicated in text item: %+v", text)
		}
	}
	items := ToContentList(doc, SourceGolight)
	if len(items) != 3 || items[0].Text != "Report introduction." ||
		items[1].Type != ItemTypeTable || !strings.Contains(items[1].TableBody, "Alice") ||
		items[2].Text != "Report conclusion." {
		t.Fatalf("content list order/data = %+v, want body-table-body", items)
	}
}

// TestParsePDFDocumentIndexTOC 验证目录区域识别（规则 A2）：
// 连续点线+页码行构成目录区域，收进"目次"分组且逐行 label=document_index；
// 目录行字号超正文也不参与标题判定（不产生 section_header）、不进章节路径。
func TestParsePDFDocumentIndexTOC(t *testing.T) {
	var sb strings.Builder
	// 页 0：15 行普通正文（首页行数达到封面判定阈值之上，避免误触封面规则）
	for i := 1; i <= 15; i++ {
		fmt.Fprintf(&sb, "BT /F1 10 Tf 72 %g Td (Filler body line number %02d here) Tj ET\n", float64(720-18*(i-1)), i)
	}
	page0 := sb.String()
	// 页 1：4 行目录点线行（14pt，超正文 1.5pt 以上）+ 1 行正文
	sb.Reset()
	for i, e := range []struct{ title, page string }{
		{"ChapterOne", "5"}, {"ChapterTwo", "12"}, {"ChapterThree", "20"}, {"Appendix", "30"},
	} {
		fmt.Fprintf(&sb, "BT /F1 14 Tf 72 %g Td (%s............%s) Tj ET\n", float64(700-20*i), e.title, e.page)
	}
	sb.WriteString("BT /F1 10 Tf 72 600 Td (Body after toc region.) Tj ET\n")
	doc, err := ParsePDF(buildTestPDF(t, []testPDFPage{{content: page0}, {content: sb.String()}}))
	if err != nil {
		t.Fatalf("ParsePDF: %v", err)
	}
	if len(doc.Texts) != 6 {
		t.Fatalf("expected 6 texts (1 body block + 4 toc + 1 body), got %d: %+v", len(doc.Texts), doc.Texts)
	}
	// 目录行逐行 document_index，挂"目次"分组下
	for i := 1; i <= 4; i++ {
		tx := doc.Texts[i]
		if tx.Label != LabelDocumentIndex {
			t.Fatalf("texts[%d] label = %v, want document_index", i, tx.Label)
		}
		if tx.Parent == nil || tx.Parent.Kind != refGroups {
			t.Fatalf("texts[%d] parent = %+v, want toc group", i, tx.Parent)
		}
	}
	g := doc.Groups[doc.Texts[1].Parent.Idx]
	if g.Name != "目次" || g.Label != GroupLabelSection || len(g.Children) != 4 {
		t.Fatalf("toc group = %+v, want 目次/section with 4 children", g)
	}
	// 目录行不产生 section_header（14pt 超正文但被排除标题判定）
	for i, tx := range doc.Texts {
		if tx.Label == LabelSectionHeader {
			t.Fatalf("texts[%d] unexpected section_header %+v", i, tx)
		}
	}
	// 简化器产物：目录行归通用 text 且 label 透传，章节路径为空（组名不入路径）
	var tocItems int
	for _, it := range ToContentList(doc, SourceGolight) {
		if it.Label != "document_index" {
			continue
		}
		tocItems++
		if it.Type != ItemTypeText || len(it.SectionPath) != 0 {
			t.Fatalf("toc item = %+v, want type=text empty section path", it)
		}
	}
	if tocItems != 4 {
		t.Fatalf("expected 4 document_index items, got %d", tocItems)
	}
}

// TestParsePDFCoverTitle 验证封面主标题识别（规则 A3）：
// 首页行数受限且存在全文档最大字号短行时判为 title 挂 body；
// 封面其余大字行（版本号/日期）降为普通 text，不再挤占标题层级；
// 后续页面标题正常走字号启发式；title 按现状入章节路径栈。
func TestParsePDFCoverTitle(t *testing.T) {
	data := buildTestPDF(t, []testPDFPage{
		{
			content: "BT /F1 26 Tf 72 720 Td (My Great Protocol Title) Tj ET\n" +
				"BT /F1 20 Tf 72 680 Td (Version: V1.3) Tj ET\n" +
				"BT /F1 18 Tf 72 640 Td (2025 June) Tj ET\n" +
				"BT /F1 10 Tf 72 600 Td (Company Name) Tj ET\n",
		},
		{
			content: "BT /F1 18 Tf 72 700 Td (Chapter One) Tj ET\n" +
				"BT /F1 10 Tf 72 640 Td (Body under chapter one.) Tj ET\n" +
				"BT /F1 10 Tf 72 620 Td (More body line A.) Tj ET\n" +
				"BT /F1 10 Tf 72 600 Td (More body line B.) Tj ET\n",
		},
		{
			content: "BT /F1 10 Tf 72 600 Td (Tail body page.) Tj ET\n",
		},
	})
	doc, err := ParsePDF(data)
	if err != nil {
		t.Fatalf("ParsePDF: %v", err)
	}
	if len(doc.Texts) != 5 {
		t.Fatalf("expected 5 texts (title + cover body + 1 heading + 2 body), got %d: %+v", len(doc.Texts), doc.Texts)
	}
	title := doc.Texts[0]
	if title.Label != LabelTitle || title.Text != "My Great Protocol Title" {
		t.Fatalf("texts[0] = %+v, want title 'My Great Protocol Title'", title)
	}
	if title.Parent == nil || title.Parent.String() != "#/body" {
		t.Fatalf("title parent = %+v, want #/body", title.Parent)
	}
	if p := title.Prov; len(p) != 1 || p[0].PageNo != 1 || p[0].BBox.T != 720 {
		t.Fatalf("title prov = %+v, want page 1 top=720", p)
	}
	// 封面其余大字行降为普通 text，与 CompanyName 同页聚合成一个正文块
	cover := doc.Texts[1]
	if cover.Label != LabelText || cover.Text != "Version: V1.3\n2025 June\nCompany Name" {
		t.Fatalf("texts[1] = %+v, want cover lines demoted to one text block", cover)
	}
	// 后续页面标题正常：ChapterOne level 1 挂 body（26pt title 不挤占层级）
	h := doc.Texts[2]
	if h.Label != LabelSectionHeader || h.TextLevel != 1 || h.Text != "Chapter One" {
		t.Fatalf("texts[2] = %+v, want 'Chapter One' level 1", h)
	}
	if h.Parent == nil || h.Parent.String() != "#/body" {
		t.Fatalf("heading parent = %+v, want #/body", h.Parent)
	}
	if doc.Texts[3].Parent == nil || doc.Texts[3].Parent.String() != "#/texts/2" {
		t.Fatalf("page2 body parent = %+v, want #/texts/2", doc.Texts[3].Parent)
	}
	// title 按简化器现状入栈：title 自身路径含自身；封面正文与后续章节
	// 的路径由树祖先链推导（title 是 body 直接子节点，不在其祖先链上，
	// 后续内容仅携带章节标题链）
	var titlePath, coverPath, tailPath string
	for _, it := range ToContentList(doc, SourceGolight) {
		switch {
		case it.Text == "My Great Protocol Title":
			titlePath = strings.Join(it.SectionPath, ">")
		case strings.HasPrefix(it.Text, "Version: V1.3"):
			coverPath = strings.Join(it.SectionPath, ">")
		case it.Text == "Tail body page.":
			tailPath = strings.Join(it.SectionPath, ">")
		}
	}
	if titlePath != "My Great Protocol Title" {
		t.Fatalf("title path = %q, want My Great Protocol Title (title kept in path stack)", titlePath)
	}
	if coverPath != "" {
		t.Fatalf("cover line path = %q, want empty (title not an ancestor of body siblings)", coverPath)
	}
	if tailPath != "Chapter One" {
		t.Fatalf("tail body path = %q, want 'Chapter One'", tailPath)
	}
}

// TestTextGarbageRatio 验证乱码率统计（规则 A4）：U+FFFD 与私用区字符
// 占非空白 rune 比例，空白不计入，纯正常文本为 0。
func TestTextGarbageRatio(t *testing.T) {
	cases := []struct {
		name string
		text string
		want float64
	}{
		{"纯中文", "设备管理系统技术协议", 0},
		{"纯英文", "plain ascii text", 0},
		{"空文本", "", 0},
		{"全空白", " \t\n\u3000", 0},
		{"三成替换符", "\uFFFD\uFFFD\uFFFD设备管理abc", 0.3},
		{"私用区计入", "\uE000\uE001设备", 0.5},
		{"私用区边界外不计入", "\uF900\uFFFE设备", 0},
	}
	for _, tc := range cases {
		if got := textGarbageRatio(tc.text); math.Abs(got-tc.want) > 1e-9 {
			t.Fatalf("%s: textGarbageRatio = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestContentListLabelPassthrough 验证简化器细粒度 label 透传口径（规则 A5）：
// page_header/page_footer/document_index/list_item/code/checkbox/caption
// 填充 Item.Label；formula 由 type=equation 表达、text/paragraph 不填；
// label 缺省时 JSON 序列化不出现在产物（向后兼容）。
func TestContentListLabelPassthrough(t *testing.T) {
	doc := NewDoclingDocument("test")
	doc.AddText(LabelPageHeader, "h", nil, nil)
	doc.AddText(LabelPageFooter, "f", nil, nil)
	doc.AddText(LabelDocumentIndex, "idx", nil, nil)
	list := doc.AddListGroup("list", nil)
	doc.AddListItem(list, "item", false, "·", nil)
	doc.AddCode("code", "go", nil, nil)
	doc.AddCheckbox(true, "cb", nil, nil)
	doc.AddText(LabelCaption, "cap", nil, nil)
	doc.AddFormula("x=1", nil, nil)
	doc.AddText(LabelParagraph, "para", nil, nil)
	doc.AddText(LabelText, "plain", nil, nil)
	got := map[string]Item{}
	for _, it := range ToContentList(doc, SourceGolight) {
		got[it.Text] = it
	}
	checks := []struct {
		text      string
		wantLabel string
		wantType  ItemType
	}{
		{"h", "page_header", ItemTypeText},
		{"f", "page_footer", ItemTypeText},
		{"idx", "document_index", ItemTypeText},
		{"item", "list_item", ItemTypeText},
		{"```go\ncode\n```", "code", ItemTypeText},
		{"cb", "checkbox_selected", ItemTypeText},
		{"cap", "caption", ItemTypeText},
		{"x=1", "", ItemTypeEquation},
		{"para", "", ItemTypeText},
		{"plain", "", ItemTypeText},
	}
	for _, c := range checks {
		it, ok := got[c.text]
		if !ok {
			t.Fatalf("missing item %q in %+v", c.text, got)
		}
		if it.Label != c.wantLabel || it.Type != c.wantType {
			t.Fatalf("item %q = {type:%s label:%q}, want type=%s label=%q", c.text, it.Type, it.Label, c.wantType, c.wantLabel)
		}
	}
	// JSON 向后兼容：label 缺省不序列化
	b, err := json.Marshal(Item{Type: ItemTypeText, Text: "x"})
	if err != nil {
		t.Fatalf("marshal item: %v", err)
	}
	if strings.Contains(string(b), "label") {
		t.Fatalf("empty label should be omitted, got %s", b)
	}
}
