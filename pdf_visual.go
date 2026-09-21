// pdf_visual.go 定义 PDF 复杂页面的质量评估与可选大模型结构化视觉协议。
// 纯 Go 坐标解析始终是主路径；只有页面存在扫描、乱码、疑似未恢复表格、
// 公式密集或栏位歧义时才建议调用钩子，结果无效时安全回退原解析结果。
package docling

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// PDFStructuredVisualPrompt 是结构化视觉模型的内置严格提示词。
const PDFStructuredVisualPrompt = `逐字分析这一页 PDF，不要总结、改写或臆造。只返回 JSON 对象 {"items":[...]}，items 按阅读顺序排列。每项必须包含 label、bbox、confidence；bbox 必须是页面范围内的 {"l":数字,"t":数字,"r":数字,"b":数字,"coord_origin":"BOTTOMLEFT"}，单位为 PDF point。请使用请求中已应用 CropBox、UserUnit 和 /Rotate 的 width/height 显示坐标，不要返回原始 MediaBox 坐标。正文使用 text/paragraph，标题使用 title/section_header 并给 level，列表使用 list_item、text、enumerated、marker，公式使用 formula 且 text 为 LaTeX，图片使用 picture 和可检索说明。表格使用 label=table 和 table_data；table_data 必须含 num_rows、num_cols、orientation="rot_0"、table_cells，每个单元格必须含 text、row_span、col_span、start_row_offset_idx、end_row_offset_idx、start_col_offset_idx、end_col_offset_idx、column_header、row_header、row_section、fillable。无法确认的内容不要输出，不要使用 Markdown 围栏或附加说明。`

// pdfVisualMinConfidence 是模型对象可覆盖纯 Go 页面结果的最低置信度。
const pdfVisualMinConfidence = 0.5

// PDFPageQuality 保存纯 Go 页面提取的质量信号；Score 越高越可靠。
type PDFPageQuality struct {
	Score              float64  `json:"score"`
	GarbageRatio       float64  `json:"garbage_ratio"`
	TextRunes          int      `json:"text_runes"`
	WordCount          int      `json:"word_count"`
	LineCount          int      `json:"line_count"`
	CandidateTableRows int      `json:"candidate_table_rows"`
	DetectedTables     int      `json:"detected_tables"`
	FormulaDensity     float64  `json:"formula_density"`
	ColumnConfidence   float64  `json:"column_confidence"`
	ImageCount         int      `json:"image_count"`        // ImageCount 是实际绘制的图片次数。
	LargeImageCount    int      `json:"large_image_count"`  // LargeImageCount 是达到视觉路由阈值的图片数。
	ImageCoverage      float64  `json:"image_coverage"`     // ImageCoverage 是图片 bbox 并集占页面面积的比例。
	MaxImageCoverage   float64  `json:"max_image_coverage"` // MaxImageCoverage 是单张图片的最大页面覆盖率。
	PageRotation       int      `json:"page_rotation"`      // PageRotation 是 PDF 页顺时针旋转角度。
	UserUnit           float64  `json:"user_unit"`          // UserUnit 是 PDF 用户坐标的物理缩放倍率。
	NeedsVisual        bool     `json:"needs_visual"`
	Reasons            []string `json:"reasons"`
}

// PDFVisualRequest 描述一次页级结构化视觉请求。
type PDFVisualRequest struct {
	PageNo             int64            `json:"page_no"`                       // PageNo 是从 1 开始的页号。
	MIMEType           string           `json:"mimetype"`                      // MIMEType 是原文件 MIME。
	Filename           string           `json:"filename"`                      // Filename 是原文件名。
	Data               []byte           `json:"-"`                             // Data 是原始 PDF 字节。
	PageData           []byte           `json:"-"`                             // PageData 是 Docling 安全抽取的单页 PDF。
	Width              float64          `json:"width"`                         // Width 是页面宽度，单位为 point。
	Height             float64          `json:"height"`                        // Height 是页面高度，单位为 point。
	ExistingText       string           `json:"existing_text"`                 // ExistingText 是现有规则文本。
	Quality            PDFPageQuality   `json:"quality"`                       // Quality 是纯 Go 页面质量信号。
	EmbeddedImages     []PDFVisualImage `json:"embedded_images,omitempty"`     // EmbeddedImages 是已解码图片及其页面位置。
	Prompt             string           `json:"prompt"`                        // Prompt 是严格结构化输出提示词。
	Attempt            int              `json:"attempt"`                       // Attempt 是从 1 开始的调用次数。
	ValidationFeedback string           `json:"validation_feedback,omitempty"` // ValidationFeedback 是前一次失败原因。
}

// PDFVisualImage 描述页面内一个已由纯 Go 解码的图片资产；Image 保存像素
// 引用，BBox 保存该资源在 PDF 页面上的实际绘制区域。
type PDFVisualImage struct {
	Image *ImageRef    `json:"image"`
	BBox  *DoclingBBox `json:"bbox"`
}

// PDFVisualItem 是模型返回的单个版面对象。
type PDFVisualItem struct {
	Label      DocItemLabel `json:"label"`
	Text       string       `json:"text,omitempty"`
	Level      int64        `json:"level,omitempty"`
	BBox       *DoclingBBox `json:"bbox"`
	Confidence float64      `json:"confidence"`
	TableData  *TableData   `json:"table_data,omitempty"`
	Enumerated bool         `json:"enumerated,omitempty"`
	Marker     string       `json:"marker,omitempty"`
}

// PDFVisualResult 是结构化视觉钩子的返回协议。
type PDFVisualResult struct {
	Items []PDFVisualItem `json:"items"`
}

// PDFVisualHook 对疑难页面执行结构化视觉识别。
type PDFVisualHook func(request PDFVisualRequest) (PDFVisualResult, error)

// evaluatePDFPageQuality 计算页面质量与视觉增强触发原因。
func evaluatePDFPageQuality(lines []pdfLine) PDFPageQuality {
	return evaluatePDFPageQualityWithImages(lines, nil, 0, 0)
}

// evaluatePDFPageQualityWithGeometry 在通用质量信号上补充页面旋转语义。
// 非零旋转页强制进入可选视觉增强，因为字体文本矩阵可能与页旋转叠加。
func evaluatePDFPageQualityWithGeometry(lines []pdfLine, images []pdfImagePlacement, geometry pdfPageGeometry) PDFPageQuality {
	quality := evaluatePDFPageQualityWithImages(lines, images, geometry.width, geometry.height)
	quality.PageRotation = geometry.rotation
	quality.UserUnit = geometry.userUnit
	if geometry.rotation != 0 {
		quality.Reasons = append(quality.Reasons, "rotated_page")
		quality.Score = math.Max(0, quality.Score-0.2)
		quality.NeedsVisual = true
	}
	return quality
}

// evaluatePDFPageQualityWithImages 在文本质量基础上加入页面图片覆盖信号。
func evaluatePDFPageQualityWithImages(lines []pdfLine, images []pdfImagePlacement, width, height float64) PDFPageQuality {
	text := joinPDFLineText(lines)
	quality := PDFPageQuality{
		Score: 1, GarbageRatio: textGarbageRatio(text), TextRunes: utf8.RuneCountInString(strings.TrimSpace(text)),
		LineCount: len(lines), UserUnit: 1, Reasons: []string{},
	}
	for _, line := range lines {
		quality.WordCount += len(line.Words)
		if len(splitPDFTableRow(line)) >= 2 {
			quality.CandidateTableRows++
		}
	}
	coordinateLessText := false
	for _, line := range lines {
		if line.Fallback && line.OCRSub == nil && strings.TrimSpace(line.Text) != "" {
			coordinateLessText = true
			break
		}
	}
	tables, _ := detectPDFTables(lines)
	if len(tables) == 0 {
		tables, _ = detectPDFSparseTables(lines)
	}
	quality.DetectedTables = len(tables)
	quality.FormulaDensity = pdfFormulaDensity(text)
	if len(lines) > 0 {
		if _, ok := findPDFLayoutMainCut(lines); ok {
			quality.ColumnConfidence = 1
		} else if len(lines) >= pdfLayoutMinColumnLines*2 && pdfLayoutLinesHaveBounds(lines) {
			quality.ColumnConfidence = 0.5
		}
	}
	quality.ImageCount = len(images)
	quality.ImageCoverage = pdfImageCoverage(images, width, height)
	pageArea := width * height
	for _, image := range images {
		if image.BBox == nil || pageArea <= 0 {
			continue
		}
		coverage := (image.BBox.R - image.BBox.L) * (image.BBox.T - image.BBox.B) / pageArea
		quality.MaxImageCoverage = math.Max(quality.MaxImageCoverage, coverage)
		pixels := image.PixelWidth * image.PixelHeight
		if coverage >= pdfMixedImageMinCoverage && (pixels >= pdfLargeImageMinPixels || coverage >= 0.25) {
			quality.LargeImageCount++
		}
	}
	addReason := func(reason string, penalty float64) {
		quality.Reasons = append(quality.Reasons, reason)
		quality.Score -= penalty
	}
	switch {
	case quality.TextRunes == 0:
		addReason("empty_text_layer", 1)
	case quality.GarbageRatio >= defaultPDFOCRGarbageThreshold:
		addReason("garbled_text", math.Min(0.8, quality.GarbageRatio))
	}
	if quality.CandidateTableRows >= 2 && quality.DetectedTables == 0 {
		addReason("unresolved_table", 0.25)
	}
	if quality.FormulaDensity >= 0.08 && quality.TextRunes >= 12 {
		addReason("formula_dense", 0.2)
	}
	if quality.ColumnConfidence == 0.5 && pdfDistinctLeftAnchors(lines) >= 3 {
		addReason("ambiguous_columns", 0.15)
	}
	if quality.TextRunes >= 12 && quality.LargeImageCount > 0 && quality.ImageCoverage >= pdfMixedImageMinCoverage {
		addReason("mixed_text_image", 0.2)
	}
	if coordinateLessText {
		addReason("coordinate_less_text", 0.2)
	}
	quality.Score = math.Max(0, math.Min(1, quality.Score))
	quality.NeedsVisual = len(quality.Reasons) > 0
	return quality
}

// pdfFormulaDensity 估算公式字符密度，用于触发视觉公式恢复而非推导内容。
func pdfFormulaDensity(text string) float64 {
	var total, formula int
	for _, r := range text {
		if unicode.IsSpace(r) {
			continue
		}
		total++
		if strings.ContainsRune("=∑∏√∫≈≠≤≥±×÷∞∂∆∇∈∉⊂⊃∪∩^_{}[]", r) || unicode.In(r, unicode.Sm) {
			formula++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(formula) / float64(total)
}

// pdfDistinctLeftAnchors 统计行左边缘的粗粒度聚类数。
func pdfDistinctLeftAnchors(lines []pdfLine) int {
	anchors := make([]float64, 0, len(lines))
	for _, line := range lines {
		if line.MaxX <= line.MinX {
			continue
		}
		matched := false
		for _, anchor := range anchors {
			if math.Abs(anchor-line.MinX) <= 18 {
				matched = true
				break
			}
		}
		if !matched {
			anchors = append(anchors, line.MinX)
		}
	}
	return len(anchors)
}

// runPDFVisualHook 执行结构化视觉钩子并对错误或无效结果重试一次。
func runPDFVisualHook(hook PDFVisualHook, request PDFVisualRequest) (PDFVisualResult, bool) {
	if hook == nil {
		return PDFVisualResult{}, false
	}
	if request.Prompt == "" {
		request.Prompt = PDFStructuredVisualPrompt
	}
	for attempt := 0; attempt < 2; attempt++ {
		request.Attempt = attempt + 1
		result, err := hook(request)
		if err != nil {
			request.ValidationFeedback = "上一次模型调用失败，请重新分析并严格按 JSON 协议返回"
			continue
		}
		normalizePDFVisualResult(&result, request.Height)
		if validationErr := validatePDFVisualResult(result, request.Width, request.Height); validationErr == nil {
			return result, true
		} else {
			request.ValidationFeedback = "上一次结构化结果无效：" + validationErr.Error()
		}
	}
	return PDFVisualResult{}, false
}

// normalizePDFVisualResult 把模型常见的 TOPLEFT bbox 转换为 Docling PDF
// 使用的 BOTTOMLEFT；未声明坐标原点时按提示词约定视为 BOTTOMLEFT。
func normalizePDFVisualResult(result *PDFVisualResult, pageHeight float64) {
	if result == nil {
		return
	}
	for itemIndex := range result.Items {
		item := &result.Items[itemIndex]
		normalizePDFVisualBBox(item.BBox, pageHeight)
		if item.TableData == nil {
			continue
		}
		if item.TableData.Orientation == "" {
			item.TableData.Orientation = TableOrientation0
		}
		for cellIndex := range item.TableData.TableCells {
			normalizePDFVisualBBox(item.TableData.TableCells[cellIndex].BBox, pageHeight)
		}
	}
}

// normalizePDFVisualBBox 原地转换单个模型 bbox。
func normalizePDFVisualBBox(bbox *DoclingBBox, pageHeight float64) {
	if bbox == nil {
		return
	}
	switch bbox.CoordOrigin {
	case "", CoordOriginBottomLeft:
		bbox.CoordOrigin = CoordOriginBottomLeft
	case CoordOriginTopLeft:
		if pageHeight <= 0 {
			return
		}
		top, bottom := bbox.T, bbox.B
		bbox.B = pageHeight - bottom
		bbox.T = pageHeight - top
		bbox.CoordOrigin = CoordOriginBottomLeft
	}
}

// validatePDFVisualResult 校验标签、置信度、坐标及表格矩形范围。
func validatePDFVisualResult(result PDFVisualResult, width, height float64) error {
	if len(result.Items) == 0 || len(result.Items) > 2000 {
		return errors.New("docling: PDF 视觉结果元素数量无效")
	}
	allowed := map[DocItemLabel]bool{
		LabelTitle: true, LabelSectionHeader: true, LabelText: true, LabelParagraph: true,
		LabelListItem: true, LabelCode: true, LabelFormula: true, LabelCaption: true,
		LabelFootnote: true, LabelReference: true, LabelTable: true, LabelPicture: true,
	}
	seen := make(map[string][]*DoclingBBox, len(result.Items))
	var visibleText strings.Builder
	totalRunes := 0
	for _, item := range result.Items {
		if !allowed[item.Label] || !isFinitePDFVisualNumber(item.Confidence) || item.Confidence < 0 || item.Confidence > 1 || item.BBox == nil {
			return errors.New("docling: PDF 视觉结果基础字段无效")
		}
		if item.Confidence < pdfVisualMinConfidence {
			return errors.New("docling: PDF 视觉结果置信度过低")
		}
		if item.BBox.CoordOrigin != CoordOriginBottomLeft {
			return errors.New("docling: PDF 视觉结果坐标原点无效")
		}
		if !isFinitePDFVisualBBox(item.BBox) || item.BBox.L < 0 || item.BBox.B < 0 || item.BBox.R <= item.BBox.L || item.BBox.T <= item.BBox.B ||
			(width > 0 && item.BBox.R > width) || (height > 0 && item.BBox.T > height) {
			return errors.New("docling: PDF 视觉结果坐标越界")
		}
		if item.Label == LabelTable {
			if err := validatePDFVisualTable(item.TableData); err != nil {
				return err
			}
			if err := validatePDFVisualTableCellGeometry(item.TableData, item.BBox, width, height); err != nil {
				return err
			}
			for _, cell := range item.TableData.TableCells {
				visibleText.WriteString(cell.Text)
				visibleText.WriteByte('\n')
				totalRunes += utf8.RuneCountInString(cell.Text)
			}
		} else if strings.TrimSpace(item.Text) == "" {
			return errors.New("docling: PDF 视觉结果文本为空")
		} else {
			visibleText.WriteString(item.Text)
			visibleText.WriteByte('\n')
			totalRunes += utf8.RuneCountInString(item.Text)
		}
		if totalRunes > ocrMaxResultRunes {
			return errors.New("docling: PDF 视觉结果文本过长")
		}
		if item.Label == LabelSectionHeader && (item.Level < 1 || item.Level > 9) {
			return errors.New("docling: PDF 视觉结果标题层级无效")
		}
		fingerprint := pdfVisualItemFingerprint(item)
		for _, previousBBox := range seen[fingerprint] {
			if pdfBBoxIntersectionRatio(previousBBox, item.BBox) >= 0.9 {
				return errors.New("docling: PDF 视觉结果包含重复对象")
			}
		}
		seen[fingerprint] = append(seen[fingerprint], item.BBox)
	}
	content := visibleText.String()
	if containsOCRRefusal(content) {
		return errors.New("docling: PDF 视觉结果包含拒答或无法识别说明")
	}
	if textGarbageRatio(content) >= 0.5 {
		return errors.New("docling: PDF 视觉结果含明显乱码")
	}
	return nil
}

// isFinitePDFVisualNumber 判断模型输出的数值可安全参与几何计算。
func isFinitePDFVisualNumber(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

// isFinitePDFVisualBBox 判断矩形四个坐标均为有限值。
func isFinitePDFVisualBBox(bbox *DoclingBBox) bool {
	return bbox != nil && isFinitePDFVisualNumber(bbox.L) && isFinitePDFVisualNumber(bbox.T) &&
		isFinitePDFVisualNumber(bbox.R) && isFinitePDFVisualNumber(bbox.B)
}

// validatePDFVisualTableCellGeometry 校验模型可选返回的单元格 bbox。允许
// 一点的边界误差，但不允许单元格越出页面或所属表格区域。
func validatePDFVisualTableCellGeometry(table *TableData, tableBBox *DoclingBBox, width, height float64) error {
	const boundaryTolerance = 1.0
	for _, cell := range table.TableCells {
		if cell.BBox == nil {
			continue
		}
		if cell.BBox.CoordOrigin != CoordOriginBottomLeft {
			return errors.New("docling: PDF 视觉表格单元格坐标原点无效")
		}
		if !isFinitePDFVisualBBox(cell.BBox) || cell.BBox.L < 0 || cell.BBox.B < 0 ||
			cell.BBox.R <= cell.BBox.L || cell.BBox.T <= cell.BBox.B ||
			(width > 0 && cell.BBox.R > width) || (height > 0 && cell.BBox.T > height) {
			return errors.New("docling: PDF 视觉表格单元格坐标越界")
		}
		if tableBBox != nil && (cell.BBox.L < tableBBox.L-boundaryTolerance ||
			cell.BBox.B < tableBBox.B-boundaryTolerance || cell.BBox.R > tableBBox.R+boundaryTolerance ||
			cell.BBox.T > tableBBox.T+boundaryTolerance) {
			return errors.New("docling: PDF 视觉表格单元格坐标越出表格区域")
		}
	}
	return nil
}

// pdfVisualItemFingerprint 生成重复对象检测使用的内容指纹；表格包含网格
// 坐标和单元格文本，避免只凭空的 item.text 把不同表格误判为重复。
func pdfVisualItemFingerprint(item PDFVisualItem) string {
	var builder strings.Builder
	builder.WriteString(string(item.Label))
	builder.WriteByte('\x00')
	if item.TableData == nil {
		builder.WriteString(normalizePDFVisualText(item.Text))
		return builder.String()
	}
	builder.WriteString(strconv.FormatInt(item.TableData.NumRows, 10))
	builder.WriteByte('x')
	builder.WriteString(strconv.FormatInt(item.TableData.NumCols, 10))
	for _, cell := range item.TableData.TableCells {
		builder.WriteByte('|')
		builder.WriteString(strconv.FormatInt(cell.StartRowOffsetIdx, 10))
		builder.WriteByte(':')
		builder.WriteString(strconv.FormatInt(cell.StartColOffsetIdx, 10))
		builder.WriteByte(':')
		builder.WriteString(strconv.FormatInt(cell.EndRowOffsetIdx, 10))
		builder.WriteByte(':')
		builder.WriteString(strconv.FormatInt(cell.EndColOffsetIdx, 10))
		builder.WriteByte(':')
		builder.WriteString(normalizePDFVisualText(cell.Text))
	}
	return builder.String()
}

// validatePDFVisualTable 校验视觉表格的尺寸、方向、跨度与网格占用；允许
// 模型省略空白单元格，但至少要有一个非空单元格，且已有单元格不能重叠。
func validatePDFVisualTable(table *TableData) error {
	if table == nil || table.NumRows < 1 || table.NumCols < 1 || table.NumRows > 500 || table.NumCols > 100 {
		return errors.New("docling: PDF 视觉表格尺寸无效")
	}
	switch table.Orientation {
	case TableOrientation0, TableOrientation90, TableOrientation180, TableOrientation270:
	default:
		return errors.New("docling: PDF 视觉表格方向无效")
	}
	if len(table.TableCells) == 0 || int64(len(table.TableCells)) > table.NumRows*table.NumCols {
		return errors.New("docling: PDF 视觉表格单元格数量无效")
	}
	occupied := make(map[int64]struct{}, len(table.TableCells))
	hasText := false
	for _, cell := range table.TableCells {
		if cell.StartRowOffsetIdx < 0 || cell.StartColOffsetIdx < 0 ||
			cell.EndRowOffsetIdx <= cell.StartRowOffsetIdx || cell.EndColOffsetIdx <= cell.StartColOffsetIdx ||
			cell.EndRowOffsetIdx > table.NumRows || cell.EndColOffsetIdx > table.NumCols {
			return errors.New("docling: PDF 视觉表格单元格越界")
		}
		if cell.RowSpan != cell.EndRowOffsetIdx-cell.StartRowOffsetIdx ||
			cell.ColSpan != cell.EndColOffsetIdx-cell.StartColOffsetIdx {
			return errors.New("docling: PDF 视觉表格单元格跨度不一致")
		}
		if strings.TrimSpace(cell.Text) != "" {
			hasText = true
		}
		for row := cell.StartRowOffsetIdx; row < cell.EndRowOffsetIdx; row++ {
			for column := cell.StartColOffsetIdx; column < cell.EndColOffsetIdx; column++ {
				position := row*table.NumCols + column
				if _, exists := occupied[position]; exists {
					return errors.New("docling: PDF 视觉表格单元格重叠")
				}
				occupied[position] = struct{}{}
			}
		}
	}
	if !hasText {
		return errors.New("docling: PDF 视觉表格内容为空")
	}
	return nil
}

// pdfVisualResultDocument 把校验后的视觉结果转换为 Docling 子文档。
func pdfVisualResultDocument(result PDFVisualResult, pageNo int64) *DoclingDocument {
	doc := NewDoclingDocument("pdf-visual")
	var listGroup *RefItem
	for _, item := range result.Items {
		bbox := *item.BBox
		bbox.CoordOrigin = CoordOriginBottomLeft
		prov := []ProvenanceItem{{PageNo: pageNo, BBox: &bbox, CharSpan: [2]int64{0, int64(utf8.RuneCountInString(item.Text))}}}
		switch item.Label {
		case LabelListItem:
			if listGroup == nil {
				ref := doc.AddListGroup("visual-list", nil)
				listGroup = &ref
			}
			doc.AddListItem(*listGroup, item.Text, item.Enumerated, item.Marker, prov)
		case LabelTable:
			listGroup = nil
			ref := doc.AddTable(item.TableData.TableCells, item.TableData.NumRows, item.TableData.NumCols, prov, nil)
			doc.Tables[ref.Idx].Data.Orientation = item.TableData.Orientation
		case LabelPicture:
			listGroup = nil
			ref := doc.AddPicture(nil, prov, nil)
			captionRef := RefItem{Kind: refTexts, Idx: int64(len(doc.Texts))}
			doc.Texts = append(doc.Texts, TextItem{
				SelfRef: captionRef.String(), Parent: &ref, Children: []RefItem{},
				ContentLayer: LayerBody, Label: LabelCaption, Prov: prov,
				Orig: item.Text, Text: item.Text,
			})
			doc.Pictures[ref.Idx].Captions = append(doc.Pictures[ref.Idx].Captions, captionRef)
		case LabelSectionHeader:
			listGroup = nil
			doc.AddHeading(item.Level, item.Text, prov, nil)
		case LabelTitle:
			listGroup = nil
			doc.AddTitle(item.Text, prov, nil)
		case LabelFormula:
			listGroup = nil
			doc.AddFormula(item.Text, prov, nil)
		case LabelCode:
			listGroup = nil
			doc.AddCode(item.Text, "", prov, nil)
		default:
			listGroup = nil
			doc.AddText(item.Label, item.Text, prov, nil)
		}
	}
	return doc
}

// mergePDFVisualPage 按视觉对象 bbox 移除重叠规则文本，再插入结构化子文档，
// 避免复杂表格或公式同时以正文与结构化对象重复出现。
func mergePDFVisualPage(lines []pdfLine, result PDFVisualResult, pageIdx int64) []pdfLine {
	if len(result.Items) == 0 {
		return lines
	}
	visualText := strings.Builder{}
	var union *DoclingBBox
	for _, item := range result.Items {
		union = unionPDFTableBBox(union, item.BBox)
		visualText.WriteString(item.Text)
		visualText.WriteByte('\n')
		if item.TableData != nil {
			for _, cell := range item.TableData.TableCells {
				visualText.WriteString(cell.Text)
				visualText.WriteByte('\n')
			}
		}
	}
	normalizedVisual := normalizePDFVisualText(visualText.String())
	merged := make([]pdfLine, 0, len(lines)+1)
	for _, line := range lines {
		hasBounds := line.MaxX > line.MinX && line.MaxY > line.MinY
		if hasBounds {
			if pdfLineOverlapsVisual(line, result.Items) {
				continue
			}
			// 有可靠坐标的文本只按区域去重。相同短文本可能合法地出现在
			// 页面的其他位置，不能用全页字符串包含关系将其删除。
			merged = append(merged, line)
			continue
		}
		lineText := normalizePDFVisualText(line.Text)
		if lineText != "" && strings.Contains(normalizedVisual, lineText) {
			continue
		}
		merged = append(merged, line)
	}
	placeholder := pdfLine{PageIdx: pageIdx, Fallback: true, OCRSub: pdfVisualResultDocument(result, pageIdx+1)}
	if union != nil {
		placeholder.MinX, placeholder.MaxX = union.L, union.R
		placeholder.MinY, placeholder.MaxY = union.B, union.T
	}
	return append(merged, placeholder)
}

// pdfLineOverlapsVisual 判断规则行中心是否落入任一视觉区域。
func pdfLineOverlapsVisual(line pdfLine, items []PDFVisualItem) bool {
	if line.MaxX <= line.MinX || line.MaxY <= line.MinY {
		return false
	}
	x, y := (line.MinX+line.MaxX)/2, (line.MinY+line.MaxY)/2
	for _, item := range items {
		if item.BBox != nil && x >= item.BBox.L && x <= item.BBox.R && y >= item.BBox.B && y <= item.BBox.T {
			return true
		}
	}
	return false
}

// normalizePDFVisualText 生成区域去重使用的宽松文本指纹。
func normalizePDFVisualText(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(text), ""))
}
