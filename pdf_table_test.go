// pdf_table_test.go 验证 PDF 坐标表格识别的网格恢复、正文去重和保守误判边界。
package docling

import (
	"strings"
	"testing"
)

// TestDetectPDFTables 验证稳定列锚点可恢复 3x3 表格，并从正文流移除表格行。
func TestDetectPDFTables(t *testing.T) {
	lines := []pdfLine{
		pdfTableTestLine(0, 700, "姓名", 50, 78, "部门", 160, 188, "分数", 270, 298),
		pdfTableTestLine(0, 680, "张三", 50, 78, "研发", 160, 188, "95", 270, 284),
		pdfTableTestLine(0, 660, "李四", 50, 78, "产品", 160, 188, "88", 270, 284),
		pdfTableTestLine(0, 630, "表格后的正文", 50, 130),
	}

	tables, remaining := detectPDFTables(lines)
	if len(tables) != 1 {
		t.Fatalf("期望识别一个表格，实际 %+v", tables)
	}
	table := tables[0]
	if table.PageIdx != 0 || table.Data.NumRows != 3 || table.Data.NumCols != 3 {
		t.Fatalf("表格尺寸或页号错误: %+v", table)
	}
	if len(table.Data.TableCells) != 9 {
		t.Fatalf("表格单元格数量错误: %+v", table.Data.TableCells)
	}
	if got := table.Data.TableCells[4].Text; got != "研发" {
		t.Fatalf("网格顺序错误: %q", got)
	}
	if !table.Data.TableCells[0].ColumnHeader || table.Data.TableCells[3].ColumnHeader {
		t.Fatalf("首行表头推断错误: %+v", table.Data.TableCells)
	}
	if table.BBox == nil || table.BBox.L != 50 || table.BBox.R != 298 || table.BBox.T != 710 || table.BBox.B != 657 ||
		table.BBox.CoordOrigin != CoordOriginBottomLeft {
		t.Fatalf("表格 bbox 并集错误: %+v", table.BBox)
	}
	if len(remaining) != 1 || remaining[0].Text != "表格后的正文" {
		t.Fatalf("表格行未从正文流准确移除: %+v", remaining)
	}
}

// TestDetectPDFTablesTwoByTwo 验证最小的两行两列表格也能被识别。
func TestDetectPDFTablesTwoByTwo(t *testing.T) {
	lines := []pdfLine{
		pdfTableTestLine(0, 500, "项目", 45, 75, "数量", 180, 210),
		pdfTableTestLine(0, 480, "设备", 45, 75, "12", 180, 194),
	}

	tables, remaining := detectPDFTables(lines)
	if len(tables) != 1 || tables[0].Data.NumRows != 2 || tables[0].Data.NumCols != 2 {
		t.Fatalf("未识别最小 2x2 表格: tables=%+v remaining=%+v", tables, remaining)
	}
	if len(remaining) != 0 {
		t.Fatalf("表格行不应残留正文: %+v", remaining)
	}
}

// TestDetectPDFTablesKeepsPageOrder 验证跨页识别不会打乱剩余正文与表格的页内位置。
func TestDetectPDFTablesKeepsPageOrder(t *testing.T) {
	lines := []pdfLine{
		pdfTableTestLine(0, 700, "第一页正文", 50, 120),
		pdfTableTestLine(1, 700, "键", 50, 62, "值", 170, 182),
		pdfTableTestLine(1, 680, "温度", 50, 78, "25", 170, 184),
		pdfTableTestLine(1, 650, "第二页正文", 50, 120),
	}

	tables, remaining := detectPDFTables(lines)
	if len(tables) != 1 || tables[0].PageIdx != 1 || tables[0].StartLine != 1 || tables[0].EndLine != 3 {
		t.Fatalf("表格来源位置错误: %+v", tables)
	}
	if len(remaining) != 2 || remaining[0].Text != "第一页正文" || remaining[1].Text != "第二页正文" {
		t.Fatalf("剩余正文顺序错误: %+v", remaining)
	}
}

// TestDetectPDFTablesRejectsUnstableAnchors 验证列起点漂移明显时不强行生成表格。
func TestDetectPDFTablesRejectsUnstableAnchors(t *testing.T) {
	lines := []pdfLine{
		pdfTableTestLine(0, 700, "A", 40, 48, "B", 160, 168),
		pdfTableTestLine(0, 680, "C", 82, 90, "D", 230, 238),
		pdfTableTestLine(0, 660, "E", 30, 38, "F", 120, 128),
	}

	tables, remaining := detectPDFTables(lines)
	if len(tables) != 0 || len(remaining) != len(lines) {
		t.Fatalf("漂移列不应识别为表格: tables=%+v remaining=%+v", tables, remaining)
	}
}

// TestDetectPDFTablesRejectsTwoColumnProse 验证普通双栏长正文不会因栏起点稳定而误判表格。
func TestDetectPDFTablesRejectsTwoColumnProse(t *testing.T) {
	leftA := "这是左栏的一段普通正文内容，用来验证双栏版面不会被错误识别为数据表格"
	rightA := "这是右栏的另一段连续正文内容，其语义与左栏内容彼此独立而且长度较长"
	leftB := "左栏下一行仍然是自然语言段落，不具备单元格所要求的紧凑数据特征"
	rightB := "右栏下一行同样只是排版后的正文，不应该进入结构化表格集合"
	lines := []pdfLine{
		pdfTableTestLine(0, 700, leftA, 40, 250, rightA, 330, 540),
		pdfTableTestLine(0, 680, leftB, 40, 250, rightB, 330, 540),
	}

	tables, remaining := detectPDFTables(lines)
	if len(tables) != 0 || len(remaining) != 2 {
		t.Fatalf("双栏正文被误判为表格: tables=%+v remaining=%+v", tables, remaining)
	}
}

// TestDetectPDFTablesRejectsNumberedList 验证序号与短文本形成的两段式列表不误判表格。
func TestDetectPDFTablesRejectsNumberedList(t *testing.T) {
	lines := []pdfLine{
		pdfTableTestLine(0, 700, "1.", 40, 48, "准备材料", 66, 112),
		pdfTableTestLine(0, 680, "2.", 40, 48, "提交申请", 66, 112),
		pdfTableTestLine(0, 660, "3.", 40, 48, "等待审核", 66, 112),
	}

	tables, remaining := detectPDFTables(lines)
	if len(tables) != 0 || len(remaining) != len(lines) {
		t.Fatalf("编号列表被误判为表格: tables=%+v remaining=%+v", tables, remaining)
	}
}

// TestDetectPDFTablesMergesWordsInCell 验证单元格内普通词间距不会被拆成额外列。
func TestDetectPDFTablesMergesWordsInCell(t *testing.T) {
	lines := []pdfLine{
		pdfTableTestLine(0, 700, "Full", 40, 62, "Name", 66, 90, "Score", 190, 222),
		pdfTableTestLine(0, 680, "Alice", 40, 68, "Smith", 72, 100, "90", 190, 204),
	}

	tables, _ := detectPDFTables(lines)
	if len(tables) != 1 || tables[0].Data.NumCols != 2 {
		t.Fatalf("单元格内词语被错误拆列: %+v", tables)
	}
	if got := tables[0].Data.TableCells[0].Text; got != "Full Name" {
		t.Fatalf("单元格词语合并错误: %q", got)
	}
}

// TestDetectPDFSparseTablesAllowsMissingCells 验证无边框表格中个别空单元格
// 不会导致整张表退化为正文。
func TestDetectPDFSparseTablesAllowsMissingCells(t *testing.T) {
	lines := []pdfLine{
		pdfTableTestLine(0, 700, "姓名", 40, 68, "部门", 160, 188, "分数", 280, 308),
		pdfTableTestLine(0, 680, "张三", 40, 68, "95", 280, 294),
		pdfTableTestLine(0, 660, "李四", 40, 68, "产品", 160, 188, "88", 280, 294),
	}
	if tables, _ := detectPDFTables(lines); len(tables) != 0 {
		t.Fatalf("strict detector should reject sparse rows: %+v", tables)
	}
	tables, remaining := detectPDFSparseTables(lines)
	if len(tables) != 1 || len(remaining) != 0 || tables[0].Data.NumRows != 3 || tables[0].Data.NumCols != 3 || len(tables[0].Data.TableCells) != 8 {
		t.Fatalf("sparse table recovery wrong: tables=%+v remaining=%+v", tables, remaining)
	}
	if prepared := preparePDFLinesForDocument(lines, nil); len(prepared) != 1 || prepared[0].Table == nil {
		t.Fatalf("sparse table not integrated into PDF flow: %+v", prepared)
	}
}

// TestDetectPDFSparseTablesRejectsThreeColumnProse 验证稀疏补充路径仍不接受
// 每个单元格都是长自然语言的三栏正文。
func TestDetectPDFSparseTablesRejectsThreeColumnProse(t *testing.T) {
	long := "这是一段较长的自然语言正文内容，不是紧凑的数据单元格"
	lines := []pdfLine{
		pdfTableTestLine(0, 700, long, 40, 180, long, 230, 370, long, 420, 560),
		pdfTableTestLine(0, 680, long, 40, 180, long, 230, 370),
		pdfTableTestLine(0, 660, long, 40, 180, long, 230, 370, long, 420, 560),
	}
	if tables, remaining := detectPDFSparseTables(lines); len(tables) != 0 || len(remaining) != len(lines) {
		t.Fatalf("three-column prose misdetected: tables=%+v remaining=%+v", tables, remaining)
	}
}

// TestMergePDFContinuationTablesJoinsRepeatedHeader 验证跨页续表在列锚和
// 重复表头一致时合并为一个 TableItem，并保留两页来源框。
func TestMergePDFContinuationTablesJoinsRepeatedHeader(t *testing.T) {
	lines := []pdfLine{
		pdfTableTestLine(0, 125, "名称", 40, 70, "数值", 180, 210),
		pdfTableTestLine(0, 105, "甲", 40, 54, "1", 180, 188),
		pdfTableTestLine(1, 745, "名称", 42, 72, "数值", 182, 212),
		pdfTableTestLine(1, 725, "乙", 42, 56, "2", 182, 190),
	}
	prepared := preparePDFLinesForDocument(lines, nil)
	pages := map[string]PageItem{
		"1": {PageNo: 1, Size: &ImageSize{Width: 600, Height: 800}},
		"2": {PageNo: 2, Size: &ImageSize{Width: 600, Height: 800}},
	}
	merged := mergePDFContinuationTables(prepared, pages)
	if len(merged) != 1 || merged[0].Table == nil {
		t.Fatalf("continuation result=%+v", merged)
	}
	table := merged[0].Table
	if table.Data.NumRows != 3 || table.Data.NumCols != 2 || len(table.Data.TableCells) != 6 {
		t.Fatalf("merged table=%+v", table.Data)
	}
	if table.Data.TableCells[4].Text != "乙" || table.Data.TableCells[4].StartRowOffsetIdx != 2 {
		t.Fatalf("second page row not shifted: %+v", table.Data.TableCells)
	}

	doc := NewDoclingDocument("continuation")
	doc.Pages = pages
	buildPDFDoclingDocument(buildPDFBlocks(buildPDFElements(merged)), doc)
	if len(doc.Tables) != 1 || len(doc.Tables[0].Prov) != 2 ||
		doc.Tables[0].Prov[0].PageNo != 1 || doc.Tables[0].Prov[1].PageNo != 2 {
		t.Fatalf("table provenance=%+v", doc.Tables)
	}
}

// TestMergePDFContinuationTablesRejectsIndependentTables 验证未贴近翻页边缘的
// 两张独立同构表格不会仅因表头相同被误合并。
func TestMergePDFContinuationTablesRejectsIndependentTables(t *testing.T) {
	lines := []pdfLine{
		pdfTableTestLine(0, 500, "名称", 40, 70, "数值", 180, 210),
		pdfTableTestLine(0, 480, "甲", 40, 54, "1", 180, 188),
		pdfTableTestLine(1, 500, "名称", 40, 70, "数值", 180, 210),
		pdfTableTestLine(1, 480, "乙", 40, 54, "2", 180, 188),
	}
	prepared := preparePDFLinesForDocument(lines, nil)
	pages := map[string]PageItem{
		"1": {PageNo: 1, Size: &ImageSize{Width: 600, Height: 800}},
		"2": {PageNo: 2, Size: &ImageSize{Width: 600, Height: 800}},
	}
	if merged := mergePDFContinuationTables(prepared, pages); len(merged) != 2 {
		t.Fatalf("independent tables merged: %+v", merged)
	}
}

// pdfTableTestLine 构造一行 PDF 坐标词；每三个参数依次为文本与左右边界。
func pdfTableTestLine(page int64, y float64, values ...any) pdfLine {
	words := make([]pdfWord, 0, len(values)/3)
	texts := make([]string, 0, len(values)/3)
	for i := 0; i+2 < len(values); i += 3 {
		text := values[i].(string)
		minX := pdfTableTestFloat(values[i+1])
		maxX := pdfTableTestFloat(values[i+2])
		words = append(words, pdfWord{
			Text: text, MinX: minX, MaxX: maxX,
			MinY: y - 3, MaxY: y + 10, FontSize: 10,
		})
		texts = append(texts, text)
	}
	line := pdfLine{
		Text: strings.Join(texts, " "), PageIdx: page, MaxFontSize: 10,
		MinY: y - 3, MaxY: y + 10, Words: words,
	}
	if len(words) > 0 {
		line.MinX = words[0].MinX
		line.MaxX = words[len(words)-1].MaxX
	}
	return line
}

// pdfTableTestFloat 把 fixture 中便于阅读的整数/浮点坐标统一转为 float64。
func pdfTableTestFloat(value any) float64 {
	switch number := value.(type) {
	case int:
		return float64(number)
	case float64:
		return number
	default:
		panic("不支持的测试坐标类型")
	}
}
