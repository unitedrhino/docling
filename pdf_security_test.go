// pdf_security_test.go 覆盖 PDF 文件、页数和结构资源边界，以及统一入口
// 对限制配置的透传；所有超限必须返回可由 errors.Is 稳定识别的错误。
package docling

import (
	"errors"
	"strings"
	"testing"

	pdfcpumodel "github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	pdfcputypes "github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// TestDefaultPDFLimits 验证公开默认值与知识摄入安全口径一致。
func TestDefaultPDFLimits(t *testing.T) {
	limits := DefaultPDFLimits()
	if limits.MaxFileBytes != 50<<20 || limits.MaxPages != 2000 {
		t.Fatalf("DefaultPDFLimits=%+v", limits)
	}
}

// TestParsePDFRejectsFileLimit 验证文件大小在进入第三方解析器前被拒绝。
func TestParsePDFRejectsFileLimit(t *testing.T) {
	data := buildTestPDF(t, []testPDFPage{{
		content: "BT /F1 10 Tf 72 720 Td (safe text) Tj ET",
	}})
	_, err := ParsePDFWithOptions(data, PDFOptions{Limits: PDFLimits{MaxFileBytes: int64(len(data) - 1)}})
	if !errors.Is(err, ErrPDFResourceLimit) {
		t.Fatalf("file limit err=%v, want ErrPDFResourceLimit", err)
	}
	var limitErr *PDFResourceLimitError
	if !errors.As(err, &limitErr) || limitErr.Resource != "file bytes" ||
		limitErr.Actual != int64(len(data)) || limitErr.Limit != int64(len(data)-1) {
		t.Fatalf("file limit detail=%+v", limitErr)
	}
}

// TestParsePDFRejectsPageLimit 验证页数来自安全预检结果且超限时不继续解析。
func TestParsePDFRejectsPageLimit(t *testing.T) {
	data := buildTestPDF(t, []testPDFPage{
		{content: "BT /F1 10 Tf 72 720 Td (page one) Tj ET"},
		{content: "BT /F1 10 Tf 72 720 Td (page two) Tj ET"},
	})
	_, err := ParsePDFWithOptions(data, PDFOptions{Limits: PDFLimits{MaxPages: 1}})
	if !errors.Is(err, ErrPDFResourceLimit) {
		t.Fatalf("page limit err=%v, want ErrPDFResourceLimit", err)
	}
	var limitErr *PDFResourceLimitError
	if !errors.As(err, &limitErr) || limitErr.Resource != "pages" ||
		limitErr.Actual != 2 || limitErr.Limit != 1 {
		t.Fatalf("page limit detail=%+v", limitErr)
	}
}

// TestParseByExtForwardsPDFLimits 验证统一入口不会绕过 PDF 安全配置。
func TestParseByExtForwardsPDFLimits(t *testing.T) {
	data := buildTestPDF(t, []testPDFPage{{content: "BT /F1 10 Tf 72 720 Td (text) Tj ET"}})
	_, err := ParseByExtWithOptions("report.pdf", data, ParseOptions{
		PDFLimits: PDFLimits{MaxFileBytes: int64(len(data) - 1)},
	})
	if !errors.Is(err, ErrPDFResourceLimit) {
		t.Fatalf("ParseByExt limit err=%v, want ErrPDFResourceLimit", err)
	}
}

// TestPDFCPULimitErrorClassification 验证 pdfcpu 的结构限制错误会被识别，
// 普通语法错误仍保持校验失败语义。
func TestPDFCPULimitErrorClassification(t *testing.T) {
	err := errors.New("xref entry count 1000001 exceeds limit 1000000")
	if !isPDFCPULimitError(err) {
		t.Fatal("pdfcpu limit error should be classified")
	}
	limitErr := newPDFCPULimitError(err)
	if limitErr == nil || limitErr.Resource != "xref entries" ||
		limitErr.Actual != 1_000_001 || limitErr.Limit != 1_000_000 {
		t.Fatalf("classified limit error=%+v", limitErr)
	}
	if isPDFCPULimitError(errors.New("missing xref table")) {
		t.Fatal("ordinary validation error must not be classified as a resource limit")
	}
}

// TestPDFCPUConfigurationLimits 验证 Docling 固定维护的底层限制不会随
// pdfcpu 默认值变化而放宽。
func TestPDFCPUConfigurationLimits(t *testing.T) {
	limits := newPDFCPUConfiguration().Limits
	if limits.MaxStreamBytes != 64<<20 || limits.MaxDecodeBytes != 128<<20 ||
		limits.MaxImagePixels != 64*1024*1024 || limits.MaxImageBytes != 256<<20 ||
		limits.MaxObjectCount != 1_000_000 || limits.MaxXRefEntries != 1_000_000 ||
		limits.MaxObjectStreamCount != 100_000 || limits.MaxObjectStreamFirst != 8<<20 ||
		limits.MaxRecursionDepth != 64 {
		t.Fatalf("unexpected pdfcpu limits: %+v", limits)
	}
}

// TestPDFCPURejectsOversizedXRef 验证 Docling 配置能让 pdfcpu 在 xref
// stream 声明巨大 /Size 时先拒绝，且错误可被资源限制分类器识别。
func TestPDFCPURejectsOversizedXRef(t *testing.T) {
	stream := pdfcputypes.StreamDict{Dict: pdfcputypes.Dict{
		"Size": pdfcputypes.Integer(1_000_001),
	}}
	_, err := pdfcpumodel.ParseXRefStreamDictWithLimits(&stream, newPDFCPUConfiguration().Limits)
	if err == nil || !isPDFCPULimitError(err) {
		t.Fatalf("oversized xref err=%v, want classified resource limit", err)
	}
}

// TestParsePDFMalformedXRefDoesNotPanic 验证畸形 xref 返回普通解析错误，
// 不被误报为资源超限，也不会进入正文解析器。
func TestParsePDFMalformedXRefDoesNotPanic(t *testing.T) {
	data := []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\nstartxref\n999999\n%%EOF\n")
	_, err := ParsePDFWithOptions(data, PDFOptions{})
	if err == nil {
		t.Fatal("malformed xref should fail")
	}
	if errors.Is(err, ErrPDFResourceLimit) || strings.TrimSpace(err.Error()) == "" {
		t.Fatalf("malformed xref err=%v", err)
	}
}
