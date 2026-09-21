// Package docling 提供知识库/文档场景的通用结构化解析能力：
// 把 Markdown、docx、PDF、xlsx、csv、纯文本统一解析为 content_list
// （[]Item 元素列表），元素携带标题层级（TextLevel）与章节路径
// （SectionPath），表格独立成 table 元素并渲染为 GFM Markdown。
// 供切片、索引、检索等上层消费，各服务无需重复实现格式解析。
//
// 各格式解析要点：
//   - Markdown：goldmark（GFM 扩展）AST 解析，代码块/表格结构化提取；
//   - docx：WordprocessingML 流式状态机，pStyle 标题层级 + 表格行列；
//   - PDF：ledongthuc/pdf 字符级字号/坐标，字号启发式识别标题层级，
//     按 Y 坐标聚合行、按页归组正文；
//   - xlsx：excelize 遍历全部 sheet，每 sheet 独立 table 元素，大表分段；
//   - csv：首行表头 + 行数分段；
//   - eml：RFC 5322 头 + MIME 递归（text/html、text/plain、嵌套邮件）；
//   - 图片（png/jpg/bmp/webp）：尺寸/DPI PictureItem + 可选 OCRHook 识别文本。
package docling

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/unitedrhino/docling/internal/pdfenc"
)

// ItemType 定义 content_list 中元素的模态类型。
type ItemType string

const (
	// ItemTypeText 普通文本段落或标题
	ItemTypeText ItemType = "text"
	// ItemTypeImage 图片及其说明
	ItemTypeImage ItemType = "image"
	// ItemTypeTable 表格及其说明
	ItemTypeTable ItemType = "table"
	// ItemTypeEquation 公式（LaTeX 形式）
	ItemTypeEquation ItemType = "equation"
	// ItemTypeGeneric 其他通用内容块
	ItemTypeGeneric ItemType = "generic"
)

// BBox 表示元素在页面内的边界框（left/top/right/bottom）。
type BBox struct {
	Left   float64 `json:"l"`
	Top    float64 `json:"t"`
	Right  float64 `json:"r"`
	Bottom float64 `json:"b"`
}

// ParseByExt 按文件扩展名分发的统一解析入口，产出 DoclingDocument
// （详细 JSON 协议）；content_list 可经 ToContentList(doc, source) 派生，
// Markdown 文本可经 ParseByExtToMarkdown 一步取得。
// 供 CLI、双跑对比与调用方按文件类型统一解析使用；未知扩展名返回错误。
func ParseByExt(name string, data []byte) (*DoclingDocument, error) {
	return ParseByExtWithOptions(name, data, ParseOptions{})
}

// ParseOptions 是统一解析入口的可选配置；PageOCRHook 作为兼容钩子保留。
type ParseOptions struct {
	// OriginURI 指定文档来源 URI；为空时不输出 origin.uri。
	OriginURI string
	// PDFLimits 指定 PDF 文件与页数限制；非正字段使用安全默认值。
	PDFLimits PDFLimits
	// OCRHook 是带完整文件上下文的新 OCR 钩子，优先于 PageOCRHook。
	OCRHook OCRHook
	// PageOCRHook 是旧版页级 OCR 钩子，供 PDF 与图片解析复用。
	PageOCRHook PageOCRHook
	// PDFVisualHook 是 PDF 疑难页面的结构化视觉钩子。
	PDFVisualHook PDFVisualHook
	// PDFVisualAlways 强制所有 PDF 页尝试视觉解析；默认仅按质量信号触发。
	PDFVisualAlways bool
	// MaxPDFVisualPages 限制单文档最多尝试的视觉页数；0 表示不额外限制。
	MaxPDFVisualPages int
	// DisablePopplerFallback 禁用 PDF 的可选 Poppler 降级提取。
	DisablePopplerFallback bool
	// DisablePDFEmbeddedImageExtraction 禁用 PDF 内嵌图片资产的纯 Go 提取。
	DisablePDFEmbeddedImageExtraction bool
	// GarbageThreshold 指定 PDF 文本乱码率触发 OCR 的阈值。
	GarbageThreshold float64
}

// ParseByExtWithOptions 按扩展名和选项统一解析文档，并补齐官方 origin。
func ParseByExtWithOptions(name string, data []byte, options ParseOptions) (*DoclingDocument, error) {
	doc, err := parseByExtData(name, data, options)
	if err != nil {
		return nil, err
	}
	applyDocumentOrigin(doc, name, data, options.OriginURI)
	return doc, nil
}

// parseByExtData 只负责按扩展名调用格式解析器；来源信息由 ParseByExt
// 在解析成功后统一补齐，避免各格式后端形成不同的哈希与 MIME 口径。
func parseByExtData(name string, data []byte, options ParseOptions) (*DoclingDocument, error) {
	mimeType := documentMIMEForExt(strings.ToLower(filepath.Ext(name)))
	switch strings.ToLower(filepath.Ext(name)) {
	case ".txt":
		return ParseText(data)
	case ".md", ".markdown":
		return ParseMarkdown(data)
	case ".docx":
		return ParseDocx(data)
	case ".xlsx":
		return ParseXLSX(data)
	case ".csv":
		return ParseCSV(data)
	case ".pdf":
		return ParsePDFWithOptions(data, PDFOptions{
			Limits:                         options.PDFLimits,
			VisualHook:                     options.PDFVisualHook,
			VisualAlways:                   options.PDFVisualAlways,
			MaxVisualPages:                 options.MaxPDFVisualPages,
			OCRHook:                        options.OCRHook,
			PageOCRHook:                    options.PageOCRHook,
			MIMEType:                       mimeType,
			Filename:                       filepath.Base(name),
			DisablePopplerFallback:         options.DisablePopplerFallback,
			DisableEmbeddedImageExtraction: options.DisablePDFEmbeddedImageExtraction,
			GarbageThreshold:               options.GarbageThreshold,
		})
	case ".pptx":
		return ParsePPTX(data)
	case ".html", ".htm":
		return ParseHTML(data)
	case ".adoc", ".asciidoc":
		return ParseAsciiDoc(data)
	case ".eml":
		return ParseEML(data)
	case ".png", ".jpg", ".jpeg", ".bmp", ".webp":
		return ParseImageWithOptions(data, PDFOptions{
			OCRHook:     options.OCRHook,
			PageOCRHook: options.PageOCRHook,
			MIMEType:    mimeType,
			Filename:    filepath.Base(name),
		})
	default:
		return nil, fmt.Errorf("docling: 不支持的文件类型 %s", filepath.Ext(name))
	}
}

// applyDocumentOrigin 按 Docling 口径补齐工作名称与来源信息。
func applyDocumentOrigin(doc *DoclingDocument, name string, data []byte, originURI string) {
	if doc == nil {
		return
	}
	filename := filepath.Base(strings.TrimSpace(name))
	if filename == "" || filename == "." {
		filename = doc.Name
	}
	ext := strings.ToLower(filepath.Ext(filename))
	stem := strings.TrimSuffix(filename, filepath.Ext(filename))
	if strings.TrimSpace(stem) != "" {
		doc.Name = stem
	}
	hash := sha256.Sum256(data)
	doc.Origin = &DocumentOrigin{
		Mimetype:   documentMIMEForExt(ext),
		BinaryHash: binary.BigEndian.Uint64(hash[len(hash)-8:]),
		Filename:   filename,
		URI:        strings.TrimSpace(originURI),
	}
}

// documentMIMEForExt 返回 Docling DocumentOrigin 使用的规范 MIME。
func documentMIMEForExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".txt":
		return "text/plain"
	case ".md", ".markdown":
		return "text/markdown"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case ".csv":
		return "text/csv"
	case ".pdf":
		return "application/pdf"
	case ".pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	case ".html", ".htm":
		return "text/html"
	case ".adoc", ".asciidoc":
		return "text/asciidoc"
	case ".eml":
		return "message/rfc822"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".bmp":
		return "image/bmp"
	case ".webp":
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}

// ParseByExtToMarkdown 按文件扩展名分发解析并直接返回 Markdown 文本
// （AI 场景一步到位，等价 ParseByExt 后调用 ToMarkdown）；未知扩展名
// 返回错误，解析成功但文档为空时返回仅含换行的空文档文本。
func ParseByExtToMarkdown(name string, data []byte) (string, error) {
	doc, err := ParseByExt(name, data)
	if err != nil {
		return "", err
	}
	return doc.ToMarkdown(), nil
}

// Item 是 content_list 中的单个元素，覆盖文本、图片、表格、公式等模态。
// 字段按需填充：文本类型填 Text；图片类型填 ImgPath/ImageCaption；
// 表格类型填 TableBody/TableCaption；公式类型填 LaTeX。
// JSON 标签为 content_list 持久化协议，变更需保证向后兼容。
type Item struct {
	Type          ItemType `json:"type"`
	PageIdx       int64    `json:"page_idx,omitempty"`
	Text          string   `json:"text,omitempty"`           // text / equation 的文本或 LaTeX
	TextLevel     int64    `json:"text_level,omitempty"`     // text 标题层级（0=正文，1=一级标题，以此类推）
	Label         string   `json:"label,omitempty"`          // 细粒度元素分类，page_header/page_footer/document_index/list_item/code 等非通用 label 时填充，供下游过滤/分流
	LaTeX         string   `json:"latex,omitempty"`          // equation 的 LaTeX 源码
	ImgPath       string   `json:"img_path,omitempty"`       // image 资源路径（对象存储 key 或相对路径）
	ImageCaption  string   `json:"image_caption,omitempty"`  // image 标题
	ImageFootnote string   `json:"image_footnote,omitempty"` // image 脚注
	TableBody     string   `json:"table_body,omitempty"`     // table，Markdown 格式
	TableCaption  string   `json:"table_caption,omitempty"`  // table 标题
	TableFootnote string   `json:"table_footnote,omitempty"` // table 脚注
	SectionPath   []string `json:"_section_path,omitempty"`  // 章节路径，如 ["1 引言", "1.1 背景"]
	BBox          *BBox    `json:"_bbox,omitempty"`          // 页面内边界框
	OrderIndex    int64    `json:"_order_index,omitempty"`   // 阅读顺序序号
	Source        string   `json:"source,omitempty"`         // 解析器来源（golight/docling），供下游分流
}

// sanitizeText 清理文本中的控制字符（保留换行/回车/制表符），
// 并剔除非法 UTF-8 序列，保证产物可安全入库与序列化。
func sanitizeText(text string) string {
	if text == "" {
		return ""
	}
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "")
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return r
		}
		if r < 32 {
			return -1
		}
		return r
	}, text)
}

// textGarbageRatio 计算文本乱码率：U+FFFD、非空白控制字符与 Unicode
// 全部私用区字符占全部非空白 rune 的比例（0-1）。
// 空文本或全空白文本返回 0；用于解析产物质量检测（如 PDF CMap 缺失
// 导致的替换符乱码，乱码率超阈值的文档可降级处理）。
func textGarbageRatio(text string) float64 {
	total, bad := 0, 0
	for _, r := range text {
		if unicode.IsSpace(r) {
			continue
		}
		total++
		if r == '\uFFFD' || unicode.IsControl(r) || pdfenc.IsUnicodePrivateUse(r) {
			bad++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(bad) / float64(total)
}

// ItemToText 把任意 Item 转换成可入向量库的纯文本表示。
// 图片/表格/公式的详细描述由上层按需生成，这里只输出已有文本字段。
func ItemToText(item Item) string {
	switch item.Type {
	case ItemTypeText:
		return item.Text
	case ItemTypeImage:
		parts := make([]string, 0, 3)
		if item.ImageCaption != "" {
			parts = append(parts, item.ImageCaption)
		}
		if item.Text != "" {
			parts = append(parts, item.Text)
		}
		if item.ImageFootnote != "" {
			parts = append(parts, item.ImageFootnote)
		}
		return strings.Join(parts, "\n")
	case ItemTypeTable:
		parts := make([]string, 0, 4)
		if item.TableCaption != "" {
			parts = append(parts, item.TableCaption)
		}
		if item.TableBody != "" {
			parts = append(parts, item.TableBody)
		}
		if item.TableFootnote != "" {
			parts = append(parts, item.TableFootnote)
		}
		return strings.Join(parts, "\n")
	case ItemTypeEquation:
		if item.LaTeX != "" {
			return item.LaTeX
		}
		return item.Text
	case ItemTypeGeneric:
		return item.Text
	default:
		return item.Text
	}
}

// JoinItemTexts 把多个元素文本按阅读顺序拼接，用换行分隔。
func JoinItemTexts(items []Item) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		text := ItemToText(item)
		if strings.TrimSpace(text) == "" {
			continue
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n")
}

// ItemHeadingPath 把元素的章节路径拼接成字符串（"A > B" 形式）。
func ItemHeadingPath(item Item) string {
	if len(item.SectionPath) == 0 {
		return ""
	}
	return strings.Join(item.SectionPath, " > ")
}
