// pdf_image.go 纯 Go 解析 PDF 页面 Image XObject 的绘制位置与覆盖率，
// 并通过 pdfcpu 将可安全解码的图片保存为带 data URI 的 PictureItem 中间行。
// 几何识别与像素解码相互独立：解码失败仍保留图片质量信号用于视觉路由。
package docling

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/ledongthuc/pdf"
	pdfcpufilter "github.com/pdfcpu/pdfcpu/pkg/filter"
	pdfcpucore "github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	pdfcpumodel "github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	_ "golang.org/x/image/tiff"
)

const (
	// pdfMaxImagePlacements 限制单页记录的图片绘制次数，避免恶意内容流放大内存。
	pdfMaxImagePlacements = 256
	// pdfMaxFormDepth 限制 Form XObject 递归层数，避免循环关系或极深嵌套。
	pdfMaxFormDepth = 8
	// pdfMixedImageMinCoverage 是混合文本页进入视觉识别的最小图片覆盖率。
	pdfMixedImageMinCoverage = 0.05
	// pdfLargeImageMinPixels 排除 Logo、项目符号等低像素小资源。
	pdfLargeImageMinPixels = 4096
	// pdfMaxExtractPagePixels 限制单页声明的不同图片资源总像素，避免畸形
	// Width/Height 诱导解码器分配失控；超过时该页仅保留几何质量信号。
	pdfMaxExtractPagePixels = 128 * 1024 * 1024
	// pdfMaxExtractDimension 限制单张图片的声明边长。
	pdfMaxExtractDimension = 50000
	// pdfMaxVisualEmbeddedImages 限制随单页 PDF 一同发送给大模型的原始图片数。
	pdfMaxVisualEmbeddedImages = 8
	// pdfMaxImageContentBytes 限制 inline image 扫描读取的单页或 Form 内容流。
	pdfMaxImageContentBytes = 64 << 20
	// pdfMaxInlineDecodedBytes 限制内联图片解压后像素，避免高压缩比
	// Flate 数据在 PNG 编码前占用过多内存。
	pdfMaxInlineDecodedBytes = 64 << 20
	// pdfMaxInlineDictionaryBytes 限制就地解析的 DecodeParms 字典大小。
	pdfMaxInlineDictionaryBytes = 4096
)

// pdfImagePlacement 描述一个 Image XObject 在页面上的实际绘制位置。
type pdfImagePlacement struct {
	Name        string             // Name 是页面或 Form 资源字典中的 XObject 名称。
	PixelWidth  int64              // PixelWidth 是图片资源声明的像素宽度。
	PixelHeight int64              // PixelHeight 是图片资源声明的像素高度。
	BBox        *DoclingBBox       // BBox 是当前 CTM 映射后的 BOTTOMLEFT 页面边界框。
	InlineImage *pdfExtractedImage // InlineImage 是可证明格式的 BI/ID/EI 像素资产。
}

// pdfExtractedImage 保存 pdfcpu 已编码为常见图片格式的资源数据。
type pdfExtractedImage struct {
	Name        string
	MIMEType    string
	Data        []byte
	PixelWidth  int64
	PixelHeight int64
}

// pdfVisualImagesForPage 选取单页面积最大的可用图片资产提供给视觉模型。
// 无 URI 的超限图片不进入模型输入，但其几何仍保留在质量信号中。
func pdfVisualImagesForPage(pictures []pdfLine, pageIdx int64) []PDFVisualImage {
	candidates := make([]pdfLine, 0)
	for _, picture := range pictures {
		if picture.PageIdx == pageIdx && picture.Picture != nil && picture.Picture.URI != "" {
			candidates = append(candidates, picture)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		leftArea := (candidates[i].MaxX - candidates[i].MinX) * (candidates[i].MaxY - candidates[i].MinY)
		rightArea := (candidates[j].MaxX - candidates[j].MinX) * (candidates[j].MaxY - candidates[j].MinY)
		if leftArea != rightArea {
			return leftArea > rightArea
		}
		return candidates[i].MaxY > candidates[j].MaxY
	})
	if len(candidates) > pdfMaxVisualEmbeddedImages {
		candidates = candidates[:pdfMaxVisualEmbeddedImages]
	}
	images := make([]PDFVisualImage, 0, len(candidates))
	for _, candidate := range candidates {
		images = append(images, PDFVisualImage{
			Image: candidate.Picture,
			BBox: &DoclingBBox{
				L: candidate.MinX, B: candidate.MinY, R: candidate.MaxX, T: candidate.MaxY,
				CoordOrigin: CoordOriginBottomLeft,
			},
		})
	}
	return images
}

// pdfAffine 是 PDF 二维仿射矩阵的行向量表示。
type pdfAffine [3][3]float64

// identityPDFAffine 返回单位仿射矩阵。
func identityPDFAffine() pdfAffine {
	return pdfAffine{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}
}

// pdfAffineFromValues 按 PDF 的 a b c d e f 顺序创建仿射矩阵。
func pdfAffineFromValues(values []pdf.Value) (pdfAffine, bool) {
	if len(values) != 6 {
		return pdfAffine{}, false
	}
	return pdfAffine{
		{values[0].Float64(), values[1].Float64(), 0},
		{values[2].Float64(), values[3].Float64(), 0},
		{values[4].Float64(), values[5].Float64(), 1},
	}, true
}

// mulPDFAffine 按 PDF 内容流的矩阵连接顺序合并两个矩阵。
func mulPDFAffine(left, right pdfAffine) pdfAffine {
	var result pdfAffine
	for row := 0; row < 3; row++ {
		for col := 0; col < 3; col++ {
			for k := 0; k < 3; k++ {
				result[row][col] += left[row][k] * right[k][col]
			}
		}
	}
	return result
}

// transformPDFPoint 把单位图片坐标映射为页面坐标。
func transformPDFPoint(matrix pdfAffine, x, y float64) (float64, float64) {
	return x*matrix[0][0] + y*matrix[1][0] + matrix[2][0],
		x*matrix[0][1] + y*matrix[1][1] + matrix[2][1]
}

// inspectPDFPageImages 解析页面及嵌套 Form 中实际执行 Do 的 Image XObject。
// width/height 仅作 MediaBox 无法读取时的兼容尺寸；正常页面由 CropBox、
// UserUnit 和 Rotate 决定显示坐标。
func inspectPDFPageImages(page pdf.Page, width, height float64) (placements []pdfImagePlacement) {
	geometry := pdfPageGeometryForPage(page)
	if geometry.width <= 0 || geometry.height <= 0 {
		geometry = pdfPageGeometry{width: width, height: height, userUnit: 1, transform: identityPDFAffine()}
	}
	return inspectPDFPageImagesWithGeometry(page, geometry)
}

// inspectPDFPageImagesWithGeometry 使用统一页面变换恢复图片的显示坐标。
func inspectPDFPageImagesWithGeometry(page pdf.Page, geometry pdfPageGeometry) (placements []pdfImagePlacement) {
	if page.V.IsNull() || page.V.Key("Contents").IsNull() {
		return nil
	}
	return inspectPDFImageStream(
		page.V.Key("Contents"), page.Resources(), geometry.transform,
		geometry.width, geometry.height, 0, placements,
	)
}

// inspectPDFImageStream 解释一个页面或 Form 内容流并递归收集图片。
func inspectPDFImageStream(
	stream, resources pdf.Value,
	initial pdfAffine,
	pageWidth, pageHeight float64,
	depth int,
	placements []pdfImagePlacement,
) (result []pdfImagePlacement) {
	result = placements
	if stream.IsNull() || depth > pdfMaxFormDepth || len(result) >= pdfMaxImagePlacements {
		return result
	}
	// ledongthuc 的内容解释器对不支持过滤器和畸形栈使用 panic；图片感知是
	// 尽力增强，必须吞掉该局部异常并保留已收集结果，不影响 PDF 主解析。
	defer func() {
		if recover() != nil {
			result = result[:min(len(result), pdfMaxImagePlacements)]
		}
	}()
	if content, ok := readPDFImageContent(stream, 0); ok {
		result = inspectPDFInlineImageContentWithResources(content, resources, initial, pageWidth, pageHeight, result)
		if len(result) >= pdfMaxImagePlacements {
			return result[:pdfMaxImagePlacements]
		}
	}
	current := initial
	stack := make([]pdfAffine, 0, 4)
	pdf.Interpret(stream, func(operand *pdf.Stack, operator string) {
		values := popPDFOperands(operand)
		switch operator {
		case "q":
			stack = append(stack, current)
		case "Q":
			if len(stack) > 0 {
				current = stack[len(stack)-1]
				stack = stack[:len(stack)-1]
			}
		case "cm":
			if matrix, ok := pdfAffineFromValues(values); ok {
				current = mulPDFAffine(matrix, current)
			}
		case "Do":
			if len(values) != 1 || len(result) >= pdfMaxImagePlacements {
				return
			}
			name := values[0].Name()
			xObject := resources.Key("XObject").Key(name)
			switch xObject.Key("Subtype").Name() {
			case "Image":
				bbox := pdfImageBBox(current, pageWidth, pageHeight)
				if bbox != nil {
					result = append(result, pdfImagePlacement{
						Name: name, PixelWidth: xObject.Key("Width").Int64(),
						PixelHeight: xObject.Key("Height").Int64(), BBox: bbox,
					})
				}
			case "Form":
				formMatrix := identityPDFAffine()
				if raw := xObject.Key("Matrix"); raw.Len() == 6 {
					values := make([]pdf.Value, 6)
					for i := range values {
						values[i] = raw.Index(i)
					}
					if parsed, ok := pdfAffineFromValues(values); ok {
						formMatrix = parsed
					}
				}
				formResources := xObject.Key("Resources")
				if formResources.IsNull() {
					formResources = resources
				}
				result = inspectPDFImageStream(
					xObject, formResources, mulPDFAffine(formMatrix, current),
					pageWidth, pageHeight, depth+1, result,
				)
			}
		}
	})
	return result
}

// readPDFImageContent 有界读取页面或 Form 内容流；数组按 PDF 语义连接，
// 单个损坏子流被跳过，所有内容的累计解压量仍受统一上限约束。
func readPDFImageContent(value pdf.Value, depth int) (data []byte, ok bool) {
	if value.IsNull() || depth > 4 {
		return nil, false
	}
	defer func() {
		if recover() != nil {
			data, ok = nil, false
		}
	}()
	switch value.Kind() {
	case pdf.Stream:
		reader := value.Reader()
		defer reader.Close()
		data, err := io.ReadAll(io.LimitReader(reader, pdfMaxImageContentBytes+1))
		if err != nil || len(data) == 0 || len(data) > pdfMaxImageContentBytes {
			return nil, false
		}
		return data, true
	case pdf.Array:
		var output bytes.Buffer
		for index := 0; index < value.Len(); index++ {
			part, read := readPDFImageContent(value.Index(index), depth+1)
			if !read {
				continue
			}
			if output.Len()+len(part)+1 > pdfMaxImageContentBytes {
				return nil, false
			}
			output.Write(part)
			output.WriteByte('\n')
		}
		return output.Bytes(), output.Len() > 0
	default:
		return nil, false
	}
}

// inspectPDFInlineImageContent 解释与图片几何相关的 q/Q/cm/BI 操作符。
// inline image 像素可能使用任意过滤器，本函数只恢复可靠的声明尺寸和 CTM，
// 像素本体继续由单页 PDF 交给结构化视觉模型读取。
func inspectPDFInlineImageContent(
	content []byte,
	initial pdfAffine,
	pageWidth, pageHeight float64,
	placements []pdfImagePlacement,
) []pdfImagePlacement {
	return inspectPDFInlineImageContentWithResources(content, pdf.Value{}, initial, pageWidth, pageHeight, placements)
}

// inspectPDFInlineImageContentWithResources 在恢复几何时同时解析页面或 Form
// 的命名 ColorSpace，供 Indexed 等内联图片复用资源字典。
func inspectPDFInlineImageContentWithResources(
	content []byte,
	resources pdf.Value,
	initial pdfAffine,
	pageWidth, pageHeight float64,
	placements []pdfImagePlacement,
) []pdfImagePlacement {
	current := pdfInlineGraphicsState{matrix: initial, fill: color.NRGBA{A: 0xff}}
	stack := make([]pdfInlineGraphicsState, 0, 4)
	operands := make([]float64, 0, 6)
	for offset := 0; offset < len(content) && len(placements) < pdfMaxImagePlacements; {
		token, next, found := nextPDFImageContentToken(content, offset)
		if !found {
			break
		}
		offset = next
		if number, err := strconv.ParseFloat(token, 64); err == nil {
			operands = append(operands, number)
			continue
		}
		switch token {
		case "q":
			stack = append(stack, current)
		case "Q":
			if len(stack) > 0 {
				current = stack[len(stack)-1]
				stack = stack[:len(stack)-1]
			}
		case "cm":
			if len(operands) == 6 {
				matrix := pdfAffine{{operands[0], operands[1], 0}, {operands[2], operands[3], 0}, {operands[4], operands[5], 1}}
				current.matrix = mulPDFAffine(matrix, current.matrix)
			}
		case "g":
			if len(operands) == 1 {
				gray := pdfColorComponent(operands[0])
				current.fill = color.NRGBA{R: gray, G: gray, B: gray, A: 0xff}
			}
		case "rg":
			if len(operands) == 3 {
				current.fill = color.NRGBA{
					R: pdfColorComponent(operands[0]), G: pdfColorComponent(operands[1]),
					B: pdfColorComponent(operands[2]), A: 0xff,
				}
			}
		case "k":
			if len(operands) == 4 {
				r, g, b := color.CMYKToRGB(
					pdfColorComponent(operands[0]), pdfColorComponent(operands[1]),
					pdfColorComponent(operands[2]), pdfColorComponent(operands[3]),
				)
				current.fill = color.NRGBA{R: r, G: g, B: b, A: 0xff}
			}
		case "BI":
			inline, end, parsed := parsePDFInlineImage(content, offset, current.fill, resources)
			if parsed {
				if bbox := pdfImageBBox(current.matrix, pageWidth, pageHeight); bbox != nil {
					placements = append(placements, pdfImagePlacement{
						Name: "inline-" + strconv.Itoa(len(placements)+1), PixelWidth: inline.width,
						PixelHeight: inline.height, BBox: bbox, InlineImage: inline.image,
					})
				}
				offset = end
			}
		}
		operands = operands[:0]
	}
	return placements
}

// pdfInlineGraphicsState 保存内联图片所需的矩阵和当前非描边颜色。
type pdfInlineGraphicsState struct {
	matrix pdfAffine
	fill   color.NRGBA
}

// pdfColorComponent 把 PDF 0..1 色彩分量裁剪并量化为 8 位。
func pdfColorComponent(value float64) byte {
	value = math.Max(0, math.Min(1, value))
	return byte(math.Round(value * 255))
}

// pdfInlineImage 保存 BI 字典与可选的已验证像素资产。
type pdfInlineImage struct {
	width, height       int64
	colorSpace          string
	bits                int64
	filter              string
	filters             []string
	decode              []float64
	imageMask           bool
	maskColor           color.NRGBA
	indexedColorSpace   *pdfInlineIndexedColorSpace
	decodeParams        *pdfInlineDecodeParams
	decodeParamsList    []*pdfInlineDecodeParams
	filterInvalid       bool
	decodeInvalid       bool
	decodeParamsInvalid bool
	image               *pdfExtractedImage
}

// pdfInlineDecodeParams 保存 FlateDecode 的 TIFF/PNG 预测参数。
type pdfInlineDecodeParams struct {
	predictor        int64
	colors           int64
	bits             int64
	columns          int64
	rows             int64
	k                int64
	earlyChange      int64
	columnsSet       bool
	rowsSet          bool
	blackIs1         bool
	encodedByteAlign bool
	endOfLine        bool
	endOfBlock       bool
	damagedRows      int64
}

// pdfInlineIndexedColorSpace 保存可就地解析的 Indexed 基础色彩与调色板。
type pdfInlineIndexedColorSpace struct {
	channels int
	highest  int
	lookup   []byte
}

// parsePDFInlineImage 读取 BI 字典中的 W/H（含长名称别名），并安全跳过
// ID 到 EI 的二进制数据。只有完整终止符存在时才报告图片，避免把正文误判。
func parsePDFInlineImage(content []byte, offset int, maskColor color.NRGBA, resources pdf.Value) (inline pdfInlineImage, end int, ok bool) {
	inline.maskColor = maskColor
	key := ""
	for offset < len(content) {
		token, next, found := nextPDFImageContentToken(content, offset)
		if !found {
			return pdfInlineImage{}, offset, false
		}
		offset = next
		if token == "ID" {
			dataStart := offset
			if dataStart < len(content) && isPDFImageWhitespace(content[dataStart]) {
				dataStart++
			}
			imageEnd, after, found := findPDFInlineImageEnd(content, dataStart)
			if !found || imageEnd < dataStart {
				return pdfInlineImage{}, offset, false
			}
			dataEnd := imageEnd
			if dataEnd > dataStart && isPDFImageWhitespace(content[dataEnd-1]) {
				dataEnd--
			}
			resolvePDFInlineColorSpace(&inline, resources)
			inline.image = decodePDFInlineImage(inline, content[dataStart:dataEnd])
			return inline, after, true
		}
		if strings.HasPrefix(token, "/") {
			if key != "" {
				assignPDFInlineImageValue(&inline, key, token)
				key = ""
			} else {
				key = strings.TrimPrefix(token, "/")
			}
			continue
		}
		if key != "" {
			assignPDFInlineImageValue(&inline, key, token)
		}
		key = ""
	}
	return pdfInlineImage{}, offset, false
}

// assignPDFInlineImageValue 保存 BI 字典中影响像素解释的标准键及别名。
func assignPDFInlineImageValue(inline *pdfInlineImage, key, value string) {
	if inline == nil {
		return
	}
	value = strings.TrimPrefix(value, "/")
	switch key {
	case "W", "Width":
		inline.width, _ = strconv.ParseInt(value, 10, 64)
	case "H", "Height":
		inline.height, _ = strconv.ParseInt(value, 10, 64)
	case "BPC", "BitsPerComponent":
		inline.bits, _ = strconv.ParseInt(value, 10, 64)
	case "IM", "ImageMask":
		inline.imageMask, _ = strconv.ParseBool(strings.ToLower(strings.TrimSpace(value)))
	case "CS", "ColorSpace":
		inline.colorSpace = value
	case "F", "Filter":
		if strings.HasPrefix(strings.TrimSpace(value), "[") {
			filters, ok := parsePDFInlineFilterList(value)
			inline.filters, inline.filterInvalid = filters, !ok
		} else {
			inline.filter = value
		}
	case "D", "Decode":
		decode, ok := parsePDFInlineNumberArray(value, 8)
		inline.decode, inline.decodeInvalid = decode, !ok
	case "DP", "DecodeParms":
		if strings.HasPrefix(strings.TrimSpace(value), "[") {
			params, ok := parsePDFInlineDecodeParamsList(value)
			inline.decodeParamsList, inline.decodeParamsInvalid = params, !ok
		} else if strings.EqualFold(strings.TrimSpace(value), "null") {
			inline.decodeParams = nil
		} else {
			params, ok := parsePDFInlineDecodeParams(value)
			inline.decodeParams, inline.decodeParamsInvalid = params, !ok
		}
	}
}

// parsePDFInlineNumberArray 有界读取 Decode 等只包含有限数值的数组。
func parsePDFInlineNumberArray(value string, maxValues int) ([]float64, bool) {
	value = strings.TrimSpace(value)
	if maxValues <= 0 || len(value) > pdfMaxInlineDictionaryBytes ||
		!strings.HasPrefix(value, "[") || !strings.HasSuffix(value, "]") {
		return nil, false
	}
	content := []byte(strings.TrimSpace(value[1 : len(value)-1]))
	values := make([]float64, 0, min(maxValues, 4))
	for offset := 0; offset < len(content); {
		if len(values) >= maxValues {
			return nil, false
		}
		token, next, found := nextPDFImageContentToken(content, offset)
		if !found {
			break
		}
		offset = next
		number, err := strconv.ParseFloat(token, 64)
		if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
			return nil, false
		}
		values = append(values, number)
	}
	return values, len(values) > 0
}

// parsePDFInlineIndexedColorSpace 读取 [/Indexed base hival lookup]；lookup
// 只接受受限的十六进制串或 PDF literal string，不解析间接对象和资源名称。
func parsePDFInlineIndexedColorSpace(value string) (pdfInlineIndexedColorSpace, bool) {
	value = strings.TrimSpace(value)
	if len(value) > pdfMaxInlineDictionaryBytes || !strings.HasPrefix(value, "[") || !strings.HasSuffix(value, "]") {
		return pdfInlineIndexedColorSpace{}, false
	}
	content := []byte(strings.TrimSpace(value[1 : len(value)-1]))
	tokens := make([]string, 0, 4)
	for offset := 0; offset < len(content); {
		if len(tokens) >= 4 {
			return pdfInlineIndexedColorSpace{}, false
		}
		token, next, found := nextPDFImageContentToken(content, offset)
		if !found {
			break
		}
		tokens = append(tokens, token)
		offset = next
	}
	if len(tokens) != 4 || (tokens[0] != "/I" && !strings.EqualFold(tokens[0], "/Indexed")) ||
		!strings.HasPrefix(tokens[1], "/") {
		return pdfInlineIndexedColorSpace{}, false
	}
	channels := pdfInlineImageChannels(strings.TrimPrefix(tokens[1], "/"))
	highest, err := strconv.Atoi(tokens[2])
	if channels == 0 || highest < 0 || highest > 255 || err != nil {
		return pdfInlineIndexedColorSpace{}, false
	}
	var lookup []byte
	var ok bool
	switch {
	case strings.HasPrefix(tokens[3], "<") && !strings.HasPrefix(tokens[3], "<<"):
		lookup, ok = parsePDFInlineHexBytes(tokens[3])
	case strings.HasPrefix(tokens[3], "("):
		lookup, ok = parsePDFInlineLiteralBytes(tokens[3])
	}
	if !ok || len(lookup) != (highest+1)*channels {
		return pdfInlineIndexedColorSpace{}, false
	}
	return pdfInlineIndexedColorSpace{channels: channels, highest: highest, lookup: lookup}, true
}

// resolvePDFInlineColorSpace 把 BI 中的命名色彩空间解析为页面或 Form
// Resources/ColorSpace 条目；无法安全读取时保留原名并走视觉回退。
func resolvePDFInlineColorSpace(inline *pdfInlineImage, resources pdf.Value) {
	if inline == nil || resources.IsNull() {
		return
	}
	name := strings.TrimSpace(inline.colorSpace)
	if name == "" || strings.HasPrefix(name, "[") || pdfInlineImageChannels(name) != 0 {
		return
	}
	value := resources.Key("ColorSpace").Key(name)
	switch value.Kind() {
	case pdf.Name:
		if resolved := value.Name(); pdfInlineImageChannels(resolved) != 0 {
			inline.colorSpace = resolved
		}
	case pdf.Array:
		if indexed, ok := parsePDFInlineIndexedColorSpaceValue(value); ok {
			inline.indexedColorSpace = &indexed
			return
		}
		if resolved, _, ok := resolvePDFInlineDeviceColorSpaceValue(value); ok {
			inline.colorSpace = resolved
		}
	}
}

// resolvePDFInlineDeviceColorSpaceValue 把 Device 或 ICCBased Value 解析为
// 可直接解释的 Device 名称；ICCBased 优先采用显式 Alternate，否则按 N 回退。
func resolvePDFInlineDeviceColorSpaceValue(value pdf.Value) (name string, channels int, ok bool) {
	if value.Kind() == pdf.Name {
		name = value.Name()
		channels = pdfInlineImageChannels(name)
		return name, channels, channels != 0
	}
	if value.Kind() != pdf.Array || value.Len() != 2 || !strings.EqualFold(value.Index(0).Name(), "ICCBased") {
		return "", 0, false
	}
	profile := value.Index(1)
	if profile.Kind() != pdf.Stream && profile.Kind() != pdf.Dict {
		return "", 0, false
	}
	componentValue := profile.Key("N")
	if componentValue.Kind() != pdf.Integer {
		return "", 0, false
	}
	channels = int(componentValue.Int64())
	if channels != 1 && channels != 3 && channels != 4 {
		return "", 0, false
	}
	alternate := profile.Key("Alternate")
	if !alternate.IsNull() {
		if alternate.Kind() != pdf.Name {
			return "", 0, false
		}
		name = alternate.Name()
		return name, channels, pdfInlineImageChannels(name) == channels
	}
	switch channels {
	case 1:
		return "DeviceGray", channels, true
	case 3:
		return "DeviceRGB", channels, true
	case 4:
		return "DeviceCMYK", channels, true
	default:
		return "", 0, false
	}
}

// parsePDFInlineIndexedColorSpaceValue 从已解析 PDF Value 读取 Indexed
// 数组；lookup string/stream 必须与 hival 和基础通道数精确匹配。
func parsePDFInlineIndexedColorSpaceValue(value pdf.Value) (pdfInlineIndexedColorSpace, bool) {
	if value.Kind() != pdf.Array || value.Len() != 4 {
		return pdfInlineIndexedColorSpace{}, false
	}
	kind := value.Index(0).Name()
	if kind != "I" && !strings.EqualFold(kind, "Indexed") {
		return pdfInlineIndexedColorSpace{}, false
	}
	_, channels, baseOK := resolvePDFInlineDeviceColorSpaceValue(value.Index(1))
	highestValue := value.Index(2)
	if !baseOK || highestValue.Kind() != pdf.Integer {
		return pdfInlineIndexedColorSpace{}, false
	}
	highest := highestValue.Int64()
	if highest < 0 || highest > 255 || highest+1 > math.MaxInt64/int64(channels) {
		return pdfInlineIndexedColorSpace{}, false
	}
	expected := int((highest + 1) * int64(channels))
	lookup, ok := readPDFInlineLookupValue(value.Index(3), expected)
	if !ok {
		return pdfInlineIndexedColorSpace{}, false
	}
	return pdfInlineIndexedColorSpace{channels: channels, highest: int(highest), lookup: lookup}, true
}

// readPDFInlineLookupValue 有界读取 Indexed lookup 的字符串或已解压 stream。
func readPDFInlineLookupValue(value pdf.Value, expected int) (lookup []byte, ok bool) {
	defer func() {
		if recover() != nil {
			lookup, ok = nil, false
		}
	}()
	if expected <= 0 || expected > pdfMaxInlineDictionaryBytes {
		return nil, false
	}
	switch value.Kind() {
	case pdf.String:
		lookup = []byte(value.RawString())
	case pdf.Stream:
		reader := value.Reader()
		defer reader.Close()
		var err error
		lookup, err = io.ReadAll(io.LimitReader(reader, int64(expected+1)))
		if err != nil {
			return nil, false
		}
	default:
		return nil, false
	}
	if len(lookup) != expected {
		return nil, false
	}
	return append([]byte(nil), lookup...), true
}

// parsePDFInlineHexBytes 解码允许空白和奇数末尾半字节的 PDF 十六进制串。
func parsePDFInlineHexBytes(value string) ([]byte, bool) {
	if len(value) < 2 || len(value) > pdfMaxInlineDictionaryBytes || value[0] != '<' || value[len(value)-1] != '>' {
		return nil, false
	}
	digits := make([]byte, 0, len(value)-2)
	for _, digit := range []byte(value[1 : len(value)-1]) {
		if isPDFImageWhitespace(digit) {
			continue
		}
		if !((digit >= '0' && digit <= '9') || (digit >= 'a' && digit <= 'f') || (digit >= 'A' && digit <= 'F')) {
			return nil, false
		}
		digits = append(digits, digit)
	}
	if len(digits)%2 != 0 {
		digits = append(digits, '0')
	}
	decoded := make([]byte, hex.DecodedLen(len(digits)))
	if _, err := hex.Decode(decoded, digits); err != nil {
		return nil, false
	}
	return decoded, true
}

// parsePDFInlineLiteralBytes 解码 PDF literal string 的常用转义、八进制字节
// 与反斜线换行；嵌套括号由外层词法器保证完整。
func parsePDFInlineLiteralBytes(value string) ([]byte, bool) {
	if len(value) < 2 || len(value) > pdfMaxInlineDictionaryBytes || value[0] != '(' || value[len(value)-1] != ')' {
		return nil, false
	}
	decoded := make([]byte, 0, len(value)-2)
	for index := 1; index < len(value)-1; index++ {
		current := value[index]
		if current != '\\' {
			decoded = append(decoded, current)
			continue
		}
		index++
		if index >= len(value)-1 {
			return nil, false
		}
		current = value[index]
		switch current {
		case 'n':
			decoded = append(decoded, '\n')
		case 'r':
			decoded = append(decoded, '\r')
		case 't':
			decoded = append(decoded, '\t')
		case 'b':
			decoded = append(decoded, '\b')
		case 'f':
			decoded = append(decoded, '\f')
		case '\n':
			continue
		case '\r':
			if index+1 < len(value)-1 && value[index+1] == '\n' {
				index++
			}
		case '0', '1', '2', '3', '4', '5', '6', '7':
			octal := int(current - '0')
			for count := 1; count < 3 && index+1 < len(value)-1 && value[index+1] >= '0' && value[index+1] <= '7'; count++ {
				index++
				octal = octal*8 + int(value[index]-'0')
			}
			decoded = append(decoded, byte(octal))
		default:
			decoded = append(decoded, current)
		}
	}
	return decoded, true
}

// parsePDFInlineFilterList 读取内联图像的过滤器名称数组。
func parsePDFInlineFilterList(value string) ([]string, bool) {
	value = strings.TrimSpace(value)
	if len(value) > pdfMaxInlineDictionaryBytes || !strings.HasPrefix(value, "[") || !strings.HasSuffix(value, "]") {
		return nil, false
	}
	content := []byte(strings.TrimSpace(value[1 : len(value)-1]))
	filters := make([]string, 0, 2)
	for offset := 0; offset < len(content); {
		if len(filters) >= 8 {
			return nil, false
		}
		token, next, found := nextPDFImageContentToken(content, offset)
		if !found {
			break
		}
		offset = next
		if !strings.HasPrefix(token, "/") || len(token) == 1 {
			return nil, false
		}
		filters = append(filters, strings.TrimPrefix(token, "/"))
	}
	return filters, len(filters) > 0
}

// parsePDFInlineDecodeParamsList 读取与过滤器数组逐项对应的 null 或参数字典。
func parsePDFInlineDecodeParamsList(value string) ([]*pdfInlineDecodeParams, bool) {
	value = strings.TrimSpace(value)
	if len(value) > pdfMaxInlineDictionaryBytes || !strings.HasPrefix(value, "[") || !strings.HasSuffix(value, "]") {
		return nil, false
	}
	content := []byte(strings.TrimSpace(value[1 : len(value)-1]))
	params := make([]*pdfInlineDecodeParams, 0, 2)
	for offset := 0; offset < len(content); {
		if len(params) >= 8 {
			return nil, false
		}
		token, next, found := nextPDFImageContentToken(content, offset)
		if !found {
			break
		}
		offset = next
		if strings.EqualFold(token, "null") {
			params = append(params, nil)
			continue
		}
		parsed, ok := parsePDFInlineDecodeParams(token)
		if !ok {
			return nil, false
		}
		params = append(params, parsed)
	}
	return params, len(params) > 0
}

// parsePDFInlineDecodeParams 有界读取 FlateDecode 预测字典。
// 间接对象、数组或非整数参数无法就地证明，返回失败以转视觉回退。
func parsePDFInlineDecodeParams(value string) (*pdfInlineDecodeParams, bool) {
	value = strings.TrimSpace(value)
	if len(value) > pdfMaxInlineDictionaryBytes || !strings.HasPrefix(value, "<<") || !strings.HasSuffix(value, ">>") {
		return nil, false
	}
	params := &pdfInlineDecodeParams{
		predictor: 1, colors: 1, bits: 8, columns: 1, earlyChange: 1, endOfBlock: true,
	}
	content := []byte(strings.TrimSpace(value[2 : len(value)-2]))
	key := ""
	for offset, tokens := 0, 0; offset < len(content); tokens++ {
		if tokens >= 64 {
			return nil, false
		}
		token, next, found := nextPDFImageContentToken(content, offset)
		if !found {
			break
		}
		offset = next
		if strings.HasPrefix(token, "/") {
			if key != "" {
				return nil, false
			}
			key = strings.TrimPrefix(token, "/")
			continue
		}
		if key == "" {
			continue
		}
		if key == "BlackIs1" || key == "EncodedByteAlign" || key == "EndOfLine" || key == "EndOfBlock" {
			flag, err := strconv.ParseBool(strings.ToLower(token))
			if err != nil {
				return nil, false
			}
			switch key {
			case "BlackIs1":
				params.blackIs1 = flag
			case "EncodedByteAlign":
				params.encodedByteAlign = flag
			case "EndOfLine":
				params.endOfLine = flag
			case "EndOfBlock":
				params.endOfBlock = flag
			}
			key = ""
			continue
		}
		number, err := strconv.ParseInt(token, 10, 64)
		if err != nil {
			return nil, false
		}
		switch key {
		case "Predictor":
			params.predictor = number
		case "Colors":
			params.colors = number
		case "BitsPerComponent":
			params.bits = number
		case "Columns":
			params.columns = number
			params.columnsSet = true
		case "Rows":
			params.rows = number
			params.rowsSet = true
		case "K":
			params.k = number
		case "DamagedRowsBeforeError":
			params.damagedRows = number
		case "EarlyChange":
			params.earlyChange = number
		}
		key = ""
	}
	if key != "" || params.predictor < 1 || params.colors <= 0 || params.bits <= 0 || params.columns <= 0 {
		return nil, false
	}
	return params, true
}

// decodePDFInlineImage 提取 JPEG，或把 8-bit Gray/RGB/CMYK 的无过滤及
// 基础过滤器像素编码为 PNG。未知颜色空间、位深或过滤器只保留几何。
func decodePDFInlineImage(inline pdfInlineImage, data []byte) *pdfExtractedImage {
	filter := strings.ToUpper(strings.TrimSpace(inline.filter))
	if len(data) == 0 || inline.filterInvalid || inline.decodeInvalid || inline.decodeParamsInvalid {
		return nil
	}
	if inline.imageMask {
		return decodePDFInlineImageMask(inline, data)
	}
	if inline.indexedColorSpace != nil {
		return decodePDFInlineIndexed(inline, data, *inline.indexedColorSpace)
	}
	if strings.HasPrefix(strings.TrimSpace(inline.colorSpace), "[") {
		indexed, ok := parsePDFInlineIndexedColorSpace(inline.colorSpace)
		if !ok {
			return nil
		}
		return decodePDFInlineIndexed(inline, data, indexed)
	}
	if len(inline.filters) > 0 {
		return decodePDFInlineFilterChain(inline, data)
	}
	if filter == "CCF" || filter == "CCITTFAXDECODE" {
		return decodePDFInlineCCITT(inline, data, inline.decodeParams)
	}
	if filter == "DCT" || filter == "DCTDECODE" {
		if len(inline.decode) > 0 {
			return nil
		}
		return decodePDFInlineJPEG(inline, data)
	}
	if inline.bits == 1 || inline.bits == 2 || inline.bits == 4 {
		return decodePDFInlinePackedGray(inline, data)
	}
	if filter != "" && filter != "FL" && filter != "FLATEDECODE" && filter != "LZW" && filter != "LZWDECODE" &&
		filter != "A85" && filter != "ASCII85DECODE" && filter != "AHX" && filter != "ASCIIHEXDECODE" &&
		filter != "RL" && filter != "RUNLENGTHDECODE" {
		return nil
	}
	if inline.decodeParams != nil && filter != "FL" && filter != "FLATEDECODE" && filter != "LZW" && filter != "LZWDECODE" {
		return nil
	}
	if inline.width <= 0 || inline.height <= 0 || inline.width > pdfMaxExtractDimension ||
		inline.height > pdfMaxExtractDimension || inline.bits != 8 {
		return nil
	}
	channels := pdfInlineImageChannels(inline.colorSpace)
	if channels == 0 || inline.width > math.MaxInt64/inline.height ||
		inline.width*inline.height > pdfMaxExtractPagePixels || inline.width*inline.height > math.MaxInt64/int64(channels) {
		return nil
	}
	expected := inline.width * inline.height * int64(channels)
	if expected > pdfMaxInlineDecodedBytes {
		return nil
	}
	pixels := data
	if filter == "A85" || filter == "ASCII85DECODE" || filter == "AHX" || filter == "ASCIIHEXDECODE" ||
		filter == "RL" || filter == "RUNLENGTHDECODE" {
		var decoded bool
		pixels, decoded = decodePDFInlineSimpleFilter(data, filter, expected)
		if !decoded {
			return nil
		}
	} else if filter == "FL" || filter == "FLATEDECODE" || filter == "LZW" || filter == "LZWDECODE" {
		encodedExpected, predictorRows, ok := pdfInlinePredictorShape(inline, channels, expected)
		if !ok {
			return nil
		}
		if filter == "FL" || filter == "FLATEDECODE" {
			reader, err := zlib.NewReader(bytes.NewReader(data))
			if err != nil {
				return nil
			}
			pixels, err = io.ReadAll(io.LimitReader(reader, encodedExpected+1))
			_ = reader.Close()
			if err != nil {
				return nil
			}
		} else {
			var decoded bool
			pixels, decoded = decodePDFInlineLZW(data, inline.decodeParams, encodedExpected)
			if !decoded {
				return nil
			}
		}
		if int64(len(pixels)) != encodedExpected {
			return nil
		}
		pixels = applyPDFInlinePredictor(pixels, inline.decodeParams, channels, expected, predictorRows)
		if pixels == nil {
			return nil
		}
	}
	if int64(len(pixels)) != expected {
		return nil
	}
	var decoded bool
	pixels, decoded = applyPDFInlineDecodeArray(pixels, channels, 8, inline.decode)
	if !decoded {
		return nil
	}
	return encodePDFInlineRaster(inline, pixels, channels)
}

// decodePDFInlineFilterChain 按声明顺序解码最多八级过滤器，并仅在末级
// 应用与图像尺寸一致的 Predictor，防止中间数据被错误解释为像素。
func decodePDFInlineFilterChain(inline pdfInlineImage, data []byte) *pdfExtractedImage {
	lastFilter := strings.ToUpper(strings.TrimSpace(inline.filters[len(inline.filters)-1]))
	if lastFilter == "CCF" || lastFilter == "CCITTFAXDECODE" {
		return decodePDFInlineCCITTFilterChain(inline, data)
	}
	if inline.bits == 1 || inline.bits == 2 || inline.bits == 4 {
		return decodePDFInlinePackedGray(inline, data)
	}
	if inline.width <= 0 || inline.height <= 0 || inline.width > pdfMaxExtractDimension ||
		inline.height > pdfMaxExtractDimension || inline.bits != 8 || len(inline.filters) > 8 {
		return nil
	}
	channels := pdfInlineImageChannels(inline.colorSpace)
	if channels == 0 || inline.width > math.MaxInt64/inline.height ||
		inline.width*inline.height > pdfMaxExtractPagePixels || inline.width*inline.height > math.MaxInt64/int64(channels) {
		return nil
	}
	expected := inline.width * inline.height * int64(channels)
	if expected > pdfMaxInlineDecodedBytes ||
		(len(inline.decodeParamsList) != 0 && len(inline.decodeParamsList) != len(inline.filters)) ||
		(inline.decodeParams != nil && (len(inline.filters) != 1 || len(inline.decodeParamsList) != 0)) {
		return nil
	}
	pixels := data
	for index, rawFilter := range inline.filters {
		filter := strings.ToUpper(strings.TrimSpace(rawFilter))
		last := index == len(inline.filters)-1
		var params *pdfInlineDecodeParams
		if len(inline.decodeParamsList) > 0 {
			params = inline.decodeParamsList[index]
		} else if inline.decodeParams != nil {
			params = inline.decodeParams
		}
		switch filter {
		case "A85", "ASCII85DECODE", "AHX", "ASCIIHEXDECODE", "RL", "RUNLENGTHDECODE":
			if params != nil {
				return nil
			}
			limit := int64(pdfMaxInlineDecodedBytes)
			if last {
				limit = expected
			}
			var ok bool
			pixels, ok = decodePDFInlineSimpleStage(pixels, filter, limit)
			if !ok {
				return nil
			}
		case "FL", "FLATEDECODE", "LZW", "LZWDECODE":
			if !last && params != nil && params.predictor != 1 {
				return nil
			}
			decodedLimit, predictorRows := int64(pdfMaxInlineDecodedBytes), inline.height
			if last {
				var ok bool
				decodedLimit, predictorRows, ok = pdfInlinePredictorShape(pdfInlineImage{
					width: inline.width, height: inline.height, decodeParams: params,
				}, channels, expected)
				if !ok {
					return nil
				}
			}
			var ok bool
			if filter == "FL" || filter == "FLATEDECODE" {
				pixels, ok = decodePDFInlineFlate(pixels, decodedLimit)
			} else {
				pixels, ok = decodePDFInlineLZW(pixels, params, decodedLimit)
			}
			if !ok || (last && int64(len(pixels)) != decodedLimit) {
				return nil
			}
			if last {
				pixels = applyPDFInlinePredictor(pixels, params, channels, expected, predictorRows)
				if pixels == nil {
					return nil
				}
			}
		case "DCT", "DCTDECODE":
			if !last || params != nil {
				return nil
			}
			return decodePDFInlineJPEG(inline, pixels)
		default:
			return nil
		}
	}
	if int64(len(pixels)) != expected {
		return nil
	}
	var decoded bool
	pixels, decoded = applyPDFInlineDecodeArray(pixels, channels, 8, inline.decode)
	if !decoded {
		return nil
	}
	return encodePDFInlineRaster(inline, pixels, channels)
}

// decodePDFInlinePackedGray 解码 1/2/4-bit DeviceGray；每行末尾 padding
// 独立跳过，并支持无过滤、基础包装、Flate/LZW 及其安全过滤器链。
func decodePDFInlinePackedGray(inline pdfInlineImage, data []byte) *pdfExtractedImage {
	if inline.width <= 0 || inline.height <= 0 || inline.width > pdfMaxExtractDimension ||
		inline.height > pdfMaxExtractDimension || (inline.bits != 1 && inline.bits != 2 && inline.bits != 4) {
		return nil
	}
	colorSpace := strings.ToUpper(strings.TrimSpace(inline.colorSpace))
	if colorSpace != "G" && colorSpace != "DEVICEGRAY" {
		return nil
	}
	if inline.width > math.MaxInt64/inline.height || inline.width*inline.height > pdfMaxExtractPagePixels ||
		inline.width > math.MaxInt64/inline.bits {
		return nil
	}
	rowBytes := (inline.width*inline.bits + 7) / 8
	if rowBytes <= 0 || rowBytes > math.MaxInt64/inline.height {
		return nil
	}
	packedExpected := rowBytes * inline.height
	packed, ok := decodePDFInlinePackedData(inline, data, packedExpected)
	if !ok {
		return nil
	}
	pixels := make([]byte, inline.width*inline.height)
	maxSample := byte((1 << uint(inline.bits)) - 1)
	for y := int64(0); y < inline.height; y++ {
		for x := int64(0); x < inline.width; x++ {
			sample := pdfInlinePackedSample(packed, rowBytes, x, y, inline.bits)
			pixels[y*inline.width+x] = byte(math.Round(float64(sample) * 255 / float64(maxSample)))
		}
	}
	var decoded bool
	pixels, decoded = applyPDFInlineDecodeArray(pixels, 1, 8, inline.decode)
	if !decoded {
		return nil
	}
	return encodePDFInlineRaster(inline, pixels, 1)
}

// decodePDFInlineIndexed 把 1/2/4/8-bit 索引样本映射为受限的
// DeviceGray、DeviceRGB 或 DeviceCMYK 调色板像素。
func decodePDFInlineIndexed(inline pdfInlineImage, data []byte, indexed pdfInlineIndexedColorSpace) *pdfExtractedImage {
	if inline.width <= 0 || inline.height <= 0 || inline.width > pdfMaxExtractDimension || inline.height > pdfMaxExtractDimension ||
		(inline.bits != 1 && inline.bits != 2 && inline.bits != 4 && inline.bits != 8) ||
		inline.width > math.MaxInt64/inline.height || inline.width*inline.height > pdfMaxExtractPagePixels ||
		inline.width > math.MaxInt64/inline.bits {
		return nil
	}
	maxSample := int64((1 << uint(inline.bits)) - 1)
	if indexed.channels != 1 && indexed.channels != 3 && indexed.channels != 4 ||
		indexed.highest < 0 || int64(indexed.highest) > maxSample ||
		len(indexed.lookup) != (indexed.highest+1)*indexed.channels {
		return nil
	}
	rowBytes := (inline.width*inline.bits + 7) / 8
	if rowBytes <= 0 || rowBytes > math.MaxInt64/inline.height ||
		inline.width*inline.height > math.MaxInt64/int64(indexed.channels) {
		return nil
	}
	packed, ok := decodePDFInlinePackedData(inline, data, rowBytes*inline.height)
	if !ok {
		return nil
	}
	decodeMinimum, decodeMaximum := 0.0, float64(maxSample)
	if len(inline.decode) > 0 {
		if len(inline.decode) != 2 || math.IsNaN(inline.decode[0]) || math.IsInf(inline.decode[0], 0) ||
			math.IsNaN(inline.decode[1]) || math.IsInf(inline.decode[1], 0) {
			return nil
		}
		decodeMinimum, decodeMaximum = inline.decode[0], inline.decode[1]
	}
	pixels := make([]byte, inline.width*inline.height*int64(indexed.channels))
	for y := int64(0); y < inline.height; y++ {
		for x := int64(0); x < inline.width; x++ {
			sample := float64(pdfInlinePackedSample(packed, rowBytes, x, y, inline.bits))
			paletteIndex := int(math.Round(decodeMinimum + sample/float64(maxSample)*(decodeMaximum-decodeMinimum)))
			paletteIndex = max(0, min(indexed.highest, paletteIndex))
			target := (y*inline.width + x) * int64(indexed.channels)
			copy(pixels[target:target+int64(indexed.channels)], indexed.lookup[paletteIndex*indexed.channels:(paletteIndex+1)*indexed.channels])
		}
	}
	return encodePDFInlineRaster(inline, pixels, indexed.channels)
}

// pdfInlinePackedSample 返回按行字节对齐、MSB 优先的单个索引或灰度样本。
func pdfInlinePackedSample(packed []byte, rowBytes, x, y, bits int64) byte {
	bitOffset := x * bits
	shift := uint(8 - bits - bitOffset%8)
	mask := byte((1 << uint(bits)) - 1)
	return (packed[y*rowBytes+bitOffset/8] >> shift) & mask
}

// decodePDFInlinePackedData 对已知最终长度的紧凑样本执行可选过滤器链。
// 非 8 位 Predictor 尚不能可靠还原，遇到时保持视觉回退。
func decodePDFInlinePackedData(inline pdfInlineImage, data []byte, packedExpected int64) ([]byte, bool) {
	if packedExpected <= 0 || packedExpected > pdfMaxInlineDecodedBytes {
		return nil, false
	}
	filters := inline.filters
	if len(filters) == 0 && strings.TrimSpace(inline.filter) != "" {
		filters = []string{inline.filter}
	}
	if len(filters) > 8 ||
		(len(inline.decodeParamsList) != 0 && len(inline.decodeParamsList) != len(filters)) ||
		(inline.decodeParams != nil && (len(filters) != 1 || len(inline.decodeParamsList) != 0)) {
		return nil, false
	}
	packed := data
	for index, rawFilter := range filters {
		filter := strings.ToUpper(strings.TrimSpace(rawFilter))
		last := index == len(filters)-1
		var params *pdfInlineDecodeParams
		if len(inline.decodeParamsList) > 0 {
			params = inline.decodeParamsList[index]
		} else if inline.decodeParams != nil {
			params = inline.decodeParams
		}
		limit := int64(pdfMaxInlineDecodedBytes)
		if last {
			limit = packedExpected
		}
		applyPNGPredictor := false
		if params != nil && params.predictor != 1 {
			if !last || params.predictor < 10 || params.predictor > 15 || params.colors != 1 ||
				params.bits != inline.bits || params.columns != inline.width ||
				packedExpected > pdfMaxInlineDecodedBytes-inline.height {
				return nil, false
			}
			limit = packedExpected + inline.height
			applyPNGPredictor = true
		}
		var ok bool
		switch filter {
		case "A85", "ASCII85DECODE", "AHX", "ASCIIHEXDECODE", "RL", "RUNLENGTHDECODE":
			if params != nil {
				return nil, false
			}
			packed, ok = decodePDFInlineSimpleStage(packed, filter, limit)
		case "FL", "FLATEDECODE":
			packed, ok = decodePDFInlineFlate(packed, limit)
		case "LZW", "LZWDECODE":
			packed, ok = decodePDFInlineLZW(packed, params, limit)
		default:
			return nil, false
		}
		if !ok {
			return nil, false
		}
		if last && int64(len(packed)) != limit {
			return nil, false
		}
		if applyPNGPredictor {
			rowBytes := packedExpected / inline.height
			packed = applyPDFInlinePredictor(
				packed,
				&pdfInlineDecodeParams{predictor: params.predictor, colors: 1, columns: rowBytes},
				1, packedExpected, inline.height,
			)
			if packed == nil {
				return nil, false
			}
		}
	}
	if int64(len(packed)) != packedExpected {
		return nil, false
	}
	return packed, true
}

// applyPDFInlineDecodeArray 把样本按每通道 Decode 区间线性映射到 8 位输出。
func applyPDFInlineDecodeArray(pixels []byte, channels, bits int, decode []float64) ([]byte, bool) {
	if len(decode) == 0 {
		return pixels, true
	}
	if channels <= 0 || bits <= 0 || bits > 8 || len(decode) != channels*2 || len(pixels)%channels != 0 {
		return nil, false
	}
	output := append([]byte(nil), pixels...)
	for index, sample := range output {
		channel := index % channels
		minimum, maximum := decode[channel*2], decode[channel*2+1]
		if math.IsNaN(minimum) || math.IsInf(minimum, 0) || math.IsNaN(maximum) || math.IsInf(maximum, 0) {
			return nil, false
		}
		mapped := minimum + float64(sample)/255*(maximum-minimum)
		mapped = math.Max(0, math.Min(1, mapped))
		output[index] = byte(math.Round(mapped * 255))
	}
	return output, true
}

// decodePDFInlineImageMask 使用当前非描边颜色渲染 1 位 stencil mask；
// 默认 Decode [0 1] 中样本 0 涂色，样本 1 透明，[1 0] 则反转。
func decodePDFInlineImageMask(inline pdfInlineImage, data []byte) *pdfExtractedImage {
	if inline.width <= 0 || inline.height <= 0 || inline.width > pdfMaxExtractDimension ||
		inline.height > pdfMaxExtractDimension || (inline.bits != 0 && inline.bits != 1) ||
		strings.TrimSpace(inline.colorSpace) != "" || inline.width > math.MaxInt64/inline.height ||
		inline.width*inline.height > pdfMaxExtractPagePixels {
		return nil
	}
	decodeMinimum, decodeMaximum := 0.0, 1.0
	if len(inline.decode) > 0 {
		if len(inline.decode) != 2 ||
			!((inline.decode[0] == 0 && inline.decode[1] == 1) || (inline.decode[0] == 1 && inline.decode[1] == 0)) {
			return nil
		}
		decodeMinimum, decodeMaximum = inline.decode[0], inline.decode[1]
	}
	decodedInline := inline
	decodedInline.bits = 1
	rowBytes := (inline.width + 7) / 8
	if rowBytes <= 0 || rowBytes > math.MaxInt64/inline.height {
		return nil
	}
	packed, ok := decodePDFInlinePackedData(decodedInline, data, rowBytes*inline.height)
	if !ok {
		return nil
	}
	width, height := int(inline.width), int(inline.height)
	raster := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := int64(0); y < inline.height; y++ {
		for x := int64(0); x < inline.width; x++ {
			sample := float64(pdfInlinePackedSample(packed, rowBytes, x, y, 1))
			mapped := decodeMinimum + sample*(decodeMaximum-decodeMinimum)
			if mapped >= 0.5 {
				continue
			}
			raster.SetNRGBA(int(x), int(y), inline.maskColor)
		}
	}
	return encodePDFInlineRasterImage(inline, raster)
}

// decodePDFInlineCCITTFilterChain 解码 CCITT 前允许的文本/游程包装过滤器，
// DecodeParms 必须与过滤器逐项对齐且 CCITT 必须位于末级。
func decodePDFInlineCCITTFilterChain(inline pdfInlineImage, data []byte) *pdfExtractedImage {
	if len(inline.decodeParamsList) != 0 && len(inline.decodeParamsList) != len(inline.filters) {
		return nil
	}
	if inline.decodeParams != nil && (len(inline.filters) != 1 || len(inline.decodeParamsList) != 0) {
		return nil
	}
	decoded := data
	for index, rawFilter := range inline.filters {
		filter := strings.ToUpper(strings.TrimSpace(rawFilter))
		var params *pdfInlineDecodeParams
		if len(inline.decodeParamsList) > 0 {
			params = inline.decodeParamsList[index]
		} else if inline.decodeParams != nil {
			params = inline.decodeParams
		}
		if index == len(inline.filters)-1 {
			if filter != "CCF" && filter != "CCITTFAXDECODE" {
				return nil
			}
			return decodePDFInlineCCITT(inline, decoded, params)
		}
		if params != nil {
			return nil
		}
		var ok bool
		decoded, ok = decodePDFInlineSimpleStage(decoded, filter, pdfMaxInlineDecodedBytes)
		if !ok {
			return nil
		}
	}
	return nil
}

// decodePDFInlineCCITT 解码 Group 3 一维或 Group 4 双值传真图，并把按行
// 字节对齐的位图扩展为灰度 PNG；当前依赖不支持 K>0 的 Group 3 混合模式。
func decodePDFInlineCCITT(inline pdfInlineImage, data []byte, params *pdfInlineDecodeParams) (asset *pdfExtractedImage) {
	defer func() {
		if recover() != nil {
			asset = nil
		}
	}()
	if len(inline.decode) > 0 || inline.width <= 0 || inline.height <= 0 || inline.width > pdfMaxExtractDimension ||
		inline.height > pdfMaxExtractDimension || (inline.bits != 0 && inline.bits != 1) {
		return nil
	}
	colorSpace := strings.ToUpper(strings.TrimSpace(inline.colorSpace))
	if colorSpace != "" && colorSpace != "G" && colorSpace != "DEVICEGRAY" {
		return nil
	}
	if inline.width > math.MaxInt64/inline.height || inline.width*inline.height > pdfMaxExtractPagePixels {
		return nil
	}
	filterParams := map[string]int{
		"Columns": int(inline.width), "Rows": int(inline.height),
	}
	if params != nil {
		if params.k > 0 || params.damagedRows != 0 || (params.columnsSet && params.columns != inline.width) ||
			(params.rowsSet && params.rows != inline.height) {
			return nil
		}
		filterParams["K"] = int(params.k)
		if params.blackIs1 {
			filterParams["BlackIs1"] = 1
		}
		if params.encodedByteAlign {
			filterParams["EncodedByteAlign"] = 1
		}
	}
	rowBytes := (inline.width + 7) / 8
	if rowBytes <= 0 || rowBytes > math.MaxInt64/inline.height {
		return nil
	}
	packedExpected := rowBytes * inline.height
	decoder, err := pdfcpufilter.NewFilter(pdfcpufilter.CCITTFax, filterParams, packedExpected+1)
	if err != nil {
		return nil
	}
	reader, err := decoder.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	packed, err := io.ReadAll(io.LimitReader(reader, packedExpected+1))
	if err != nil || int64(len(packed)) != packedExpected {
		return nil
	}
	pixels := make([]byte, inline.width*inline.height)
	for y := int64(0); y < inline.height; y++ {
		for x := int64(0); x < inline.width; x++ {
			if packed[y*rowBytes+x/8]&(0x80>>uint(x%8)) != 0 {
				pixels[y*inline.width+x] = 0xff
			}
		}
	}
	return encodePDFInlineRaster(inline, pixels, 1)
}

// encodePDFInlineRaster 把已验证长度的 Device 像素编码为 PNG 资产。
func encodePDFInlineRaster(inline pdfInlineImage, pixels []byte, channels int) *pdfExtractedImage {
	width, height := int(inline.width), int(inline.height)
	var raster image.Image
	switch channels {
	case 1:
		gray := image.NewGray(image.Rect(0, 0, width, height))
		copy(gray.Pix, pixels)
		raster = gray
	case 3:
		rgba := image.NewRGBA(image.Rect(0, 0, width, height))
		for source, target := 0, 0; source+2 < len(pixels); source, target = source+3, target+4 {
			rgba.Pix[target], rgba.Pix[target+1], rgba.Pix[target+2], rgba.Pix[target+3] =
				pixels[source], pixels[source+1], pixels[source+2], 0xff
		}
		raster = rgba
	case 4:
		cmyk := image.NewCMYK(image.Rect(0, 0, width, height))
		copy(cmyk.Pix, pixels)
		raster = cmyk
	}
	if raster == nil {
		return nil
	}
	return encodePDFInlineRasterImage(inline, raster)
}

// encodePDFInlineRasterImage 把已构造的栅格编码为受限 PNG 资产。
func encodePDFInlineRasterImage(inline pdfInlineImage, raster image.Image) *pdfExtractedImage {
	var encoded bytes.Buffer
	if raster == nil || png.Encode(&encoded, raster) != nil {
		return nil
	}
	asset := encoded.Bytes()
	if len(asset) > maxMediaDataURIBytes {
		asset = nil
	}
	return &pdfExtractedImage{MIMEType: "image/png", Data: asset, PixelWidth: inline.width, PixelHeight: inline.height}
}

// decodePDFInlineSimpleFilter 使用 pdfcpu 的纯 Go 解码器处理 ASCII85、
// ASCIIHex 和 RunLength；严格匹配声明像素长度并隔离畸形输入 panic。
func decodePDFInlineSimpleFilter(data []byte, filter string, expected int64) (pixels []byte, ok bool) {
	pixels, ok = decodePDFInlineSimpleStage(data, filter, expected)
	return pixels, ok && int64(len(pixels)) == expected
}

// decodePDFInlineSimpleStage 解码单级 ASCII85、ASCIIHex 或 RunLength，
// 并以调用方给定上限约束中间输出。
func decodePDFInlineSimpleStage(data []byte, filter string, limit int64) (pixels []byte, ok bool) {
	defer func() {
		if recover() != nil {
			pixels, ok = nil, false
		}
	}()
	filterName := ""
	switch filter {
	case "A85", "ASCII85DECODE":
		filterName = pdfcpufilter.ASCII85
	case "AHX", "ASCIIHEXDECODE":
		filterName = pdfcpufilter.ASCIIHex
	case "RL", "RUNLENGTHDECODE":
		filterName = pdfcpufilter.RunLength
	default:
		return nil, false
	}
	if limit < 0 || limit > pdfMaxInlineDecodedBytes {
		return nil, false
	}
	decoder, err := pdfcpufilter.NewFilter(filterName, nil, limit+1)
	if err != nil {
		return nil, false
	}
	reader, err := decoder.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, false
	}
	pixels, err = io.ReadAll(io.LimitReader(reader, limit+1))
	return pixels, err == nil && int64(len(pixels)) <= limit
}

// decodePDFInlineFlate 有界解压单级 Flate 数据并隔离畸形输入。
func decodePDFInlineFlate(data []byte, limit int64) (pixels []byte, ok bool) {
	defer func() {
		if recover() != nil {
			pixels, ok = nil, false
		}
	}()
	if limit < 0 || limit > pdfMaxInlineDecodedBytes {
		return nil, false
	}
	reader, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, false
	}
	defer reader.Close()
	pixels, err = io.ReadAll(io.LimitReader(reader, limit+1))
	return pixels, err == nil && int64(len(pixels)) <= limit
}

// decodePDFInlineLZW 使用项目已依赖的 pdfcpu 纯 Go LZW 解码器，
// 支持 PDF EarlyChange 0/1 语义；输出长度由已验证的像素尺寸限定。
func decodePDFInlineLZW(data []byte, params *pdfInlineDecodeParams, expected int64) (pixels []byte, ok bool) {
	defer func() {
		if recover() != nil {
			pixels, ok = nil, false
		}
	}()
	earlyChange := int64(1)
	if params != nil {
		earlyChange = params.earlyChange
	}
	if earlyChange != 0 && earlyChange != 1 {
		return nil, false
	}
	decoder, err := pdfcpufilter.NewFilter(
		pdfcpufilter.LZW, map[string]int{"EarlyChange": int(earlyChange)}, expected+1,
	)
	if err != nil {
		return nil, false
	}
	reader, err := decoder.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, false
	}
	pixels, err = io.ReadAll(io.LimitReader(reader, expected+1))
	return pixels, err == nil && int64(len(pixels)) <= expected
}

// pdfInlinePredictorShape 校验预测参数并返回解压字节数和预测行数。
func pdfInlinePredictorShape(inline pdfInlineImage, channels int, expected int64) (encodedBytes, rows int64, ok bool) {
	params := inline.decodeParams
	if params == nil || params.predictor == 1 {
		return expected, inline.height, true
	}
	if params.bits != 8 || params.colors != int64(channels) || params.columns != inline.width ||
		params.colors > math.MaxInt64/params.columns {
		return 0, 0, false
	}
	rowBytes := params.colors * params.columns
	if rowBytes <= 0 || expected%rowBytes != 0 {
		return 0, 0, false
	}
	rows = expected / rowBytes
	switch {
	case params.predictor == 2:
		return expected, rows, true
	case params.predictor >= 10 && params.predictor <= 15 && expected <= math.MaxInt64-rows:
		return expected + rows, rows, true
	default:
		return 0, 0, false
	}
}

// applyPDFInlinePredictor 还原 8-bit TIFF 水平差分或 PNG 逐行过滤。
func applyPDFInlinePredictor(data []byte, params *pdfInlineDecodeParams, channels int, expected, rows int64) []byte {
	if params == nil || params.predictor == 1 {
		return data
	}
	rowBytes := int(params.columns * params.colors)
	if rowBytes <= 0 || rows <= 0 || rows > int64(^uint(0)>>1) {
		return nil
	}
	if params.predictor == 2 {
		for row := 0; row < int(rows); row++ {
			start := row * rowBytes
			for index := channels; index < rowBytes; index++ {
				data[start+index] += data[start+index-channels]
			}
		}
		return data
	}
	output := make([]byte, int(expected))
	inputOffset, outputOffset := 0, 0
	for row := 0; row < int(rows); row++ {
		filter := int(data[inputOffset])
		inputOffset++
		// PDF 允许 Predictor 10..15 与行首实际 PNG 过滤器不同，
		// 因此只以行首 0..4 为权威算法标记。
		if filter > 4 {
			return nil
		}
		for index := 0; index < rowBytes; index++ {
			raw := data[inputOffset+index]
			left, up, upperLeft := byte(0), byte(0), byte(0)
			if index >= channels {
				left = output[outputOffset+index-channels]
			}
			if row > 0 {
				up = output[outputOffset-rowBytes+index]
				if index >= channels {
					upperLeft = output[outputOffset-rowBytes+index-channels]
				}
			}
			switch filter {
			case 1:
				raw += left
			case 2:
				raw += up
			case 3:
				raw += byte((int(left) + int(up)) / 2)
			case 4:
				raw += pdfPaethPredictor(left, up, upperLeft)
			}
			output[outputOffset+index] = raw
		}
		inputOffset += rowBytes
		outputOffset += rowBytes
	}
	return output
}

// pdfPaethPredictor 返回 PNG Paeth 过滤器的最近邻居字节。
func pdfPaethPredictor(left, up, upperLeft byte) byte {
	prediction := int(left) + int(up) - int(upperLeft)
	leftDistance := absPDFInt(prediction - int(left))
	upDistance := absPDFInt(prediction - int(up))
	upperLeftDistance := absPDFInt(prediction - int(upperLeft))
	if leftDistance <= upDistance && leftDistance <= upperLeftDistance {
		return left
	}
	if upDistance <= upperLeftDistance {
		return up
	}
	return upperLeft
}

// absPDFInt 返回预测器小整数差的绝对值。
func absPDFInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// decodePDFInlineJPEG 校验 DCTDecode 数据与 BI 尺寸声明一致。
func decodePDFInlineJPEG(inline pdfInlineImage, data []byte) *pdfExtractedImage {
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != "jpeg" || config.Width <= 0 || config.Height <= 0 {
		return nil
	}
	width, height := int64(config.Width), int64(config.Height)
	if inline.width > 0 && inline.height > 0 && (inline.width != width || inline.height != height) {
		return nil
	}
	asset := append([]byte(nil), data...)
	if len(asset) > maxMediaDataURIBytes {
		asset = nil
	}
	return &pdfExtractedImage{MIMEType: "image/jpeg", Data: asset, PixelWidth: width, PixelHeight: height}
}

// pdfInlineImageChannels 返回可确定解释的 Device 颜色空间通道数。
func pdfInlineImageChannels(colorSpace string) int {
	switch strings.ToUpper(strings.TrimSpace(colorSpace)) {
	case "G", "DEVICEGRAY":
		return 1
	case "RGB", "DEVICERGB":
		return 3
	case "CMYK", "DEVICECMYK":
		return 4
	default:
		return 0
	}
}

// findPDFInlineImageEnd 查找由空白分隔的 EI；为降低压缩数据内偶然命中的
// 风险，终止符之后必须是文件尾、图形状态 Q，或可继续解析的内容 token。
func findPDFInlineImageEnd(content []byte, start int) (imageEnd, after int, ok bool) {
	for index := start + 1; index+1 < len(content); index++ {
		if content[index] != 'E' || content[index+1] != 'I' || !isPDFImageWhitespace(content[index-1]) {
			continue
		}
		next := index + 2
		if next < len(content) && !isPDFImageWhitespace(content[next]) {
			continue
		}
		for next < len(content) && isPDFImageWhitespace(content[next]) {
			next++
		}
		if next == len(content) {
			return index, next, true
		}
		token, _, found := nextPDFImageContentToken(content, next)
		if found && (token == "Q" || token == "q" || token == "BT" || token == "ET" || token == "cm" ||
			strings.HasPrefix(token, "/") || isPDFImageNumberToken(token)) {
			return index, index + 2, true
		}
	}
	return 0, 0, false
}

// nextPDFImageContentToken 返回内容流的下一个外层 token；字符串、数组、
// 十六进制串和字典作为整体跳过，防止其中的 BI/EI 文本产生误报。
func nextPDFImageContentToken(content []byte, offset int) (string, int, bool) {
	offset = skipPDFImageContentSpace(content, offset)
	if offset >= len(content) {
		return "", offset, false
	}
	switch content[offset] {
	case '(':
		end := skipPDFImageLiteralString(content, offset)
		if end-offset > pdfMaxInlineDictionaryBytes {
			return "invalid-string", end, true
		}
		return string(content[offset:end]), end, true
	case '[':
		end := skipPDFImageBalanced(content, offset, '[', ']')
		if end-offset > pdfMaxInlineDictionaryBytes {
			return "invalid-array", end, true
		}
		return string(content[offset:end]), end, true
	case '<':
		if offset+1 < len(content) && content[offset+1] == '<' {
			end := skipPDFImageDictionary(content, offset)
			if end-offset > pdfMaxInlineDictionaryBytes {
				return "invalid-dictionary", end, true
			}
			return string(content[offset:end]), end, true
		}
		end := skipPDFImageUntil(content, offset+1, '>')
		if end-offset > pdfMaxInlineDictionaryBytes {
			return "invalid-hex-string", end, true
		}
		return string(content[offset:end]), end, true
	case '/':
		end := offset + 1
		for end < len(content) && !isPDFImageDelimiter(content[end]) {
			end++
		}
		return string(content[offset:end]), end, true
	default:
		end := offset
		for end < len(content) && !isPDFImageDelimiter(content[end]) {
			end++
		}
		if end == offset {
			return string(content[offset : offset+1]), offset + 1, true
		}
		return string(content[offset:end]), end, true
	}
}

// skipPDFImageContentSpace 跳过 PDF 空白和百分号行注释。
func skipPDFImageContentSpace(content []byte, offset int) int {
	for offset < len(content) {
		if isPDFImageWhitespace(content[offset]) {
			offset++
			continue
		}
		if content[offset] != '%' {
			break
		}
		for offset < len(content) && content[offset] != '\n' && content[offset] != '\r' {
			offset++
		}
	}
	return offset
}

// skipPDFImageLiteralString 跳过支持嵌套括号和反斜线转义的字符串。
func skipPDFImageLiteralString(content []byte, offset int) int {
	depth := 0
	for offset < len(content) {
		switch content[offset] {
		case '\\':
			offset += 2
			continue
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return offset + 1
			}
		}
		offset++
	}
	return len(content)
}

// skipPDFImageBalanced 跳过简单的数组结构，字符串内容单独处理。
func skipPDFImageBalanced(content []byte, offset int, open, close byte) int {
	depth := 0
	for offset < len(content) {
		if content[offset] == '(' {
			offset = skipPDFImageLiteralString(content, offset)
			continue
		}
		if content[offset] == open {
			depth++
		} else if content[offset] == close {
			depth--
			if depth == 0 {
				return offset + 1
			}
		}
		offset++
	}
	return len(content)
}

// skipPDFImageDictionary 跳过可能嵌套的 << >> 字典。
func skipPDFImageDictionary(content []byte, offset int) int {
	depth := 0
	for offset+1 < len(content) {
		if content[offset] == '(' {
			offset = skipPDFImageLiteralString(content, offset)
			continue
		}
		if content[offset] == '<' && content[offset+1] == '<' {
			depth++
			offset += 2
			continue
		}
		if content[offset] == '>' && content[offset+1] == '>' {
			depth--
			offset += 2
			if depth == 0 {
				return offset
			}
			continue
		}
		offset++
	}
	return len(content)
}

// skipPDFImageUntil 跳过到指定结束符（含结束符）。
func skipPDFImageUntil(content []byte, offset int, end byte) int {
	for offset < len(content) && content[offset] != end {
		offset++
	}
	return min(len(content), offset+1)
}

// isPDFImageWhitespace 判断 PDF 内容流空白。
func isPDFImageWhitespace(value byte) bool {
	return value == 0 || value == 9 || value == 10 || value == 12 || value == 13 || value == 32
}

// isPDFImageDelimiter 判断 PDF token 分隔符。
func isPDFImageDelimiter(value byte) bool {
	return isPDFImageWhitespace(value) || strings.ContainsRune("()<>[]{}/%", rune(value))
}

// isPDFImageNumberToken 判断 token 是否为有限数值操作数。
func isPDFImageNumberToken(token string) bool {
	value, err := strconv.ParseFloat(token, 64)
	return err == nil && !math.IsNaN(value) && !math.IsInf(value, 0)
}

// popPDFOperands 按原始顺序取出当前操作符之前的全部操作数。
func popPDFOperands(stack *pdf.Stack) []pdf.Value {
	values := make([]pdf.Value, stack.Len())
	for i := len(values) - 1; i >= 0; i-- {
		values[i] = stack.Pop()
	}
	return values
}

// pdfImageBBox 计算单位图片方形的旋转/倾斜后外接矩形并裁剪到页面。
func pdfImageBBox(matrix pdfAffine, pageWidth, pageHeight float64) *DoclingBBox {
	points := [4][2]float64{}
	points[0][0], points[0][1] = transformPDFPoint(matrix, 0, 0)
	points[1][0], points[1][1] = transformPDFPoint(matrix, 1, 0)
	points[2][0], points[2][1] = transformPDFPoint(matrix, 0, 1)
	points[3][0], points[3][1] = transformPDFPoint(matrix, 1, 1)
	bbox := &DoclingBBox{L: points[0][0], R: points[0][0], B: points[0][1], T: points[0][1], CoordOrigin: CoordOriginBottomLeft}
	for _, point := range points[1:] {
		bbox.L = math.Min(bbox.L, point[0])
		bbox.R = math.Max(bbox.R, point[0])
		bbox.B = math.Min(bbox.B, point[1])
		bbox.T = math.Max(bbox.T, point[1])
	}
	if pageWidth > 0 {
		bbox.L = math.Max(0, math.Min(pageWidth, bbox.L))
		bbox.R = math.Max(0, math.Min(pageWidth, bbox.R))
	}
	if pageHeight > 0 {
		bbox.B = math.Max(0, math.Min(pageHeight, bbox.B))
		bbox.T = math.Max(0, math.Min(pageHeight, bbox.T))
	}
	if bbox.R <= bbox.L || bbox.T <= bbox.B {
		return nil
	}
	return bbox
}

// pdfImageCoverage 计算图片 bbox 矩形并集占页面面积的比例。
func pdfImageCoverage(images []pdfImagePlacement, pageWidth, pageHeight float64) float64 {
	if len(images) == 0 || pageWidth <= 0 || pageHeight <= 0 {
		return 0
	}
	xs := make([]float64, 0, len(images)*2)
	for _, image := range images {
		if image.BBox != nil {
			xs = append(xs, image.BBox.L, image.BBox.R)
		}
	}
	sort.Float64s(xs)
	area := 0.0
	for i := 0; i+1 < len(xs); i++ {
		left, right := xs[i], xs[i+1]
		if right <= left {
			continue
		}
		intervals := make([][2]float64, 0, len(images))
		for _, image := range images {
			if image.BBox != nil && image.BBox.L < right && image.BBox.R > left {
				intervals = append(intervals, [2]float64{image.BBox.B, image.BBox.T})
			}
		}
		sort.Slice(intervals, func(i, j int) bool { return intervals[i][0] < intervals[j][0] })
		height := 0.0
		for j := 0; j < len(intervals); {
			bottom, top := intervals[j][0], intervals[j][1]
			j++
			for j < len(intervals) && intervals[j][0] <= top {
				top = math.Max(top, intervals[j][1])
				j++
			}
			height += top - bottom
		}
		area += (right - left) * height
	}
	return math.Max(0, math.Min(1, area/(pageWidth*pageHeight)))
}

// extractPDFEmbeddedPictureLines 把页面 Image XObject 几何与 pdfcpu 解码结果
// 关联为图片中间行。单个资源被多次绘制时复用同一像素数据但保留每次 bbox；
// 解码失败、资源不受支持或超过安全上限时只跳过该图片，不影响 PDF 正文。
func extractPDFEmbeddedPictureLines(data []byte, placementsByPage map[int64][]pdfImagePlacement) []pdfLine {
	runtime, err := newPDFRuntime(data, PDFLimits{})
	if err != nil {
		return extractPDFEmbeddedPictureLinesWithContext(nil, placementsByPage)
	}
	return extractPDFEmbeddedPictureLinesWithContext(runtime.context, placementsByPage)
}

// extractPDFEmbeddedPictureLinesWithContext 复用主解析阶段已通过安全预检的
// pdfcpu Context；单张资源解码失败仍由逐对象隔离逻辑跳过。
func extractPDFEmbeddedPictureLinesWithContext(context *pdfcpumodel.Context, placementsByPage map[int64][]pdfImagePlacement) []pdfLine {
	selectedPages := safePDFImagePages(placementsByPage)
	if len(selectedPages) == 0 {
		return nil
	}
	lines := make([]pdfLine, 0)
	for _, pageIdx := range selectedPages {
		for _, placement := range placementsByPage[pageIdx] {
			if placement.InlineImage == nil || placement.BBox == nil {
				continue
			}
			resource := placement.InlineImage
			imageRef := &ImageRef{
				Mimetype: resource.MIMEType,
				Dpi:      effectivePDFImageDPI(resource.PixelWidth, resource.PixelHeight, placement.BBox),
				Size:     &ImageSize{Width: float64(resource.PixelWidth), Height: float64(resource.PixelHeight)},
				URI:      pdfImageDataURI(resource.MIMEType, resource.Data),
			}
			lines = append(lines, pdfLine{PageIdx: pageIdx, Picture: imageRef,
				MinX: placement.BBox.L, MaxX: placement.BBox.R, MinY: placement.BBox.B, MaxY: placement.BBox.T})
		}
	}
	if context == nil {
		return lines
	}
	extractedByPage := collectPDFCPUPageImages(selectedPages, func(pageNo int) (map[int]pdfcpumodel.Image, error) {
		return extractPDFCPUPageImages(context, pageNo)
	}, func(objectNumber int, decoded pdfExtractedImage) pdfExtractedImage {
		decoded = decodePDFJPXImage(decoded)
		return applyPDFLowDepthSoftMask(context, objectNumber, decodePDFJBIG2Image(context, objectNumber, decoded))
	})

	for _, pageIdx := range selectedPages {
		resources := extractedByPage[pageIdx]
		for _, placement := range placementsByPage[pageIdx] {
			if placement.InlineImage != nil {
				continue
			}
			resource, ok := matchPDFExtractedImage(resources, placement)
			if !ok || placement.BBox == nil {
				continue
			}
			width, height := resource.PixelWidth, resource.PixelHeight
			if width <= 0 {
				width = placement.PixelWidth
			}
			if height <= 0 {
				height = placement.PixelHeight
			}
			imageRef := &ImageRef{
				Mimetype: resource.MIMEType,
				Dpi:      effectivePDFImageDPI(width, height, placement.BBox),
				Size:     &ImageSize{Width: float64(width), Height: float64(height)},
				URI:      pdfImageDataURI(resource.MIMEType, resource.Data),
			}
			lines = append(lines, pdfLine{
				PageIdx: pageIdx, Picture: imageRef,
				MinX: placement.BBox.L, MaxX: placement.BBox.R,
				MinY: placement.BBox.B, MaxY: placement.BBox.T,
			})
		}
	}
	return lines
}

// collectPDFCPUPageImages 按页读取已优化的 pdfcpu 上下文。单页图片资源损坏
// 只跳过该页，不能像批量 API 一样丢弃此前已经成功提取的其他页面。apply 在
// 每个对象成功解码后执行（如低位深软蒙版合成），失败不回滚原提取结果。
func collectPDFCPUPageImages(selectedPages []int64, extract func(pageNo int) (map[int]pdfcpumodel.Image, error), apply func(objectNumber int, decoded pdfExtractedImage) pdfExtractedImage) map[int64][]pdfExtractedImage {
	extractedByPage := make(map[int64][]pdfExtractedImage, len(selectedPages))
	if extract == nil {
		return extractedByPage
	}
	for _, pageIdx := range selectedPages {
		imageMap, err := extract(int(pageIdx + 1))
		if err != nil {
			continue
		}
		objectNumbers := make([]int, 0, len(imageMap))
		for objectNumber := range imageMap {
			objectNumbers = append(objectNumbers, objectNumber)
		}
		sort.Ints(objectNumbers)
		for _, objectNumber := range objectNumbers {
			resource := imageMap[objectNumber]
			decoded, ok := readPDFExtractedImage(resource)
			if !ok {
				continue
			}
			if apply != nil {
				decoded = apply(objectNumber, decoded)
			}
			decoded, ok = validatePDFExtractedImage(decoded)
			if !ok {
				continue
			}
			extractedByPage[pageIdx] = append(extractedByPage[pageIdx], decoded)
		}
	}
	return extractedByPage
}

// extractPDFCPUPageImages 逐对象提取单页图片。pdfcpu 的页级 API 遇到一个
// 损坏蒙版会放弃整页；这里把故障隔离到单个对象，保留同页其余有效图片。
func extractPDFCPUPageImages(context *pdfcpumodel.Context, pageNo int) (images map[int]pdfcpumodel.Image, err error) {
	if context == nil || context.Optimize == nil || pageNo < 1 {
		return nil, fmt.Errorf("invalid PDF image extraction context or page %d", pageNo)
	}
	images = make(map[int]pdfcpumodel.Image)
	for _, objectNumber := range pdfcpucore.ImageObjNrs(context, pageNo) {
		object := context.Optimize.ImageObjects[objectNumber]
		if object == nil || object.ImageDict == nil {
			continue
		}
		resourceName, ok := object.ResourceNames[pageNo-1]
		if !ok {
			continue
		}
		image, imageErr := extractPDFCPUImage(context, object, resourceName, objectNumber)
		if imageErr != nil || image == nil {
			continue
		}
		image.PageNr = pageNo
		images[objectNumber] = *image
	}
	return images, nil
}

// extractPDFCPUImage 把第三方解码器可能出现的 panic 转为当前图片失败。
func extractPDFCPUImage(context *pdfcpumodel.Context, object *pdfcpumodel.ImageObject, resourceName string, objectNumber int) (image *pdfcpumodel.Image, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			image = nil
			err = fmt.Errorf("extract PDF image object %d panic: %v", objectNumber, recovered)
		}
	}()
	return pdfcpucore.ExtractImage(context, object.ImageDict, false, resourceName, objectNumber, false)
}

// safePDFImagePages 返回允许进入像素解码的 0 起页号。安全预算按页面中
// 不同资源名累计声明像素，重复绘制同一资源不会重复计入。
func safePDFImagePages(placementsByPage map[int64][]pdfImagePlacement) []int64 {
	pages := make([]int64, 0, len(placementsByPage))
	for pageIdx, placements := range placementsByPage {
		seen := map[string]bool{}
		pixels := int64(0)
		safe := len(placements) > 0
		for _, placement := range placements {
			if placement.PixelWidth <= 0 || placement.PixelHeight <= 0 ||
				placement.PixelWidth > pdfMaxExtractDimension || placement.PixelHeight > pdfMaxExtractDimension {
				safe = false
				break
			}
			key := placement.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			if placement.PixelWidth > pdfMaxExtractPagePixels/placement.PixelHeight {
				safe = false
				break
			}
			pixels += placement.PixelWidth * placement.PixelHeight
			if pixels > pdfMaxExtractPagePixels {
				safe = false
				break
			}
		}
		if safe {
			pages = append(pages, pageIdx)
		}
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i] < pages[j] })
	return pages
}

// readPDFExtractedImage 读取单个 pdfcpu 图片结果并限制可内嵌字节数。
func readPDFExtractedImage(resource pdfcpumodel.Image) (pdfExtractedImage, bool) {
	if resource.Reader == nil || resource.Thumb {
		return pdfExtractedImage{}, false
	}
	raw, err := io.ReadAll(io.LimitReader(resource.Reader, maxMediaDataURIBytes+1))
	if err != nil || len(raw) == 0 {
		return pdfExtractedImage{}, false
	}
	mimeType := pdfExtractedImageMIME(resource.FileType, raw)
	width, height := int64(resource.Width), int64(resource.Height)
	if config, _, decodeErr := image.DecodeConfig(bytes.NewReader(raw)); decodeErr == nil {
		width, height = int64(config.Width), int64(config.Height)
	}
	// 超限资源仍保留类型和尺寸，但不把截断内容写入 URI。
	if len(raw) > maxMediaDataURIBytes {
		raw = nil
	}
	return pdfExtractedImage{
		Name: strings.TrimPrefix(resource.Name, "/"), MIMEType: mimeType,
		Data: raw, PixelWidth: width, PixelHeight: height,
	}, true
}

// validatePDFExtractedImage 在生成 data URI 前验证最终图片字节和解码尺寸。
// 损坏、无法验证或超出 pdfcpu 图片预算的单张资源直接跳过，不影响正文。
func validatePDFExtractedImage(decoded pdfExtractedImage) (pdfExtractedImage, bool) {
	if len(decoded.Data) == 0 {
		return pdfExtractedImage{}, false
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(decoded.Data))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return pdfExtractedImage{}, false
	}
	width, height := int64(config.Width), int64(config.Height)
	limits := newPDFCPUConfiguration().Limits
	if width > pdfMaxExtractDimension || height > pdfMaxExtractDimension ||
		width > limits.MaxImagePixels/height {
		return pdfExtractedImage{}, false
	}
	pixels := width * height
	if pixels > limits.MaxImagePixels || pixels > limits.MaxImageBytes/4 {
		return pdfExtractedImage{}, false
	}
	decoded.PixelWidth, decoded.PixelHeight = width, height
	return decoded, true
}

// matchPDFExtractedImage 按资源名优先、像素尺寸次之关联几何和像素数据。
func matchPDFExtractedImage(resources []pdfExtractedImage, placement pdfImagePlacement) (pdfExtractedImage, bool) {
	name := strings.TrimPrefix(placement.Name, "/")
	for _, resource := range resources {
		if resource.Name == name {
			return resource, true
		}
	}
	for _, resource := range resources {
		if resource.PixelWidth == placement.PixelWidth && resource.PixelHeight == placement.PixelHeight {
			return resource, true
		}
	}
	if len(resources) == 1 {
		return resources[0], true
	}
	return pdfExtractedImage{}, false
}

// pdfExtractedImageMIME 根据 pdfcpu 文件类型和实际魔数返回图片 MIME。
func pdfExtractedImageMIME(fileType string, data []byte) string {
	if ext := detectImageExt(data); ext != "" {
		return ooxmlMediaMime(ext)
	}
	switch strings.ToLower(strings.TrimPrefix(fileType, ".")) {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "tif", "tiff":
		return "image/tiff"
	case "jpx", "jp2", "jpeg2000":
		return "image/jp2"
	case "jbig2", "jb2":
		return "image/jbig2"
	default:
		return "application/octet-stream"
	}
}

// effectivePDFImageDPI 依据图片像素和页面显示尺寸估算有效 DPI。
func effectivePDFImageDPI(pixelWidth, pixelHeight int64, bbox *DoclingBBox) int64 {
	if bbox == nil || pixelWidth <= 0 || pixelHeight <= 0 || bbox.R <= bbox.L || bbox.T <= bbox.B {
		return defaultImageDPI
	}
	xDPI := float64(pixelWidth) * 72 / (bbox.R - bbox.L)
	yDPI := float64(pixelHeight) * 72 / (bbox.T - bbox.B)
	dpi := int64(math.Round((xDPI + yDPI) / 2))
	if dpi < 1 {
		return 1
	}
	if dpi > 2400 {
		return 2400
	}
	return dpi
}

// pdfImageDataURI 将受大小限制后的图片字节编码为 data URI。
func pdfImageDataURI(mimeType string, data []byte) string {
	if len(data) == 0 {
		return ""
	}
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// mergePDFPictureLines 把图片插入已完成表格恢复和 XY-cut 的文本流。若图片
// 与文本共同形成稳定栏位则重新执行 XY-cut；否则只在相关水平区域内按纵向
// 插入，避免破坏已经恢复好的多栏正文顺序。
func mergePDFPictureLines(lines, pictures []pdfLine) []pdfLine {
	if len(pictures) == 0 {
		return lines
	}
	remainingPictures := make([]pdfLine, 0, len(pictures))
	for _, picture := range pictures {
		if !attachPDFPictureToVisualResult(lines, picture) {
			remainingPictures = append(remainingPictures, picture)
		}
	}
	pictures = remainingPictures
	if len(pictures) == 0 {
		return lines
	}
	pages := map[int64][]pdfLine{}
	pageNumbers := map[int64]bool{}
	for _, line := range lines {
		pages[line.PageIdx] = append(pages[line.PageIdx], line)
		pageNumbers[line.PageIdx] = true
	}
	picturesByPage := map[int64][]pdfLine{}
	for _, picture := range pictures {
		picturesByPage[picture.PageIdx] = append(picturesByPage[picture.PageIdx], picture)
		pageNumbers[picture.PageIdx] = true
	}
	orderedPages := make([]int64, 0, len(pageNumbers))
	for pageIdx := range pageNumbers {
		orderedPages = append(orderedPages, pageIdx)
	}
	sort.Slice(orderedPages, func(i, j int) bool { return orderedPages[i] < orderedPages[j] })
	result := make([]pdfLine, 0, len(lines)+len(pictures))
	for _, pageIdx := range orderedPages {
		pageLines := pages[pageIdx]
		pagePictures := pdfLayoutSortTopDown(picturesByPage[pageIdx])
		combined := append(append([]pdfLine(nil), pageLines...), pagePictures...)
		if len(combined) >= pdfLayoutMinColumnLines*2 && pdfLayoutLinesHaveBounds(combined) {
			if _, ok := findPDFLayoutMainCut(combined); ok {
				result = append(result, layoutPDFRegionXYCut(combined, 0)...)
				continue
			}
		}
		for _, picture := range pagePictures {
			pageLines = insertPDFPictureLine(pageLines, picture)
		}
		result = append(result, pageLines...)
	}
	return result
}

// attachPDFPictureToVisualResult 将原始图片资产挂到同区域的视觉 PictureItem，
// 避免一个图既作为无说明原图、又作为带说明视觉图重复进入多模态知识库。
func attachPDFPictureToVisualResult(lines []pdfLine, picture pdfLine) bool {
	if picture.Picture == nil {
		return false
	}
	pictureBBox := &DoclingBBox{
		L: picture.MinX, B: picture.MinY, R: picture.MaxX, T: picture.MaxY,
		CoordOrigin: CoordOriginBottomLeft,
	}
	for lineIndex := range lines {
		sub := lines[lineIndex].OCRSub
		if sub == nil {
			continue
		}
		for pictureIndex := range sub.Pictures {
			visualPicture := &sub.Pictures[pictureIndex]
			for _, prov := range visualPicture.Prov {
				if prov.BBox == nil || prov.PageNo > 0 && prov.PageNo != picture.PageIdx+1 ||
					pdfBBoxIntersectionRatio(pictureBBox, prov.BBox) < 0.25 {
					continue
				}
				if visualPicture.Image == nil || visualPicture.Image.URI == "" {
					visualPicture.Image = picture.Picture
				}
				return true
			}
		}
	}
	return false
}

// pdfBBoxIntersectionRatio 返回交集面积占较小矩形面积的比例。
func pdfBBoxIntersectionRatio(left, right *DoclingBBox) float64 {
	if left == nil || right == nil {
		return 0
	}
	width := math.Min(left.R, right.R) - math.Max(left.L, right.L)
	height := math.Min(left.T, right.T) - math.Max(left.B, right.B)
	if width <= 0 || height <= 0 {
		return 0
	}
	leftArea := (left.R - left.L) * (left.T - left.B)
	rightArea := (right.R - right.L) * (right.T - right.B)
	if leftArea <= 0 || rightArea <= 0 {
		return 0
	}
	return width * height / math.Min(leftArea, rightArea)
}

// insertPDFPictureLine 在不改变已有文本相对顺序的前提下插入一张图片。
func insertPDFPictureLine(lines []pdfLine, picture pdfLine) []pdfLine {
	insertAt, lastRelated := -1, -1
	pictureCenter := pdfLayoutLineCenterY(picture)
	for index, line := range lines {
		overlap := math.Min(line.MaxX, picture.MaxX) - math.Max(line.MinX, picture.MinX)
		if overlap <= 0 {
			continue
		}
		lastRelated = index
		if pdfLayoutLineCenterY(line) < pictureCenter {
			insertAt = index
			break
		}
	}
	if insertAt < 0 && lastRelated >= 0 {
		insertAt = lastRelated + 1
	}
	if insertAt < 0 {
		insertAt = len(lines)
		for index, line := range lines {
			if pdfLayoutLineCenterY(line) < pictureCenter {
				insertAt = index
				break
			}
		}
	}
	lines = append(lines, pdfLine{})
	copy(lines[insertAt+1:], lines[insertAt:])
	lines[insertAt] = picture
	return lines
}
