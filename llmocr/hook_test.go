// hook_test.go 覆盖钩子适配层：输入组装（图片/单页 PDF data URI）、提示词
// 上下文、页数预算、视觉结果 JSON 解码与嵌入图片 bbox 标注。
package llmocr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/unitedrhino/docling"
)

// fakeClient 捕获请求并返回固定应答。
type fakeClient struct {
	requests []VisionRequest
	response string
	err      error
}

func (f *fakeClient) Complete(_ context.Context, req VisionRequest) (string, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		return "", f.err
	}
	return f.response, nil
}

// TestOCRHookImageInput 验证图片请求直接转 data URI 且提示词带页号。
func TestOCRHookImageInput(t *testing.T) {
	client := &fakeClient{response: "# 标题\n正文"}
	png := []byte("fake-png-bytes")
	hook := NewOCRHook(client, Options{})
	result, err := hook(docling.OCRRequest{
		PageNo:       1,
		MIMEType:     "image/png",
		Filename:     "scan.png",
		Data:         png,
		ExistingText: "已有文本",
	})
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	if result != client.response {
		t.Fatalf("result=%q", result)
	}
	if len(client.requests) != 1 {
		t.Fatalf("calls=%d", len(client.requests))
	}
	req := client.requests[0]
	if len(req.Images) != 1 || req.Images[0].MIMEType != "image/png" {
		t.Fatalf("images=%+v", req.Images)
	}
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	if req.Images[0].DataURI != want {
		t.Fatalf("data uri mismatch")
	}
	if !strings.Contains(req.Prompt, "页号：1") || !strings.Contains(req.Prompt, "已有文本") {
		t.Fatalf("prompt=%q", req.Prompt)
	}
	if req.SystemPrompt != DefaultOCRSystemPrompt {
		t.Fatalf("system=%q", req.SystemPrompt)
	}
}

// TestOCRHookPDFSinglePage 验证 PDF 请求优先使用 Docling 提供的 PageData。
func TestOCRHookPDFSinglePage(t *testing.T) {
	client := &fakeClient{response: "识别文本"}
	pageData := []byte("%PDF-1.4\nsingle-page\n%%EOF")
	hook := NewOCRHook(client, Options{})
	if _, err := hook(docling.OCRRequest{
		PageNo: 2, MIMEType: "application/pdf", Data: []byte("original-pdf"), PageData: pageData,
	}); err != nil {
		t.Fatalf("hook: %v", err)
	}
	uri := client.requests[0].Images[0].DataURI
	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(uri, "data:application/pdf;base64,"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(payload) != string(pageData) {
		t.Fatalf("payload=%q, want PageData %q", payload, pageData)
	}
}

// TestOCRHookPDFOriginalFallback 验证旧调用方未提供 PageData 时仅在安全
// 大小内回退原始 PDF，并在提示词中严格限制目标页。
func TestOCRHookPDFOriginalFallback(t *testing.T) {
	client := &fakeClient{response: "识别文本"}
	hook := NewOCRHook(client, Options{})
	original := []byte("%PDF-1.4\noriginal\n%%EOF")
	if _, err := hook(docling.OCRRequest{PageNo: 3, MIMEType: "application/pdf", Data: original}); err != nil {
		t.Fatalf("hook: %v", err)
	}
	if !strings.Contains(client.requests[0].Prompt, "原始多页 PDF") ||
		!strings.Contains(client.requests[0].Prompt, "页号：3") {
		t.Fatalf("fallback prompt=%q", client.requests[0].Prompt)
	}
}

// TestOCRHookBudget 验证页数预算：超限调用返回空且不再发请求。
func TestOCRHookBudget(t *testing.T) {
	client := &fakeClient{response: "x"}
	hook := NewOCRHook(client, Options{MaxPages: 1})
	png := []byte("p")
	for i := 0; i < 3; i++ {
		result, err := hook(docling.OCRRequest{PageNo: 1, MIMEType: "image/png", Data: png})
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if i >= 1 && result != "" {
			t.Fatalf("call %d should be over budget, got %q", i, result)
		}
	}
	if len(client.requests) != 1 {
		t.Fatalf("calls=%d, want 1", len(client.requests))
	}
}

// TestVisualHookDecode 验证视觉结果去围栏解码与提示词上下文。
func TestVisualHookDecode(t *testing.T) {
	items := docling.PDFVisualResult{Items: []docling.PDFVisualItem{{
		Label: docling.LabelTitle, Text: "标题", Level: 1, Confidence: 0.9,
	}}}
	raw, _ := json.Marshal(items)
	client := &fakeClient{response: "```json\n" + string(raw) + "\n```"}
	hook := NewPDFVisualHook(client, Options{})
	request := docling.PDFVisualRequest{
		PageNo: 1, MIMEType: "application/pdf", Data: []byte("original"), PageData: []byte("single-page"),
		Width: 612, Height: 792,
		Quality: docling.PDFPageQuality{NeedsVisual: true, Reasons: []string{"low_text"}},
		Prompt:  docling.PDFStructuredVisualPrompt,
	}
	result, err := hook(request)
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	if len(result.Items) != 1 || result.Items[0].Text != "标题" {
		t.Fatalf("result=%+v", result)
	}
	prompt := client.requests[0].Prompt
	for _, want := range []string{"第 1 页", "612", "low_text", "BOTTOMLEFT"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q", want)
		}
	}
}

// TestVisualHookEmbeddedImages 验证嵌入图片附 data URI 且提示词标注 bbox。
func TestVisualHookEmbeddedImages(t *testing.T) {
	client := &fakeClient{response: `{"items":[]}`}
	hook := NewPDFVisualHook(client, Options{})
	embedded := docling.PDFVisualImage{
		Image: &docling.ImageRef{URI: "data:image/png;base64,QUJD", Mimetype: "image/png"},
		BBox:  &docling.DoclingBBox{L: 10, B: 20, R: 110, T: 120},
	}
	if _, err := hook(docling.PDFVisualRequest{
		PageNo: 1, MIMEType: "application/pdf", Data: []byte("original"), PageData: []byte("single-page"),
		EmbeddedImages: []docling.PDFVisualImage{embedded},
		Prompt:         "协议",
	}); err != nil {
		t.Fatalf("hook: %v", err)
	}
	if len(client.requests[0].Images) != 2 {
		t.Fatalf("images=%d, want 2 (pdf + png)", len(client.requests[0].Images))
	}
	if !strings.Contains(client.requests[0].Prompt, "bbox：l=10.00,b=20.00,r=110.00,t=120.00") {
		t.Fatalf("prompt missing bbox: %q", client.requests[0].Prompt)
	}
}
