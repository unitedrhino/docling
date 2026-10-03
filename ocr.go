// ocr.go 定义 PDF 解析的外部 OCR 钩子能力：扫描件页与高乱码页可回调
// 调用方注入的识别实现（如大模型视觉识别服务），识别结果以
// Markdown 结构化子文档并入主文档——docling 本身不引入任何 OCR
// 引擎或渲染依赖，识别完全由调用方决定。
package docling

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
)

// defaultPDFOCRGarbageThreshold 页乱码率触发 OCR 重识别的默认阈值。
const defaultPDFOCRGarbageThreshold = 0.3

const (
	// ocrMaxResultRunes 限制单页模型结果长度，防止异常循环输出放大内存。
	ocrMaxResultRunes = 1_000_000
	// ocrRepeatedLineMinCount 是判定同一正文行异常复读的最少出现次数。
	ocrRepeatedLineMinCount = 4
	// ocrRepeatedLineMinTotal 是启用整页低多样性判定的最少有效行数。
	ocrRepeatedLineMinTotal = 8
)

// PageOCRHook 扫描页/高乱码页识别兼容钩子：pageNo 从 1 起，pdfBytes 为
// 原始 PDF 全量字节。新调用方应优先使用 OCRHook 的安全单页 PageData；
// 本钩子返回该页识别文本（推荐 Markdown，可携带标题层级/表格/列表），
// 返回空文本或错误时该页维持原有兜底行为。
type PageOCRHook func(pageNo int64, pdfBytes []byte) (string, error)

// OCRRequest 描述一次页级或图片 OCR 请求，向实现方提供文件上下文与当前
// 规则解析文本，便于实现方选择正确输入方式并做质量比较。
type OCRRequest struct {
	PageNo             int64  // PageNo 是从 1 开始的页号；独立图片固定为 1。
	MIMEType           string // MIMEType 是原文件 MIME。
	Filename           string // Filename 是原文件名。
	Data               []byte // Data 是原始文件字节，作为现有调用方兼容字段保留。
	PageData           []byte // PageData 是 Docling 安全抽取的单页 PDF；图片输入为空。
	ExistingText       string // ExistingText 是 Go Docling 或 Poppler 已提取的页面文本。
	Attempt            int    // Attempt 是从 1 开始的本页调用次数。
	ValidationFeedback string // ValidationFeedback 是前一次失败的纠错提示，首次为空。
}

// OCRHook 是带完整上下文的新 OCR 钩子；与 PageOCRHook 同时配置时优先使用。
type OCRHook func(request OCRRequest) (string, error)

// PDFOptions PDF 解析可选能力。
type PDFOptions struct {
	// Limits 是 PDF 文件与页数限制；非正字段使用 DefaultPDFLimits。
	Limits PDFLimits
	// VisualHook 是复杂页面的结构化视觉钩子；nil 时完全保持纯 Go 路径。
	VisualHook PDFVisualHook
	// VisualAlways 要求对每个有效页尝试结构化视觉；主要用于调用方已自行
	// 控制页范围或图像型表格。默认仅对质量评估命中的疑难页调用。
	VisualAlways bool
	// MaxVisualPages 限制单文档最多调用的视觉页数；0 表示不额外限制。
	MaxVisualPages int
	// OCRHook 是新版 OCR 钩子，优先于 PageOCRHook。
	OCRHook OCRHook
	// PageOCRHook 扫描页/高乱码页识别钩子；nil 时维持纯文本兜底行为。
	PageOCRHook PageOCRHook
	// MIMEType 是原文件 MIME；为空时 PDF 默认 application/pdf。
	MIMEType string
	// Filename 是原文件名，供模型侧保留输入上下文。
	Filename string
	// DisablePopplerFallback 禁用 poppler（pdftotext -bbox-layout）降级提取。
	// 默认（false）启用：ledongthuc 提取为空或乱码超限时自动降级，环境缺失
	// pdftotext 时自动跳过（LookPath 检测，无副作用）。
	DisablePopplerFallback bool
	// DisableEmbeddedImageExtraction 禁用 PDF Image XObject 像素解码；页面
	// 图片几何仍参与质量评估和视觉路由。默认启用纯 Go 图片资产提取。
	DisableEmbeddedImageExtraction bool
	// GarbageThreshold 页乱码率超过该值时触发 hook 重识别；
	// 0（零值）= 仅对无字符内容的扫描兜底页触发。
	GarbageThreshold float64
}

// ocrFenceRe 匹配模型常见的整段 Markdown 代码围栏。
var ocrFenceRe = regexp.MustCompile("(?is)^\\s*```(?:markdown|md)?\\s*\\n?(.*?)\\n?```\\s*$")

// ocrRefusalPhrases 是 OCR 与结构化视觉共用的拒答/无法识别特征。
var ocrRefusalPhrases = []string{
	"抱歉，我无法", "无法识别", "不能识别", "无法提供", "看不清", "没有可识别",
	"i'm sorry", "i am sorry", "cannot recognize", "can't recognize", "unable to recognize",
}

// hasOCRHook 判断新旧 OCR 钩子是否至少配置一个。
func hasOCRHook(opt PDFOptions) bool {
	return opt.OCRHook != nil || opt.PageOCRHook != nil
}

// runOCR 执行新旧 OCR 钩子并内置一次无效结果重试。新钩子优先；旧钩子
// 通过请求中的页号和原始数据适配。已有文本仅在新结果乱码率更低时替换。
func runOCR(opt PDFOptions, request OCRRequest) (string, bool) {
	if request.MIMEType == "" {
		request.MIMEType = opt.MIMEType
	}
	if request.Filename == "" {
		request.Filename = opt.Filename
	}
	for attempt := 0; attempt < 2; attempt++ {
		request.Attempt = attempt + 1
		var (
			result string
			err    error
		)
		if opt.OCRHook != nil {
			result, err = opt.OCRHook(request)
		} else if opt.PageOCRHook != nil {
			result, err = opt.PageOCRHook(request.PageNo, request.Data)
		} else {
			return "", false
		}
		if err != nil {
			request.ValidationFeedback = "上一次模型调用失败，请重新识别并严格按输出协议返回"
			continue
		}
		result = cleanOCRResult(result)
		if validationErr := validateOCRResult(result); validationErr != nil {
			request.ValidationFeedback = "上一次 OCR 结果无效：" + validationErr.Error()
			continue
		}
		if existing := strings.TrimSpace(request.ExistingText); existing != "" &&
			textGarbageRatio(result) >= textGarbageRatio(existing) {
			return "", false
		}
		return result, true
	}
	return "", false
}

// cleanOCRResult 去除模型常见的 Markdown 围栏和结果说明，只保留正文。
func cleanOCRResult(result string) string {
	result = strings.TrimSpace(result)
	if match := ocrFenceRe.FindStringSubmatch(result); match != nil {
		result = strings.TrimSpace(match[1])
	}
	lines := strings.Split(result, "\n")
	if len(lines) > 0 {
		first := strings.TrimSpace(lines[0])
		for _, prefix := range []string{"以下是识别结果", "识别结果如下", "OCR 结果如下", "Here is the OCR result"} {
			if !strings.HasPrefix(strings.ToLower(first), strings.ToLower(prefix)) {
				continue
			}
			remainder := strings.TrimSpace(strings.TrimLeft(first[len(prefix):], "：:"))
			if remainder == "" {
				lines = lines[1:]
			} else {
				lines[0] = remainder
			}
			break
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// validOCRResult 拒绝空结果、模型拒答与明显乱码，避免污染文档正文。
func validOCRResult(result string) bool {
	return validateOCRResult(result) == nil
}

// validateOCRResult 返回可反馈给重试模型的分类校验错误。
func validateOCRResult(result string) error {
	result = strings.TrimSpace(result)
	if result == "" {
		return fmt.Errorf("结果为空")
	}
	if utf8.RuneCountInString(result) > ocrMaxResultRunes {
		return fmt.Errorf("结果过长")
	}
	if textGarbageRatio(result) >= 0.5 {
		return fmt.Errorf("结果含明显乱码")
	}
	if containsOCRRefusal(result) {
		return fmt.Errorf("结果包含拒答或无法识别说明")
	}
	if hasRepeatedOCRLines(result) {
		return fmt.Errorf("结果包含异常重复内容")
	}
	return nil
}

// containsOCRRefusal 判断模型文本是否包含已知拒答或无法识别说明。
func containsOCRRefusal(result string) bool {
	lower := strings.ToLower(result)
	for _, phrase := range ocrRefusalPhrases {
		if strings.Contains(lower, strings.ToLower(phrase)) {
			return true
		}
	}
	return false
}

// hasRepeatedOCRLines 检测大模型常见的逐行循环输出。只统计具有实际文本
// 的行并忽略 Markdown 表格分隔行；连续四次复读，或至少八行中同一行占
// 一半且出现四次以上时才拒绝，避免误伤短列表和规则表格。
func hasRepeatedOCRLines(result string) bool {
	counts := make(map[string]int)
	validLines := 0
	maxCount := 0
	previous := ""
	consecutive := 0
	for _, rawLine := range strings.Split(result, "\n") {
		line := normalizeOCRRepetitionLine(rawLine)
		if line == "" {
			continue
		}
		validLines++
		counts[line]++
		if counts[line] > maxCount {
			maxCount = counts[line]
		}
		if line == previous {
			consecutive++
		} else {
			previous = line
			consecutive = 1
		}
		if consecutive >= ocrRepeatedLineMinCount {
			return true
		}
	}
	return validLines >= ocrRepeatedLineMinTotal &&
		maxCount >= ocrRepeatedLineMinCount && maxCount*2 >= validLines
}

// normalizeOCRRepetitionLine 生成复读检测指纹；空行、过短标记和 GFM
// 表格分隔行不提供足够证据，返回空串跳过。
func normalizeOCRRepetitionLine(line string) string {
	line = strings.ToLower(strings.Join(strings.Fields(line), " "))
	if utf8.RuneCountInString(line) < 4 || isMarkdownTableSeparatorLine(line) {
		return ""
	}
	return line
}

// isMarkdownTableSeparatorLine 判断只由竖线、冒号、连字符和空白构成的
// GFM 表头分隔行，防止合法表格语法触发复读保护。
func isMarkdownTableSeparatorLine(line string) bool {
	if !strings.Contains(line, "|") || !strings.Contains(line, "-") {
		return false
	}
	for _, char := range line {
		switch char {
		case '|', ':', '-', ' ', '\t':
		default:
			return false
		}
	}
	return true
}

// ParsePDFWithOptions 解析 PDF 并支持外部 OCR 钩子（能力全集入口）；
// ParsePDF 等价于 ParsePDFWithOptions(data, PDFOptions{})。
//
// 触发时机（逐页）：
//   - 无字符内容的扫描兜底页（GetPlainText 亦为空）→ 调 hook；
//   - 页文本乱码率（textGarbageRatio）超过 GarbageThreshold → 调 hook 重识别；
//
// hook 成功且返回非空文本时，识别文本经 ParseMarkdown 转为结构化子文档，
// 按页序并入主文档（元素 prov 标注实际页号）；失败或未设置钩子时维持
// 原有行为（扫描页无内容、乱码页保留原文本）。
func ParsePDFWithOptions(data []byte, opt PDFOptions) (*DoclingDocument, error) {
	runtime, err := newPDFRuntime(data, opt.Limits)
	if err != nil {
		return nil, err
	}
	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	doc := NewDoclingDocument("pdf")
	pageCount := reader.NumPage()
	// 文档级元数据尽力填充：PDF 仅可靠取到页数（标题/作者需解析 Info 字典，
	// 取不到不填）。
	doc.Meta = &DocMeta{PageCount: pageCount}
	imagePlacementsByPage := make(map[int64][]pdfImagePlacement, pageCount)
	pageGeometries := make(map[int]pdfPageGeometry, pageCount)
	for i := 1; i <= pageCount; i++ {
		page := reader.Page(i)
		if page.V.IsNull() {
			continue
		}
		geometry := pdfPageGeometryForPage(page)
		pageGeometries[i] = geometry
		if placements := inspectPDFPageImagesWithGeometry(page, geometry); len(placements) > 0 {
			imagePlacementsByPage[int64(i-1)] = placements
		}
	}
	var pictureLines []pdfLine
	if !opt.DisableEmbeddedImageExtraction {
		pictureLines = extractPDFEmbeddedPictureLinesWithContext(runtime.context, imagePlacementsByPage)
	}
	var allLines []pdfLine
	sawText := false
	threshold := opt.GarbageThreshold
	if opt.MIMEType == "" {
		opt.MIMEType = "application/pdf"
	}
	ocrAttempted := make(map[int]bool, pageCount)
	var popplerBk *popplerBackend
	visualPages := 0
	// 各页矢量框线段（显示坐标），供有线表格还原；仅在有文本行时登记消费
	pageEdges := make(map[int64][]pdfRuleEdge, pageCount)
	for i := 1; i <= pageCount; i++ {
		page := reader.Page(i)
		if page.V.IsNull() {
			continue
		}
		geometry := pageGeometries[i]
		if geometry.width <= 0 || geometry.height <= 0 {
			geometry = pdfPageGeometryForPage(page)
		}
		width, height := geometry.width, geometry.height
		doc.AddPage(int64(i), width, height)
		lines := normalizePDFLinesGeometry(assemblePDFPageLines(page, int64(i-1)), geometry)
		// poppler 降级提取（纯规则）：ledongthuc 提取为空或乱码超限时，
		// 尝试 pdftotext -bbox-layout（中文 CMap 支持完善），取乱码率更低者
		pageText := joinPDFLineText(lines)
		if !opt.DisablePopplerFallback &&
			(len(lines) == 0 || textGarbageRatio(pageText) > popplerFallbackGarbageThreshold) {
			if popplerBk == nil {
				popplerBk = &popplerBackend{data: data}
			}
			if pl := tryPopplerFallback(popplerBk, int64(i-1), lines); len(pl) > 0 {
				lines = pl
			}
		}
		// 各页矢量框线段（显示坐标），供有线表格还原；页面无文本行时
		// 表格无从装填文字，跳过提取
		if len(lines) > 0 {
			pageEdges[int64(i-1)] = extractPDFPageRuleEdges(page, geometry)
		}
		// 结构化视觉增强只在显式配置后运行。默认由纯 Go 质量信号触发；
		// 结果经过强校验与一次重试，再按 bbox 区域替换以避免重复正文。
		pageText = joinPDFLineText(lines)
		pageImages := imagePlacementsByPage[int64(i-1)]
		quality := evaluatePDFPageQualityWithGeometry(lines, pageImages, geometry)
		canRunVisual := opt.VisualHook != nil && (opt.MaxVisualPages <= 0 || visualPages < opt.MaxVisualPages)
		if canRunVisual && (opt.VisualAlways || quality.NeedsVisual) {
			visualPages++
			request := PDFVisualRequest{
				PageNo: int64(i), MIMEType: opt.MIMEType, Filename: opt.Filename,
				Data: data, PageData: runtime.singlePageData(i),
				Width: width, Height: height, ExistingText: pageText, Quality: quality,
				EmbeddedImages: pdfVisualImagesForPage(pictureLines, int64(i-1)),
			}
			if visualResult, ok := runPDFVisualHook(opt.VisualHook, request); ok {
				lines = mergePDFVisualPage(lines, visualResult, int64(i-1))
				allLines = append(allLines, lines...)
				sawText = true
				continue
			}
		}
		// 外部 OCR 钩子：扫描页（无任何文本行）必触发；有文本但乱码率
		// 超阈值的页触发重识别（阈值>0 才启用）
		if hasOCRHook(opt) {
			pageText = joinPDFLineText(lines)
			needOCR := len(lines) == 0 ||
				(threshold > 0 && textGarbageRatio(pageText) > threshold)
			if needOCR {
				ocrAttempted[i] = true
				if ocrText, ok := runOCR(opt, OCRRequest{
					PageNo: int64(i), Data: data, PageData: runtime.singlePageData(i), ExistingText: pageText,
				}); ok {
					if sub, subErr := ParseMarkdown([]byte(ocrText)); subErr == nil &&
						len(sub.Texts)+len(sub.Tables) > 0 {
						allLines = append(allLines, pdfLine{
							PageIdx:  int64(i - 1),
							Fallback: true,
							OCRSub:   sub,
						})
						sawText = true
						continue
					}
				}
			}
		}
		if len(lines) == 0 {
			continue
		}
		sawText = true
		allLines = append(allLines, lines...)
	}
	if !sawText {
		// 全文档无可用文本层（扫描件/字体编码不被解析器支持）：先尝试
		// poppler 逐页降级提取（纯规则），仍无内容再逐页交给 OCR 钩子
		//（受调用方成本控制约束）；均无产出时维持报错语义
		if !opt.DisablePopplerFallback && popplerAvailable() {
			bk := &popplerBackend{data: data}
			for i := 1; i <= pageCount; i++ {
				if pl := bk.linesFor(int64(i - 1)); len(pl) > 0 {
					allLines = append(allLines, pl...)
					sawText = true
				}
			}
		}
		if sawText {
			classifyPDFFurnitureLines(allLines, pageCount)
			annotatePDFTOCRegions(allLines)
			allLines = preparePDFLinesForDocument(allLines, pageEdges)
			allLines = mergePDFContinuationTables(allLines, doc.Pages)
			allLines = mergePDFPictureLines(allLines, pictureLines)
			buildPDFDoclingDocument(buildPDFBlocks(buildPDFElements(allLines)), doc)
			if strings.TrimSpace(sanitizeText(doc.Text())) != "" || len(doc.Pictures) > 0 {
				return doc, nil
			}
			return nil, fmt.Errorf("pdf content is empty")
		}
		// 即使没有文本层和 OCR，成功解码的内嵌图片本身也是有效结构，
		// 先写入文档，后续 OCR 子文档继续追加且不会把图片导入判为失败。
		if len(pictureLines) > 0 {
			buildPDFDoclingDocument(buildPDFBlocks(buildPDFElements(pictureLines)), doc)
		}
		if !hasOCRHook(opt) {
			if len(doc.Pictures) > 0 {
				return doc, nil
			}
			return nil, fmt.Errorf("pdf content is empty")
		}
		for i := 1; i <= pageCount; i++ {
			if ocrAttempted[i] {
				continue
			}
			ocrText, ok := runOCR(opt, OCRRequest{
				PageNo: int64(i), Data: data, PageData: runtime.singlePageData(i),
			})
			if !ok {
				continue
			}
			if sub, subErr := ParseMarkdown([]byte(ocrText)); subErr == nil &&
				len(sub.Texts)+len(sub.Tables) > 0 {
				mergeOCRSubDocument(doc, sub, int64(i))
			}
		}
		if len(doc.Texts)+len(doc.Tables)+len(doc.Pictures) == 0 {
			return nil, fmt.Errorf("pdf content is empty")
		}
		return doc, nil
	}
	// 版式增强：先跨页指纹识别页眉/页脚和目录区域，再恢复坐标表格并
	// 应用 XY-cut；版式行与表格均不参与正文字号统计与标题判定。
	classifyPDFFurnitureLines(allLines, pageCount)
	annotatePDFTOCRegions(allLines)
	allLines = preparePDFLinesForDocument(allLines, pageEdges)
	allLines = mergePDFContinuationTables(allLines, doc.Pages)
	allLines = mergePDFPictureLines(allLines, pictureLines)
	buildPDFDoclingDocument(buildPDFBlocks(buildPDFElements(allLines)), doc)
	// 兜底语义保留：产物聚合纯文本为空（如正文全为空白字符）时视为解析失败
	if strings.TrimSpace(sanitizeText(doc.Text())) == "" && len(doc.Pictures) == 0 {
		return nil, fmt.Errorf("pdf content is empty")
	}
	return doc, nil
}

// joinPDFLineText 拼接一组的行文本（乱码率统计用）。
func joinPDFLineText(lines []pdfLine) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Text)
		b.WriteString("\n")
	}
	return b.String()
}

// mergeOCRSubDocument 把 OCR 识别产生的结构化子文档并入主文档：
// 各集合元素追加并修正 self_ref/parent/children 的引用偏移，body 子树
// 追加到主文档 body 末尾；子文档元素缺 prov 时补页级 prov（标注实际页号，
// 保证简化后 PageIdx 正确）。引用偏移方案保留子文档全部内部结构
// （标题层级树/列表分组/表格网格）。
func mergeOCRSubDocument(doc *DoclingDocument, sub *DoclingDocument, pageNo int64) {
	if sub == nil || sub.Body == nil {
		return
	}
	textOff := int64(len(doc.Texts))
	tableOff := int64(len(doc.Tables))
	picOff := int64(len(doc.Pictures))
	groupOff := int64(len(doc.Groups))
	// offsetRef 把子文档引用平移到主文档索引空间。
	offsetRef := func(r RefItem) RefItem {
		switch r.Kind {
		case refTexts:
			r.Idx += textOff
		case refTables:
			r.Idx += tableOff
		case refPictures:
			r.Idx += picOff
		case refGroups:
			r.Idx += groupOff
		case refBody:
			r = doc.bodyRef()
		case refFurniture:
			r = doc.furnitureRef()
		}
		return r
	}
	offsetRefPtr := func(r *RefItem) *RefItem {
		if r == nil {
			return nil
		}
		out := offsetRef(*r)
		return &out
	}
	offsetFineRefs := func(refs []FineRef) []FineRef {
		for index := range refs {
			refs[index].RefItem = offsetRef(refs[index].RefItem)
		}
		return refs
	}
	appendProv := func(prov []ProvenanceItem) []ProvenanceItem {
		if len(prov) > 0 {
			return prov
		}
		if pageNo <= 0 {
			return []ProvenanceItem{}
		}
		return []ProvenanceItem{{PageNo: pageNo}}
	}
	for i := range sub.Texts {
		t := sub.Texts[i]
		t.SelfRef = fmt.Sprintf("#/texts/%d", textOff+int64(i))
		t.Parent = offsetRefPtr(t.Parent)
		t.Children = offsetRefList(t.Children, offsetRef)
		t.Captions = offsetRefList(t.Captions, offsetRef)
		t.References = offsetRefList(t.References, offsetRef)
		t.Footnotes = offsetRefList(t.Footnotes, offsetRef)
		t.Comments = offsetFineRefs(t.Comments)
		t.Prov = appendProv(t.Prov)
		doc.Texts = append(doc.Texts, t)
	}
	for i := range sub.Tables {
		tb := sub.Tables[i]
		tb.SelfRef = fmt.Sprintf("#/tables/%d", tableOff+int64(i))
		tb.Parent = offsetRefPtr(tb.Parent)
		tb.Children = offsetRefList(tb.Children, offsetRef)
		tb.Captions = offsetRefList(tb.Captions, offsetRef)
		tb.References = offsetRefList(tb.References, offsetRef)
		tb.Footnotes = offsetRefList(tb.Footnotes, offsetRef)
		tb.Comments = offsetFineRefs(tb.Comments)
		if tb.Data != nil {
			for cellIndex := range tb.Data.TableCells {
				tb.Data.TableCells[cellIndex].Ref = offsetRefPtr(tb.Data.TableCells[cellIndex].Ref)
			}
		}
		tb.Prov = appendProv(tb.Prov)
		doc.Tables = append(doc.Tables, tb)
	}
	for i := range sub.Pictures {
		p := sub.Pictures[i]
		p.SelfRef = fmt.Sprintf("#/pictures/%d", picOff+int64(i))
		p.Parent = offsetRefPtr(p.Parent)
		p.Children = offsetRefList(p.Children, offsetRef)
		p.Captions = offsetRefList(p.Captions, offsetRef)
		p.References = offsetRefList(p.References, offsetRef)
		p.Footnotes = offsetRefList(p.Footnotes, offsetRef)
		p.Comments = offsetFineRefs(p.Comments)
		p.Prov = appendProv(p.Prov)
		doc.Pictures = append(doc.Pictures, p)
	}
	for i := range sub.Groups {
		g := sub.Groups[i]
		g.SelfRef = fmt.Sprintf("#/groups/%d", groupOff+int64(i))
		g.Parent = offsetRefPtr(g.Parent)
		g.Children = offsetRefList(g.Children, offsetRef)
		doc.Groups = append(doc.Groups, g)
	}
	for _, c := range sub.Body.Children {
		doc.Body.Children = append(doc.Body.Children, offsetRef(c))
	}
	// OCR 页面尺寸登记（子文档 pages 键即页号，通常为 "1"）
	for k, p := range sub.Pages {
		if _, exists := doc.Pages[k]; !exists {
			doc.Pages[k] = p
		}
	}
}

// offsetRefList 引用列表批量平移。
func offsetRefList(refs []RefItem, offset func(RefItem) RefItem) []RefItem {
	if refs == nil {
		return nil
	}
	out := make([]RefItem, 0, len(refs))
	for _, r := range refs {
		out = append(out, offset(r))
	}
	return out
}
