// hook.go 把 LLMClient 适配为 docling 的 OCRHook 与 PDFVisualHook：负责单页
// PageData 的 data URI 组装、提示词构建与视觉结果 JSON 解码；调用预算（页数上限）
// 在钩子闭包内计数，超限后返回空结果交还 docling 内置的纯 Go 兜底路径。
package llmocr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/unitedrhino/docling"
)

// maxOriginalPDFFallbackBytes 是单页抽取失败时允许回退整份 PDF 的大小上限，
// 防止超大文档整包塞进模型请求。
const maxOriginalPDFFallbackBytes = 20 << 20

// maxExistingTextRunes 限制写入提示词的已有规则文本长度。
const maxExistingTextRunes = 12000

// Options 是钩子构造的可选项。
type Options struct {
	// SystemPrompt 覆盖默认系统提示；为空用 DefaultOCRSystemPrompt /
	// DefaultVisualSystemPrompt。
	SystemPrompt string
	// MaxPages 限制本钩子被调用的总页数（OCR 与视觉各自计数）；0 表示不限。
	// 超限调用返回空结果，由 docling 维持纯 Go 兜底行为。
	MaxPages int
}

// pageBudget 是钩子闭包内的调用预算。
type pageBudget struct {
	max  int
	used int
	over bool
}

// allow 消耗一次预算；返回 false 表示已超限。
func (b *pageBudget) allow() bool {
	if b.max <= 0 {
		return true
	}
	if b.used >= b.max {
		b.over = true
		return false
	}
	b.used++
	return true
}

// NewOCRHook 把 client 适配为 docling.OCRHook。图片 Data 直接转 data URI；
// PDF 优先使用 Docling 提供的 PageData，缺失时仅对安全大小内的原始 Data
// 做兼容回退，并由提示词限定页号。
func NewOCRHook(client LLMClient, opts Options) docling.OCRHook {
	budget := &pageBudget{max: opts.MaxPages}
	system := opts.SystemPrompt
	if system == "" {
		system = DefaultOCRSystemPrompt
	}
	return func(request docling.OCRRequest) (string, error) {
		if !budget.allow() {
			return "", nil
		}
		images, originalFallback, err := ocrInputs(request)
		if err != nil {
			return "", err
		}
		prompt := ocrPrompt(request, originalFallback)
		return client.Complete(context.Background(), VisionRequest{
			SystemPrompt: system,
			Prompt:       prompt,
			Images:       images,
		})
	}
}

// ocrInputs 按 MIME 组装识别输入：图片整份转 data URI；PDF 使用 PageData。
func ocrInputs(request docling.OCRRequest) ([]ImageInput, bool, error) {
	mimeType := request.MIMEType
	if mimeType == "" {
		mimeType = "application/pdf"
	}
	if strings.HasPrefix(mimeType, "image/") {
		return []ImageInput{{
			MIMEType: mimeType,
			DataURI:  fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(request.Data)),
		}}, false, nil
	}
	payload, originalFallback, err := pdfPagePayload(request.Data, request.PageData, int(request.PageNo))
	if err != nil {
		return nil, false, err
	}
	return []ImageInput{{
		MIMEType: "application/pdf",
		DataURI:  "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(payload),
	}}, originalFallback, nil
}

// ocrPrompt 构建单页识别的用户提示词；已有规则文本仅作校对参考，重试时
// 透传 docling 内置的纠错反馈。
func ocrPrompt(request docling.OCRRequest, originalFallback bool) string {
	mimeType := request.MIMEType
	if mimeType == "" {
		mimeType = "application/pdf"
	}
	var b strings.Builder
	b.WriteString("逐字识别这份文档的当前页，不要总结、改写或臆造。\n")
	if mimeType == "application/pdf" {
		if originalFallback {
			b.WriteString("输入是原始多页 PDF，只能识别指定页，禁止输出其他页面。")
		} else {
			b.WriteString("输入是仅包含该页的单页 PDF。")
		}
	} else {
		b.WriteString("输入是完整图片文件。")
	}
	if request.Filename != "" {
		fmt.Fprintf(&b, "文件名：%s。", request.Filename)
	}
	fmt.Fprintf(&b, "页号：%d。", request.PageNo)
	if s := strings.TrimSpace(request.ExistingText); s != "" {
		b.WriteString("\n以下是规则解析已提取的文本，仅用于逐字校对；乱码、漏字与顺序以页面视觉为准：\n<existing_text>\n")
		b.WriteString(truncateRunes(s, maxExistingTextRunes))
		b.WriteString("\n</existing_text>\n")
	}
	if request.ValidationFeedback != "" {
		b.WriteString("\n")
		b.WriteString(request.ValidationFeedback)
		b.WriteString("\n")
	}
	return b.String()
}

// NewPDFVisualHook 把 client 适配为 docling.PDFVisualHook。提示词以请求内置
// 的严格 JSON 协议（request.Prompt）为基础，补充页号、页面尺寸与质量信号；
// 返回值经去围栏处理后解码为 PDFVisualResult，再由 docling 内置校验合并。
func NewPDFVisualHook(client LLMClient, opts Options) docling.PDFVisualHook {
	budget := &pageBudget{max: opts.MaxPages}
	system := opts.SystemPrompt
	if system == "" {
		system = DefaultVisualSystemPrompt
	}
	return func(request docling.PDFVisualRequest) (docling.PDFVisualResult, error) {
		if !budget.allow() {
			return docling.PDFVisualResult{}, nil
		}
		images, originalFallback, err := visualInputs(request)
		if err != nil {
			return docling.PDFVisualResult{}, err
		}
		response, err := client.Complete(context.Background(), VisionRequest{
			SystemPrompt: system,
			Prompt:       visualPrompt(request, originalFallback),
			Images:       images,
		})
		if err != nil {
			return docling.PDFVisualResult{}, err
		}
		return decodeVisualResult(response)
	}
}

func visualInputs(request docling.PDFVisualRequest) ([]ImageInput, bool, error) {
	payload, originalFallback, err := pdfPagePayload(request.Data, request.PageData, int(request.PageNo))
	if err != nil {
		return nil, false, err
	}
	images := []ImageInput{{
		MIMEType: "application/pdf",
		DataURI:  "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(payload),
	}}
	for _, embedded := range request.EmbeddedImages {
		if embedded.Image == nil {
			continue
		}
		uri := strings.TrimSpace(embedded.Image.URI)
		comma := strings.IndexByte(uri, ',')
		if !strings.HasPrefix(uri, "data:") ||
			comma <= len("data:") || !strings.HasSuffix(strings.ToLower(uri[:comma]), ";base64") {
			continue
		}
		mimeType := strings.TrimSuffix(strings.TrimPrefix(uri[:comma], "data:"), ";base64")
		if !strings.HasPrefix(strings.ToLower(mimeType), "image/") {
			continue
		}
		images = append(images, ImageInput{MIMEType: mimeType, DataURI: uri})
	}
	return images, originalFallback, nil
}

// visualPrompt 在内置协议提示词上补充页号、页面尺寸、质量触发原因与有界
// 已有文本，避免模型在不知道坐标范围时猜测 bbox。
func visualPrompt(request docling.PDFVisualRequest, originalFallback bool) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(request.Prompt))
	fmt.Fprintf(&b, "\n当前仅分析第 %d 页；页面宽 %.2f pt、高 %.2f pt。", request.PageNo, request.Width, request.Height)
	if originalFallback {
		b.WriteString("附件是原始多页 PDF，只能分析指定页，禁止输出其他页面。")
	}
	b.WriteString("所有 bbox 必须落在该范围内并使用 BOTTOMLEFT：l/r 从左向右，b/t 从下向上；若视觉坐标来自左上角，先按 PDF 高度换算后再输出。")
	if len(request.Quality.Reasons) > 0 {
		b.WriteString("\nGo Docling 质量检测触发原因：")
		b.WriteString(strings.Join(request.Quality.Reasons, ", "))
		b.WriteString("。")
	}
	for i, embedded := range request.EmbeddedImages {
		if embedded.Image == nil {
			continue
		}
		uri := strings.TrimSpace(embedded.Image.URI)
		comma := strings.IndexByte(uri, ',')
		if !strings.HasPrefix(uri, "data:") ||
			comma <= len("data:") || !strings.HasSuffix(strings.ToLower(uri[:comma]), ";base64") {
			continue
		}
		bboxText := "未知"
		if embedded.BBox != nil {
			bboxText = fmt.Sprintf("l=%.2f,b=%.2f,r=%.2f,t=%.2f,coord_origin=BOTTOMLEFT",
				embedded.BBox.L, embedded.BBox.B, embedded.BBox.R, embedded.BBox.T)
		}
		fmt.Fprintf(&b, "\n补充图片 %d 对应当前 PDF 页面的 bbox：%s。请结合单页 PDF 判断其是照片、图表、图示还是图片表格。", i+1, bboxText)
	}
	if s := strings.TrimSpace(request.ExistingText); s != "" {
		b.WriteString("\n以下是 Go Docling 已提取文本，仅用于逐字校对；乱码、漏字和顺序必须以页面视觉为准：\n<existing_text>\n")
		b.WriteString(truncateRunes(s, maxExistingTextRunes))
		b.WriteString("\n</existing_text>")
	}
	if request.ValidationFeedback != "" {
		b.WriteString("\n")
		b.WriteString(request.ValidationFeedback)
	}
	return b.String()
}

// decodeVisualResult 清理模型常见 JSON 围栏与说明文字并解码结果。
func decodeVisualResult(response string) (docling.PDFVisualResult, error) {
	var result docling.PDFVisualResult
	payload := strings.TrimSpace(response)
	if strings.HasPrefix(payload, "```") {
		lines := strings.Split(payload, "\n")
		if len(lines) >= 3 {
			payload = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}
	start, end := strings.Index(payload, "{"), strings.LastIndex(payload, "}")
	if start < 0 || end < start {
		return docling.PDFVisualResult{}, fmt.Errorf("llmocr: visual response has no json object")
	}
	if err := json.Unmarshal([]byte(payload[start:end+1]), &result); err != nil {
		return docling.PDFVisualResult{}, fmt.Errorf("llmocr: decode visual result failed: %w", err)
	}
	return result, nil
}

// pdfPagePayload 优先返回 Docling 已安全抽取的单页 PDF；PageData 缺失时
// 仅对安全大小内的原始 PDF 做兼容回退，不再重复调用 pdfcpu 解析原文件。
func pdfPagePayload(pdfBytes, pageData []byte, pageNo int) ([]byte, bool, error) {
	if pageNo < 1 {
		return nil, false, fmt.Errorf("llmocr: page number %d out of range", pageNo)
	}
	if len(pageData) > 0 {
		return pageData, false, nil
	}
	if len(pdfBytes) == 0 || len(pdfBytes) > maxOriginalPDFFallbackBytes {
		return nil, false, fmt.Errorf("llmocr: single page PDF unavailable for page %d and original file exceeds fallback limit", pageNo)
	}
	return pdfBytes, true, nil
}

// truncateRunes 按 rune 数截断文本并追加省略标记。
func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
