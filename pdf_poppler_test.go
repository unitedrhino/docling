// pdf_poppler_test.go 验证 Poppler XHTML 词盒到统一 PDF 行/词模型的映射。
package docling

import "testing"

// TestPopplerFallbackForSparseReplacement 验证只在备用文本清除了少量坏字
// 且保留了足量正文时，才替换原页。
func TestPopplerFallbackForSparseReplacement(t *testing.T) {
	original := []pdfLine{{Text: "这是一段带有一个未解码字形的中文正�，其余文字完整保留。", Fallback: true}}
	clean := "这是一段带有一个未解码字形的中文正文，其余文字完整保留。"
	backend := &popplerBackend{pages: map[int64][]pdfLine{
		0: {{Text: clean}},
		1: {{Text: "只有部分正文。"}},
	}, ok: true}
	backend.once.Do(func() {})
	if got := tryPopplerFallback(backend, 0, original); len(got) != 1 || got[0].Text != clean {
		t.Fatalf("完整恢复结果应进入文档：%v", got)
	}
	if got := tryPopplerFallback(backend, 1, original); got != nil {
		t.Fatalf("缺少正文的恢复结果应被拒绝：%v", got)
	}
	original[0].Fallback = false
	if got := tryPopplerFallback(backend, 0, original); got != nil {
		t.Fatalf("有坐标的页面应保留既有版面：%v", got)
	}
}

// TestParsePopplerPagesConvertsToBottomLeft 验证 Poppler XHTML 页高被用于将
// TOPLEFT 行盒与词盒转换为统一 BOTTOMLEFT，并完整保留每个词的 bbox 与字号。
func TestParsePopplerPagesConvertsToBottomLeft(t *testing.T) {
	xhtml := []byte(`<html><body><doc><page width="200" height="100"><flow><block>
<line xMin="10" yMin="20" xMax="80" yMax="32">
<word xMin="45" yMin="21" xMax="80" yMax="31">second</word>
<word xMin="10" yMin="20" xMax="40" yMax="32">first</word>
</line></block></flow></page></doc></body></html>`)
	pages, ok := parsePopplerPages(xhtml)
	if !ok {
		t.Fatal("parsePopplerPages returned invalid result")
	}
	if len(pages[0]) != 1 {
		t.Fatalf("pages = %+v, want one line on page zero", pages)
	}
	got := pages[0][0]
	if got.Text != "first second" || got.PageIdx != 0 || got.MinX != 10 || got.MaxX != 80 ||
		got.MinY != 68 || got.MaxY != 80 || got.MaxFontSize != 12 {
		t.Fatalf("line = %+v, want bottom-left line bbox [10,68,80,80]", got)
	}
	want := []pdfWord{
		{Text: "first", MinX: 10, MinY: 68, MaxX: 40, MaxY: 80, FontSize: 12},
		{Text: "second", MinX: 45, MinY: 69, MaxX: 80, MaxY: 79, FontSize: 10},
	}
	if len(got.Words) != len(want) {
		t.Fatalf("words = %+v, want %+v", got.Words, want)
	}
	for i := range want {
		if got.Words[i] != want[i] {
			t.Fatalf("words[%d] = %+v, want %+v", i, got.Words[i], want[i])
		}
	}
}
