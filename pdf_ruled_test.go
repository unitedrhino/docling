// pdf_ruled_test.go 验证矢量框线表格还原：内容流图形提取（CTM/填充背景/
// 裁剪/斜线过滤）、含合并单元格的网格还原（纵向合并、区域内部伪边界坍缩、
// 列表头标记）与端到端解析（表格独立成 TableItem、表外文字不消费）。
package docling

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ledongthuc/pdf"
)

// pdfRuledTestWord 构造一个词盒（y 为基线，MinY=MaxY=y）。
func pdfRuledTestWord(text string, minX, maxX, y float64) pdfWord {
	return pdfWord{Text: text, MinX: minX, MaxX: maxX, MinY: y, MaxY: y, FontSize: 12}
}

// pdfRuledTestLine 把词聚成一行并累计行 bbox。
func pdfRuledTestLine(pageIdx int64, words ...pdfWord) pdfLine {
	line := pdfLine{PageIdx: pageIdx, Words: words, MaxFontSize: 12}
	for i, word := range words {
		if i > 0 {
			line.Text += " "
		}
		line.Text += word.Text
		if i == 0 {
			line.MinX, line.MaxX = word.MinX, word.MaxX
			line.MinY, line.MaxY = word.MinY, word.MaxY
			continue
		}
		if word.MinX < line.MinX {
			line.MinX = word.MinX
		}
		if word.MaxX > line.MaxX {
			line.MaxX = word.MaxX
		}
		if word.MinY < line.MinY {
			line.MinY = word.MinY
		}
		if word.MaxY > line.MaxY {
			line.MaxY = word.MaxY
		}
	}
	return line
}

// pdfRuledTestEdges 构造框线段集合：hs 为 (y, from, to)，vs 为 (x, from, to)。
func pdfRuledTestEdges(hs [][3]float64, vs [][3]float64) []pdfRuleEdge {
	edges := make([]pdfRuleEdge, 0, len(hs)+len(vs))
	for _, h := range hs {
		edges = append(edges, pdfRuleEdge{Horizontal: true, Pos: h[0], From: h[1], To: h[2]})
	}
	for _, v := range vs {
		edges = append(edges, pdfRuleEdge{Horizontal: false, Pos: v[0], From: v[1], To: v[2]})
	}
	return edges
}

// TestExtractPDFPageRuleEdges 验证内容流图形提取：Y 翻转 CTM、填充矩形
// 展开为四边、整页背景底色过滤、裁剪路径（re W n）不产线段、斜线被丢弃。
func TestExtractPDFPageRuleEdges(t *testing.T) {
	content := "q 1 0 0 -1 0 792 cm\n" +
		"0 0 612 792 re f\n" + // 整页背景：必须过滤
		"40 640 260 60 re f\n" + // 单元格底色：展开为 4 条边
		"Q\n" +
		"0 0 612 792 re W n\n" + // 裁剪路径：不产线段
		"100 100 m 200 100 l S\n" +
		"40 700 m 300 700 l S\n" +
		"40 700 m 40 760 l S\n" +
		"0 0 m 50 50 l S\n" // 斜线：丢弃
	page := struct{}{}
	_ = page
	data := buildTestPDF(t, []testPDFPage{{content: content}})
	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open pdf: %v", err)
	}
	docPage := reader.Page(1)
	geometry := pdfPageGeometryForPage(docPage)
	edges := extractPDFPageRuleEdges(docPage, geometry)

	findEdge := func(horizontal bool, pos, from, to float64) bool {
		for _, edge := range edges {
			if edge.Horizontal == horizontal &&
				absDiff(edge.Pos, pos) <= pdfRuleAxisTolerancePt &&
				absDiff(edge.From, from) <= pdfRuleAxisTolerancePt &&
				absDiff(edge.To, to) <= pdfRuleAxisTolerancePt {
				return true
			}
		}
		return false
	}
	// 单元格底色矩形（Y 翻转后显示坐标 y ∈ [92,152]）
	if !findEdge(true, 92, 40, 300) || !findEdge(true, 152, 40, 300) ||
		!findEdge(false, 40, 92, 152) || !findEdge(false, 300, 92, 152) {
		t.Fatalf("cell rect edges missing: %+v", edges)
	}
	if !findEdge(true, 100, 100, 200) { // q/Q 之外的描边是原始坐标
		t.Fatalf("stroked horizontal edge missing: %+v", edges)
	}
	if !findEdge(false, 40, 700, 760) {
		t.Fatalf("stroked vertical edge missing: %+v", edges)
	}
	for _, edge := range edges {
		if edge.Horizontal && edge.To-edge.From >= 600 {
			t.Fatalf("page background fill leaked an edge: %+v", edge)
		}
	}
}

func absDiff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

// TestDetectPDFRuledTablesMergedCells 验证 Word 式表格还原：左列纵向合并、
// 合并区内部的填空下划线伪边界坍缩、首行列表头标记、消费行从正文移除。
func TestDetectPDFRuledTablesMergedCells(t *testing.T) {
	// 3 列 × 3 行：首行表头；数据带左列纵向合并（y=150 的横线不跨左列），
	// 合并区内 y=120 有一条 x∈[60,90] 的填空下划线（伪边界）
	edges := pdfRuledTestEdges(
		[][3]float64{
			{100, 50, 350},  // 底边
			{150, 150, 350}, // 数据带内横线（不跨左列 → 左列纵向合并）
			{120, 60, 90},   // 左列合并区内的下划线
			{200, 50, 350},  // 表头下横线
			{250, 50, 350},  // 顶边
		},
		[][3]float64{
			{50, 100, 250},
			{150, 100, 250},
			{250, 100, 250},
			{350, 100, 250},
		},
	)
	lines := []pdfLine{
		pdfRuledTestLine(0, pdfRuledTestWord("甲", 60, 80, 225)),
		pdfRuledTestLine(0, pdfRuledTestWord("乙", 160, 180, 225)),
		pdfRuledTestLine(0, pdfRuledTestWord("丙", 260, 280, 225)),
		pdfRuledTestLine(0, pdfRuledTestWord("1", 160, 170, 175)),
		pdfRuledTestLine(0, pdfRuledTestWord("2", 160, 170, 125)),
		pdfRuledTestLine(0, pdfRuledTestWord("3", 260, 270, 175)),
		pdfRuledTestLine(0, pdfRuledTestWord("4", 260, 270, 125)),
	}
	tables, consumed := detectPDFRuledTables(lines, map[int64][]pdfRuleEdge{0: edges})
	if len(tables) != 1 {
		t.Fatalf("tables=%d", len(tables))
	}
	table := tables[0]
	if table.Data.NumRows != 3 || table.Data.NumCols != 3 {
		t.Fatalf("grid=%dx%d", table.Data.NumRows, table.Data.NumCols)
	}
	if len(table.Data.TableCells) != 8 { // 3 表头 + 1 左列合并 + 2×2 数据
		t.Fatalf("cells=%d: %+v", len(table.Data.TableCells), table.Data.TableCells)
	}
	var tall *DoclingTableCell
	for i := range table.Data.TableCells {
		cell := &table.Data.TableCells[i]
		if cell.StartColOffsetIdx == 0 && cell.EndRowOffsetIdx-cell.StartRowOffsetIdx > 1 {
			tall = cell
		}
	}
	if tall == nil || tall.StartRowOffsetIdx != 1 || tall.EndRowOffsetIdx != 3 {
		t.Fatalf("vertical merged cell wrong: %+v", tall)
	}
	// 伪边界坍缩后行高应跨满合并区（下划线不再细分）
	if tall.BBox.T-tall.BBox.B < 95 {
		t.Fatalf("merged cell bbox shrunk: %+v", tall.BBox)
	}
	for i := 0; i < 3; i++ {
		if !table.Data.TableCells[i].ColumnHeader {
			t.Fatalf("header not marked: %+v", table.Data.TableCells[i])
		}
	}
	for index := range lines {
		if !consumed[index] {
			t.Fatalf("table line %d not consumed: %+v", index, consumed)
		}
	}
	// 全部行都被表格消费
	for _, line := range lines {
		found := false
		for _, used := range table.Lines {
			if used.Text == line.Text {
				found = true
			}
		}
		if !found {
			t.Fatalf("line %q not consumed into table", line.Text)
		}
	}
}

// TestParsePDFRuledTableEndToEnd 验证端到端：带框线的文本型 PDF 解析出
// 独立 TableItem，表格外标题行保留在正文，Markdown 输出 GFM 表格。
func TestParsePDFRuledTableEndToEnd(t *testing.T) {
	content := "BT /F1 12 Tf 60 760 Td (Table fruit) Tj ET\n" +
		"BT /F1 12 Tf 60 700 Td (Name) Tj ET\n" +
		"BT /F1 12 Tf 200 700 Td (Qty) Tj ET\n" +
		"BT /F1 12 Tf 60 660 Td (apple) Tj ET\n" +
		"BT /F1 12 Tf 200 660 Td (5) Tj ET\n" +
		"40 640 m 320 640 l S\n" +
		"40 680 m 320 680 l S\n" +
		"40 730 m 320 730 l S\n" +
		"40 640 m 40 730 l S\n" +
		"180 640 m 180 730 l S\n" +
		"320 640 m 320 730 l S\n" +
		"60 655 m 100 655 l S\n" // 单元格内填空下划线：伪边界
	data := buildTestPDF(t, []testPDFPage{{content: content}})
	doc, err := ParsePDFWithOptions(data, PDFOptions{DisablePopplerFallback: true})
	if err != nil {
		t.Fatalf("ParsePDFWithOptions: %v", err)
	}
	if len(doc.Tables) != 1 {
		t.Fatalf("tables=%d", len(doc.Tables))
	}
	table := doc.Tables[0]
	if table.Data == nil || table.Data.NumRows != 2 || table.Data.NumCols != 2 || len(table.Data.TableCells) != 4 {
		t.Fatalf("table=%+v", table.Data)
	}
	texts := map[string]bool{}
	for _, cell := range table.Data.TableCells {
		texts[cell.Text] = true
		if strings.Contains(cell.Text, "Table") {
			t.Fatalf("caption leaked into table: %+v", cell)
		}
	}
	for _, want := range []string{"Name", "Qty", "apple", "5"} {
		if !texts[want] {
			t.Fatalf("cell %q missing: %+v", want, table.Data.TableCells)
		}
	}
	if !table.Data.TableCells[0].ColumnHeader || !table.Data.TableCells[1].ColumnHeader {
		t.Fatalf("header not marked: %+v", table.Data.TableCells)
	}
	if !strings.Contains(doc.Text(), "Table fruit") {
		t.Fatalf("caption lost from body: %q", doc.Text())
	}
	markdown := doc.ToMarkdown()
	for _, want := range []string{"| Name | Qty |", "| apple | 5 |"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("markdown missing %q:\n%s", want, markdown)
		}
	}
}

// TestDetectPDFRuledTablesRejectsOpenGraphics 验证非闭合图形（标题下划线、
// 散乱线段）不会被当成表格。
func TestDetectPDFRuledTablesRejectsOpenGraphics(t *testing.T) {
	edges := pdfRuledTestEdges(
		[][3]float64{{700, 50, 300}, {698, 50, 300}},
		[][3]float64{{50, 698, 700}},
	)
	lines := []pdfLine{pdfRuledTestLine(0, pdfRuledTestWord("标题", 60, 120, 705))}
	tables, consumed := detectPDFRuledTables(lines, map[int64][]pdfRuleEdge{0: edges})
	if len(tables) != 0 {
		t.Fatalf("open graphics detected as table: %+v", tables)
	}
	if consumed != nil {
		t.Fatalf("consumed should be nil: %+v", consumed)
	}
}
