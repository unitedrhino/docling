// pdf_furniture_toc_fix_test.go 邻接页眉判定与空格引导点目录的回归测试。
package docling

import (
	"fmt"
	"strings"
	"testing"
)

// TestHasConsecutivePDFPages 邻接判定的纯函数边界：恰满 4、短于 4、
// 中断分段、乱序输入、散点无连续。
func TestHasConsecutivePDFPages(t *testing.T) {
	mk := func(pages ...int64) map[int64]bool {
		m := make(map[int64]bool, len(pages))
		for _, p := range pages {
			m[p] = true
		}
		return m
	}
	cases := []struct {
		name  string
		pages map[int64]bool
		n     int
		want  bool
	}{
		{"空集合", mk(), 4, false},
		{"恰满4连续", mk(1, 2, 3, 4), 4, true},
		{"仅3连续", mk(1, 2, 3), 4, false},
		{"4但中间断开", mk(1, 2, 4, 5), 4, false},
		{"第二段满4", mk(1, 2, 3, 5, 6, 7, 8), 4, true},
		{"散点无连续", mk(1, 5, 9, 13), 4, false},
		{"乱序输入", mk(9, 7, 8, 10, 6), 4, true},
		{"两段各3", mk(1, 2, 3, 10, 11, 12), 4, false},
	}
	for _, c := range cases {
		if got := hasConsecutivePDFPages(c.pages, c.n); got != c.want {
			t.Errorf("%s: hasConsecutivePDFPages(%v, %d) = %v, want %v", c.name, c.pages, c.n, got, c.want)
		}
	}
}

// TestFoldPDFTOCDots 空格引导点折叠：命中形态折叠为连续点并被目录正则
// 识别；编号点/无页码行/连续点原形态不受影响。
func TestFoldPDFTOCDots(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantMatch bool // 折叠后 pdfTOCLineRe 是否命中
	}{
		{"空格点+页码", "ChapterOne . . . . . 5", true},
		{"连续点+页码（回归保护）", "ChapterOne............5", true},
		{"全角间隔号空格点", "附录Ａ · · · · 30", true},
		{"编号点非引导线", "3.1.2 权限设置", false},
		{"空格点无页码", "Section title . . . . .", false},
		{"省略号不足4", "Title……5", false},
	}
	for _, c := range cases {
		folded := foldPDFTOCDots(c.in)
		if got := pdfTOCLineRe.MatchString(folded); got != c.wantMatch {
			t.Errorf("%s: fold(%q)=%q match=%v, want %v", c.name, c.in, folded, got, c.wantMatch)
		}
	}
	// 不含点-空格序列的输入不得被改写（折叠只作用于点-空格引导线）
	for _, in := range []string{"3.1.2 权限设置", "Title……5", "ChapterOne............5"} {
		if got := foldPDFTOCDots(in); got != in {
			t.Errorf("无点-空格序列输入不应被改写: %q -> %q", in, got)
		}
	}
}

// furniturePage 构造一页三行几何（页眉/正文/页脚），与上游
// TestParsePDFPageHeaderFooter 同款 Y 坐标，保证区域判定稳定。
func furniturePage(header string, i int) testPDFPage {
	content := ""
	if header != "" {
		content += fmt.Sprintf("BT /F1 20 Tf 72 760 Td (%s) Tj ET\n", header)
	}
	content += fmt.Sprintf("BT /F1 10 Tf 72 600 Td (Body content of page %02d.) Tj ET\n"+
		"BT /F1 10 Tf 72 80 Td (Fixed Footer Notice) Tj ET\n", i)
	return testPDFPage{content: content}
}

// countPageHeaders 统计文档中 page_header 文本条目。
func countPageHeaders(doc *DoclingDocument, text string) int {
	n := 0
	for _, tx := range doc.Texts {
		if tx.Label == LabelPageHeader && tx.Text == text {
			n++
		}
	}
	return n
}

// TestParsePDFChapterVaryingHeader 两段各 4 连续页的章节变化页眉（各只覆盖
// 50% 页面，只有邻接规则可命中）均应标 page_header 并挂 furniture。
func TestParsePDFChapterVaryingHeader(t *testing.T) {
	pages := make([]testPDFPage, 0, 8)
	for i := 1; i <= 8; i++ {
		header := "Chapter One Header"
		if i > 4 {
			header = "Chapter Two Header"
		}
		pages = append(pages, furniturePage(header, i))
	}
	doc, err := ParsePDF(buildTestPDF(t, pages))
	if err != nil {
		t.Fatalf("ParsePDF: %v", err)
	}
	if got := countPageHeaders(doc, "Chapter One Header"); got != 4 {
		t.Fatalf("Chapter One Header 应识别 4 次, got %d", got)
	}
	if got := countPageHeaders(doc, "Chapter Two Header"); got != 4 {
		t.Fatalf("Chapter Two Header 应识别 4 次, got %d", got)
	}
	for _, tx := range doc.Texts {
		if tx.Label != LabelPageHeader {
			continue
		}
		if tx.Parent == nil || tx.Parent.String() != "#/furniture" {
			t.Fatalf("header %+v parent = %+v, want furniture", tx.Text, tx.Parent)
		}
	}
}

// TestParsePDFHeaderBodyOccurrenceBlocksAdjacency 页眉文本在正文区出现过后，
// 即使顶部满足 >=4 连续页也不判页眉。
func TestParsePDFHeaderBodyOccurrenceBlocksAdjacency(t *testing.T) {
	pages := make([]testPDFPage, 0, 10)
	for i := 1; i <= 10; i++ {
		p := furniturePage("", i)
		if i <= 4 {
			p = furniturePage("Shared Header Text", i)
		}
		if i == 6 {
			// 正文中部出现同文本（Y=500 位于页眉带与页脚带之间）
			p.content += "BT /F1 10 Tf 72 500 Td (Shared Header Text) Tj ET\n"
		}
		pages = append(pages, p)
	}
	doc, err := ParsePDF(buildTestPDF(t, pages))
	if err != nil {
		t.Fatalf("ParsePDF: %v", err)
	}
	if got := countPageHeaders(doc, "Shared Header Text"); got != 0 {
		t.Fatalf("正文区出现过的页眉文本不应被识别, got %d 次", got)
	}
}

// TestParsePDFHeaderNonConsecutiveNotFurniture 页眉出现在 1/2/4/5 页顶部
// （无 >=4 连续段），不判页眉。
func TestParsePDFHeaderNonConsecutiveNotFurniture(t *testing.T) {
	pages := make([]testPDFPage, 0, 7)
	for i := 1; i <= 7; i++ {
		header := ""
		if i == 1 || i == 2 || i == 4 || i == 5 {
			header = "Intermittent Header"
		}
		pages = append(pages, furniturePage(header, i))
	}
	doc, err := ParsePDF(buildTestPDF(t, pages))
	if err != nil {
		t.Fatalf("ParsePDF: %v", err)
	}
	if got := countPageHeaders(doc, "Intermittent Header"); got != 0 {
		t.Fatalf("非连续页眉不应被识别, got %d 次", got)
	}
}

// TestParsePDFSpacedDotTOC 空格引导点目录行应标注 document_index 并挂
// "目次"分组，不产生 section_header。
func TestParsePDFSpacedDotTOC(t *testing.T) {
	var sb strings.Builder
	for i := 1; i <= 15; i++ {
		fmt.Fprintf(&sb, "BT /F1 10 Tf 72 %g Td (Filler body line number %02d here) Tj ET\n", float64(720-18*(i-1)), i)
	}
	page0 := sb.String()
	sb.Reset()
	for i, e := range []struct{ title, page string }{
		{"ChapterOne", "5"}, {"ChapterTwo", "12"}, {"ChapterThree", "20"}, {"Appendix", "30"},
	} {
		fmt.Fprintf(&sb, "BT /F1 14 Tf 72 %g Td (%s . . . . . %s) Tj ET\n", float64(700-20*i), e.title, e.page)
	}
	sb.WriteString("BT /F1 10 Tf 72 600 Td (Body after toc region.) Tj ET\n")
	doc, err := ParsePDF(buildTestPDF(t, []testPDFPage{{content: page0}, {content: sb.String()}}))
	if err != nil {
		t.Fatalf("ParsePDF: %v", err)
	}
	if len(doc.Texts) != 6 {
		t.Fatalf("expected 6 texts (1 body block + 4 toc + 1 body), got %d: %+v", len(doc.Texts), doc.Texts)
	}
	for i := 1; i <= 4; i++ {
		tx := doc.Texts[i]
		if tx.Label != LabelDocumentIndex {
			t.Fatalf("texts[%d] label = %v, want document_index", i, tx.Label)
		}
		if tx.Parent == nil || tx.Parent.Kind != refGroups {
			t.Fatalf("texts[%d] parent = %+v, want toc group", i, tx.Parent)
		}
	}
	for i, tx := range doc.Texts {
		if tx.Label == LabelSectionHeader {
			t.Fatalf("texts[%d] unexpected section_header %+v", i, tx)
		}
	}
}
