// pdf_security_test.go 覆盖 PDF 文件、页数和结构资源边界，以及统一入口
// 对限制配置的透传；所有超限必须返回可由 errors.Is 稳定识别的错误。
package docling

import (
	"bytes"
	"compress/zlib"
	"errors"
	"strings"
	"testing"

	pdfcpufilter "github.com/pdfcpu/pdfcpu/pkg/filter"
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

// TestPDFCPURejectsObjectStreamLimits 验证对象流条目数与头部偏移的固定
// 限制均在对象流解码前生效，并统一转换为公开资源限制错误。
func TestPDFCPURejectsObjectStreamLimits(t *testing.T) {
	limits := newPDFCPUConfiguration().Limits
	tests := []struct {
		name  string
		count int
		first int
	}{
		{name: "object count", count: limits.MaxObjectStreamCount + 1},
		{name: "header bytes", count: 1, first: int(limits.MaxObjectStreamFirst + 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stream := pdfcputypes.StreamDict{Dict: pdfcputypes.Dict{
				"N":     pdfcputypes.Integer(test.count),
				"First": pdfcputypes.Integer(test.first),
			}}
			_, err := pdfcpumodel.ObjectStreamDictWithLimits(&stream, limits)
			if err == nil || !isPDFCPULimitError(err) {
				t.Fatalf("object stream err=%v, want classified resource limit", err)
			}
			limitErr := newPDFCPULimitError(err)
			if limitErr == nil || limitErr.Resource != "object stream" {
				t.Fatalf("object stream limit detail=%+v", limitErr)
			}
		})
	}
}

// TestPDFCPURejectsDecodedStreamExpansion 验证小体积压缩流不能绕过单流
// 解压上限形成内存膨胀，底层哨兵错误会映射为公开资源限制错误。
func TestPDFCPURejectsDecodedStreamExpansion(t *testing.T) {
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	if _, err := writer.Write(bytes.Repeat([]byte("A"), 4096)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	stream := pdfcputypes.StreamDict{
		Raw:            compressed.Bytes(),
		FilterPipeline: []pdfcputypes.PDFFilter{{Name: pdfcpufilter.Flate}},
	}
	err := stream.DecodeWithLimit(512)
	if !errors.Is(err, pdfcpufilter.ErrDecodeLimitExceeded) || !isPDFCPULimitError(err) {
		t.Fatalf("decode expansion err=%v, want classified decode limit", err)
	}
	limitErr := newPDFCPULimitError(err)
	if limitErr == nil || limitErr.Resource != "decoded stream bytes" {
		t.Fatalf("decode limit detail=%+v", limitErr)
	}
}

// TestPDFCPURejectsRecursionDepth 验证对象图递归超过固定深度时停止遍历，
// 并保留实际深度与限制值供调用方诊断。
func TestPDFCPURejectsRecursionDepth(t *testing.T) {
	maxDepth := newPDFCPUConfiguration().Limits.MaxRecursionDepth
	err := pdfcpumodel.CheckRecursionDepth("object graph", maxDepth+1, maxDepth)
	if !errors.Is(err, pdfcpumodel.ErrMaxRecursionDepthExceeded) || !isPDFCPULimitError(err) {
		t.Fatalf("recursion err=%v, want classified recursion limit", err)
	}
	limitErr := newPDFCPULimitError(err)
	if limitErr == nil || limitErr.Resource != "recursion depth" ||
		limitErr.Actual != int64(maxDepth+1) || limitErr.Limit != int64(maxDepth) {
		t.Fatalf("recursion limit detail=%+v", limitErr)
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
