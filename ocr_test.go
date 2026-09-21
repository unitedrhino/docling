// ocr_test.go 验证外部 OCR 钩子链路：扫描页触发、识别结果结构化并入
// （标题/表格/文本，prov 标注实际页号）、钩子失败回退原兜底行为、
// 正常页不触发；以及子文档合并的引用偏移正确性。
package docling

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/ledongthuc/pdf"
)

// TestParsePDFOCRRequestContext 验证新版 OCRHook 收到完整 PDF 上下文，且
// 同时配置兼容钩子时只调用新版钩子。
func TestParsePDFOCRRequestContext(t *testing.T) {
	data := buildTestPDF(t, []testPDFPage{{content: ""}})
	legacyCalled := false
	requestCalled := false
	doc, err := ParsePDFWithOptions(data, PDFOptions{
		Filename: "扫描件.pdf",
		OCRHook: func(request OCRRequest) (string, error) {
			requestCalled = true
			if request.PageNo != 1 || request.MIMEType != "application/pdf" ||
				request.Filename != "扫描件.pdf" || request.ExistingText != "" ||
				!bytes.Equal(request.Data, data) {
				t.Fatalf("OCR request wrong: %+v", request)
			}
			if len(request.PageData) == 0 {
				t.Fatal("OCR request should contain an extracted single-page PDF")
			}
			pageReader, pageErr := pdf.NewReader(bytes.NewReader(request.PageData), int64(len(request.PageData)))
			if pageErr != nil {
				t.Fatalf("open OCR PageData: %v", pageErr)
			}
			if pageReader.NumPage() != 1 {
				t.Fatalf("OCR PageData pages=%d, want 1", pageReader.NumPage())
			}
			return "扫描页正文。", nil
		},
		PageOCRHook: func(int64, []byte) (string, error) {
			legacyCalled = true
			return "legacy", nil
		},
	})
	if err != nil {
		t.Fatalf("ParsePDFWithOptions: %v", err)
	}
	if !requestCalled || legacyCalled || !strings.Contains(doc.Text(), "扫描页正文。") {
		t.Fatalf("hook priority/context failed: new=%v old=%v text=%q", requestCalled, legacyCalled, doc.Text())
	}
}

// TestParsePDFInvalidOCRRetriesOnce 验证整份扫描 PDF 的无效结果每页最多
// 调用两次，不会在全文档兜底阶段重复识别同一页。
func TestParsePDFInvalidOCRRetriesOnce(t *testing.T) {
	calls := 0
	_, err := ParsePDFWithOptions(buildTestPDF(t, []testPDFPage{{content: ""}}), PDFOptions{
		DisablePopplerFallback: true,
		OCRHook: func(OCRRequest) (string, error) {
			calls++
			return "抱歉，我无法识别。", nil
		},
	})
	if err == nil {
		t.Fatal("invalid OCR on empty PDF should preserve empty-content error")
	}
	if calls != 2 {
		t.Fatalf("OCR calls = %d, want exactly one retry", calls)
	}
}

// TestRunOCRRetryCarriesFeedback 验证无效 OCR 的第二次调用收到失败原因。
func TestRunOCRRetryCarriesFeedback(t *testing.T) {
	calls := 0
	result, ok := runOCR(PDFOptions{OCRHook: func(request OCRRequest) (string, error) {
		calls++
		if request.Attempt != calls {
			t.Fatalf("attempt=%d calls=%d", request.Attempt, calls)
		}
		if calls == 1 {
			return "抱歉，我无法识别。", nil
		}
		if !strings.Contains(request.ValidationFeedback, "拒答") {
			t.Fatalf("retry feedback=%q", request.ValidationFeedback)
		}
		return "有效识别正文", nil
	}}, OCRRequest{PageNo: 1, Data: []byte("pdf")})
	if !ok || result != "有效识别正文" || calls != 2 {
		t.Fatalf("result=%q ok=%v calls=%d", result, ok, calls)
	}
}

// TestRunOCRRejectsRepetitiveOutput 验证模型陷入逐行复读时不会把循环内容
// 写入正文，并把重复原因反馈给唯一一次重试。
func TestRunOCRRejectsRepetitiveOutput(t *testing.T) {
	calls := 0
	result, ok := runOCR(PDFOptions{OCRHook: func(request OCRRequest) (string, error) {
		calls++
		if calls == 1 {
			return strings.Repeat("错误重复的识别正文行\n", 12), nil
		}
		if !strings.Contains(request.ValidationFeedback, "重复") {
			t.Fatalf("retry feedback=%q", request.ValidationFeedback)
		}
		return "第一行有效正文\n第二行有效正文", nil
	}}, OCRRequest{PageNo: 1, Data: []byte("pdf")})
	if !ok || result != "第一行有效正文\n第二行有效正文" || calls != 2 {
		t.Fatalf("result=%q ok=%v calls=%d", result, ok, calls)
	}
}

// TestValidateOCRResultRejectsOversizedOutput 验证异常膨胀的模型响应在进入
// Markdown 解析前被拒绝，避免单页内容放大文档内存。
func TestValidateOCRResultRejectsOversizedOutput(t *testing.T) {
	if err := validateOCRResult(strings.Repeat("文", 1_000_001)); err == nil ||
		!strings.Contains(err.Error(), "过长") {
		t.Fatalf("oversized OCR result should be rejected, err=%v", err)
	}
}

// TestValidateOCRResultAllowsRepeatedTableSyntax 验证常见 Markdown 表格分隔
// 行不会被复读检测误判。
func TestValidateOCRResultAllowsRepeatedTableSyntax(t *testing.T) {
	result := "| 列一 | 列二 |\n| --- | --- |\n" +
		"| 甲 | 1 |\n| --- | --- |\n| 乙 | 2 |\n| --- | --- |"
	if err := validateOCRResult(result); err != nil {
		t.Fatalf("valid repeated table syntax rejected: %v", err)
	}
}

// TestRunOCRQualityFallback 验证已有正常文本不会被同质或重复 OCR 覆盖，
// 仅当已有文本明显乱码且 OCR 更干净时采用识别结果。
func TestRunOCRQualityFallback(t *testing.T) {
	calls := 0
	opt := PDFOptions{OCRHook: func(OCRRequest) (string, error) {
		calls++
		return "正常正文", nil
	}}
	if result, ok := runOCR(opt, OCRRequest{PageNo: 1, Data: []byte("pdf"), ExistingText: "正常正文"}); ok || result != "" {
		t.Fatalf("duplicate OCR should be rejected: result=%q ok=%v", result, ok)
	}
	if calls != 1 {
		t.Fatalf("valid but not better result should not retry: calls=%d", calls)
	}
	if result, ok := runOCR(opt, OCRRequest{PageNo: 2, Data: []byte("pdf"), ExistingText: "���"}); !ok || result != "正常正文" {
		t.Fatalf("clean OCR should replace garbled text: result=%q ok=%v", result, ok)
	}
}

// TestParsePDFOCRHookScannedPage 验证扫描页触发钩子且识别结果结构化并入。
func TestParsePDFOCRHookScannedPage(t *testing.T) {
	// 第 1 页：15 行正文（避开封面识别）；第 2 页：空内容流（模拟扫描件）
	var sb strings.Builder
	sb.WriteString("BT /F1 10 Tf 72 720 Td (First normal page line.) Tj ET\n")
	for i := 1; i <= 14; i++ {
		fmt.Fprintf(&sb, "BT /F1 10 Tf 72 %g Td (Body line %02d.) Tj ET\n", float64(700-20*i), i)
	}
	pages := []testPDFPage{
		{content: sb.String()},
		{content: ""},
	}
	var hookPages []int64
	hook := func(pageNo int64, pdfBytes []byte) (string, error) {
		hookPages = append(hookPages, pageNo)
		if len(pdfBytes) == 0 {
			t.Errorf("hook 收到空 pdfBytes")
		}
		return "# OCR Result Title\n\nScanned text content.\n\n| A | B |\n| --- | --- |\n| 1 | 2 |", nil
	}
	doc, err := ParsePDFWithOptions(buildTestPDF(t, pages), PDFOptions{PageOCRHook: hook})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(hookPages) != 1 || hookPages[0] != 2 {
		t.Fatalf("钩子应仅对第 2 页触发一次，实际 %v", hookPages)
	}
	// 结构化并入：标题 + 正文 + 表格，prov 标注实际页号 2
	var foundTitle, foundText bool
	for i := range doc.Texts {
		it := doc.Texts[i]
		if it.Orig == "OCR Result Title" {
			foundTitle = true
			// Markdown 一级标题走 AddTitle（label=title，对齐 md 后端语义）
			if it.Label != LabelTitle {
				t.Errorf("OCR 标题 label 错误: %+v", it)
			}
			if len(it.Prov) != 1 || it.Prov[0].PageNo != 2 {
				t.Errorf("OCR 元素 prov 页号错误: %+v", it.Prov)
			}
		}
		if it.Orig == "Scanned text content." {
			foundText = true
		}
	}
	if !foundTitle || !foundText {
		t.Fatalf("OCR 识别文本未并入: %+v", doc.Texts)
	}
	if len(doc.Tables) != 1 || len(doc.Tables[0].Data.TableCells) != 4 {
		t.Fatalf("OCR 表格未并入: %+v", doc.Tables)
	}
	// 简化后 OCR 内容 PageIdx = 2-1 = 1
	items := ToContentList(doc, SourceGolight)
	var ocrItem *Item
	for i := range items {
		if items[i].Text == "Scanned text content." {
			ocrItem = &items[i]
			break
		}
	}
	if ocrItem == nil || ocrItem.PageIdx != 1 {
		t.Fatalf("OCR 正文 PageIdx 应为 1（0 起），got %+v", ocrItem)
	}
}

// TestParsePDFOCRHookFailureFallback 验证钩子失败时回退原兜底行为（不报错）。
func TestParsePDFOCRHookFailureFallback(t *testing.T) {
	pages := []testPDFPage{
		{content: strings.TrimRight(strings.Repeat("BT /F1 10 Tf 72 720 Td (Normal line.) Tj ET\n", 15), "\n")},
		{content: ""},
	}
	hook := func(pageNo int64, pdfBytes []byte) (string, error) {
		return "", fmt.Errorf("ocr service unavailable")
	}
	doc, err := ParsePDFWithOptions(buildTestPDF(t, pages), PDFOptions{PageOCRHook: hook})
	if err != nil {
		t.Fatalf("钩子失败不应导致解析失败: %v", err)
	}
	for i := range doc.Texts {
		if strings.Contains(doc.Texts[i].Orig, "Normal line.") == false {
			continue
		}
	}
	// 第 1 页内容仍在（兜底行为保留）
	if len(doc.Texts) == 0 {
		t.Fatalf("钩子失败后首页内容丢失")
	}
}

// TestParsePDFOCRHookNormalPageNotTriggered 验证有正常文本的页不触发钩子。
func TestParsePDFOCRHookNormalPageNotTriggered(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 15; i++ {
		fmt.Fprintf(&sb, "BT /F1 10 Tf 72 %g Td (Page one line %02d.) Tj ET\n", float64(720-20*i), i)
	}
	var sb2 strings.Builder
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&sb2, "BT /F1 10 Tf 72 %g Td (Page two line %02d.) Tj ET\n", float64(720-20*i), i)
	}
	called := false
	hook := func(pageNo int64, pdfBytes []byte) (string, error) {
		called = true
		return "should not be used", nil
	}
	pages := []testPDFPage{{content: sb.String()}, {content: sb2.String()}}
	if _, err := ParsePDFWithOptions(buildTestPDF(t, pages), PDFOptions{PageOCRHook: hook}); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if called {
		t.Fatalf("正常文本页不应触发钩子")
	}
}

// TestMergeOCRSubDocument 验证子文档合并的引用偏移（结构无损）。
func TestMergeOCRSubDocument(t *testing.T) {
	main := NewDoclingDocument("main")
	main.AddText(LabelText, "existing", nil, nil)
	sub := NewDoclingDocument("ocr")
	sh := sub.AddHeading(1, "OCR 标题", nil, nil)
	sub.AddText(LabelText, "OCR 正文", nil, &sh)
	cells := []DoclingTableCell{
		{StartRowOffsetIdx: 0, EndRowOffsetIdx: 1, StartColOffsetIdx: 0, EndColOffsetIdx: 1, Text: "x", ColumnHeader: true},
	}
	sub.AddTable(cells, 1, 1, nil, &sh)

	mergeOCRSubDocument(main, sub, 3)
	// 引用偏移：子文档 texts 0/1 → 主文档 1/2；表格 0 → 0
	if len(main.Texts) != 3 {
		t.Fatalf("texts 应为 3（原 1 + OCR 2），实际 %d", len(main.Texts))
	}
	// 子文档树：标题挂 body，正文与表格挂标题——偏移后引用应保持该结构
	if main.Texts[1].SelfRef != "#/texts/1" || main.Texts[1].Parent == nil || main.Texts[1].Parent.String() != "#/body" {
		t.Errorf("标题引用偏移错误: %+v parent=%v", main.Texts[1], main.Texts[1].Parent)
	}
	if main.Texts[2].Parent == nil || main.Texts[2].Parent.String() != "#/texts/1" {
		t.Errorf("正文引用偏移错误: %+v", main.Texts[2])
	}
	if len(main.Tables) != 1 || main.Tables[0].SelfRef != "#/tables/0" || main.Tables[0].Parent == nil || main.Tables[0].Parent.String() != "#/texts/1" {
		t.Fatalf("表格引用偏移错误: %+v", main.Tables)
	}
	// 缺 prov 的元素补页级 prov
	if len(main.Texts[2].Prov) != 1 || main.Texts[2].Prov[0].PageNo != 3 {
		t.Errorf("合并元素应补页级 prov: %+v", main.Texts[2].Prov)
	}
	// body 子树追加：existing + OCR 标题（引用已偏移到 #/texts/1）
	if len(main.Body.Children) != 2 || main.Body.Children[1].String() != "#/texts/1" {
		t.Fatalf("body 子树追加错误: %+v", main.Body.Children)
	}
}

// TestParsePDFOCRHookWholeDocFallback 验证全文档无文本层时逐页交给钩子
// （国标扫描件主场景），识别结果按页号并入。
func TestParsePDFOCRHookWholeDocFallback(t *testing.T) {
	pages := []testPDFPage{{content: ""}, {content: ""}}
	var hookPages []int64
	hook := func(pageNo int64, pdfBytes []byte) (string, error) {
		hookPages = append(hookPages, pageNo)
		return fmt.Sprintf("# 扫描页 %d 识别结果\n\n识别正文内容。", pageNo), nil
	}
	doc, err := ParsePDFWithOptions(buildTestPDF(t, pages), PDFOptions{PageOCRHook: hook})
	if err != nil {
		t.Fatalf("全文档扫描+钩子应解析成功: %v", err)
	}
	if len(hookPages) != 2 {
		t.Fatalf("钩子应逐页触发 2 次，实际 %v", hookPages)
	}
	// 两个页面的标题都已并入且 prov 页号正确
	pageNos := map[int64]bool{}
	for i := range doc.Texts {
		if strings.HasPrefix(doc.Texts[i].Orig, "扫描页 ") {
			if len(doc.Texts[i].Prov) == 1 {
				pageNos[doc.Texts[i].Prov[0].PageNo] = true
			}
		}
	}
	if !pageNos[1] || !pageNos[2] {
		t.Fatalf("逐页 OCR 结果页号缺失: %v", pageNos)
	}
}

// TestParsePDFWholeDocEmptyNoHook 验证无钩子时全文档空维持报错语义。
func TestParsePDFWholeDocEmptyNoHook(t *testing.T) {
	pages := []testPDFPage{{content: ""}, {content: ""}}
	if _, err := ParsePDFWithOptions(buildTestPDF(t, pages), PDFOptions{}); err == nil {
		t.Fatalf("无钩子时全空文档应报错")
	}
}
