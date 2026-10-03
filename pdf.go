// pdf.go 实现 PDF 纯文本解析：基于 ledongthuc/pdf 的字符级 Content() 提取
// （对应 docling pypdfium2_backend 的 rect 级文本路径，Go 侧为 char 级聚合），
// 按 Y 坐标垂直分行、行内按 X 水平合并，并以字号启发式识别标题，
// 产出 DoclingDocument 文档树。
//
// 有意设计（非复刻误差）：官方无模型 PDF 路径不产 label 与 prov，
// 本项目的 Go 实现由既有字号启发式提供 label/层级，并由行聚合坐标填充
// prov（页号 + BOTTOMLEFT bbox + charspan），以满足知识库切片的定位语义。
//
// 主要模块划分：
//   - 行聚合：assemblePDFPageLines / assemblePDFLines（垂直分行 + 水平合并，
//     同时累计行 bbox 供 prov 填充）；
//   - 版式增强：classifyPDFFurnitureLines（归一化跨页指纹识别页眉/页脚）、
//     annotatePDFTOCRegions（点线+页码行识别目录区域，收进"目次"分组）、
//     封面主标题识别（首页最大字号短行判 title，封面其余大字行降为正文）；
//   - 标题启发式：medianLineFontSize / isPDFHeadingCandidate / buildPDFElements
//     （全文档行字号中位数估正文字号，字号显著更大的短行判为标题，
//     字号降序映射 1-3 级；页眉/页脚/目录行不参与统计与标题判定）；
//   - 块组装：buildPDFBlocks（标题独立成块、正文按页归组并按长度分段）；
//   - 树构建：buildPDFDoclingDocument（标题按层级父挂接表达章节路径）；
//   - 兼容出口：buildPDFContentItems / classifyPDFLines（旧 content_list 视图，
//     供既有测试与统计使用）。
package docling

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/ledongthuc/pdf"
)

// knowledgePDFHeadingMaxRunes 标题行长上限：超过该长度的行即使字号大也
// 不视为标题（正文中的强调行/表格行常伴随较大字号）。
const knowledgePDFHeadingMaxRunes = 80

// pdfMaxPageInheritanceDepth 限制畸形页树的 Parent 链遍历深度。
const pdfMaxPageInheritanceDepth = 64

// PDF 版式增强规则阈值（页眉/页脚、目录区域、封面主标题识别）。
const (
	// knowledgePDFFurnitureRepeatRatio 页眉/页脚跨页重复比例阈值：同一归一化
	// 文本出现在 >=60% 页面的顶部/底部区域才判为版式家具行。
	knowledgePDFFurnitureRepeatRatio = 0.6
	// knowledgePDFFurnitureZoneRatio 页眉/页脚判定区域占页面高度的比例：
	// 行位于按页内行 Y 范围估算的页面高度上 15% 视为顶部区域、下 15% 视为底部区域。
	knowledgePDFFurnitureZoneRatio = 0.15
	// knowledgePDFFurnitureMinPages 页眉/页脚识别启用的最少页数（样本不足不启用）。
	knowledgePDFFurnitureMinPages = 3
	// knowledgePDFFurnitureMaxRunes 页眉/页脚行长上限：超长行不可能是页眉页脚。
	knowledgePDFFurnitureMaxRunes = 100
	// knowledgePDFTOCMinRows 构成目录区域所需的最少目录行数。
	knowledgePDFTOCMinRows = 3
	// knowledgePDFCoverMaxHomeLines 首页行数不超过该值才视为封面页。
	knowledgePDFCoverMaxHomeLines = 15
	// knowledgePDFCoverTitleMaxRunes 封面主标题行长上限。
	knowledgePDFCoverTitleMaxRunes = 40
)

// pdfWord 表示 PDF 页面内由相邻字符聚合出的一个词，坐标语义与所属
// pdfLine 一致；FontSize 取词内最大字号，供后续版面与表格识别使用。
type pdfWord struct {
	Text     string  // 词文本
	MinX     float64 // 词左边缘（页内 pt）
	MaxX     float64 // 词右边缘（页内 pt）
	MinY     float64 // BOTTOMLEFT 坐标系下的词下边缘（字符路径为最低基线）
	MaxY     float64 // BOTTOMLEFT 坐标系下的词上边缘（字符路径为最高基线）
	FontSize float64 // 词内最大字号或 Poppler 词盒高度（pt）
}

// pdfLine 表示 PDF 页面内按坐标聚合出的一行文本。
type pdfLine struct {
	Text        string            // 行文本（行内按 X 坐标排序拼接）
	Words       []pdfWord         // 行内按 X 坐标排序的词及其坐标
	Table       *pdfDetectedTable // 非空：由连续坐标行恢复出的结构化表格占位
	Picture     *ImageRef         // 非空：从 Image XObject 纯 Go 提取的图片资产
	MaxFontSize float64           // 行内最大字号（pt）
	PageIdx     int64             // 所在页（从 0 开始）
	Fallback    bool              // 是否为扫描件/解析兜底页（无字号信息，不参与标题判定）
	OCRSub      *DoclingDocument  // 非空：外部 OCR 钩子返回的识别子文档（结构化并入主文档）
	MinX        float64           // 行左边缘（页内 pt，X 向右递增）
	MaxX        float64           // 行右边缘（含末字符宽度）
	MinY        float64           // BOTTOMLEFT 坐标系下的行下边缘（字符路径为最低基线）
	MaxY        float64           // BOTTOMLEFT 坐标系下的行上边缘（字符路径为最高基线）
	// Furniture 版式家具标记：page_header/page_footer（空为正文行）。
	// 由 classifyPDFFurnitureLines 跨页指纹判定，标记后不参与正文字号
	// 统计、标题判定与章节路径。
	Furniture DocItemLabel
	// InTOC 是否位于目录区域（目录点线行与区域内孤立夹入行），
	// 由 annotatePDFTOCRegions 标记，不参与正文字号统计与标题判定。
	InTOC bool
}

// pdfBlock 表示按阅读顺序组装出的一个块级元素：HeadingLevel>0 为标题行，
// FurnLabel 非空为页眉/页脚行，IsTOC 为目录区域行，IsTitle 为封面主标题行，
// 否则为同页正文行聚合成的段落块；Lines 记录组内行，供 prov 取 bbox 并集。
type pdfBlock struct {
	HeadingLevel int64             // 标题层级（1-3），0 表示非标题块
	FurnLabel    DocItemLabel      // 非空：页眉/页脚独立块
	IsTOC        bool              // 目录区域行独立块（树上收进"目次"分组）
	IsTitle      bool              // 封面主标题独立块（label=title，挂 body）
	Text         string            // 块文本（多行以换行连接）
	PageIdx      int64             // 所在页（从 0 开始）
	Lines        []pdfLine         // 组内行
	OCRSub       *DoclingDocument  // 非空：外部 OCR 识别子文档块（结构化并入）
	Table        *pdfDetectedTable // 非空：结构化表格块
	Picture      *ImageRef         // 非空：带页面绘制 bbox 的内嵌图片块
}

// pdfHeading 表示按字号启发式识别出的 PDF 标题。
type pdfHeading struct {
	Level int64
	Line  pdfLine
}

// pdfElement 表示 PDF 文档顺序中的一个块级元素：headingLevel>0 为标题，
// furnLabel 非空为页眉/页脚，isTOC 为目录区域行，isTitle 为封面主标题，
// 否则为正文行。
type pdfElement struct {
	headingLevel int64
	furnLabel    DocItemLabel
	isTOC        bool
	isTitle      bool
	table        *pdfDetectedTable
	picture      *ImageRef
	line         pdfLine
}

// preparePDFLinesForDocument 在页眉页脚和目录完成标注后恢复表格，并对每页
// 剩余内容应用 XY-cut。表格以携带原始 bbox 的占位行参与版面排序，构建文档时
// 再转成 TableItem；被表格消费的文字行不会重复进入正文。
//
// 表格识别分两层：先由 pageEdges 提供的各页矢量框线做有线表格还原
// （detectPDFRuledTables，Word/WPS 导出的表格边框路径），未被框线表格消费的
// 剩余行再走大留白切列的坐标启发式（detectPDFTables / detectPDFSparseTables，
// 无边框表格）；两层互斥消费，区间下标统一映射回原始行后按文档顺序缝合。
func preparePDFLinesForDocument(lines []pdfLine, pageEdges map[int64][]pdfRuleEdge) []pdfLine {
	if len(lines) == 0 {
		return nil
	}
	var tables []pdfDetectedTable
	ruled, consumed := detectPDFRuledTables(lines, pageEdges)
	if len(ruled) > 0 {
		tables = append(tables, ruled...)
		// 未被框线消费的剩余行继续做无边框表格识别；启发式的 StartLine/
		// EndLine 相对剩余序列，映射回原始行下标后参与统一缝合
		restIdx := make([]int, 0, len(lines))
		rest := make([]pdfLine, 0, len(lines))
		for index := range lines {
			if consumed[index] {
				continue
			}
			restIdx = append(restIdx, index)
			rest = append(rest, lines[index])
		}
		whitespace, _ := detectPDFTables(rest)
		if len(whitespace) == 0 {
			whitespace, _ = detectPDFSparseTables(rest)
		}
		for _, table := range whitespace {
			table.StartLine = restIdx[table.StartLine]
			table.EndLine = restIdx[table.EndLine-1] + 1
			tables = append(tables, table)
		}
	} else {
		whitespace, _ := detectPDFTables(lines)
		if len(whitespace) == 0 {
			whitespace, _ = detectPDFSparseTables(lines)
		}
		tables = whitespace
	}
	if len(tables) == 0 {
		return sortPDFLinesByPageXYCut(lines)
	}

	tableByStart := make(map[int]int, len(tables))
	for i := range tables {
		tableByStart[tables[i].StartLine] = i
	}
	prepared := make([]pdfLine, 0, len(lines))
	for i := 0; i < len(lines); {
		tableIndex, ok := tableByStart[i]
		if !ok {
			prepared = append(prepared, lines[i])
			i++
			continue
		}
		table := &tables[tableIndex]
		placeholder := pdfLine{PageIdx: table.PageIdx, Table: table}
		if table.BBox != nil {
			placeholder.MinX = table.BBox.L
			placeholder.MaxX = table.BBox.R
			placeholder.MinY = table.BBox.B
			placeholder.MaxY = table.BBox.T
		}
		prepared = append(prepared, placeholder)
		i = table.EndLine
	}
	return sortPDFLinesByPageXYCut(prepared)
}

// ParsePDF 解析 PDF 为 DoclingDocument（复刻 docling pypdfium2_backend 的
// PDF 纯文本路径，label/层级与 prov 为本项目有意增强）：
//   - 基于 ledongthuc/pdf 的 Content() 拿到带字号与坐标的字符，按 Y 聚合成行；
//   - 以全文档行字号中位数为正文字号，字号显著更大且行长短的行识别为标题，
//     字号降序映射层级（最大 1 级，最多 3 级），标题按层级父挂接表达章节路径；
//   - 正文挂最近的祖先标题（无则挂 body），单页正文超长时分段，
//     元素 prov 取段内各行 bbox 的并集；
//   - 无字符内容（扫描件）的页回退整页纯文本，产出无 prov 的 text 元素兜底；
//   - 每个有效页按可见 CropBox、UserUnit 和 Rotate 登记显示尺寸，
//     文本、表格、图片与视觉结果共用从零开始的 BOTTOMLEFT 坐标。
//
// 解析失败或解析后无文本内容时返回 error。
// 需要外部 OCR（扫描页/乱码页回调识别服务）时使用 ParsePDFWithOptions。
func ParsePDF(data []byte) (*DoclingDocument, error) {
	return ParsePDFWithOptions(data, PDFOptions{})
}

// pdfPageGeometry 描述 PDF 默认用户空间到可见页面坐标的映射。
// 输出始终使用从 (0,0) 开始的 BOTTOMLEFT 坐标，并已应用 CropBox、
// UserUnit 与页面顺时针旋转。
type pdfPageGeometry struct {
	minX, minY float64   // minX/minY 是与 MediaBox 相交后的可见框原点。
	maxX, maxY float64   // maxX/maxY 是可见框右上角。
	width      float64   // width 是旋转后显示页宽，单位 pt。
	height     float64   // height 是旋转后显示页高，单位 pt。
	userUnit   float64   // userUnit 是 PDF 用户单位相对 1/72 英寸的倍率。
	rotation   int       // rotation 是归一化到 0/90/180/270 的顺时针角度。
	transform  pdfAffine // transform 是内容坐标到显示坐标的仿射矩阵。
}

// pdfPageGeometryForPage 读取可继承的 MediaBox/CropBox/Rotate，
// 并从当前页字典读取不可继承的 UserUnit。
// 畸形 CropBox 回退 MediaBox，非 90 度倍数的 Rotate 忽略，避免猜测。
func pdfPageGeometryForPage(page pdf.Page) pdfPageGeometry {
	media, ok := pdfPageBox(pdfInheritedPageValue(page, "MediaBox"))
	if !ok {
		return pdfPageGeometry{userUnit: 1, transform: identityPDFAffine()}
	}
	visible := media
	if crop, cropOK := pdfPageBox(pdfInheritedPageValue(page, "CropBox")); cropOK {
		candidate := [4]float64{
			math.Max(media[0], crop[0]), math.Max(media[1], crop[1]),
			math.Min(media[2], crop[2]), math.Min(media[3], crop[3]),
		}
		if candidate[2] > candidate[0] && candidate[3] > candidate[1] {
			visible = candidate
		}
	}
	unit := page.V.Key("UserUnit").Float64()
	if !isFinitePDFGeometry(unit) || unit <= 0 || unit > 75000 {
		unit = 1
	}
	rotation := int(pdfInheritedPageValue(page, "Rotate").Int64() % 360)
	if rotation < 0 {
		rotation += 360
	}
	if rotation%90 != 0 {
		rotation = 0
	}
	baseWidth := (visible[2] - visible[0]) * unit
	baseHeight := (visible[3] - visible[1]) * unit
	if !isFinitePDFGeometry(baseWidth) || !isFinitePDFGeometry(baseHeight) || baseWidth <= 0 || baseHeight <= 0 {
		return pdfPageGeometry{userUnit: 1, transform: identityPDFAffine()}
	}
	geometry := pdfPageGeometry{
		minX: visible[0], minY: visible[1], maxX: visible[2], maxY: visible[3],
		width: baseWidth, height: baseHeight, userUnit: unit, rotation: rotation,
	}
	switch rotation {
	case 90:
		geometry.width, geometry.height = baseHeight, baseWidth
		geometry.transform = pdfAffine{{0, -unit, 0}, {unit, 0, 0}, {-visible[1] * unit, visible[2] * unit, 1}}
	case 180:
		geometry.transform = pdfAffine{{-unit, 0, 0}, {0, -unit, 0}, {visible[2] * unit, visible[3] * unit, 1}}
	case 270:
		geometry.width, geometry.height = baseHeight, baseWidth
		geometry.transform = pdfAffine{{0, unit, 0}, {-unit, 0, 0}, {visible[3] * unit, -visible[0] * unit, 1}}
	default:
		geometry.transform = pdfAffine{{unit, 0, 0}, {0, unit, 0}, {-visible[0] * unit, -visible[1] * unit, 1}}
	}
	return geometry
}

// pdfInheritedPageValue 沿页树读取首个非空页属性。
func pdfInheritedPageValue(page pdf.Page, key string) pdf.Value {
	for value, depth := page.V, 0; !value.IsNull() && depth < pdfMaxPageInheritanceDepth; value, depth = value.Key("Parent"), depth+1 {
		if inherited := value.Key(key); !inherited.IsNull() {
			return inherited
		}
	}
	return pdf.Value{}
}

// pdfPageBox 解析四元页面框并归一化反向坐标。
func pdfPageBox(value pdf.Value) ([4]float64, bool) {
	if value.IsNull() || value.Len() < 4 {
		return [4]float64{}, false
	}
	x0, y0 := value.Index(0).Float64(), value.Index(1).Float64()
	x1, y1 := value.Index(2).Float64(), value.Index(3).Float64()
	if !isFinitePDFGeometry(x0) || !isFinitePDFGeometry(y0) ||
		!isFinitePDFGeometry(x1) || !isFinitePDFGeometry(y1) {
		return [4]float64{}, false
	}
	box := [4]float64{math.Min(x0, x1), math.Min(y0, y1), math.Max(x0, x1), math.Max(y0, y1)}
	return box, box[2] > box[0] && box[3] > box[1]
}

// isFinitePDFGeometry 拒绝不能进入坐标计算的 NaN 与无穷值。
func isFinitePDFGeometry(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

// transformBBox 映射矩形四角并裁剪到显示页范围。
func (geometry pdfPageGeometry) transformBBox(minX, minY, maxX, maxY float64) *DoclingBBox {
	if geometry.width <= 0 || geometry.height <= 0 || !isFinitePDFGeometry(minX) ||
		!isFinitePDFGeometry(minY) || !isFinitePDFGeometry(maxX) || !isFinitePDFGeometry(maxY) {
		return nil
	}
	minX, maxX = math.Min(minX, maxX), math.Max(minX, maxX)
	minY, maxY = math.Min(minY, maxY), math.Max(minY, maxY)
	points := [][2]float64{{minX, minY}, {minX, maxY}, {maxX, minY}, {maxX, maxY}}
	left, bottom := math.Inf(1), math.Inf(1)
	right, top := math.Inf(-1), math.Inf(-1)
	for _, point := range points {
		x, y := transformPDFPoint(geometry.transform, point[0], point[1])
		left, bottom = math.Min(left, x), math.Min(bottom, y)
		right, top = math.Max(right, x), math.Max(top, y)
	}
	left, right = math.Max(0, left), math.Min(geometry.width, right)
	bottom, top = math.Max(0, bottom), math.Min(geometry.height, top)
	if right < left || top < bottom {
		return nil
	}
	return &DoclingBBox{L: left, B: bottom, R: right, T: top, CoordOrigin: CoordOriginBottomLeft}
}

// normalizePDFLinesGeometry 将字符后端的原始行/词坐标转换到显示页。
// 无坐标的纯文本回退行保持不变，后续由结构化视觉补位。
func normalizePDFLinesGeometry(lines []pdfLine, geometry pdfPageGeometry) []pdfLine {
	if len(lines) == 0 || geometry.width <= 0 || geometry.height <= 0 {
		return lines
	}
	normalized := lines[:0]
	for index := range lines {
		line := lines[index]
		if line.Fallback {
			normalized = append(normalized, line)
			continue
		}
		bbox := geometry.transformBBox(line.MinX, line.MinY, line.MaxX, line.MaxY)
		if bbox == nil {
			continue
		}
		line.MinX, line.MinY, line.MaxX, line.MaxY = bbox.L, bbox.B, bbox.R, bbox.T
		line.MaxFontSize *= geometry.userUnit
		words := line.Words[:0]
		for wordIndex := range line.Words {
			word := line.Words[wordIndex]
			wordBBox := geometry.transformBBox(word.MinX, word.MinY, word.MaxX, word.MaxY)
			if wordBBox == nil {
				continue
			}
			word.MinX, word.MinY, word.MaxX, word.MaxY = wordBBox.L, wordBBox.B, wordBBox.R, wordBBox.T
			word.FontSize *= geometry.userUnit
			words = append(words, word)
		}
		line.Words = words
		normalized = append(normalized, line)
	}
	// 单栏页在 XY-cut 未找到栏间空白时会保持输入顺序，因此在应用旋转后先恢复
	// 基础的从上到下、同基线从左到右顺序；多栏页随后仍由 XY-cut 分栏。
	sort.SliceStable(normalized, func(i, j int) bool {
		if normalized[i].MaxY != normalized[j].MaxY {
			return normalized[i].MaxY > normalized[j].MaxY
		}
		return normalized[i].MinX < normalized[j].MinX
	})
	return normalized
}

// assemblePDFPageLines 组装单页文本行；无字符内容时回退整页纯文本（扫描件兜底）。
func assemblePDFPageLines(page pdf.Page, pageIdx int64) []pdfLine {
	content, contentOK := safePDFPageContent(page)
	if contentOK && len(content.Text) > 0 {
		return recoverPDFUnicodeLines(page, pageIdx, assemblePDFLines(content.Text, pageIdx))
	}
	plain, plainOK := safePDFPagePlainText(page)
	if !plainOK || strings.TrimSpace(plain) == "" {
		return recoverPDFUnicodeLines(page, pageIdx, nil)
	}
	return recoverPDFUnicodeLines(page, pageIdx, []pdfLine{{Text: sanitizeText(plain), PageIdx: pageIdx, Fallback: true}})
}

// safePDFPageContent 隔离第三方内容/CMap 解释器 panic；失败后由纯 Go
// Unicode 恢复、Poppler 或大模型按既有质量链路继续处理。
func safePDFPageContent(page pdf.Page) (content pdf.Content, ok bool) {
	defer func() {
		if recover() != nil {
			content, ok = pdf.Content{}, false
		}
	}()
	return page.Content(), true
}

// safePDFPagePlainText 隔离纯文本降级路径中的第三方解析异常。
func safePDFPagePlainText(page pdf.Page) (text string, ok bool) {
	defer func() {
		if recover() != nil {
			text, ok = "", false
		}
	}()
	text, err := page.GetPlainText(nil)
	return text, err == nil
}

// pdfLineTolerance 同视觉行的基线抖动容差（pt）。
const pdfLineTolerance = 2.0

// assemblePDFLines 把页内字符按 Y 坐标聚合成行（Y 大在页面上方，故行间降序即阅读顺序），
// 行内按 X 坐标升序拼接；同一行允许 ±pdfLineTolerance 的基线抖动。
//
// 实现要点（修复历史缺陷）：
//  1. 先按 Y 升序粗排做「容差分桶」聚类，而不是用浮点精确 Y 比较直接排序——
//     同一视觉行的字符存在微小 Y 抖动（旋转/基线/生成器差异），精确比较会把
//     整行字符按 Y 误排，实测表现为整段英文/中文倒序与乱码；
//  2. 桶内按 X 升序拼接，桶间按基准 Y 降序输出，保证阅读顺序；
//  3. 行内拼接时按字符 X 间隙生成空格（PDF 内容流常不含空格字符，
//     对齐 docling.rs conformance 的 generated spaces 规则）。
func assemblePDFLines(chars []pdf.Text, pageIdx int64) []pdfLine {
	filtered := make([]pdf.Text, 0, len(chars))
	for _, c := range chars {
		if strings.TrimSpace(c.S) == "" {
			continue
		}
		filtered = append(filtered, c)
	}
	if len(filtered) == 0 {
		return nil
	}

	// 第一步：按 Y 升序粗排后做容差分桶聚类。
	sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].Y < filtered[j].Y })
	buckets := make([][]pdf.Text, 0, 16)
	var cur []pdf.Text
	var baseY float64
	for _, c := range filtered {
		if len(cur) > 0 && math.Abs(c.Y-baseY) > pdfLineTolerance {
			buckets = append(buckets, cur)
			cur = nil
		}
		if len(cur) == 0 {
			baseY = c.Y // 桶基准取首字符 Y
		}
		cur = append(cur, c)
	}
	if len(cur) > 0 {
		buckets = append(buckets, cur)
	}

	// 第二步：桶间按基准 Y 降序（页面上方先出）。
	sort.SliceStable(buckets, func(i, j int) bool {
		return bucketBaseY(buckets[i]) > bucketBaseY(buckets[j])
	})

	// 第三步：桶内按 X 升序拼接为行，含空格生成。
	lines := make([]pdfLine, 0, len(buckets))
	for _, bucket := range buckets {
		sort.SliceStable(bucket, func(i, j int) bool { return bucket[i].X < bucket[j].X })
		line := pdfLine{PageIdx: pageIdx}
		var word pdfWord
		for i, c := range bucket {
			if i == 0 {
				line.Text = c.S
				line.MinX, line.MaxX = c.X, c.X+c.W
				line.MinY, line.MaxY = c.Y, c.Y
				line.MaxFontSize = c.FontSize
				word = newPDFWord(c)
				continue
			}
			prev := bucket[i-1]
			gap := c.X - (prev.X + prev.W)
			separated := gap > math.Max(prev.W, c.W)*0.3
			// 空格生成：与前一字符的间隙超过字符宽 0.3 倍时补一个空格；
			// 中文字符（>U+2E7F）与中英边界不补，避免中文被拆散。
			if separated && !endsWithCJK(line.Text) && !startsWithCJK(c.S) {
				line.Text += " "
			}
			line.Text += c.S
			// 词边界只依赖物理间隙，不受行文本的中英文空格抑制影响，确保
			// 中文表格的相邻列也能保留为独立词盒。
			if separated {
				word.Text = normalizePDFExtractText(word.Text)
				line.Words = append(line.Words, word)
				word = newPDFWord(c)
			} else {
				appendPDFWordText(&word, c)
			}
			if c.FontSize > line.MaxFontSize {
				line.MaxFontSize = c.FontSize
			}
			if c.X < line.MinX {
				line.MinX = c.X
			}
			if c.X+c.W > line.MaxX {
				line.MaxX = c.X + c.W
			}
			if c.Y < line.MinY {
				line.MinY = c.Y
			}
			if c.Y > line.MaxY {
				line.MaxY = c.Y
			}
		}
		word.Text = normalizePDFExtractText(word.Text)
		line.Words = append(line.Words, word)
		line.Text = normalizePDFExtractText(line.Text)
		lines = append(lines, line)
	}
	return lines
}

// newPDFWord 用首个字符初始化词文本、坐标和字号。
func newPDFWord(c pdf.Text) pdfWord {
	return pdfWord{
		Text:     c.S,
		MinX:     c.X,
		MaxX:     c.X + c.W,
		MinY:     c.Y,
		MaxY:     c.Y,
		FontSize: c.FontSize,
	}
}

// appendPDFWordText 把相邻字符并入当前词，并扩展词 bbox 与最大字号。
func appendPDFWordText(word *pdfWord, c pdf.Text) {
	word.Text += c.S
	if c.X < word.MinX {
		word.MinX = c.X
	}
	if c.X+c.W > word.MaxX {
		word.MaxX = c.X + c.W
	}
	if c.Y < word.MinY {
		word.MinY = c.Y
	}
	if c.Y > word.MaxY {
		word.MaxY = c.Y
	}
	if c.FontSize > word.FontSize {
		word.FontSize = c.FontSize
	}
}

// bucketBaseY 返回桶的基准 Y（首字符基线）。
func bucketBaseY(bucket []pdf.Text) float64 {
	if len(bucket) == 0 {
		return 0
	}
	return bucket[0].Y
}

// endsWithCJK 判断文本末字符是否为 CJK（含扩展区）；CJK 之间不生成空格。
func endsWithCJK(s string) bool {
	r := []rune(s)
	if len(r) == 0 {
		return false
	}
	return isCJKCodepoint(r[len(r)-1])
}

// startsWithCJK 判断文本首字符是否为 CJK。
func startsWithCJK(s string) bool {
	for _, r := range s {
		return isCJKCodepoint(r)
	}
	return false
}

// isCJKCodepoint 判断码点是否属于 CJK 统一表意文字及其兼容/扩展区。
func isCJKCodepoint(r rune) bool {
	return (r >= 0x2E80 && r <= 0x9FFF) || // CJK 部首/笔画/统一表意文字
		(r >= 0x3400 && r <= 0x4DBF) ||
		(r >= 0xF900 && r <= 0xFAFF) || // 兼容表意文字
		(r >= 0x20000 && r <= 0x3FFFF) // 扩展 B-F
}

// normalizePDFExtractText 对 PDF 提取文本做读取级归一化：
// 连字折解（ﬁ→fi 等）、去除组合变音符前的分隔伪影；在行文本拼接后调用。
func normalizePDFExtractText(s string) string {
	replacer := strings.NewReplacer(
		"\uFB01", "fi", "\uFB02", "fl", "\uFB00", "ff", "\uFB03", "ffi", "\uFB04", "ffl",
		"\u00AD", "", // 软连字符
	)
	return replacer.Replace(s)
}

// normalizePDFLineFingerprint 把行文本归一化为跨页指纹键：
// 移除全部空白字符并把全角 ASCII（U+FF01-U+FF5E）折为半角，
// 消除页眉页脚在空格宽度与全半角上的排版差异。
// 返回空串表示无有效字符（调用方应跳过）。
func normalizePDFLineFingerprint(text string) string {
	var b strings.Builder
	for _, r := range text {
		if unicode.IsSpace(r) {
			continue
		}
		if r >= 0xFF01 && r <= 0xFF5E {
			r -= 0xFEE0
		}
		b.WriteRune(r)
	}
	return b.String()
}

// classifyPDFFurnitureLines 跨页指纹识别页眉/页脚行，结果写入行 Furniture 字段：
//   - 行文本归一化（normalizePDFLineFingerprint）后，同一指纹出现在
//     >=60% 页面的顶部区域判 page_header、底部区域判 page_footer；
//   - 顶部/底部区域按该页行 Y 范围估算的页面高度上/下 15% 划分
//     （库内 Y 向上递增，行 MaxY 靠近页顶为顶部、MinY 靠近页底为底部）；
//   - 页数 < knowledgePDFFurnitureMinPages 时样本不足不启用；
//   - 扫描件兜底行（无坐标）、空指纹行与超长行（>100 字）不参与。
//
// 参数 lines 为全文档行，pageCount 为文档总页数。
func classifyPDFFurnitureLines(lines []pdfLine, pageCount int) {
	if pageCount < knowledgePDFFurnitureMinPages || len(lines) == 0 {
		return
	}
	// 每页行 Y 范围估算页面几何：MaxY 最大为页顶、MinY 最小为页底
	type pdfPageGeo struct{ top, bottom float64 }
	geos := map[int64]pdfPageGeo{}
	for _, l := range lines {
		if l.Fallback {
			continue
		}
		g, ok := geos[l.PageIdx]
		if !ok {
			g = pdfPageGeo{top: l.MaxY, bottom: l.MinY}
		} else {
			if l.MaxY > g.top {
				g.top = l.MaxY
			}
			if l.MinY < g.bottom {
				g.bottom = l.MinY
			}
		}
		geos[l.PageIdx] = g
	}
	// inTopZone/inBottomZone 判断行是否落入该页的顶部/底部判定区域
	inTopZone := func(l pdfLine) bool {
		g, ok := geos[l.PageIdx]
		if !ok {
			return false
		}
		if height := g.top - g.bottom; height > 0 {
			return l.MaxY >= g.top-knowledgePDFFurnitureZoneRatio*height
		}
		return false
	}
	inBottomZone := func(l pdfLine) bool {
		g, ok := geos[l.PageIdx]
		if !ok {
			return false
		}
		if height := g.top - g.bottom; height > 0 {
			return l.MinY <= g.bottom+knowledgePDFFurnitureZoneRatio*height
		}
		return false
	}
	// 指纹计数：归一化文本 → 出现过的页集合（页内取一次）
	minPages := int(math.Ceil(float64(pageCount) * knowledgePDFFurnitureRepeatRatio))
	seenTop := map[string]map[int64]bool{}
	seenBottom := map[string]map[int64]bool{}
	for _, l := range lines {
		if l.Fallback || len([]rune(l.Text)) > knowledgePDFFurnitureMaxRunes {
			continue
		}
		key := normalizePDFLineFingerprint(l.Text)
		if key == "" {
			continue
		}
		if inTopZone(l) {
			if seenTop[key] == nil {
				seenTop[key] = map[int64]bool{}
			}
			seenTop[key][l.PageIdx] = true
		}
		if inBottomZone(l) {
			if seenBottom[key] == nil {
				seenBottom[key] = map[int64]bool{}
			}
			seenBottom[key][l.PageIdx] = true
		}
	}
	// 第二遍按行落点打标：同一文本也出现在正文区时不误标（页眉只认顶部区域行）
	for i := range lines {
		l := &lines[i]
		if l.Fallback || len([]rune(l.Text)) > knowledgePDFFurnitureMaxRunes {
			continue
		}
		key := normalizePDFLineFingerprint(l.Text)
		if key == "" {
			continue
		}
		switch {
		case inTopZone(*l) && len(seenTop[key]) >= minPages:
			l.Furniture = LabelPageHeader
		case inBottomZone(*l) && len(seenBottom[key]) >= minPages:
			l.Furniture = LabelPageFooter
		}
	}
}

// pdfTOCLineRe 目录行形态：前缀文本（1-80 字）+ 连续点线（半角句点/全角句点/
// 间隔号/省略号，>=4 个）+ 行尾页码（1-4 位数字）。
var pdfTOCLineRe = regexp.MustCompile(`^(.{1,80}?[.．·…]{4,})\s*([0-9]{1,4})\s*$`)

// annotatePDFTOCRegions 标记目录区域，结果写入行 InTOC 字段：
// 连续 >=knowledgePDFTOCMinRows 行目录行（允许区域中间孤立夹入 1 行非目录行）
// 构成目录区域，区域内所有非版式家具行标记 InTOC；已判为页眉/页脚的行
// 不作为目录行参与（版式优先）。标记后各行不参与正文字号统计与标题判定。
func annotatePDFTOCRegions(lines []pdfLine) {
	isTOC := make([]bool, len(lines))
	for i, l := range lines {
		isTOC[i] = l.Furniture == "" && pdfTOCLineRe.MatchString(l.Text)
	}
	for i := 0; i < len(lines); {
		if !isTOC[i] {
			i++
			continue
		}
		// 从 i 出发扩展区域：目录行推进边界，孤立非目录行（后随目录行）跳过夹入
		end, tocCount := i, 0
		for j := i; j < len(lines); {
			if isTOC[j] {
				tocCount++
				end = j
				j++
				continue
			}
			if j+1 < len(lines) && isTOC[j+1] {
				j++
				continue
			}
			break
		}
		if tocCount >= knowledgePDFTOCMinRows {
			for k := i; k <= end; k++ {
				if lines[k].Furniture == "" {
					lines[k].InTOC = true
				}
			}
			i = end + 1
			continue
		}
		// 不足最少行数不构成区域：仅推进一行，避免漏检后续区域起点
		i++
	}
}

// medianLineFontSize 取行字号的中位数作为正文字号估计（忽略扫描件兜底行）。
func medianLineFontSize(lines []pdfLine) float64 {
	sizes := make([]float64, 0, len(lines))
	for _, l := range lines {
		if !l.Fallback && l.MaxFontSize > 0 {
			sizes = append(sizes, l.MaxFontSize)
		}
	}
	if len(sizes) == 0 {
		return 0
	}
	sort.Float64s(sizes)
	return sizes[len(sizes)/2]
}

// isPDFHeadingCandidate 判断行是否为标题候选：字号显著大于正文（≥1.5pt）
// 且行长度受限。扫描件兜底行与无字号文档（bodySize=0）不参与判定。
func isPDFHeadingCandidate(line pdfLine, bodySize float64) bool {
	if line.Fallback || bodySize <= 0 {
		return false
	}
	return line.MaxFontSize-bodySize >= 1.5 && len([]rune(line.Text)) <= knowledgePDFHeadingMaxRunes
}

// buildPDFElements 把全文档行分类为有序元素流：
// 第一遍统计正文字号与标题字号排名（字号降序映射 1/2/3 级），
// 第二遍按文档顺序产出页眉/页脚、目录区域、封面主标题、标题、正文元素。
// 参与正文字号统计与标题判定的行先剔除页眉/页脚行（Furniture）与
// 目录区域行（InTOC）；封面主标题识别见 findPDFCoverTitleIdx。
func buildPDFElements(allLines []pdfLine) []pdfElement {
	// 正文统计范围：剔除版式家具行与目录区域行
	statLines := make([]pdfLine, 0, len(allLines))
	homeLines := 0
	for _, l := range allLines {
		if l.PageIdx == 0 && l.Picture == nil {
			homeLines++
		}
		if l.Table == nil && l.Picture == nil && l.Furniture == "" && !l.InTOC {
			statLines = append(statLines, l)
		}
	}
	bodySize := medianLineFontSize(statLines)
	coverTitleIdx := findPDFCoverTitleIdx(allLines, statLines, homeLines)

	// 标题字号去重降序排名；封面页整体退出标题通道（主标题走 title、
	// 其余大字行降为正文），不参与层级排名（避免封面装饰字挤占正文标题层级）
	sizeRank := map[float64]int64{}
	if bodySize > 0 {
		var headingSizes []float64
		for _, l := range allLines {
			if l.Table != nil || l.Picture != nil || l.Furniture != "" || l.InTOC {
				continue
			}
			if coverTitleIdx >= 0 && l.PageIdx == 0 {
				continue
			}
			if isPDFHeadingCandidate(l, bodySize) {
				headingSizes = append(headingSizes, l.MaxFontSize)
			}
		}
		sort.Float64s(headingSizes)
		var uniqDesc []float64
		for i := len(headingSizes) - 1; i >= 0; i-- {
			if i < len(headingSizes)-1 && headingSizes[i] == headingSizes[i+1] {
				continue
			}
			uniqDesc = append(uniqDesc, headingSizes[i])
		}
		for i, size := range uniqDesc {
			level := int64(i + 1)
			if level > 3 {
				level = 3
			}
			sizeRank[size] = level
		}
	}

	elements := make([]pdfElement, 0, len(allLines))
	for i, l := range allLines {
		switch {
		case l.Picture != nil:
			elements = append(elements, pdfElement{picture: l.Picture, line: l})
		case l.Table != nil:
			elements = append(elements, pdfElement{table: l.Table, line: l})
		case l.Furniture != "":
			elements = append(elements, pdfElement{furnLabel: l.Furniture, line: l})
		case l.InTOC:
			elements = append(elements, pdfElement{isTOC: true, line: l})
		case i == coverTitleIdx:
			elements = append(elements, pdfElement{isTitle: true, line: l})
		case coverTitleIdx >= 0 && l.PageIdx == 0:
			// 封面页其余大字行（版本号/日期等）降为普通正文，消除误判标题
			elements = append(elements, pdfElement{line: l})
		default:
			if level, ok := sizeRank[l.MaxFontSize]; ok {
				elements = append(elements, pdfElement{headingLevel: level, line: l})
				continue
			}
			elements = append(elements, pdfElement{line: l})
		}
	}
	return elements
}

// findPDFCoverTitleIdx 识别封面主标题行，返回其在 allLines 中的下标（-1 为无）：
// 首页（page 1）行数 < knowledgePDFCoverMaxHomeLines，且存在字号为全文档最大
// （按剔除版式/目录行后的 statLines 统计）、长度 <=knowledgePDFCoverTitleMaxRunes
// 的行时，取文档顺序首个满足行判为封面主标题；无字号信息时不启用。
func findPDFCoverTitleIdx(allLines, statLines []pdfLine, homeLines int) int {
	if homeLines >= knowledgePDFCoverMaxHomeLines || len(statLines) == 0 {
		return -1
	}
	maxSize := 0.0
	for _, l := range statLines {
		if l.MaxFontSize > maxSize {
			maxSize = l.MaxFontSize
		}
	}
	if maxSize <= 0 {
		return -1
	}
	for i, l := range allLines {
		if l.PageIdx != 0 || l.Table != nil || l.Furniture != "" || l.InTOC {
			continue
		}
		if l.MaxFontSize == maxSize && len([]rune(l.Text)) <= knowledgePDFCoverTitleMaxRunes {
			return i
		}
	}
	return -1
}

// knowledgePDFBodyChunkRunes 单个正文块的最大字符数（rune）：超长页按此分段，
// 避免单页大段文本生成巨型元素。
const knowledgePDFBodyChunkRunes = 900

// buildPDFBlocks 把元素流组装为块序列（标题、正文段落与版式块的统一中间表示）：
// 标题行、页眉/页脚行、目录区域行、封面主标题行各自独立成块；
// 正文按页归组（页切换即分段），单页正文按 knowledgePDFBodyChunkRunes 分段，
// 组内行记录于 Lines 供 prov 取 bbox 并集；
// 扫描件兜底行走同一正文通道（单行成块，以 Lines[0].Fallback 标记）。
func buildPDFBlocks(elements []pdfElement) []pdfBlock {
	blocks := make([]pdfBlock, 0, len(elements))
	var bodyBuf strings.Builder
	var bodyLines []pdfLine
	bodyPage := int64(-1)
	flushBody := func() {
		text := strings.TrimSpace(bodyBuf.String())
		page := bodyPage
		lines := bodyLines
		bodyBuf.Reset()
		bodyLines = nil
		bodyPage = -1
		if text == "" {
			return
		}
		blocks = append(blocks, pdfBlock{Text: text, PageIdx: page, Lines: lines})
	}
	// appendSingleLineBlock 版式类单行独立块（页眉页脚/目录/封面主标题），
	// 产出前先冲刷正文缓冲保证文档顺序
	appendSingleLineBlock := func(b pdfBlock, line pdfLine) {
		flushBody()
		b.Text = line.Text
		b.PageIdx = line.PageIdx
		b.Lines = []pdfLine{line}
		blocks = append(blocks, b)
	}
	for _, el := range elements {
		switch {
		case el.picture != nil:
			appendSingleLineBlock(pdfBlock{Picture: el.picture}, el.line)
		case el.table != nil:
			appendSingleLineBlock(pdfBlock{Table: el.table}, el.line)
		case el.line.OCRSub != nil:
			// 外部 OCR 识别子文档块：独立透传，主文档构建时结构化并入
			appendSingleLineBlock(pdfBlock{OCRSub: el.line.OCRSub}, el.line)
		case el.furnLabel != "":
			appendSingleLineBlock(pdfBlock{FurnLabel: el.furnLabel}, el.line)
		case el.isTOC:
			appendSingleLineBlock(pdfBlock{IsTOC: true}, el.line)
		case el.isTitle:
			appendSingleLineBlock(pdfBlock{IsTitle: true}, el.line)
		case el.headingLevel > 0:
			flushBody()
			blocks = append(blocks, pdfBlock{
				HeadingLevel: el.headingLevel,
				Text:         el.line.Text,
				PageIdx:      el.line.PageIdx,
				Lines:        []pdfLine{el.line},
			})
		default:
			if bodyPage >= 0 && el.line.PageIdx != bodyPage {
				flushBody()
			}
			if bodyBuf.Len() > 0 && bodyBuf.Len()+len([]rune(el.line.Text))+1 > knowledgePDFBodyChunkRunes {
				flushBody()
			}
			if bodyBuf.Len() > 0 {
				bodyBuf.WriteString("\n")
			}
			bodyBuf.WriteString(el.line.Text)
			bodyLines = append(bodyLines, el.line)
			if bodyPage < 0 {
				bodyPage = el.line.PageIdx
			}
		}
	}
	flushBody()
	return blocks
}

// pdfBlockProv 把正文/标题块聚合为单个 prov（块内同页）：
// BOTTOMLEFT bbox 取组内行 bbox 的并集（上边缘取最大、下边缘取最小、
// 左右取最小/最大），charspan 覆盖整块文本（[0, rune 数]）；
// 扫描件兜底块无坐标信息，返回 nil 产出无 prov 元素。
func pdfBlockProv(b pdfBlock) []ProvenanceItem {
	if b.Table != nil {
		if provenance := pdfDetectedTableProvenance(b.Table); len(provenance) > 0 {
			return provenance
		}
	}
	for _, l := range b.Lines {
		if l.Fallback {
			return nil
		}
	}
	if len(b.Lines) == 0 {
		return nil
	}
	prov := ProvenanceItem{
		PageNo:   b.PageIdx + 1,
		CharSpan: [2]int64{0, int64(len([]rune(b.Text)))},
		BBox:     &DoclingBBox{CoordOrigin: CoordOriginBottomLeft},
	}
	for i, l := range b.Lines {
		if i == 0 {
			prov.BBox.L, prov.BBox.R = l.MinX, l.MaxX
			prov.BBox.T, prov.BBox.B = l.MaxY, l.MinY
			continue
		}
		if l.MinX < prov.BBox.L {
			prov.BBox.L = l.MinX
		}
		if l.MaxX > prov.BBox.R {
			prov.BBox.R = l.MaxX
		}
		// 库内 Y 向上递增：并集上边缘取最大 MaxY、下边缘取最小 MinY
		if l.MaxY > prov.BBox.T {
			prov.BBox.T = l.MaxY
		}
		if l.MinY < prov.BBox.B {
			prov.BBox.B = l.MinY
		}
	}
	return []ProvenanceItem{prov}
}

// buildPDFDoclingDocument 把块序列挂接为 DoclingDocument 文档树：
//   - 标题按层级父挂接（headingRefs 栈模式，对齐 markdown.go：处理标题时先失效
//     自身级及更深层级，再挂最近的可用祖先标题，跳级不补隐式分组），
//     正文挂最近的祖先标题（无则挂 body），章节路径由简化器从树推导；
//   - 页眉/页脚块以 label=page_header/page_footer 直接挂 body；
//   - 连续目录块收进同一"目次"分组（AddSectionGroup），行以
//     label=document_index 挂组下，遇标题/正文块关闭分组；
//   - 封面主标题块走 AddTitle（label=title，挂 body，为后续标题的树祖先）。
func buildPDFDoclingDocument(blocks []pdfBlock, doc *DoclingDocument) {
	// 章节父栈：headingRefs[level] 记录最近该级标题引用（层级 1-3）
	var headingRefs [4]RefItem
	var hasHeading [4]bool
	nearestParent := func() *RefItem {
		for lv := 3; lv >= 1; lv-- {
			if hasHeading[lv] {
				r := headingRefs[lv]
				return &r
			}
		}
		return nil
	}
	// tocGroup 当前打开的"目次"分组引用；页眉/页脚块不打断分组
	// （跨页目录区域中间可夹页眉页脚行）
	var tocGroup *RefItem
	for _, b := range blocks {
		prov := pdfBlockProv(b)
		switch {
		case b.Picture != nil:
			tocGroup = nil
			doc.AddPicture(b.Picture, prov, nearestParent())
		case b.Table != nil:
			tocGroup = nil
			doc.AddTable(b.Table.Data.TableCells, b.Table.Data.NumRows, b.Table.Data.NumCols, prov, nearestParent())
		case b.OCRSub != nil:
			// 外部 OCR 识别结果：结构化子文档按页号并入（元素自带页级 prov）
			tocGroup = nil
			mergeOCRSubDocument(doc, b.OCRSub, b.PageIdx+1)
		case b.FurnLabel != "":
			furniture := doc.furnitureRef()
			ref := doc.AddText(b.FurnLabel, b.Text, prov, &furniture)
			doc.Texts[ref.Idx].ContentLayer = LayerFurniture
		case b.IsTOC:
			if tocGroup == nil {
				g := doc.AddSectionGroup("目次", nil)
				tocGroup = &g
			}
			doc.AddText(LabelDocumentIndex, b.Text, prov, tocGroup)
		case b.IsTitle:
			tocGroup = nil
			doc.AddTitle(b.Text, prov, nil)
		case b.HeadingLevel > 0:
			tocGroup = nil
			hasHeading[b.HeadingLevel] = false
			for lv := b.HeadingLevel + 1; lv <= 3; lv++ {
				hasHeading[lv] = false
			}
			ref := doc.AddHeading(b.HeadingLevel, b.Text, prov, nearestParent())
			headingRefs[b.HeadingLevel] = ref
			hasHeading[b.HeadingLevel] = true
		default:
			tocGroup = nil
			doc.AddText(LabelText, b.Text, prov, nearestParent())
		}
	}
}

// buildPDFContentItems 把元素流转为旧 content_list 视图（Item 列表）：
// 标题刷新章节路径栈并独立成元素（路径含自身，对齐 go_light 旧行为），
// 正文按块携带 0 起页号与所在章节路径；页眉/页脚与目录区域块以
// Label 字段标注（page_header/page_footer/document_index），
// 封面主标题块按简化器 walkText 现状视作 1 级标题入路径栈。
// 供既有测试与统计口径使用；主流程的章节路径已改由树父挂接表达
// （buildPDFDoclingDocument），由简化器 ToContentList 统一推导。
func buildPDFContentItems(elements []pdfElement) []Item {
	blocks := buildPDFBlocks(elements)
	items := make([]Item, 0, len(blocks))
	var headingStack []string
	for _, b := range blocks {
		switch {
		case b.Picture != nil:
			item := Item{Type: ItemTypeImage, PageIdx: b.PageIdx, ImgPath: b.Picture.URI}
			item.SectionPath = append([]string(nil), headingStack...)
			items = append(items, item)
		case b.Table != nil:
			item := Item{
				Type:      ItemTypeTable,
				PageIdx:   b.PageIdx,
				TableBody: renderTableMarkdown(b.Table.Data.TableCells, b.Table.Data.NumRows, b.Table.Data.NumCols),
			}
			item.SectionPath = append([]string(nil), headingStack...)
			items = append(items, item)
		case b.FurnLabel != "":
			item := Item{Type: ItemTypeText, Text: b.Text, PageIdx: b.PageIdx, Label: string(b.FurnLabel)}
			item.SectionPath = append([]string(nil), headingStack...)
			items = append(items, item)
		case b.IsTOC:
			item := Item{Type: ItemTypeText, Text: b.Text, PageIdx: b.PageIdx, Label: string(LabelDocumentIndex)}
			item.SectionPath = append([]string(nil), headingStack...)
			items = append(items, item)
		case b.IsTitle:
			// title 无层级语义，对齐 walkText：入栈但不做层级截断
			headingStack = append(headingStack, b.Text)
			item := Item{Type: ItemTypeText, Text: b.Text, TextLevel: 1, PageIdx: b.PageIdx}
			item.SectionPath = append([]string(nil), headingStack...)
			items = append(items, item)
		case b.HeadingLevel > 0:
			if int(b.HeadingLevel-1) < len(headingStack) {
				headingStack = headingStack[:b.HeadingLevel-1]
			}
			headingStack = append(headingStack, b.Text)
			item := Item{Type: ItemTypeText, Text: b.Text, TextLevel: b.HeadingLevel}
			item.SectionPath = append([]string(nil), headingStack...)
			items = append(items, item)
		default:
			item := Item{Type: ItemTypeText, Text: b.Text, PageIdx: b.PageIdx}
			item.SectionPath = append([]string(nil), headingStack...)
			items = append(items, item)
		}
	}
	return items
}

// classifyPDFLines 便捷封装：把行分类为标题与正文行（供测试与统计使用）。
// 与主流程一致，先做页眉/页脚与目录区域打标（页数按入参行覆盖的
// 最大页号推导），标题/正文字号统计不受版式行干扰。
func classifyPDFLines(lines []pdfLine) ([]pdfHeading, []pdfLine) {
	// 复制入参再打标，避免污染调用方切片
	annotated := append([]pdfLine(nil), lines...)
	maxPage := int64(0)
	for _, l := range annotated {
		if l.PageIdx > maxPage {
			maxPage = l.PageIdx
		}
	}
	classifyPDFFurnitureLines(annotated, int(maxPage)+1)
	annotatePDFTOCRegions(annotated)
	elements := buildPDFElements(annotated)
	var headings []pdfHeading
	var body []pdfLine
	for _, el := range elements {
		if el.headingLevel > 0 {
			headings = append(headings, pdfHeading{Level: el.headingLevel, Line: el.line})
			continue
		}
		body = append(body, el.line)
	}
	return headings, body
}
