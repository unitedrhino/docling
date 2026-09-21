// pdf_visual_test.go 验证 PDF 页面质量路由、视觉结果强校验与区域去重合并。
package docling

import (
	"errors"
	"strings"
	"testing"
)

// TestEvaluatePDFPageQualityFormulaAndTableSignals 验证公式密集与未恢复表格
// 会触发可选视觉增强，正常纯文本仍保持纯 Go 路径。
func TestEvaluatePDFPageQualityFormulaAndTableSignals(t *testing.T) {
	formula := []pdfLine{{Text: "f(x)=∫_0^∞ e^{-x} dx + ∑_{i=1}^{n} x_i", MinX: 10, MaxX: 250, MinY: 700, MaxY: 715}}
	quality := evaluatePDFPageQuality(formula)
	if !quality.NeedsVisual || quality.FormulaDensity < 0.08 || !containsString(quality.Reasons, "formula_dense") {
		t.Fatalf("formula quality signals missing: %+v", quality)
	}
	normal := []pdfLine{
		{Text: "第一段正常正文", Words: []pdfWord{{Text: "第一段正常正文", MinX: 10, MaxX: 100}}, MinX: 10, MaxX: 100, MinY: 700, MaxY: 712},
		{Text: "第二段正常正文", Words: []pdfWord{{Text: "第二段正常正文", MinX: 10, MaxX: 100}}, MinX: 10, MaxX: 100, MinY: 680, MaxY: 692},
	}
	if got := evaluatePDFPageQuality(normal); got.NeedsVisual {
		t.Fatalf("normal page should stay pure Go: %+v", got)
	}
}

// TestRunPDFVisualHookRetriesAndValidates 验证越界结果会被拒绝并仅重试一次。
func TestRunPDFVisualHookRetriesAndValidates(t *testing.T) {
	calls := 0
	hook := func(PDFVisualRequest) (PDFVisualResult, error) {
		calls++
		if calls == 1 {
			return PDFVisualResult{Items: []PDFVisualItem{{Label: LabelText, Text: "越界", Confidence: 1, BBox: &DoclingBBox{L: 0, B: 0, R: 900, T: 20}}}}, nil
		}
		return PDFVisualResult{Items: []PDFVisualItem{{Label: LabelText, Text: "有效", Confidence: 0.9, BBox: &DoclingBBox{L: 10, B: 10, R: 80, T: 30}}}}, nil
	}
	result, ok := runPDFVisualHook(hook, PDFVisualRequest{Width: 100, Height: 100})
	if !ok || calls != 2 || result.Items[0].Text != "有效" {
		t.Fatalf("retry/validation wrong: ok=%v calls=%d result=%+v", ok, calls, result)
	}
	failedCalls := 0
	if _, ok := runPDFVisualHook(func(PDFVisualRequest) (PDFVisualResult, error) {
		failedCalls++
		return PDFVisualResult{}, errors.New("model unavailable")
	}, PDFVisualRequest{}); ok || failedCalls != 2 {
		t.Fatalf("model failure should retry once: ok=%v calls=%d", ok, failedCalls)
	}
}

// TestRunPDFVisualHookRetryCarriesFeedback 验证第二次调用获得首次校验失败原因，
// 便于模型定向修正而不是盲目重复相同输出。
func TestRunPDFVisualHookRetryCarriesFeedback(t *testing.T) {
	calls := 0
	_, ok := runPDFVisualHook(func(request PDFVisualRequest) (PDFVisualResult, error) {
		calls++
		if request.Attempt != calls {
			t.Fatalf("attempt=%d calls=%d", request.Attempt, calls)
		}
		if calls == 1 {
			if request.ValidationFeedback != "" {
				t.Fatalf("first feedback=%q", request.ValidationFeedback)
			}
			return PDFVisualResult{Items: []PDFVisualItem{{Label: LabelTable, BBox: &DoclingBBox{L: 1, B: 1, R: 10, T: 10}, Confidence: 0.9}}}, nil
		}
		if !strings.Contains(request.ValidationFeedback, "表格") {
			t.Fatalf("retry feedback=%q", request.ValidationFeedback)
		}
		return PDFVisualResult{Items: []PDFVisualItem{{Label: LabelText, Text: "正文", BBox: &DoclingBBox{L: 1, B: 1, R: 10, T: 10}, Confidence: 0.9}}}, nil
	}, PDFVisualRequest{Width: 100, Height: 100})
	if !ok || calls != 2 {
		t.Fatalf("ok=%v calls=%d", ok, calls)
	}
}

// TestRunPDFVisualHookNormalizesTopLeftBBox 验证模型返回 TOPLEFT 坐标时，
// 在强校验和文档合并前按页面高度转换为官方 BOTTOMLEFT。
func TestRunPDFVisualHookNormalizesTopLeftBBox(t *testing.T) {
	result, ok := runPDFVisualHook(func(PDFVisualRequest) (PDFVisualResult, error) {
		return PDFVisualResult{Items: []PDFVisualItem{{
			Label: LabelText, Text: "正文", Confidence: 0.9,
			BBox: &DoclingBBox{L: 10, T: 20, R: 80, B: 50, CoordOrigin: CoordOriginTopLeft},
		}}}, nil
	}, PDFVisualRequest{Width: 100, Height: 200})
	if !ok || len(result.Items) != 1 || result.Items[0].BBox == nil {
		t.Fatalf("TOPLEFT 结果未通过规范化: ok=%v result=%+v", ok, result)
	}
	bbox := result.Items[0].BBox
	if bbox.L != 10 || bbox.R != 80 || bbox.B != 150 || bbox.T != 180 || bbox.CoordOrigin != CoordOriginBottomLeft {
		t.Fatalf("TOPLEFT 转换错误: %+v", bbox)
	}
}

// TestRunPDFVisualHookRejectsOverlappingTableCells 验证模型返回互相覆盖的
// 表格单元格时会收到拓扑纠错反馈，第二次合法网格才被采用。
func TestRunPDFVisualHookRejectsOverlappingTableCells(t *testing.T) {
	calls := 0
	result, ok := runPDFVisualHook(func(request PDFVisualRequest) (PDFVisualResult, error) {
		calls++
		table := &TableData{NumRows: 2, NumCols: 2, TableCells: []DoclingTableCell{
			{Text: "表头", RowSpan: 1, ColSpan: 2, StartRowOffsetIdx: 0, EndRowOffsetIdx: 1, StartColOffsetIdx: 0, EndColOffsetIdx: 2},
			{Text: "重复占位", RowSpan: 1, ColSpan: 1, StartRowOffsetIdx: 0, EndRowOffsetIdx: 1, StartColOffsetIdx: 1, EndColOffsetIdx: 2},
		}}
		if calls == 2 {
			if !strings.Contains(request.ValidationFeedback, "重叠") {
				t.Fatalf("retry feedback=%q", request.ValidationFeedback)
			}
			table.TableCells = []DoclingTableCell{
				{Text: "表头", RowSpan: 1, ColSpan: 2, StartRowOffsetIdx: 0, EndRowOffsetIdx: 1, StartColOffsetIdx: 0, EndColOffsetIdx: 2},
				{Text: "甲", RowSpan: 1, ColSpan: 1, StartRowOffsetIdx: 1, EndRowOffsetIdx: 2, StartColOffsetIdx: 0, EndColOffsetIdx: 1},
				{Text: "乙", RowSpan: 1, ColSpan: 1, StartRowOffsetIdx: 1, EndRowOffsetIdx: 2, StartColOffsetIdx: 1, EndColOffsetIdx: 2},
			}
		}
		return PDFVisualResult{Items: []PDFVisualItem{{
			Label: LabelTable, Confidence: 0.95,
			BBox: &DoclingBBox{L: 10, B: 10, R: 90, T: 90}, TableData: table,
		}}}, nil
	}, PDFVisualRequest{Width: 100, Height: 100})
	if !ok || calls != 2 || result.Items[0].TableData.Orientation != TableOrientation0 {
		t.Fatalf("result=%+v ok=%v calls=%d", result, ok, calls)
	}
}

// TestValidatePDFVisualTableRejectsSpanMismatchAndEmptyShell 验证跨度声明与
// 偏移不一致、以及没有任何可见内容的空表格都会被拒绝。
func TestValidatePDFVisualTableRejectsSpanMismatchAndEmptyShell(t *testing.T) {
	spanMismatch := &TableData{NumRows: 2, NumCols: 1, Orientation: TableOrientation0, TableCells: []DoclingTableCell{{
		Text: "值", RowSpan: 2, ColSpan: 1,
		StartRowOffsetIdx: 0, EndRowOffsetIdx: 1, StartColOffsetIdx: 0, EndColOffsetIdx: 1,
	}}}
	if err := validatePDFVisualTable(spanMismatch); err == nil || !strings.Contains(err.Error(), "跨度") {
		t.Fatalf("span mismatch should be rejected, err=%v", err)
	}
	empty := &TableData{NumRows: 1, NumCols: 1, Orientation: TableOrientation0, TableCells: []DoclingTableCell{{
		RowSpan: 1, ColSpan: 1,
		StartRowOffsetIdx: 0, EndRowOffsetIdx: 1, StartColOffsetIdx: 0, EndColOffsetIdx: 1,
	}}}
	if err := validatePDFVisualTable(empty); err == nil || !strings.Contains(err.Error(), "内容为空") {
		t.Fatalf("empty table should be rejected, err=%v", err)
	}
}

// TestValidatePDFVisualResultRejectsInvalidCellGeometry 验证模型提供单元格
// 坐标时，坐标必须有限、位于页面内且不能越出所属表格区域。
func TestValidatePDFVisualResultRejectsInvalidCellGeometry(t *testing.T) {
	table := &TableData{NumRows: 1, NumCols: 1, Orientation: TableOrientation0, TableCells: []DoclingTableCell{{
		Text: "值", RowSpan: 1, ColSpan: 1,
		StartRowOffsetIdx: 0, EndRowOffsetIdx: 1, StartColOffsetIdx: 0, EndColOffsetIdx: 1,
		BBox: &DoclingBBox{L: 80, B: 80, R: 95, T: 95, CoordOrigin: CoordOriginBottomLeft},
	}}}
	result := PDFVisualResult{Items: []PDFVisualItem{{
		Label: LabelTable, Confidence: 0.9,
		BBox:      &DoclingBBox{L: 10, B: 10, R: 90, T: 90, CoordOrigin: CoordOriginBottomLeft},
		TableData: table,
	}}}
	if err := validatePDFVisualResult(result, 100, 100); err == nil || !strings.Contains(err.Error(), "单元格坐标") {
		t.Fatalf("cell outside table should be rejected, err=%v", err)
	}

	table.TableCells[0].BBox = &DoclingBBox{L: 20, B: 20, R: 40, T: 40, CoordOrigin: CoordOriginTopLeft}
	if err := validatePDFVisualResult(result, 100, 100); err == nil || !strings.Contains(err.Error(), "坐标原点") {
		t.Fatalf("unnormalized cell origin should be rejected, err=%v", err)
	}
}

// TestValidatePDFVisualResultRejectsDuplicateObjects 验证模型重复返回同一
// 区域的相同对象时整页结果失效，以免重复正文进入文档和知识库。
func TestValidatePDFVisualResultRejectsDuplicateObjects(t *testing.T) {
	result := PDFVisualResult{Items: []PDFVisualItem{
		{Label: LabelText, Text: "重复正文", Confidence: 0.9, BBox: &DoclingBBox{L: 10, B: 10, R: 80, T: 30, CoordOrigin: CoordOriginBottomLeft}},
		{Label: LabelText, Text: " 重复正文 ", Confidence: 0.8, BBox: &DoclingBBox{L: 11, B: 10, R: 80, T: 30, CoordOrigin: CoordOriginBottomLeft}},
	}}
	if err := validatePDFVisualResult(result, 100, 100); err == nil || !strings.Contains(err.Error(), "重复对象") {
		t.Fatalf("duplicate visual objects should be rejected, err=%v", err)
	}
}

// TestValidatePDFVisualResultRejectsLowConfidenceAndInvalidText 验证结构合法但
// 低置信度、拒答或明显乱码的模型结果不会覆盖纯 Go 页面。
func TestValidatePDFVisualResultRejectsLowConfidenceAndInvalidText(t *testing.T) {
	box := &DoclingBBox{L: 10, B: 10, R: 80, T: 30, CoordOrigin: CoordOriginBottomLeft}
	tests := []struct {
		name string
		item PDFVisualItem
		want string
	}{
		{name: "low confidence", item: PDFVisualItem{Label: LabelText, Text: "正文", Confidence: 0.49, BBox: box}, want: "置信度"},
		{name: "refusal", item: PDFVisualItem{Label: LabelText, Text: "抱歉，我无法识别这一页", Confidence: 0.9, BBox: box}, want: "拒答"},
		{name: "garbage", item: PDFVisualItem{Label: LabelText, Text: strings.Repeat("�", 20), Confidence: 0.9, BBox: box}, want: "乱码"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validatePDFVisualResult(PDFVisualResult{Items: []PDFVisualItem{test.item}}, 100, 100)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation err=%v, want %q", err, test.want)
			}
		})
	}
}

// TestParsePDFStructuredVisualRegionMerge 验证视觉表格替换重叠规则文字，
// 保留区域外正文并输出官方表格结构。
func TestParsePDFStructuredVisualRegionMerge(t *testing.T) {
	content := "BT /F1 10 Tf 72 720 Td (A  B) Tj ET\nBT /F1 10 Tf 72 680 Td (outside paragraph) Tj ET"
	data := buildTestPDF(t, []testPDFPage{{content: content}})
	calls := 0
	table := &TableData{NumRows: 1, NumCols: 2, TableCells: []DoclingTableCell{
		{Text: "A", RowSpan: 1, ColSpan: 1, StartRowOffsetIdx: 0, EndRowOffsetIdx: 1, StartColOffsetIdx: 0, EndColOffsetIdx: 1},
		{Text: "B", RowSpan: 1, ColSpan: 1, StartRowOffsetIdx: 0, EndRowOffsetIdx: 1, StartColOffsetIdx: 1, EndColOffsetIdx: 2},
	}}
	doc, err := ParsePDFWithOptions(data, PDFOptions{
		Filename: "复杂表格.pdf", DisablePopplerFallback: true, VisualAlways: true,
		VisualHook: func(request PDFVisualRequest) (PDFVisualResult, error) {
			calls++
			if request.PageNo != 1 || request.Filename != "复杂表格.pdf" || request.MIMEType != "application/pdf" || request.Prompt != PDFStructuredVisualPrompt || request.Quality.LineCount != 2 {
				t.Fatalf("visual request wrong: %+v", request)
			}
			return PDFVisualResult{Items: []PDFVisualItem{{
				Label: LabelTable, Confidence: 0.98, BBox: &DoclingBBox{L: 60, B: 705, R: 180, T: 740}, TableData: table,
			}}}, nil
		},
	})
	if err != nil {
		t.Fatalf("parse PDF: %v", err)
	}
	if calls != 1 || len(doc.Tables) != 1 || doc.Tables[0].Data.NumCols != 2 {
		t.Fatalf("visual table missing: calls=%d tables=%+v", calls, doc.Tables)
	}
	text := doc.Text()
	if !strings.Contains(text, "outside paragraph") || strings.Contains(text, "A  B") {
		t.Fatalf("region merge duplicated or dropped text: %q", text)
	}
	validateDocumentWithDoclingCore110ForTest(t, "pdf-structured-visual", doc)
}

// TestMergePDFVisualPagePreservesSameTextOutsideRegion 验证页面不同位置的
// 同名正文不会被全页字符串去重误删；只有落入视觉区域的坐标文本，以及
// 无坐标但已被视觉结果覆盖的降级文本会移除。
func TestMergePDFVisualPagePreservesSameTextOutsideRegion(t *testing.T) {
	lines := []pdfLine{
		{Text: "重复标题", MinX: 10, MinY: 80, MaxX: 80, MaxY: 90},
		{Text: "重复标题", MinX: 10, MinY: 20, MaxX: 80, MaxY: 30},
		{Text: "重复标题", Fallback: true},
	}
	result := PDFVisualResult{Items: []PDFVisualItem{{
		Label: LabelText, Text: "重复标题", Confidence: 0.9,
		BBox: &DoclingBBox{L: 5, B: 75, R: 85, T: 95, CoordOrigin: CoordOriginBottomLeft},
	}}}
	merged := mergePDFVisualPage(lines, result, 0)
	if len(merged) != 2 || merged[0].Text != "重复标题" || merged[0].MinY != 20 || merged[1].OCRSub == nil {
		t.Fatalf("visual region merge removed outside text or kept duplicate: %+v", merged)
	}
}

// TestParsePDFVisualLimitAndFallback 验证页数上限与模型失败回退纯 Go 文本。
func TestParsePDFVisualLimitAndFallback(t *testing.T) {
	data := buildTestPDF(t, []testPDFPage{{content: "BT /F1 10 Tf 72 720 Td (page one) Tj ET"}, {content: "BT /F1 10 Tf 72 720 Td (page two) Tj ET"}})
	calls := 0
	doc, err := ParsePDFWithOptions(data, PDFOptions{
		DisablePopplerFallback: true, VisualAlways: true, MaxVisualPages: 1,
		VisualHook: func(PDFVisualRequest) (PDFVisualResult, error) {
			calls++
			return PDFVisualResult{}, errors.New("temporary failure")
		},
	})
	if err != nil {
		t.Fatalf("pure Go fallback failed: %v", err)
	}
	if calls != 2 {
		t.Fatalf("one visual page with one retry should make 2 calls, got %d", calls)
	}
	if !strings.Contains(doc.Text(), "page one") || !strings.Contains(doc.Text(), "page two") {
		t.Fatalf("fallback text missing: %q", doc.Text())
	}
}

// TestParseByExtForwardsPDFVisualOptions 验证统一入口透传视觉钩子、文件名和页数选项。
func TestParseByExtForwardsPDFVisualOptions(t *testing.T) {
	data := buildTestPDF(t, []testPDFPage{{content: "BT /F1 10 Tf 72 720 Td (old text) Tj ET"}})
	calls := 0
	doc, err := ParseByExtWithOptions("folder/标准.pdf", data, ParseOptions{
		DisablePopplerFallback: true, PDFVisualAlways: true, MaxPDFVisualPages: 1,
		PDFVisualHook: func(request PDFVisualRequest) (PDFVisualResult, error) {
			calls++
			if request.Filename != "标准.pdf" {
				t.Fatalf("filename=%q", request.Filename)
			}
			if len(request.PageData) == 0 {
				t.Fatal("visual request should contain an extracted single-page PDF")
			}
			return PDFVisualResult{Items: []PDFVisualItem{{Label: LabelText, Text: "新正文", Confidence: 1, BBox: &DoclingBBox{L: 60, B: 700, R: 180, T: 740}}}}, nil
		},
	})
	if err != nil {
		t.Fatalf("ParseByExt visual: %v", err)
	}
	if calls != 1 || !strings.Contains(doc.Text(), "新正文") || doc.Origin == nil || doc.Origin.Filename != "标准.pdf" {
		t.Fatalf("ParseByExt visual forwarding wrong: calls=%d text=%q origin=%+v", calls, doc.Text(), doc.Origin)
	}
}

// TestPDFVisualResultDocumentListAndCaptionRefs 验证视觉列表按组保存，图片
// caption 在并入已有文档后完成引用偏移。
func TestPDFVisualResultDocumentListAndCaptionRefs(t *testing.T) {
	box := &DoclingBBox{L: 10, B: 10, R: 100, T: 30}
	sub := pdfVisualResultDocument(PDFVisualResult{Items: []PDFVisualItem{
		{Label: LabelListItem, Text: "第一项", Enumerated: true, Marker: "1.", Confidence: 1, BBox: box},
		{Label: LabelListItem, Text: "第二项", Enumerated: true, Marker: "2.", Confidence: 1, BBox: box},
		{Label: LabelPicture, Text: "结构示意图", Confidence: 1, BBox: box},
	}}, 2)
	if len(sub.Groups) != 1 || len(sub.Groups[0].Children) != 2 || len(sub.Pictures) != 1 || len(sub.Pictures[0].Captions) != 1 {
		t.Fatalf("visual subdocument structure wrong: groups=%+v pictures=%+v", sub.Groups, sub.Pictures)
	}
	doc := NewDoclingDocument("main")
	doc.AddText(LabelText, "已有正文", nil, nil)
	mergeOCRSubDocument(doc, sub, 2)
	caption := doc.Pictures[0].Captions[0]
	if caption.Idx != 3 || doc.Texts[caption.Idx].Text != "结构示意图" || doc.Texts[caption.Idx].Parent == nil || doc.Texts[caption.Idx].Parent.String() != doc.Pictures[0].SelfRef {
		t.Fatalf("caption refs not offset: caption=%+v texts=%+v picture=%+v", caption, doc.Texts, doc.Pictures[0])
	}
}

// containsString 判断字符串列表是否包含目标值。
func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
