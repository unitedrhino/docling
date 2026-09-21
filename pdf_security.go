// pdf_security.go 定义 PDF 解析的统一资源边界，并在任何文本、图片或
// 页级模型输入处理之前通过 pdfcpu 完成结构校验。pdfcpu Context 在一次
// 解析内复用，避免图片提取和单页 PDF 生成重复读取原始文件。
package docling

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	pdfcpuapi "github.com/pdfcpu/pdfcpu/pkg/api"
	pdfcpufilter "github.com/pdfcpu/pdfcpu/pkg/filter"
	pdfcpucore "github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	pdfcpumodel "github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

const (
	// defaultPDFMaxFileBytes 是默认允许解析的 PDF 原始字节数。
	defaultPDFMaxFileBytes int64 = 50 << 20
	// defaultPDFMaxPages 是默认允许解析的 PDF 页数。
	defaultPDFMaxPages = 2000
	// pdfMaxSinglePageBytes 限制提供给 OCR/视觉钩子的单页 PDF 大小。
	pdfMaxSinglePageBytes int64 = 32 << 20
)

// ErrPDFResourceLimit 表示 PDF 输入超过 Docling 的安全资源边界。
var ErrPDFResourceLimit = errors.New("pdf resource limit exceeded")

// pdfCPULimitValues 匹配 pdfcpu 资源错误中的实际值与限制值。
var pdfCPULimitValues = regexp.MustCompile(`(?i)(\d+)\s+exceed[a-z]* limit\s+(\d+)`)

// PDFResourceLimitError 描述具体超限资源。Actual 与 Limit 可用于调用方
// 生成稳定提示；底层 pdfcpu 无法暴露精确数值时保留 Cause 的原始错误。
type PDFResourceLimitError struct {
	Resource string // Resource 是超限资源名称。
	Actual   int64  // Actual 是实际值；未知时为 0。
	Limit    int64  // Limit 是限制值；未知时为 0。
	Cause    error  // Cause 是底层校验错误；直接检查输入时为空。
}

// Error 返回适合日志和最终用户提示的超限说明。
func (e *PDFResourceLimitError) Error() string {
	if e == nil {
		return ErrPDFResourceLimit.Error()
	}
	if e.Actual > 0 && e.Limit > 0 {
		return fmt.Sprintf("pdf %s %d exceeds limit %d", e.Resource, e.Actual, e.Limit)
	}
	if e.Cause != nil {
		return fmt.Sprintf("pdf %s exceeds safety limit: %v", e.Resource, e.Cause)
	}
	return fmt.Sprintf("pdf %s exceeds safety limit", e.Resource)
}

// Unwrap 同时暴露稳定哨兵和底层原因，便于 errors.Is 分类处理。
func (e *PDFResourceLimitError) Unwrap() []error {
	if e == nil || e.Cause == nil {
		return []error{ErrPDFResourceLimit}
	}
	return []error{ErrPDFResourceLimit, e.Cause}
}

// PDFLimits 是调用方可覆盖的 PDF 高层资源限制。非正值使用安全默认值，
// 不表示无限制；pdfcpu 的底层对象、流、图片和递归限制由 Docling 固定维护。
type PDFLimits struct {
	MaxFileBytes int64 // MaxFileBytes 是原始 PDF 最大字节数。
	MaxPages     int   // MaxPages 是最大页数。
}

// DefaultPDFLimits 返回 Docling 的默认 PDF 资源限制。
func DefaultPDFLimits() PDFLimits {
	return PDFLimits{MaxFileBytes: defaultPDFMaxFileBytes, MaxPages: defaultPDFMaxPages}
}

// normalizedPDFLimits 用安全默认值补齐调用方未设置的限制。
func normalizedPDFLimits(limits PDFLimits) PDFLimits {
	defaults := DefaultPDFLimits()
	if limits.MaxFileBytes <= 0 {
		limits.MaxFileBytes = defaults.MaxFileBytes
	}
	if limits.MaxPages <= 0 {
		limits.MaxPages = defaults.MaxPages
	}
	return limits
}

// pdfRuntime 保存一次 PDF 解析复用的安全上下文与单页输出缓存。
type pdfRuntime struct {
	context       *pdfcpumodel.Context
	pageNo        int
	pageData      []byte
	pageAttempted bool
}

// newPDFRuntime 校验文件大小、PDF 结构和页数，并返回可复用的 pdfcpu Context。
func newPDFRuntime(data []byte, limits PDFLimits) (*pdfRuntime, error) {
	limits = normalizedPDFLimits(limits)
	if int64(len(data)) > limits.MaxFileBytes {
		return nil, &PDFResourceLimitError{
			Resource: "file bytes", Actual: int64(len(data)), Limit: limits.MaxFileBytes,
		}
	}
	configuration := newPDFCPUConfiguration()
	context, err := pdfcpuapi.ReadContext(bytes.NewReader(data), configuration)
	if err != nil {
		if limitErr := newPDFCPULimitError(err); limitErr != nil {
			return nil, limitErr
		}
		return nil, fmt.Errorf("read pdf structure: %w", err)
	}
	// pdfcpu 的格式校验比文本后端更严格：部分现实 PDF 缺少非关键 CMap
	// 字段但仍可安全恢复文字。普通格式错误只让 pdfcpu 增强尽力运行；资源
	// 限制错误仍是硬失败，不能绕过后继续交给其他解析器。
	if limitErr := newPDFCPULimitError(safePDFCPUValidate(context)); limitErr != nil {
		return nil, limitErr
	}
	if context.PageCount > limits.MaxPages {
		return nil, &PDFResourceLimitError{
			Resource: "pages", Actual: int64(context.PageCount), Limit: int64(limits.MaxPages),
		}
	}
	// 优化用于构建图片对象索引。损坏 BI 等局部资源可能让优化失败，此时
	// Context 仍可供页数校验、单页抽取和逐对象容错使用。
	if limitErr := newPDFCPULimitError(safePDFCPUOptimize(context)); limitErr != nil {
		return nil, limitErr
	}
	return &pdfRuntime{context: context}, nil
}

// safePDFCPUValidate 隔离第三方校验器处理畸形对象时可能触发的 panic。
func safePDFCPUValidate(context *pdfcpumodel.Context) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("pdfcpu validate panic: %v", recovered)
		}
	}()
	return pdfcpuapi.ValidateContext(context)
}

// safePDFCPUOptimize 隔离可选图片索引优化中的第三方 panic。
func safePDFCPUOptimize(context *pdfcpumodel.Context) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("pdfcpu optimize panic: %v", recovered)
		}
	}()
	return pdfcpuapi.OptimizeContext(context)
}

// newPDFCPUConfiguration 创建知识摄入场景使用的 pdfcpu 安全配置。
func newPDFCPUConfiguration() *pdfcpumodel.Configuration {
	configuration := pdfcpumodel.NewDefaultConfiguration()
	configuration.Cmd = pdfcpumodel.EXTRACTIMAGES
	configuration.UnsupportedResourcePolicy = pdfcpumodel.UnsupportedResourceSkip
	configuration.Limits = pdfcpumodel.ResourceLimits{
		MaxStreamBytes:       64 << 20,
		MaxDecodeBytes:       128 << 20,
		MaxImagePixels:       64 * 1024 * 1024,
		MaxImageBytes:        256 << 20,
		MaxObjectCount:       1_000_000,
		MaxObjectStreamCount: 100_000,
		MaxObjectStreamFirst: 8 << 20,
		MaxXRefEntries:       1_000_000,
		MaxRecursionDepth:    64,
	}
	return configuration
}

// isPDFCPULimitError 判断 pdfcpu 返回的错误是否来自已配置资源边界。
func isPDFCPULimitError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, pdfcpumodel.ErrMaxRecursionDepthExceeded) {
		return true
	}
	if errors.Is(err, pdfcpufilter.ErrDecodeLimitExceeded) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "exceeds limit") ||
		strings.Contains(message, "exceeded limit") ||
		strings.Contains(message, "max recursion depth")
}

// newPDFCPULimitError 把 pdfcpu 的资源错误转换为稳定公开类型，并尽量
// 提取资源名、实际值和限制值；底层未报告实际值时 Actual 保持为零。
func newPDFCPULimitError(err error) *PDFResourceLimitError {
	if !isPDFCPULimitError(err) {
		return nil
	}
	message := strings.ToLower(err.Error())
	resource := "structure"
	switch {
	case strings.Contains(message, "xref") || strings.Contains(message, `"size"`):
		resource = "xref entries"
	case strings.Contains(message, "object stream"):
		resource = "object stream"
	case strings.Contains(message, "object count"):
		resource = "objects"
	case strings.Contains(message, "image"):
		resource = "image"
	case strings.Contains(message, "decode"):
		resource = "decoded stream bytes"
	case strings.Contains(message, "stream"):
		resource = "stream bytes"
	case strings.Contains(message, "recursion") || strings.Contains(message, "depth"):
		resource = "recursion depth"
	}
	limitErr := &PDFResourceLimitError{Resource: resource, Cause: err}
	if match := pdfCPULimitValues.FindStringSubmatch(message); len(match) == 3 {
		limitErr.Actual, _ = strconv.ParseInt(match[1], 10, 64)
		limitErr.Limit, _ = strconv.ParseInt(match[2], 10, 64)
	}
	return limitErr
}

// singlePageData 返回指定页的独立 PDF。抽取失败不会破坏正文解析，调用方
// 可继续使用请求中的原始 Data；同一页的模型重试复用缓存结果。
func (r *pdfRuntime) singlePageData(pageNo int) (data []byte) {
	defer func() {
		if recover() != nil {
			data = nil
		}
	}()
	if r == nil || r.context == nil || pageNo < 1 || pageNo > r.context.PageCount {
		return nil
	}
	if r.pageNo == pageNo && r.pageAttempted {
		return r.pageData
	}
	// PDF 页面按顺序处理，只保留当前页缓存，避免页数上限与单页上限
	// 相乘后形成文档级大额常驻内存。
	r.pageNo = pageNo
	r.pageData = nil
	r.pageAttempted = true

	pageContext, err := pdfcpucore.ExtractPages(r.context, []int{pageNo}, false)
	if err != nil {
		return nil
	}
	var output bytes.Buffer
	writer := &pdfLimitedWriter{writer: &output, limit: pdfMaxSinglePageBytes}
	if err := pdfcpuapi.WriteContext(pageContext, writer); err != nil {
		return nil
	}
	if output.Len() == 0 {
		return nil
	}
	r.pageData = output.Bytes()
	return r.pageData
}

// pdfLimitedWriter 在 pdfcpu 序列化单页时阻止输出缓冲无界增长。
type pdfLimitedWriter struct {
	writer  io.Writer
	limit   int64
	written int64
}

// Write 写入不超过限制的数据，超限时返回可分类的资源错误。
func (w *pdfLimitedWriter) Write(data []byte) (int, error) {
	actual := w.written + int64(len(data))
	if actual > w.limit {
		return 0, &PDFResourceLimitError{
			Resource: "single page bytes", Actual: actual, Limit: w.limit,
		}
	}
	n, err := w.writer.Write(data)
	w.written += int64(n)
	return n, err
}
