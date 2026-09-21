// pdf_image_test.go 验证 PDF 内嵌图片的纯 Go 版面感知与视觉增强自动路由。
package docling

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/ledongthuc/pdf"
	"github.com/mrjoshuak/go-jpeg2000"
	pdfcpufilter "github.com/pdfcpu/pdfcpu/pkg/filter"
	pdfcpumodel "github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// buildPDFImageFixture 构造同时包含正常文本层和 Image XObject 的单页 PDF。
// 图片通过 CTM 放置，便于精确验证页面覆盖率，不依赖外部生成器。
func buildPDFImageFixture(t *testing.T, imageMatrix string) []byte {
	t.Helper()
	fontWidths := strings.TrimRight(strings.Repeat("500 ", 95), " ")
	content := fmt.Sprintf("q %s cm /Im0 Do Q\nBT /F1 10 Tf 72 720 Td (normal text layer) Tj ET", imageMatrix)
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [4 0 R] /Count 1 >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding /FirstChar 32 /LastChar 126 /Widths [" + fontWidths + "] >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 5 0 R /Resources << /Font << /F1 3 0 R >> /XObject << /Im0 6 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
		"<< /Type /XObject /Subtype /Image /Width 100 /Height 50 /ColorSpace /DeviceRGB /BitsPerComponent 8 /Length 3 >>\nstream\nRGB\nendstream",
	}
	var out strings.Builder
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, object := range objects {
		offsets[i] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return []byte(out.String())
}

// buildPDFInlineColorResourceFixture 构造带页面 ColorSpace 资源的单页 PDF，
// 用于验证 BI 内联图片通过资源名称解析色彩空间。
func buildPDFInlineColorResourceFixture(t *testing.T, colorSpaces string, content []byte, extraObjects ...[]byte) []byte {
	t.Helper()
	objects := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 100 100] /Contents 4 0 R /Resources << /ColorSpace << %s >> >> >>", colorSpaces)),
		append([]byte(fmt.Sprintf("<< /Length %d >>\nstream\n", len(content))), append(content, []byte("\nendstream")...)...),
	}
	objects = append(objects, extraObjects...)
	var output bytes.Buffer
	output.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for index, object := range objects {
		offsets[index] = output.Len()
		fmt.Fprintf(&output, "%d 0 obj\n", index+1)
		output.Write(object)
		output.WriteString("\nendobj\n")
	}
	xref := output.Len()
	fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&output, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&output, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return output.Bytes()
}

// TestCollectPDFCPUPageImagesIsolatesMalformedPage 验证单页图片损坏不会丢弃
// 同一次 PDF 读取中其他页面已经可以解码的图片。
func TestCollectPDFCPUPageImagesIsolatesMalformedPage(t *testing.T) {
	called := []int{}
	images := collectPDFCPUPageImages([]int64{0, 1, 2}, func(pageNo int) (map[int]pdfcpumodel.Image, error) {
		called = append(called, pageNo)
		if pageNo == 2 {
			return nil, errors.New("invalid image mask")
		}
		return map[int]pdfcpumodel.Image{
			pageNo: {Reader: bytes.NewReader(minimalPNG), Name: fmt.Sprintf("Im%d", pageNo), FileType: "png", PageNr: pageNo},
		}, nil
	}, nil)
	if fmt.Sprint(called) != "[1 2 3]" || len(images[0]) != 1 || len(images[1]) != 0 || len(images[2]) != 1 {
		t.Fatalf("called=%v images=%+v", called, images)
	}
}

// buildPDFJPEGImageFixture 构造包含可解码 JPEG XObject 的单页 PDF；
// withText 控制是否同时写入正常文本层，用于覆盖纯图片与混合图文两条路径。
func buildPDFJPEGImageFixture(t *testing.T, imageMatrix string, withText bool) []byte {
	t.Helper()
	pixels := image.NewRGBA(image.Rect(0, 0, 2, 2))
	pixels.Set(0, 0, color.RGBA{R: 255, A: 255})
	pixels.Set(1, 0, color.RGBA{G: 255, A: 255})
	pixels.Set(0, 1, color.RGBA{B: 255, A: 255})
	pixels.Set(1, 1, color.RGBA{R: 255, G: 255, B: 255, A: 255})
	var jpegData bytes.Buffer
	if err := jpeg.Encode(&jpegData, pixels, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode fixture JPEG: %v", err)
	}
	fontWidths := strings.TrimRight(strings.Repeat("500 ", 95), " ")
	content := fmt.Sprintf("q %s cm /Im0 Do Q\n", imageMatrix)
	if withText {
		content += "BT /F1 10 Tf 72 720 Td (normal text layer) Tj ET"
	}
	objects := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [4 0 R] /Count 1 >>"),
		[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding /FirstChar 32 /LastChar 126 /Widths [" + fontWidths + "] >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 5 0 R /Resources << /Font << /F1 3 0 R >> /XObject << /Im0 6 0 R >> >> >>"),
		[]byte(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content)),
	}
	var imageObject bytes.Buffer
	fmt.Fprintf(&imageObject, "<< /Type /XObject /Subtype /Image /Width 2 /Height 2 /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n", jpegData.Len())
	imageObject.Write(jpegData.Bytes())
	imageObject.WriteString("\nendstream")
	objects = append(objects, imageObject.Bytes())

	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for index, object := range objects {
		offsets[index] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n", index+1)
		out.Write(object)
		out.WriteString("\nendobj\n")
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return out.Bytes()
}

// TestInspectPDFPageImagesTracksPlacementCoverage 验证图片像素信息、页面 bbox
// 和覆盖率由资源字典与 CTM 纯 Go 恢复。
func TestInspectPDFPageImagesTracksPlacementCoverage(t *testing.T) {
	data := buildPDFImageFixture(t, "300 0 0 200 100 300")
	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	page := reader.Page(1)
	images := inspectPDFPageImages(page, 612, 792)
	if len(images) != 1 {
		t.Fatalf("images=%+v, want one placement", images)
	}
	image := images[0]
	if image.Name != "Im0" || image.PixelWidth != 100 || image.PixelHeight != 50 {
		t.Fatalf("image metadata=%+v", image)
	}
	if image.BBox == nil || image.BBox.L != 100 || image.BBox.B != 300 || image.BBox.R != 400 || image.BBox.T != 500 {
		t.Fatalf("image bbox=%+v", image.BBox)
	}
	wantCoverage := 300.0 * 200.0 / (612.0 * 792.0)
	if math.Abs(pdfImageCoverage(images, 612, 792)-wantCoverage) > 0.0001 {
		t.Fatalf("coverage=%f want=%f", pdfImageCoverage(images, 612, 792), wantCoverage)
	}
}

// TestInspectPDFInlineImageTracksPlacementCoverage 验证内容流 BI/ID/EI 内联
// 图片无需 XObject 资源也能恢复像素声明和当前 CTM 页面位置。
func TestInspectPDFInlineImageTracksPlacementCoverage(t *testing.T) {
	content := "q 300 0 0 200 100 300 cm BI /W 120 /H 80 /CS /RGB /BPC 8 ID abc EI Q\n" +
		"BT /F1 10 Tf 72 720 Td (normal text layer) Tj ET"
	data := buildTestPDF(t, []testPDFPage{{content: content}})
	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open inline image fixture: %v", err)
	}
	images := inspectPDFPageImages(reader.Page(1), 612, 792)
	if len(images) != 1 {
		t.Fatalf("inline image placements=%+v", images)
	}
	image := images[0]
	if image.Name != "inline-1" || image.PixelWidth != 120 || image.PixelHeight != 80 ||
		image.BBox == nil || image.BBox.L != 100 || image.BBox.B != 300 || image.BBox.R != 400 || image.BBox.T != 500 {
		t.Fatalf("inline image placement=%+v", image)
	}
}

// TestParsePDFInlineImageTriggersVisual 验证正常文本与大幅 inline image 混排
// 时自动进入结构化视觉，使未提取像素的图片表格仍可由单页 PDF 识别。
func TestParsePDFInlineImageTriggersVisual(t *testing.T) {
	content := "q 300 0 0 200 100 300 cm BI /W 120 /H 80 /CS /RGB /BPC 8 ID abc EI Q\n" +
		"BT /F1 10 Tf 72 720 Td (normal text layer) Tj ET"
	data := buildTestPDF(t, []testPDFPage{{content: content}})
	calls := 0
	doc, err := ParsePDFWithOptions(data, PDFOptions{DisablePopplerFallback: true, VisualHook: func(request PDFVisualRequest) (PDFVisualResult, error) {
		calls++
		if request.Quality.ImageCount != 1 || request.Quality.LargeImageCount != 1 ||
			!containsString(request.Quality.Reasons, "mixed_text_image") {
			t.Fatalf("inline image quality=%+v", request.Quality)
		}
		return PDFVisualResult{Items: []PDFVisualItem{{
			Label: LabelText, Text: "视觉恢复正文", Confidence: 0.95,
			BBox: &DoclingBBox{L: 100, B: 300, R: 400, T: 500, CoordOrigin: CoordOriginBottomLeft},
		}}}, nil
	}})
	if err != nil || calls != 1 || !strings.Contains(doc.Text(), "视觉恢复正文") {
		t.Fatalf("inline image visual result calls=%d text=%q err=%v", calls, doc.Text(), err)
	}
}

// TestInspectPDFInlineImageIgnoresLiteralAndFalseEnd 验证字符串/注释中的 BI
// 不会误报，图片数据内偶然出现的“EI + 普通单词”也不会提前截断。
func TestInspectPDFInlineImageIgnoresLiteralAndFalseEnd(t *testing.T) {
	content := []byte("(BI /W 999 /H 999 ID fake EI Q) Tj\n" +
		"% BI /W 999 /H 999 ID fake EI Q\n" +
		"q 20 0 0 10 5 6 cm BI /Width 2 /Height 3 ID abc EI bogus data EI Q")
	images := inspectPDFInlineImageContent(content, identityPDFAffine(), 100, 100, nil)
	if len(images) != 1 || images[0].PixelWidth != 2 || images[0].PixelHeight != 3 ||
		images[0].BBox == nil || images[0].BBox.L != 5 || images[0].BBox.B != 6 ||
		images[0].BBox.R != 25 || images[0].BBox.T != 16 {
		t.Fatalf("inline image false-positive/end handling=%+v", images)
	}
	malformed := inspectPDFInlineImageContent([]byte("BI /W 10 /H 10 ID no-end"), identityPDFAffine(), 100, 100, nil)
	if len(malformed) != 0 {
		t.Fatalf("unterminated inline image should be ignored: %+v", malformed)
	}
}

// TestParsePDFExtractsInlineJPEGPicture 验证 DCTDecode inline image 可直接
// 保存为 PictureItem，使只接受 image_url 的模型也能获得原始像素资产。
func TestParsePDFExtractsInlineJPEGPicture(t *testing.T) {
	pixels := image.NewRGBA(image.Rect(0, 0, 2, 2))
	pixels.Set(0, 0, color.RGBA{R: 255, A: 255})
	pixels.Set(1, 0, color.RGBA{G: 255, A: 255})
	pixels.Set(0, 1, color.RGBA{B: 255, A: 255})
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, pixels, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode inline JPEG: %v", err)
	}
	var content bytes.Buffer
	content.WriteString("q 200 0 0 100 50 300 cm BI /W 2 /H 2 /CS /RGB /BPC 8 /F /DCT ID ")
	content.Write(encoded.Bytes())
	content.WriteString(" EI Q")
	data := buildTestPDF(t, []testPDFPage{{content: content.String()}})
	doc, err := ParsePDFWithOptions(data, PDFOptions{DisablePopplerFallback: true})
	if err != nil {
		t.Fatalf("parse inline JPEG PDF: %v", err)
	}
	if len(doc.Pictures) != 1 || doc.Pictures[0].Image == nil ||
		doc.Pictures[0].Image.Mimetype != "image/jpeg" ||
		!strings.HasPrefix(doc.Pictures[0].Image.URI, "data:image/jpeg;base64,") ||
		doc.Pictures[0].Image.Size == nil || doc.Pictures[0].Image.Size.Width != 2 || doc.Pictures[0].Image.Size.Height != 2 {
		t.Fatalf("inline JPEG picture=%+v", doc.Pictures)
	}
}

// TestParsePDFExtractsInlineRawAndFlatePictures 验证 8-bit DeviceRGB 的无过滤
// 与 FlateDecode inline image 会被纯 Go 编码为 PNG PictureItem。
func TestParsePDFExtractsInlineRawAndFlatePictures(t *testing.T) {
	raw := []byte{255, 0, 0, 0, 255, 0}
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	if _, err := zw.Write(raw); err != nil {
		t.Fatalf("compress inline RGB: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close inline RGB compressor: %v", err)
	}
	tests := []struct {
		name   string
		filter string
		data   []byte
	}{
		{name: "raw", data: raw},
		{name: "flate", filter: "/F /Fl ", data: compressed.Bytes()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var content bytes.Buffer
			content.WriteString("q 200 0 0 100 50 300 cm BI /W 2 /H 1 /CS /RGB /BPC 8 ")
			content.WriteString(test.filter)
			content.WriteString("ID ")
			content.Write(test.data)
			content.WriteString(" EI Q")
			doc, err := ParsePDFWithOptions(buildTestPDF(t, []testPDFPage{{content: content.String()}}), PDFOptions{DisablePopplerFallback: true})
			if err != nil {
				t.Fatalf("parse inline %s PDF: %v", test.name, err)
			}
			if len(doc.Pictures) != 1 || doc.Pictures[0].Image == nil ||
				doc.Pictures[0].Image.Mimetype != "image/png" ||
				!strings.HasPrefix(doc.Pictures[0].Image.URI, "data:image/png;base64,") {
				t.Fatalf("inline %s picture=%+v", test.name, doc.Pictures)
			}
		})
	}
}

// TestParsePDFDecodesInlineFlatePredictors 验证 FlateDecode 内联图片的
// TIFF 和 PNG 预测器会还原真实像素，不把差分字节当成原图。
func TestParsePDFDecodesInlineFlatePredictors(t *testing.T) {
	target := []byte{10, 20, 30, 40, 50, 60}
	tests := []struct {
		name      string
		predictor int
		encoded   []byte
	}{
		{name: "tiff", predictor: 2, encoded: []byte{10, 20, 30, 30, 30, 30}},
		{name: "png-sub", predictor: 15, encoded: []byte{1, 10, 20, 30, 30, 30, 30}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var compressed bytes.Buffer
			zw := zlib.NewWriter(&compressed)
			if _, err := zw.Write(test.encoded); err != nil {
				t.Fatalf("compress predictor fixture: %v", err)
			}
			if err := zw.Close(); err != nil {
				t.Fatalf("close predictor fixture: %v", err)
			}
			var content bytes.Buffer
			content.WriteString("q 200 0 0 100 50 300 cm BI /W 2 /H 1 /CS /RGB /BPC 8 /F /Fl ")
			fmt.Fprintf(&content, "/DP << /Predictor %d /Colors 3 /BitsPerComponent 8 /Columns 2 >> ID ", test.predictor)
			content.Write(compressed.Bytes())
			content.WriteString(" EI Q")
			doc, err := ParsePDFWithOptions(buildTestPDF(t, []testPDFPage{{content: content.String()}}), PDFOptions{DisablePopplerFallback: true})
			if err != nil {
				t.Fatalf("parse %s predictor: %v", test.name, err)
			}
			if len(doc.Pictures) != 1 || doc.Pictures[0].Image == nil {
				t.Fatalf("%s predictor picture=%+v", test.name, doc.Pictures)
			}
			payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(doc.Pictures[0].Image.URI, "data:image/png;base64,"))
			if err != nil {
				t.Fatalf("decode %s data URI: %v", test.name, err)
			}
			raster, err := png.Decode(bytes.NewReader(payload))
			if err != nil {
				t.Fatalf("decode %s PNG: %v", test.name, err)
			}
			got := make([]byte, 0, len(target))
			for x := 0; x < 2; x++ {
				r, g, b, _ := raster.At(x, 0).RGBA()
				got = append(got, byte(r>>8), byte(g>>8), byte(b>>8))
			}
			if !bytes.Equal(got, target) {
				t.Fatalf("%s predictor pixels=%v want=%v", test.name, got, target)
			}
		})
	}
}

// TestApplyPDFInlinePNGFilters 验证 PNG None/Sub/Up/Average/Paeth 行过滤全部
// 按已解码上一行与左侧像素还原，不依赖 Predictor 数值与行过滤一致。
func TestApplyPDFInlinePNGFilters(t *testing.T) {
	want := []byte{10, 20, 30, 15, 25, 40}
	tests := []struct {
		name string
		data []byte
	}{
		{name: "none", data: []byte{0, 10, 20, 30, 0, 15, 25, 40}},
		{name: "sub", data: []byte{1, 10, 10, 10, 1, 15, 10, 15}},
		{name: "up", data: []byte{2, 10, 20, 30, 2, 5, 5, 10}},
		{name: "average", data: []byte{3, 10, 15, 20, 3, 10, 8, 13}},
		{name: "paeth", data: []byte{4, 10, 10, 10, 4, 5, 5, 10}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			params := &pdfInlineDecodeParams{predictor: 10, colors: 1, bits: 8, columns: 3}
			got := applyPDFInlinePredictor(append([]byte(nil), test.data...), params, 1, int64(len(want)), 2)
			if !bytes.Equal(got, want) {
				t.Fatalf("PNG %s pixels=%v want=%v", test.name, got, want)
			}
		})
	}
	params := &pdfInlineDecodeParams{predictor: 15, colors: 1, bits: 8, columns: 1}
	if got := applyPDFInlinePredictor([]byte{5, 1}, params, 1, 1, 1); got != nil {
		t.Fatalf("invalid PNG filter should be rejected: %v", got)
	}
}

// TestDecodePDFInlineImageRejectsUnsafePredictorParams 验证不能与图像声明
// 一致解释的预测参数只保留几何，不产生错误像素资产。
func TestDecodePDFInlineImageRejectsUnsafePredictorParams(t *testing.T) {
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	_, _ = zw.Write([]byte{10, 20, 30, 30, 30, 30})
	_ = zw.Close()
	base := pdfInlineImage{width: 2, height: 1, colorSpace: "RGB", bits: 8, filter: "Fl"}
	tests := []pdfInlineImage{
		func() pdfInlineImage {
			value := base
			value.decodeParamsInvalid = true
			return value
		}(),
		func() pdfInlineImage {
			value := base
			value.decodeParams = &pdfInlineDecodeParams{predictor: 2, colors: 4, bits: 8, columns: 2}
			return value
		}(),
		func() pdfInlineImage {
			value := base
			value.decodeParams = &pdfInlineDecodeParams{predictor: 3, colors: 3, bits: 8, columns: 2}
			return value
		}(),
		{width: 2, height: 1, colorSpace: "RGB", bits: 8, decodeParams: &pdfInlineDecodeParams{predictor: 2}},
	}
	for index, inline := range tests {
		if image := decodePDFInlineImage(inline, compressed.Bytes()); image != nil {
			t.Fatalf("unsafe predictor case %d decoded: %+v", index, image)
		}
	}
}

// TestParsePDFDecodesInlineLZWPredictor 验证 LZWDecode 内联图片经解码后
// 继续应用 TIFF Predictor，并作为真实 PNG 资产输出。
func TestParsePDFDecodesInlineLZWPredictor(t *testing.T) {
	target := []byte{10, 20, 30, 40, 50, 60}
	differenced := []byte{10, 20, 30, 30, 30, 30}
	encoder, err := pdfcpufilter.NewFilter(pdfcpufilter.LZW, map[string]int{"EarlyChange": 1})
	if err != nil {
		t.Fatalf("create LZW encoder: %v", err)
	}
	encodedReader, err := encoder.Encode(bytes.NewReader(differenced))
	if err != nil {
		t.Fatalf("encode LZW fixture: %v", err)
	}
	encoded, err := io.ReadAll(encodedReader)
	if err != nil {
		t.Fatalf("read LZW fixture: %v", err)
	}
	decoded, ok := decodePDFInlineLZW(encoded, &pdfInlineDecodeParams{earlyChange: 1}, int64(len(differenced)))
	if !ok || !bytes.Equal(decoded, differenced) {
		t.Fatalf("decode LZW fixture ok=%v decoded=%v encoded=%v", ok, decoded, encoded)
	}
	var content bytes.Buffer
	content.WriteString("q 200 0 0 100 50 300 cm BI /W 2 /H 1 /CS /RGB /BPC 8 /F /LZW ")
	content.WriteString("/DP << /Predictor 2 /Colors 3 /BitsPerComponent 8 /Columns 2 /EarlyChange 1 >> ID ")
	content.Write(encoded)
	content.WriteString(" EI Q")
	doc, err := ParsePDFWithOptions(buildTestPDF(t, []testPDFPage{{content: content.String()}}), PDFOptions{DisablePopplerFallback: true})
	if err != nil {
		t.Fatalf("parse LZW predictor: %v", err)
	}
	if len(doc.Pictures) != 1 || doc.Pictures[0].Image == nil {
		t.Fatalf("LZW picture=%+v", doc.Pictures)
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(doc.Pictures[0].Image.URI, "data:image/png;base64,"))
	if err != nil {
		t.Fatalf("decode LZW data URI: %v", err)
	}
	raster, err := png.Decode(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("decode LZW PNG: %v", err)
	}
	got := make([]byte, 0, len(target))
	for x := 0; x < 2; x++ {
		r, g, b, _ := raster.At(x, 0).RGBA()
		got = append(got, byte(r>>8), byte(g>>8), byte(b>>8))
	}
	if !bytes.Equal(got, target) {
		t.Fatalf("LZW predictor pixels=%v want=%v", got, target)
	}
}

// TestParsePDFDecodesInlineLZWEarlyChangeZero 验证跨过 LZW 码宽增长边界时，
// PDF 非默认 EarlyChange=0 仍能还原完整灰度像素。
func TestParsePDFDecodesInlineLZWEarlyChangeZero(t *testing.T) {
	target := make([]byte, 2048)
	for index := range target {
		target[index] = byte((index*73 + index/7) % 251)
	}
	encoder, err := pdfcpufilter.NewFilter(pdfcpufilter.LZW, map[string]int{"EarlyChange": 0})
	if err != nil {
		t.Fatalf("create EarlyChange=0 encoder: %v", err)
	}
	encodedReader, err := encoder.Encode(bytes.NewReader(target))
	if err != nil {
		t.Fatalf("encode EarlyChange=0 fixture: %v", err)
	}
	encoded, err := io.ReadAll(encodedReader)
	if err != nil {
		t.Fatalf("read EarlyChange=0 fixture: %v", err)
	}
	var content bytes.Buffer
	content.WriteString("q 400 0 0 20 50 300 cm BI /W 2048 /H 1 /CS /G /BPC 8 /F /LZW ")
	content.WriteString("/DP << /EarlyChange 0 >> ID ")
	content.Write(encoded)
	content.WriteString(" EI Q")
	doc, err := ParsePDFWithOptions(buildTestPDF(t, []testPDFPage{{content: content.String()}}), PDFOptions{DisablePopplerFallback: true})
	if err != nil {
		t.Fatalf("parse EarlyChange=0 LZW: %v", err)
	}
	if len(doc.Pictures) != 1 || doc.Pictures[0].Image == nil {
		t.Fatalf("EarlyChange=0 picture=%+v", doc.Pictures)
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(doc.Pictures[0].Image.URI, "data:image/png;base64,"))
	if err != nil {
		t.Fatalf("decode EarlyChange=0 data URI: %v", err)
	}
	raster, err := png.Decode(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("decode EarlyChange=0 PNG: %v", err)
	}
	got := make([]byte, len(target))
	for x := range got {
		r, _, _, _ := raster.At(x, 0).RGBA()
		got[x] = byte(r >> 8)
	}
	if !bytes.Equal(got, target) {
		t.Fatalf("EarlyChange=0 pixels mismatch")
	}
}

// TestParsePDFDecodesInlineSimpleFilters 验证 ASCII85、ASCIIHex 和 RunLength
// 单过滤器的内联原始像素可直接转为 PNG。
func TestParsePDFDecodesInlineSimpleFilters(t *testing.T) {
	target := []byte{10, 20, 30, 40, 50, 60}
	tests := []struct {
		name       string
		filterName string
		inlineName string
	}{
		{name: "ascii85", filterName: pdfcpufilter.ASCII85, inlineName: "A85"},
		{name: "ascii-hex", filterName: pdfcpufilter.ASCIIHex, inlineName: "AHx"},
		{name: "run-length", filterName: pdfcpufilter.RunLength, inlineName: "RL"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoder, err := pdfcpufilter.NewFilter(test.filterName, nil)
			if err != nil {
				t.Fatalf("create %s encoder: %v", test.name, err)
			}
			encodedReader, err := encoder.Encode(bytes.NewReader(target))
			if err != nil {
				t.Fatalf("encode %s fixture: %v", test.name, err)
			}
			encoded, err := io.ReadAll(encodedReader)
			if err != nil {
				t.Fatalf("read %s fixture: %v", test.name, err)
			}
			var content bytes.Buffer
			fmt.Fprintf(&content, "q 200 0 0 100 50 300 cm BI /W 2 /H 1 /CS /RGB /BPC 8 /F /%s ID ", test.inlineName)
			content.Write(encoded)
			content.WriteString(" EI Q")
			doc, err := ParsePDFWithOptions(buildTestPDF(t, []testPDFPage{{content: content.String()}}), PDFOptions{DisablePopplerFallback: true})
			if err != nil {
				t.Fatalf("parse %s inline image: %v", test.name, err)
			}
			if len(doc.Pictures) != 1 || doc.Pictures[0].Image == nil {
				t.Fatalf("%s picture=%+v", test.name, doc.Pictures)
			}
			payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(doc.Pictures[0].Image.URI, "data:image/png;base64,"))
			if err != nil {
				t.Fatalf("decode %s data URI: %v", test.name, err)
			}
			raster, err := png.Decode(bytes.NewReader(payload))
			if err != nil {
				t.Fatalf("decode %s PNG: %v", test.name, err)
			}
			got := make([]byte, 0, len(target))
			for x := 0; x < 2; x++ {
				r, g, b, _ := raster.At(x, 0).RGBA()
				got = append(got, byte(r>>8), byte(g>>8), byte(b>>8))
			}
			if !bytes.Equal(got, target) {
				t.Fatalf("%s pixels=%v want=%v", test.name, got, target)
			}
		})
	}
}

// TestParsePDFDecodesInlineFilterChain 验证过滤器与 DecodeParms 数组按位置
// 对齐，并依次还原 ASCII85、Flate 与 TIFF Predictor 像素。
func TestParsePDFDecodesInlineFilterChain(t *testing.T) {
	target := []byte{10, 20, 30, 40, 50, 60}
	predicted := []byte{10, 20, 30, 30, 30, 30}
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	if _, err := zw.Write(predicted); err != nil {
		t.Fatalf("write flate fixture: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close flate fixture: %v", err)
	}
	encoder, err := pdfcpufilter.NewFilter(pdfcpufilter.ASCII85, nil)
	if err != nil {
		t.Fatalf("create ASCII85 encoder: %v", err)
	}
	encodedReader, err := encoder.Encode(bytes.NewReader(compressed.Bytes()))
	if err != nil {
		t.Fatalf("encode ASCII85 fixture: %v", err)
	}
	encoded, err := io.ReadAll(encodedReader)
	if err != nil {
		t.Fatalf("read ASCII85 fixture: %v", err)
	}
	var content bytes.Buffer
	content.WriteString("q 200 0 0 100 50 300 cm BI /W 2 /H 1 /CS /RGB /BPC 8 ")
	content.WriteString("/F [/A85 /Fl] /DP [null << /Predictor 2 /Colors 3 /BitsPerComponent 8 /Columns 2 >>] ID ")
	content.Write(encoded)
	content.WriteString(" EI Q")
	doc, err := ParsePDFWithOptions(buildTestPDF(t, []testPDFPage{{content: content.String()}}), PDFOptions{DisablePopplerFallback: true})
	if err != nil {
		t.Fatalf("parse inline filter chain: %v", err)
	}
	if len(doc.Pictures) != 1 || doc.Pictures[0].Image == nil {
		t.Fatalf("filter chain picture=%+v", doc.Pictures)
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(doc.Pictures[0].Image.URI, "data:image/png;base64,"))
	if err != nil {
		t.Fatalf("decode filter chain data URI: %v", err)
	}
	raster, err := png.Decode(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("decode filter chain PNG: %v", err)
	}
	got := make([]byte, 0, len(target))
	for x := 0; x < 2; x++ {
		r, g, b, _ := raster.At(x, 0).RGBA()
		got = append(got, byte(r>>8), byte(g>>8), byte(b>>8))
	}
	if !bytes.Equal(got, target) {
		t.Fatalf("filter chain pixels=%v want=%v", got, target)
	}
}

// TestDecodePDFInlineImageRejectsUnsafeFilterChains 验证无法逐项解释的过滤器
// 链不会产生伪造像素，也不会因畸形压缩数据触发 panic。
func TestDecodePDFInlineImageRejectsUnsafeFilterChains(t *testing.T) {
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	_, _ = zw.Write([]byte{10, 20, 30, 40, 50, 60})
	_ = zw.Close()
	encoder, err := pdfcpufilter.NewFilter(pdfcpufilter.ASCII85, nil)
	if err != nil {
		t.Fatalf("create unsafe-chain encoder: %v", err)
	}
	encodedReader, err := encoder.Encode(bytes.NewReader(compressed.Bytes()))
	if err != nil {
		t.Fatalf("encode unsafe-chain fixture: %v", err)
	}
	encoded, err := io.ReadAll(encodedReader)
	if err != nil {
		t.Fatalf("read unsafe-chain fixture: %v", err)
	}
	base := pdfInlineImage{
		width: 2, height: 1, colorSpace: "RGB", bits: 8,
		filters: []string{"A85", "Fl"},
	}
	tests := []pdfInlineImage{
		func() pdfInlineImage {
			value := base
			value.decodeParamsList = []*pdfInlineDecodeParams{nil}
			return value
		}(),
		func() pdfInlineImage {
			value := base
			value.decodeParams = &pdfInlineDecodeParams{predictor: 1, colors: 1, bits: 8, columns: 1}
			return value
		}(),
		func() pdfInlineImage {
			value := base
			value.decodeParamsList = []*pdfInlineDecodeParams{{predictor: 1}, nil}
			return value
		}(),
		func() pdfInlineImage {
			value := base
			value.filters = []string{"Fl", "A85"}
			value.decodeParamsList = []*pdfInlineDecodeParams{{predictor: 2, colors: 3, bits: 8, columns: 2}, nil}
			return value
		}(),
		func() pdfInlineImage {
			value := base
			value.filters = []string{"A85", "Unknown"}
			return value
		}(),
	}
	for index, inline := range tests {
		if image := decodePDFInlineImage(inline, encoded); image != nil {
			t.Fatalf("unsafe filter chain case %d decoded: %+v", index, image)
		}
	}
	if image := decodePDFInlineImage(base, []byte("malformed encoded bytes")); image != nil {
		t.Fatalf("malformed filter chain decoded: %+v", image)
	}
}

// TestParsePDFDecodesInlineCCITTGroup4 验证纯 Go 解码一行 Group 4 位图，
// 并按 BlackIs1 参数解释相同传真位流的黑白含义。
func TestParsePDFDecodesInlineCCITTGroup4(t *testing.T) {
	// 第一行相对全白参考行使用 V(0)=1，随后写入两个 12-bit EOL 作为 EOFB。
	group4 := []byte{0x80, 0x08, 0x00, 0x80}
	// Group 3 以 EOL 开始，随后是白色游程 8；缺少 RTC 的常见截断尾部可安全接受。
	group3 := []byte{0x00, 0x19, 0x80}
	tests := []struct {
		name       string
		encoded    []byte
		k          int
		blackIs1   bool
		wantSample byte
	}{
		{name: "group4-white-is-one", encoded: group4, k: -1, wantSample: 0xff},
		{name: "group4-black-is-one", encoded: group4, k: -1, blackIs1: true, wantSample: 0x00},
		{name: "group3-one-dimensional", encoded: group3, wantSample: 0xff},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var content bytes.Buffer
			fmt.Fprintf(&content, "q 80 0 0 10 10 20 cm BI /W 8 /H 1 /CS /G /BPC 1 /F /CCF /DP << /K %d /Columns 8 /Rows 1 /BlackIs1 %t /EndOfBlock false >> ID ", test.k, test.blackIs1)
			content.Write(test.encoded)
			content.WriteString(" EI Q")
			doc, err := ParsePDFWithOptions(buildTestPDF(t, []testPDFPage{{content: content.String()}}), PDFOptions{DisablePopplerFallback: true})
			if err != nil {
				t.Fatalf("parse CCITT inline image: %v", err)
			}
			if len(doc.Pictures) != 1 || doc.Pictures[0].Image == nil {
				t.Fatalf("CCITT picture=%+v", doc.Pictures)
			}
			payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(doc.Pictures[0].Image.URI, "data:image/png;base64,"))
			if err != nil {
				t.Fatalf("decode CCITT data URI: %v", err)
			}
			raster, err := png.Decode(bytes.NewReader(payload))
			if err != nil {
				t.Fatalf("decode CCITT PNG: %v", err)
			}
			for x := 0; x < 8; x++ {
				r, _, _, _ := raster.At(x, 0).RGBA()
				if got := byte(r >> 8); got != test.wantSample {
					t.Fatalf("CCITT pixel %d=%d want=%d", x, got, test.wantSample)
				}
			}
		})
	}
}

// TestDecodePDFInlineCCITTRejectsUnsafeParams 验证暂不支持的 Group 3 混合
// 模式与声明行列不一致时只保留几何，不解码错误像素。
func TestDecodePDFInlineCCITTRejectsUnsafeParams(t *testing.T) {
	inline := pdfInlineImage{width: 8, height: 1, colorSpace: "G", bits: 1, filter: "CCF"}
	tests := []*pdfInlineDecodeParams{
		{k: 1, columns: 8, rows: 1, columnsSet: true, rowsSet: true},
		{k: -1, columns: 7, rows: 1, columnsSet: true, rowsSet: true},
		{k: -1, columns: 8, rows: 2, columnsSet: true, rowsSet: true},
	}
	for index, params := range tests {
		value := inline
		value.decodeParams = params
		if image := decodePDFInlineImage(value, []byte{0x80, 0x08, 0x00, 0x80}); image != nil {
			t.Fatalf("unsafe CCITT params case %d decoded: %+v", index, image)
		}
	}
}

// TestParsePDFDecodesInlinePackedGray 验证 1/2/4-bit DeviceGray 按 PDF
// 每行字节对齐规则展开，并应用可选 Decode 反转数组。
func TestParsePDFDecodesInlinePackedGray(t *testing.T) {
	tests := []struct {
		name       string
		width      int
		height     int
		bits       int
		decode     string
		encoded    []byte
		wantPixels []byte
	}{
		{
			name: "one-bit-row-padding", width: 9, height: 2, bits: 1,
			encoded: []byte{0xaa, 0x80, 0xff, 0x80},
			wantPixels: []byte{
				255, 0, 255, 0, 255, 0, 255, 0, 255,
				255, 255, 255, 255, 255, 255, 255, 255, 255,
			},
		},
		{
			name: "two-bit", width: 3, height: 1, bits: 2,
			encoded: []byte{0x18}, wantPixels: []byte{0, 85, 170},
		},
		{
			name: "four-bit", width: 3, height: 1, bits: 4,
			encoded: []byte{0x08, 0xf0}, wantPixels: []byte{0, 136, 255},
		},
		{
			name: "decode-inversion", width: 2, height: 1, bits: 1,
			decode: " /D [1 0]", encoded: []byte{0x80}, wantPixels: []byte{0, 255},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var content bytes.Buffer
			fmt.Fprintf(&content, "q 100 0 0 50 10 20 cm BI /W %d /H %d /CS /G /BPC %d%s ID ", test.width, test.height, test.bits, test.decode)
			content.Write(test.encoded)
			content.WriteString(" EI Q")
			doc, err := ParsePDFWithOptions(buildTestPDF(t, []testPDFPage{{content: content.String()}}), PDFOptions{DisablePopplerFallback: true})
			if err != nil {
				t.Fatalf("parse packed gray image: %v", err)
			}
			if len(doc.Pictures) != 1 || doc.Pictures[0].Image == nil {
				t.Fatalf("packed gray picture=%+v", doc.Pictures)
			}
			payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(doc.Pictures[0].Image.URI, "data:image/png;base64,"))
			if err != nil {
				t.Fatalf("decode packed gray data URI: %v", err)
			}
			raster, err := png.Decode(bytes.NewReader(payload))
			if err != nil {
				t.Fatalf("decode packed gray PNG: %v", err)
			}
			got := make([]byte, 0, len(test.wantPixels))
			for y := 0; y < test.height; y++ {
				for x := 0; x < test.width; x++ {
					r, _, _, _ := raster.At(x, y).RGBA()
					got = append(got, byte(r>>8))
				}
			}
			if !bytes.Equal(got, test.wantPixels) {
				t.Fatalf("packed gray pixels=%v want=%v", got, test.wantPixels)
			}
		})
	}
}

// TestParsePDFDecodesInlinePackedGrayFilterChain 验证低位深像素经过
// ASCII85→Flate 链后仍按行展开，并在最后应用 Decode 反转。
func TestParsePDFDecodesInlinePackedGrayFilterChain(t *testing.T) {
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	if _, err := zw.Write([]byte{0x18}); err != nil {
		t.Fatalf("write packed gray flate fixture: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close packed gray flate fixture: %v", err)
	}
	encoder, err := pdfcpufilter.NewFilter(pdfcpufilter.ASCII85, nil)
	if err != nil {
		t.Fatalf("create packed gray ASCII85 encoder: %v", err)
	}
	encodedReader, err := encoder.Encode(bytes.NewReader(compressed.Bytes()))
	if err != nil {
		t.Fatalf("encode packed gray ASCII85 fixture: %v", err)
	}
	encoded, err := io.ReadAll(encodedReader)
	if err != nil {
		t.Fatalf("read packed gray ASCII85 fixture: %v", err)
	}
	var content bytes.Buffer
	content.WriteString("q 100 0 0 50 10 20 cm BI /W 3 /H 1 /CS /G /BPC 2 /D [1 0] ")
	content.WriteString("/F [/A85 /Fl] /DP [null null] ID ")
	content.Write(encoded)
	content.WriteString(" EI Q")
	doc, err := ParsePDFWithOptions(buildTestPDF(t, []testPDFPage{{content: content.String()}}), PDFOptions{DisablePopplerFallback: true})
	if err != nil {
		t.Fatalf("parse packed gray filter chain: %v", err)
	}
	if len(doc.Pictures) != 1 || doc.Pictures[0].Image == nil {
		t.Fatalf("packed gray filter chain picture=%+v", doc.Pictures)
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(doc.Pictures[0].Image.URI, "data:image/png;base64,"))
	if err != nil {
		t.Fatalf("decode packed gray filter chain data URI: %v", err)
	}
	raster, err := png.Decode(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("decode packed gray filter chain PNG: %v", err)
	}
	want := []byte{255, 170, 85}
	for x := 0; x < len(want); x++ {
		r, _, _, _ := raster.At(x, 0).RGBA()
		if got := byte(r >> 8); got != want[x] {
			t.Fatalf("packed gray filter chain pixel %d=%d want=%d", x, got, want[x])
		}
	}
}

// TestParsePDFAppliesInlineDeviceDecodeArray 验证 8-bit DeviceRGB 的 Decode
// 数组按通道映射，不把单通道反转错误应用到其他通道。
func TestParsePDFAppliesInlineDeviceDecodeArray(t *testing.T) {
	content := "q 50 0 0 50 10 20 cm BI /W 1 /H 1 /CS /RGB /BPC 8 " +
		"/D [1 0 0 1 0 1] ID " + string([]byte{255, 0, 128}) + " EI Q"
	doc, err := ParsePDFWithOptions(buildTestPDF(t, []testPDFPage{{content: content}}), PDFOptions{DisablePopplerFallback: true})
	if err != nil {
		t.Fatalf("parse DeviceRGB Decode array: %v", err)
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(doc.Pictures[0].Image.URI, "data:image/png;base64,"))
	if err != nil {
		t.Fatalf("decode DeviceRGB data URI: %v", err)
	}
	raster, err := png.Decode(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("decode DeviceRGB PNG: %v", err)
	}
	r, g, b, _ := raster.At(0, 0).RGBA()
	if got, want := []byte{byte(r >> 8), byte(g >> 8), byte(b >> 8)}, []byte{0, 0, 128}; !bytes.Equal(got, want) {
		t.Fatalf("DeviceRGB decoded pixel=%v want=%v", got, want)
	}
}

// TestApplyPDFInlineDecodeArrayRejectsUnsafeValues 验证通道数量不匹配和
// 非有限区间不会生成不可预测的像素。
func TestApplyPDFInlineDecodeArrayRejectsUnsafeValues(t *testing.T) {
	tests := [][]float64{
		{0},
		{0, 1, 0, 1},
		{math.NaN(), 1, 0, 1, 0, 1},
		{0, math.Inf(1), 0, 1, 0, 1},
	}
	for index, decode := range tests {
		if pixels, ok := applyPDFInlineDecodeArray([]byte{1, 2, 3}, 3, 8, decode); ok || pixels != nil {
			t.Fatalf("unsafe Decode case %d accepted: %v", index, pixels)
		}
	}
}

// TestParsePDFDecodesInlineIndexedColorSpace 验证 2-bit 索引样本通过
// 行内十六进制 RGB lookup 还原为真实颜色。
func TestParsePDFDecodesInlineIndexedColorSpace(t *testing.T) {
	content := "q 100 0 0 25 10 20 cm BI /W 4 /H 1 /BPC 2 " +
		"/CS [/I /RGB 3 <FF000000FF000000FFFFFFFF>] /D [0 3] ID " + string([]byte{0x1b}) + " EI Q"
	doc, err := ParsePDFWithOptions(buildTestPDF(t, []testPDFPage{{content: content}}), PDFOptions{DisablePopplerFallback: true})
	if err != nil {
		t.Fatalf("parse Indexed color space: %v", err)
	}
	if len(doc.Pictures) != 1 || doc.Pictures[0].Image == nil {
		t.Fatalf("Indexed picture=%+v", doc.Pictures)
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(doc.Pictures[0].Image.URI, "data:image/png;base64,"))
	if err != nil {
		t.Fatalf("decode Indexed data URI: %v", err)
	}
	raster, err := png.Decode(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("decode Indexed PNG: %v", err)
	}
	want := [][3]byte{{255, 0, 0}, {0, 255, 0}, {0, 0, 255}, {255, 255, 255}}
	for x := range want {
		r, g, b, _ := raster.At(x, 0).RGBA()
		if got := [3]byte{byte(r >> 8), byte(g >> 8), byte(b >> 8)}; got != want[x] {
			t.Fatalf("Indexed pixel %d=%v want=%v", x, got, want[x])
		}
	}
}

// TestParsePDFResolvesInlineNamedIndexedColorSpace 验证 BI 的 /CS 名称从
// 页面 Resources/ColorSpace 解析 Indexed 数组和 lookup。
func TestParsePDFResolvesInlineNamedIndexedColorSpace(t *testing.T) {
	content := append([]byte("q 40 0 0 20 10 20 cm BI /W 2 /H 1 /BPC 1 /CS /CS0 ID "), 0x40)
	content = append(content, []byte(" EI Q")...)
	data := buildPDFInlineColorResourceFixture(t, "/CS0 [/Indexed /DeviceRGB 1 <FF000000FF00>]", content)
	doc, err := ParsePDFWithOptions(data, PDFOptions{DisablePopplerFallback: true})
	if err != nil {
		t.Fatalf("parse named Indexed color space: %v", err)
	}
	if len(doc.Pictures) != 1 || doc.Pictures[0].Image == nil {
		t.Fatalf("named Indexed picture=%+v", doc.Pictures)
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(doc.Pictures[0].Image.URI, "data:image/png;base64,"))
	if err != nil {
		t.Fatalf("decode named Indexed data URI: %v", err)
	}
	raster, err := png.Decode(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("decode named Indexed PNG: %v", err)
	}
	want := [][3]byte{{255, 0, 0}, {0, 255, 0}}
	for x := range want {
		r, g, b, _ := raster.At(x, 0).RGBA()
		if got := [3]byte{byte(r >> 8), byte(g >> 8), byte(b >> 8)}; got != want[x] {
			t.Fatalf("named Indexed pixel %d=%v want=%v", x, got, want[x])
		}
	}
}

// TestParsePDFResolvesInlineNamedICCBasedColorSpace 验证命名 ICCBased
// profile 使用显式 Alternate 解释通道，不尝试伪造 ICC 色彩变换。
func TestParsePDFResolvesInlineNamedICCBasedColorSpace(t *testing.T) {
	content := append([]byte("q 20 0 0 20 10 20 cm BI /W 1 /H 1 /BPC 8 /CS /CS1 ID "), []byte{10, 20, 30}...)
	content = append(content, []byte(" EI Q")...)
	profile := []byte("<< /N 3 /Alternate /DeviceRGB /Length 0 >>\nstream\n\nendstream")
	data := buildPDFInlineColorResourceFixture(t, "/CS1 [/ICCBased 5 0 R]", content, profile)
	doc, err := ParsePDFWithOptions(data, PDFOptions{DisablePopplerFallback: true})
	if err != nil {
		t.Fatalf("parse named ICCBased color space: %v", err)
	}
	if len(doc.Pictures) != 1 || doc.Pictures[0].Image == nil {
		t.Fatalf("named ICCBased picture=%+v", doc.Pictures)
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(doc.Pictures[0].Image.URI, "data:image/png;base64,"))
	if err != nil {
		t.Fatalf("decode named ICCBased data URI: %v", err)
	}
	raster, err := png.Decode(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("decode named ICCBased PNG: %v", err)
	}
	r, g, b, _ := raster.At(0, 0).RGBA()
	if got, want := [3]byte{byte(r >> 8), byte(g >> 8), byte(b >> 8)}, [3]byte{10, 20, 30}; got != want {
		t.Fatalf("named ICCBased pixel=%v want=%v", got, want)
	}
}

// TestParsePDFResolvesInlineIndexedLookupStream 验证命名 Indexed 调色板可从
// 有界间接 stream 读取，而不是仅支持字典内字符串。
func TestParsePDFResolvesInlineIndexedLookupStream(t *testing.T) {
	content := append([]byte("q 40 0 0 20 10 20 cm BI /W 2 /H 1 /BPC 1 /CS /CS0 ID "), 0x40)
	content = append(content, []byte(" EI Q")...)
	lookup := []byte("<< /Length 6 >>\nstream\n\xff\x00\x00\x00\xff\x00\nendstream")
	data := buildPDFInlineColorResourceFixture(t, "/CS0 [/Indexed /DeviceRGB 1 5 0 R]", content, lookup)
	doc, err := ParsePDFWithOptions(data, PDFOptions{DisablePopplerFallback: true})
	if err != nil {
		t.Fatalf("parse Indexed lookup stream: %v", err)
	}
	if len(doc.Pictures) != 1 || doc.Pictures[0].Image == nil {
		t.Fatalf("Indexed stream picture=%+v", doc.Pictures)
	}
}

// TestParsePDFResolvesInlineICCBasedDefaultAndRejectsMismatch 验证 ICC profile
// 缺少 Alternate 时按 N 回退，显式 Alternate 通道不一致时拒绝。
func TestParsePDFResolvesInlineICCBasedDefaultAndRejectsMismatch(t *testing.T) {
	content := append([]byte("q 20 0 0 20 10 20 cm BI /W 1 /H 1 /BPC 8 /CS /CS1 ID "), 0x80)
	content = append(content, []byte(" EI Q")...)
	grayProfile := []byte("<< /N 1 /Length 0 >>\nstream\n\nendstream")
	data := buildPDFInlineColorResourceFixture(t, "/CS1 [/ICCBased 5 0 R]", content, grayProfile)
	if doc, err := ParsePDFWithOptions(data, PDFOptions{DisablePopplerFallback: true}); err != nil ||
		len(doc.Pictures) != 1 || doc.Pictures[0].Image == nil {
		t.Fatalf("ICCBased default fallback doc=%+v err=%v", doc, err)
	}
	mismatchProfile := []byte("<< /N 3 /Alternate /DeviceGray /Length 0 >>\nstream\n\nendstream")
	data = buildPDFInlineColorResourceFixture(t, "/CS1 [/ICCBased 5 0 R]", content, mismatchProfile)
	if doc, err := ParsePDFWithOptions(data, PDFOptions{DisablePopplerFallback: true}); err == nil || doc != nil {
		t.Fatalf("ICCBased mismatched Alternate should fail: doc=%+v err=%v", doc, err)
	}
}

// TestDecodePDFInlineIndexedLiteralLookup 验证 PDF literal lookup 的八进制
// 字节与反向 Decode 索引映射可以还原灰度调色板。
func TestDecodePDFInlineIndexedLiteralLookup(t *testing.T) {
	indexed, ok := parsePDFInlineIndexedColorSpace(`[/Indexed /G 1 (\000\377)]`)
	if !ok || !bytes.Equal(indexed.lookup, []byte{0, 255}) {
		t.Fatalf("literal Indexed lookup=%+v ok=%v", indexed, ok)
	}
	inline := pdfInlineImage{
		width: 2, height: 1, bits: 1, colorSpace: `[/Indexed /G 1 (\000\377)]`,
		decode: []float64{1, 0},
	}
	asset := decodePDFInlineImage(inline, []byte{0x80})
	if asset == nil {
		t.Fatal("literal Indexed image was not decoded")
	}
	raster, err := png.Decode(bytes.NewReader(asset.Data))
	if err != nil {
		t.Fatalf("decode literal Indexed PNG: %v", err)
	}
	want := []byte{0, 255}
	for x := range want {
		r, _, _, _ := raster.At(x, 0).RGBA()
		if got := byte(r >> 8); got != want[x] {
			t.Fatalf("literal Indexed pixel %d=%d want=%d", x, got, want[x])
		}
	}
}

// TestDecodePDFInlineIndexedRejectsUnsafePalette 验证 lookup 长度错误、
// 间接资源和超过位深范围的 hival 不会被猜测解释。
func TestDecodePDFInlineIndexedRejectsUnsafePalette(t *testing.T) {
	invalidSpecs := []string{
		`[/Indexed /RGB 1 <FF0000>]`,
		`[/Indexed /RGB 1 7 0 R]`,
		`[/Indexed /Unknown 1 <0000>]`,
	}
	for index, spec := range invalidSpecs {
		if indexed, ok := parsePDFInlineIndexedColorSpace(spec); ok {
			t.Fatalf("unsafe Indexed spec %d accepted: %+v", index, indexed)
		}
	}
	indexed, ok := parsePDFInlineIndexedColorSpace(`[/Indexed /G 4 <0011223344>]`)
	if !ok {
		t.Fatal("valid five-entry lookup did not parse")
	}
	inline := pdfInlineImage{width: 1, height: 1, bits: 2, colorSpace: `[/Indexed /G 4 <0011223344>]`}
	if asset := decodePDFInlineIndexed(inline, []byte{0}, indexed); asset != nil {
		t.Fatalf("hival above bit depth decoded: %+v", asset)
	}
}

// TestParsePDFDecodesInlinePackedPNGPredictor 验证 2-bit 灰度每行带 PNG
// filter byte 时，按 packed row bytes 而不是像素数还原 Up 过滤。
func TestParsePDFDecodesInlinePackedPNGPredictor(t *testing.T) {
	// 两行目标 packed bytes 分别为 1b00、e4c0；第二行使用 Up 差分。
	predicted := []byte{0, 0x1b, 0x00, 2, 0xc9, 0xc0}
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	if _, err := zw.Write(predicted); err != nil {
		t.Fatalf("write packed Predictor fixture: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close packed Predictor fixture: %v", err)
	}
	var content bytes.Buffer
	content.WriteString("q 100 0 0 50 10 20 cm BI /W 5 /H 2 /CS /G /BPC 2 /F /Fl ")
	content.WriteString("/DP << /Predictor 15 /Colors 1 /BitsPerComponent 2 /Columns 5 >> ID ")
	content.Write(compressed.Bytes())
	content.WriteString(" EI Q")
	doc, err := ParsePDFWithOptions(buildTestPDF(t, []testPDFPage{{content: content.String()}}), PDFOptions{DisablePopplerFallback: true})
	if err != nil {
		t.Fatalf("parse packed PNG Predictor: %v", err)
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(doc.Pictures[0].Image.URI, "data:image/png;base64,"))
	if err != nil {
		t.Fatalf("decode packed Predictor data URI: %v", err)
	}
	raster, err := png.Decode(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("decode packed Predictor PNG: %v", err)
	}
	want := [][]byte{{0, 85, 170, 255, 0}, {255, 170, 85, 0, 255}}
	for y := range want {
		for x := range want[y] {
			r, _, _, _ := raster.At(x, y).RGBA()
			if got := byte(r >> 8); got != want[y][x] {
				t.Fatalf("packed Predictor pixel (%d,%d)=%d want=%d", x, y, got, want[y][x])
			}
		}
	}
}

// TestParsePDFDecodesInlineImageMask 验证 ImageMask 使用内容流当前非描边
// 颜色生成透明 PNG，并按 Decode 数组决定涂色位。
func TestParsePDFDecodesInlineImageMask(t *testing.T) {
	tests := []struct {
		name       string
		decode     string
		wantAlpha0 uint8
		wantAlpha1 uint8
	}{
		{name: "default-decode", wantAlpha0: 0xff},
		{name: "inverted-decode", decode: " /D [1 0]", wantAlpha1: 0xff},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := "1 0 0 rg q 20 0 0 10 10 20 cm BI /W 2 /H 1 /IM true /BPC 1" +
				test.decode + " ID " + string([]byte{0x40}) + " EI Q"
			doc, err := ParsePDFWithOptions(buildTestPDF(t, []testPDFPage{{content: content}}), PDFOptions{DisablePopplerFallback: true})
			if err != nil {
				t.Fatalf("parse inline image mask: %v", err)
			}
			if len(doc.Pictures) != 1 || doc.Pictures[0].Image == nil {
				t.Fatalf("image mask picture=%+v", doc.Pictures)
			}
			payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(doc.Pictures[0].Image.URI, "data:image/png;base64,"))
			if err != nil {
				t.Fatalf("decode image mask data URI: %v", err)
			}
			raster, err := png.Decode(bytes.NewReader(payload))
			if err != nil {
				t.Fatalf("decode image mask PNG: %v", err)
			}
			r0, g0, b0, a0 := raster.At(0, 0).RGBA()
			r1, g1, b1, a1 := raster.At(1, 0).RGBA()
			if byte(a0>>8) != test.wantAlpha0 || byte(a1>>8) != test.wantAlpha1 {
				t.Fatalf("image mask alpha=(%d,%d) want=(%d,%d)", byte(a0>>8), byte(a1>>8), test.wantAlpha0, test.wantAlpha1)
			}
			if test.wantAlpha0 != 0 && (byte(r0>>8) != 255 || g0 != 0 || b0 != 0) {
				t.Fatalf("painted first mask pixel rgba=(%d,%d,%d,%d)", byte(r0>>8), byte(g0>>8), byte(b0>>8), byte(a0>>8))
			}
			if test.wantAlpha1 != 0 && (byte(r1>>8) != 255 || g1 != 0 || b1 != 0) {
				t.Fatalf("painted second mask pixel rgba=(%d,%d,%d,%d)", byte(r1>>8), byte(g1>>8), byte(b1>>8), byte(a1>>8))
			}
		})
	}
}

// TestInspectPDFInlineImageMaskRestoresFillColor 验证 q/Q 同时保存矩阵和
// 非描边颜色，彩色蒙版结束后恢复默认黑色。
func TestInspectPDFInlineImageMaskRestoresFillColor(t *testing.T) {
	mask := string([]byte{0x40})
	content := []byte("q 0 1 0 rg 20 0 0 10 10 20 cm BI /W 2 /H 1 /IM true ID " + mask +
		" EI Q q 20 0 0 10 40 20 cm BI /W 2 /H 1 /IM true ID " + mask + " EI Q")
	placements := inspectPDFInlineImageContent(content, identityPDFAffine(), 100, 100, nil)
	if len(placements) != 2 || placements[0].InlineImage == nil || placements[1].InlineImage == nil {
		t.Fatalf("image mask placements=%+v", placements)
	}
	want := [][3]byte{{0, 255, 0}, {0, 0, 0}}
	for index, placement := range placements {
		raster, err := png.Decode(bytes.NewReader(placement.InlineImage.Data))
		if err != nil {
			t.Fatalf("decode image mask %d: %v", index, err)
		}
		r, g, b, a := raster.At(0, 0).RGBA()
		got := [3]byte{byte(r >> 8), byte(g >> 8), byte(b >> 8)}
		if got != want[index] || byte(a>>8) != 0xff {
			t.Fatalf("image mask %d rgba=%v/%d want=%v/255", index, got, byte(a>>8), want[index])
		}
	}
	if placements[0].BBox.L != 10 || placements[1].BBox.L != 40 {
		t.Fatalf("image mask bboxes=%+v %+v", placements[0].BBox, placements[1].BBox)
	}
}

// TestParsePDFNormalizesRotatedCropInlinePicture 验证 CropBox 非零原点与
// /Rotate 同时作用时，页尺寸和内联图片 provenance 使用同一显示坐标。
func TestParsePDFNormalizesRotatedCropInlinePicture(t *testing.T) {
	raw := []byte{255, 0, 0, 0, 255, 0}
	var content bytes.Buffer
	content.WriteString("q 100 0 0 50 150 250 cm BI /W 2 /H 1 /CS /RGB /BPC 8 ID ")
	content.Write(raw)
	content.WriteString(" EI Q")
	data := buildTestPDF(t, []testPDFPage{{
		content: content.String(), mediaBox: "[0 0 600 800]", cropBox: "[100 200 500 700]", rotate: 90,
	}})
	doc, err := ParsePDFWithOptions(data, PDFOptions{DisablePopplerFallback: true})
	if err != nil {
		t.Fatalf("parse rotated inline picture: %v", err)
	}
	page := doc.Pages["1"]
	if page.Size == nil || page.Size.Width != 500 || page.Size.Height != 400 {
		t.Fatalf("rotated page=%+v", page)
	}
	if len(doc.Pictures) != 1 || len(doc.Pictures[0].Prov) != 1 {
		t.Fatalf("rotated pictures=%+v", doc.Pictures)
	}
	bbox := doc.Pictures[0].Prov[0].BBox
	if bbox == nil || bbox.L != 50 || bbox.B != 250 || bbox.R != 100 || bbox.T != 350 {
		t.Fatalf("rotated picture bbox=%+v", bbox)
	}
}

// TestParsePDFRoutesRotatedPageToVisual 验证非零页旋转作为显式质量
// 信号传给结构化视觉，并携带旋转后页宽高。
func TestParsePDFRoutesRotatedPageToVisual(t *testing.T) {
	data := buildTestPDF(t, []testPDFPage{{
		content:  "BT /F1 10 Tf 150 250 Td (rotated text) Tj ET",
		mediaBox: "[0 0 600 800]", cropBox: "[100 200 500 700]", rotate: 90,
	}})
	calls := 0
	doc, err := ParsePDFWithOptions(data, PDFOptions{
		DisablePopplerFallback: true,
		VisualHook: func(request PDFVisualRequest) (PDFVisualResult, error) {
			calls++
			if request.Width != 500 || request.Height != 400 || request.Quality.PageRotation != 90 ||
				request.Quality.UserUnit != 1 || !request.Quality.NeedsVisual ||
				!containsString(request.Quality.Reasons, "rotated_page") {
				t.Fatalf("rotated visual request=%+v", request)
			}
			return PDFVisualResult{Items: []PDFVisualItem{{
				Label: LabelText, Text: "rotation normalized", Confidence: 0.95,
				BBox: &DoclingBBox{L: 20, B: 20, R: 200, T: 40, CoordOrigin: CoordOriginBottomLeft},
			}}}, nil
		},
	})
	if err != nil || calls != 1 || !strings.Contains(doc.Text(), "rotation normalized") {
		t.Fatalf("rotated visual calls=%d text=%q err=%v", calls, doc.Text(), err)
	}
}

// TestParsePDFMixedTextImageTriggersVisual 验证带正常文本层的大幅图片页面无需
// VisualAlways 也会进入结构化视觉链路，从而识别图片表格。
func TestParsePDFMixedTextImageTriggersVisual(t *testing.T) {
	data := buildPDFImageFixture(t, "300 0 0 200 100 300")
	calls := 0
	doc, err := ParsePDFWithOptions(data, PDFOptions{VisualHook: func(request PDFVisualRequest) (PDFVisualResult, error) {
		calls++
		if request.Quality.ImageCount != 1 || request.Quality.ImageCoverage < 0.12 ||
			!containsString(request.Quality.Reasons, "mixed_text_image") {
			t.Fatalf("quality=%+v", request.Quality)
		}
		return PDFVisualResult{Items: []PDFVisualItem{{
			Label: LabelTable, BBox: &DoclingBBox{L: 100, B: 300, R: 400, T: 500, CoordOrigin: CoordOriginBottomLeft}, Confidence: 0.95,
			TableData: &TableData{NumRows: 2, NumCols: 2, TableCells: []DoclingTableCell{
				{Text: "A", RowSpan: 1, ColSpan: 1, StartRowOffsetIdx: 0, EndRowOffsetIdx: 1, StartColOffsetIdx: 0, EndColOffsetIdx: 1, ColumnHeader: true},
				{Text: "B", RowSpan: 1, ColSpan: 1, StartRowOffsetIdx: 0, EndRowOffsetIdx: 1, StartColOffsetIdx: 1, EndColOffsetIdx: 2, ColumnHeader: true},
				{Text: "1", RowSpan: 1, ColSpan: 1, StartRowOffsetIdx: 1, EndRowOffsetIdx: 2, StartColOffsetIdx: 0, EndColOffsetIdx: 1},
				{Text: "2", RowSpan: 1, ColSpan: 1, StartRowOffsetIdx: 1, EndRowOffsetIdx: 2, StartColOffsetIdx: 1, EndColOffsetIdx: 2},
			}},
		}}}, nil
	}})
	if err != nil {
		t.Fatalf("ParsePDFWithOptions: %v", err)
	}
	if calls != 1 || len(doc.Tables) != 1 {
		t.Fatalf("calls=%d tables=%d", calls, len(doc.Tables))
	}
}

// TestParsePDFVisualRequestCarriesEmbeddedImage 验证疑难混合页调用大模型时
// 同时提供单页 PDF 和已纯 Go 解码的图片资产，且视觉图片说明复用原始 URI。
func TestParsePDFVisualRequestCarriesEmbeddedImage(t *testing.T) {
	data := buildPDFJPEGImageFixture(t, "400 0 0 400 100 200", true)
	doc, err := ParsePDFWithOptions(data, PDFOptions{DisablePopplerFallback: true, VisualHook: func(request PDFVisualRequest) (PDFVisualResult, error) {
		if len(request.EmbeddedImages) != 1 || request.EmbeddedImages[0].Image == nil ||
			!strings.HasPrefix(request.EmbeddedImages[0].Image.URI, "data:image/jpeg;base64,") ||
			request.EmbeddedImages[0].BBox == nil || request.EmbeddedImages[0].BBox.L != 100 {
			t.Fatalf("visual embedded images=%+v", request.EmbeddedImages)
		}
		return PDFVisualResult{Items: []PDFVisualItem{{
			Label: LabelPicture, Text: "设备照片", Confidence: 0.99,
			BBox: &DoclingBBox{L: 100, B: 200, R: 500, T: 600, CoordOrigin: CoordOriginBottomLeft},
		}}}, nil
	}})
	if err != nil {
		t.Fatalf("parse visual image PDF: %v", err)
	}
	if len(doc.Pictures) != 1 || doc.Pictures[0].Image == nil || len(doc.Pictures[0].Captions) != 1 {
		t.Fatalf("visual image should reuse one PictureItem: %+v", doc.Pictures)
	}
}

// TestParsePDFTinyImageDoesNotTriggerVisual 验证页角小图标不会无条件放大模型成本。
func TestParsePDFTinyImageDoesNotTriggerVisual(t *testing.T) {
	data := buildPDFImageFixture(t, "20 0 0 20 10 760")
	_, err := ParsePDFWithOptions(data, PDFOptions{VisualHook: func(PDFVisualRequest) (PDFVisualResult, error) {
		t.Fatal("tiny image must not trigger visual")
		return PDFVisualResult{}, nil
	}})
	if err != nil {
		t.Fatalf("ParsePDFWithOptions: %v", err)
	}
}

// TestParsePDFExtractsEmbeddedJPEGPicture 验证纯 Go 主路径把 PDF 位图保存为
// 官方 PictureItem，并保留像素尺寸、data URI、页号和页面绘制 bbox。
func TestParsePDFExtractsEmbeddedJPEGPicture(t *testing.T) {
	data := buildPDFJPEGImageFixture(t, "300 0 0 200 100 300", true)
	doc, err := ParsePDFWithOptions(data, PDFOptions{DisablePopplerFallback: true})
	if err != nil {
		t.Fatalf("parse mixed PDF: %v", err)
	}
	if len(doc.Pictures) != 1 {
		t.Fatalf("pictures=%+v, want one", doc.Pictures)
	}
	picture := doc.Pictures[0]
	if picture.Image == nil || picture.Image.Mimetype != "image/jpeg" || picture.Image.Size == nil ||
		picture.Image.Size.Width != 2 || picture.Image.Size.Height != 2 ||
		!strings.HasPrefix(picture.Image.URI, "data:image/jpeg;base64,") {
		t.Fatalf("image ref=%+v", picture.Image)
	}
	if len(picture.Prov) != 1 || picture.Prov[0].PageNo != 1 || picture.Prov[0].BBox == nil ||
		picture.Prov[0].BBox.L != 100 || picture.Prov[0].BBox.B != 300 ||
		picture.Prov[0].BBox.R != 400 || picture.Prov[0].BBox.T != 500 {
		t.Fatalf("picture provenance=%+v", picture.Prov)
	}
	if len(doc.Body.Children) != 2 || doc.Body.Children[0].Kind != refTexts || doc.Body.Children[1].Kind != refPictures {
		t.Fatalf("mixed reading order=%+v", doc.Body.Children)
	}
	validateDocumentWithDoclingCore110ForTest(t, "pdf-embedded-jpeg", doc)
}

// TestParsePDFSkipsCorruptImageAndKeepsText 验证单张图片损坏只跳过图片，
// 同页文本层仍正常进入结构化结果。
func TestParsePDFSkipsCorruptImageAndKeepsText(t *testing.T) {
	data := buildPDFJPEGImageFixture(t, "300 0 0 200 100 300", true)
	jpegStart := bytes.Index(data, []byte{0xff, 0xd8, 0xff})
	if jpegStart < 0 {
		t.Fatal("fixture JPEG start not found")
	}
	data[jpegStart], data[jpegStart+1], data[jpegStart+2] = 0, 0, 0
	doc, err := ParsePDFWithOptions(data, PDFOptions{DisablePopplerFallback: true})
	if err != nil {
		t.Fatalf("corrupt image must not fail text parsing: %v", err)
	}
	if !strings.Contains(doc.Text(), "normal text layer") || len(doc.Pictures) != 0 {
		t.Fatalf("corrupt image result text=%q pictures=%d", doc.Text(), len(doc.Pictures))
	}
}

// TestSafePDFImagePagesRejectsOversizedResources 验证超大声明维度只保留
// 几何质量信号，不进入可能产生大额内存分配的像素解码。
func TestSafePDFImagePagesRejectsOversizedResources(t *testing.T) {
	pages := safePDFImagePages(map[int64][]pdfImagePlacement{
		0: {{Name: "safe", PixelWidth: 2000, PixelHeight: 1000}},
		1: {{Name: "wide", PixelWidth: pdfMaxExtractDimension + 1, PixelHeight: 1}},
		2: {{Name: "huge", PixelWidth: 20000, PixelHeight: 20000}},
	})
	if len(pages) != 1 || pages[0] != 0 {
		t.Fatalf("safe pages=%v", pages)
	}
}

// TestMergePDFPictureLinesAttachesVisualCaptionAsset 验证结构化视觉已生成
// PictureItem 时，原始像素资源挂到该对象而不是额外产生重复图片。
func TestMergePDFPictureLinesAttachesVisualCaptionAsset(t *testing.T) {
	bbox := &DoclingBBox{L: 100, B: 300, R: 400, T: 500, CoordOrigin: CoordOriginBottomLeft}
	sub := pdfVisualResultDocument(PDFVisualResult{Items: []PDFVisualItem{{
		Label: LabelPicture, Text: "设备结构图", BBox: bbox, Confidence: 0.99,
	}}}, 1)
	visual := pdfLine{PageIdx: 0, OCRSub: sub, MinX: 100, MinY: 300, MaxX: 400, MaxY: 500}
	asset := &ImageRef{Mimetype: "image/jpeg", Dpi: 72, Size: &ImageSize{Width: 20, Height: 10}, URI: "data:image/jpeg;base64,AA=="}
	merged := mergePDFPictureLines([]pdfLine{visual}, []pdfLine{{
		PageIdx: 0, Picture: asset, MinX: 110, MinY: 310, MaxX: 390, MaxY: 490,
	}})
	if len(merged) != 1 || merged[0].OCRSub.Pictures[0].Image != asset {
		t.Fatalf("visual picture asset not attached: %+v", merged)
	}
	doc := NewDoclingDocument("visual-picture")
	buildPDFDoclingDocument(buildPDFBlocks(buildPDFElements(merged)), doc)
	if len(doc.Pictures) != 1 || doc.Pictures[0].Image == nil || len(doc.Pictures[0].Captions) != 1 {
		t.Fatalf("visual picture duplicated or incomplete: %+v", doc.Pictures)
	}
}

// TestMergePDFPictureLinesKeepsColumnTextOrder 验证无法形成新 XY-cut 时，
// 图片只插入对应水平区域，不会把左右栏正文重新按全页 Y 混排。
func TestMergePDFPictureLinesKeepsColumnTextOrder(t *testing.T) {
	leftTop := pdfLine{Text: "左上", PageIdx: 0, MinX: 10, MaxX: 100, MinY: 700, MaxY: 710}
	leftBottom := pdfLine{Text: "左下", PageIdx: 0, MinX: 10, MaxX: 100, MinY: 500, MaxY: 510}
	rightTop := pdfLine{Text: "右上", PageIdx: 0, MinX: 300, MaxX: 390, MinY: 700, MaxY: 710}
	rightBottom := pdfLine{Text: "右下", PageIdx: 0, MinX: 300, MaxX: 390, MinY: 500, MaxY: 510}
	picture := pdfLine{PageIdx: 0, Picture: &ImageRef{}, MinX: 305, MaxX: 385, MinY: 590, MaxY: 650}
	merged := mergePDFPictureLines([]pdfLine{leftTop, leftBottom, rightTop, rightBottom}, []pdfLine{picture})
	if len(merged) != 5 || merged[0].Text != "左上" || merged[1].Text != "左下" ||
		merged[2].Text != "右上" || merged[3].Picture == nil || merged[4].Text != "右下" {
		t.Fatalf("column reading order=%+v", merged)
	}
}

// TestParsePDFImageOnlyReturnsStructuredPicture 验证未配置 OCR 的纯图片 PDF
// 仍返回可供多模态链处理的 PictureItem，而不是报全文为空。
func TestParsePDFImageOnlyReturnsStructuredPicture(t *testing.T) {
	data := buildPDFJPEGImageFixture(t, "500 0 0 700 50 40", false)
	doc, err := ParsePDFWithOptions(data, PDFOptions{DisablePopplerFallback: true})
	if err != nil {
		t.Fatalf("parse image-only PDF: %v", err)
	}
	if len(doc.Pictures) != 1 || len(doc.Body.Children) != 1 || doc.Body.Children[0].Kind != refPictures {
		t.Fatalf("image-only document=%+v", doc)
	}
}

// TestParsePDFCanDisableEmbeddedImageExtraction 验证调用方可在严格内存预算
// 场景关闭像素解码，同时继续保留文本解析能力。
func TestParsePDFCanDisableEmbeddedImageExtraction(t *testing.T) {
	data := buildPDFJPEGImageFixture(t, "300 0 0 200 100 300", true)
	doc, err := ParsePDFWithOptions(data, PDFOptions{
		DisablePopplerFallback: true, DisableEmbeddedImageExtraction: true,
	})
	if err != nil {
		t.Fatalf("parse PDF with image extraction disabled: %v", err)
	}
	if len(doc.Pictures) != 0 || !strings.Contains(doc.Text(), "normal text layer") {
		t.Fatalf("disabled extraction result: pictures=%d text=%q", len(doc.Pictures), doc.Text())
	}
	viaUnified, err := ParseByExtWithOptions("mixed.pdf", data, ParseOptions{
		DisablePopplerFallback: true, DisablePDFEmbeddedImageExtraction: true,
	})
	if err != nil {
		t.Fatalf("unified disable option: %v", err)
	}
	if len(viaUnified.Pictures) != 0 {
		t.Fatalf("unified disable option pictures=%d", len(viaUnified.Pictures))
	}
}

// buildPDFLowDepthSMaskFixture 构造 2x2 DeviceRGB Flate 图片对象，其 SMask 是
// 2 位 DeviceGray Flate 流，四个像素的软蒙版值依次为 0/1/2/3（最大值 3），
// 用于验证低位深软蒙版的纯 Go 合成。
func buildPDFLowDepthSMaskFixture(t *testing.T) []byte {
	t.Helper()
	var rgb bytes.Buffer
	encoder := zlib.NewWriter(&rgb)
	if _, err := encoder.Write([]byte{255, 0, 0, 0, 255, 0, 0, 0, 255, 255, 255, 255}); err != nil {
		t.Fatalf("write RGB pixels: %v", err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatalf("close RGB zlib: %v", err)
	}
	var smask bytes.Buffer
	smaskEncoder := zlib.NewWriter(&smask)
	// 2 位深、每行 1 字节：行 0 = 值 0,1（0x10），行 1 = 值 2,3（0xB0）。
	if _, err := smaskEncoder.Write([]byte{0x10, 0xB0}); err != nil {
		t.Fatalf("write soft mask pixels: %v", err)
	}
	if err := smaskEncoder.Close(); err != nil {
		t.Fatalf("close soft mask zlib: %v", err)
	}
	content := "q 300 0 0 200 100 300 cm /Im0 Do Q\n"
	objects := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /XObject << /Im0 5 0 R >> >> >>"),
		[]byte(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content)),
		[]byte(fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width 2 /Height 2 /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /FlateDecode /SMask 6 0 R /Length %d >>\nstream\n", rgb.Len())),
		[]byte(fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width 2 /Height 2 /ColorSpace /DeviceGray /BitsPerComponent 2 /Filter /FlateDecode /Length %d >>\nstream\n", smask.Len())),
	}
	for index, payload := range []*bytes.Buffer{&rgb, &smask} {
		objects[index+4] = append(objects[index+4], payload.Bytes()...)
		objects[index+4] = append(objects[index+4], []byte("\nendstream")...)
	}
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for index, object := range objects {
		offsets[index] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n", index+1)
		out.Write(object)
		out.WriteString("\nendobj\n")
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return out.Bytes()
}

// TestExtractPDFEmbeddedPictureLinesAppliesLowDepthSoftMask 验证 2 位深软蒙版
// 在 pdfcpu 忽略（其仅支持 bpc=8）时仍被纯 Go 合成为像素 alpha：软蒙版值
// 0/1/2/3 映射为 alpha 0/85/170/255。
func TestExtractPDFEmbeddedPictureLinesAppliesLowDepthSoftMask(t *testing.T) {
	data := buildPDFLowDepthSMaskFixture(t)
	bbox := &DoclingBBox{L: 100, B: 300, R: 400, T: 500, CoordOrigin: CoordOriginBottomLeft}
	lines := extractPDFEmbeddedPictureLines(data, map[int64][]pdfImagePlacement{
		0: {{Name: "Im0", PixelWidth: 2, PixelHeight: 2, BBox: bbox}},
	})
	if len(lines) != 1 || lines[0].Picture == nil || lines[0].Picture.URI == "" {
		t.Fatalf("extracted lines=%+v", lines)
	}
	uri := lines[0].Picture.URI
	if !strings.HasPrefix(uri, "data:image/png;base64,") {
		t.Fatalf("unexpected image mime: %s", lines[0].Picture.Mimetype)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(uri, "data:image/png;base64,"))
	if err != nil {
		t.Fatalf("decode image data: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	nrgba, ok := img.(*image.NRGBA)
	if !ok {
		t.Fatalf("png model=%T", img)
	}
	wantAlpha := []uint8{0, 85, 170, 255}
	for index, want := range wantAlpha {
		got := nrgba.NRGBAAt(index%2, index/2).A
		if got != want {
			t.Fatalf("pixel %d alpha = %d, want %d", index, got, want)
		}
	}
}

// buildPDFJPXImageFixture 构造 2x2 DeviceRGB 图片对象，像素以 JPEG 2000
// codestream（/Filter /JPXDecode）保存，用于验证 JPX 图片的纯 Go 解码。
func buildPDFJPXImageFixture(t *testing.T) []byte {
	t.Helper()
	pixels := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	pixels.SetNRGBA(0, 0, color.NRGBA{R: 255, G: 0, B: 0, A: 255})
	pixels.SetNRGBA(1, 0, color.NRGBA{R: 0, G: 255, B: 0, A: 255})
	pixels.SetNRGBA(0, 1, color.NRGBA{R: 0, G: 0, B: 255, A: 255})
	pixels.SetNRGBA(1, 1, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
	var jpx bytes.Buffer
	if err := jpeg2000.Encode(&jpx, pixels, &jpeg2000.Options{Format: jpeg2000.FormatJ2K, Lossless: true}); err != nil {
		t.Fatalf("encode fixture JPEG2000: %v", err)
	}
	content := "q 300 0 0 200 100 300 cm /Im0 Do Q\n"
	objects := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /XObject << /Im0 5 0 R >> >> >>"),
		[]byte(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content)),
		[]byte(fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width 2 /Height 2 /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /JPXDecode /Length %d >>\nstream\n", jpx.Len())),
	}
	objects[4] = append(objects[4], jpx.Bytes()...)
	objects[4] = append(objects[4], []byte("\nendstream")...)
	var out bytes.Buffer
	out.WriteString("%PDF-1.5\n")
	offsets := make([]int, len(objects))
	for index, object := range objects {
		offsets[index] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n", index+1)
		out.Write(object)
		out.WriteString("\nendobj\n")
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return out.Bytes()
}

// TestExtractPDFEmbeddedPictureLinesDecodesJPX 验证 pdfcpu 无法解码的
// JPXDecode 图片由纯 Go JPEG 2000 解码路径恢复为可检索 PNG 像素。
func TestExtractPDFEmbeddedPictureLinesDecodesJPX(t *testing.T) {
	data := buildPDFJPXImageFixture(t)
	bbox := &DoclingBBox{L: 100, B: 300, R: 400, T: 500, CoordOrigin: CoordOriginBottomLeft}
	lines := extractPDFEmbeddedPictureLines(data, map[int64][]pdfImagePlacement{
		0: {{Name: "Im0", PixelWidth: 2, PixelHeight: 2, BBox: bbox}},
	})
	if len(lines) != 1 || lines[0].Picture == nil || lines[0].Picture.URI == "" {
		t.Fatalf("extracted lines=%+v", lines)
	}
	uri := lines[0].Picture.URI
	if !strings.HasPrefix(uri, "data:image/png;base64,") {
		t.Fatalf("jpx image should be re-encoded as png, got %s", lines[0].Picture.Mimetype)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(uri, "data:image/png;base64,"))
	if err != nil {
		t.Fatalf("decode image data: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	want := []color.NRGBA{
		{R: 255, G: 0, B: 0, A: 255}, {R: 0, G: 255, B: 0, A: 255},
		{R: 0, G: 0, B: 255, A: 255}, {R: 255, G: 255, B: 255, A: 255},
	}
	for index, expected := range want {
		got := color.NRGBAModel.Convert(img.At(index%2, index/2)).(color.NRGBA)
		if got != expected {
			t.Fatalf("pixel %d = %+v, want %+v", index, got, expected)
		}
	}
}

// jbig2EmbeddedSampleFixture 是 94 字节的 PDF 内嵌 JBIG2 段流（3562x851 全白
// 单页，generic-region + page-info 两个段），取自 gobig2 项目 testdata
// （github.com/dkrisman/gobig2，Apache-2.0），用于端到端验证 /JBIG2Decode
// 图片的纯 Go 兜底解码。
const jbig2EmbeddedSampleFixture = "AAAAADAAAQAAABMAAA3qAAADUwAAFxEAABcRUQAAAAAAASYAAQAAADUAAA3qAAADUwAAAAAAAAAAAgAD//3/Av7+/v9/hlMPtskiz/9//3//f/9//3//f/9//3//rA=="

// buildPDFJBIG2ImageFixture 构造图片对象为 /Filter /JBIG2Decode 的单页 PDF。
func buildPDFJBIG2ImageFixture(t *testing.T) []byte {
	t.Helper()
	jbig2Data, err := base64.StdEncoding.DecodeString(jbig2EmbeddedSampleFixture)
	if err != nil {
		t.Fatalf("decode jbig2 fixture: %v", err)
	}
	content := "q 300 0 0 200 100 300 cm /Im0 Do Q\n"
	objects := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /XObject << /Im0 5 0 R >> >> >>"),
		[]byte(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content)),
		[]byte(fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width 3562 /Height 851 /ColorSpace /DeviceGray /BitsPerComponent 1 /Filter /JBIG2Decode /Length %d >>\nstream\n", len(jbig2Data))),
	}
	objects[4] = append(objects[4], jbig2Data...)
	objects[4] = append(objects[4], []byte("\nendstream")...)
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for index, object := range objects {
		offsets[index] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n", index+1)
		out.Write(object)
		out.WriteString("\nendobj\n")
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return out.Bytes()
}

// TestExtractPDFEmbeddedPictureLinesDecodesJBIG2 验证 pdfcpu 跳过的
// /JBIG2Decode 图片由纯 Go 兜底解码恢复为可检索 PNG。
func TestExtractPDFEmbeddedPictureLinesDecodesJBIG2(t *testing.T) {
	data := buildPDFJBIG2ImageFixture(t)
	bbox := &DoclingBBox{L: 100, B: 300, R: 400, T: 500, CoordOrigin: CoordOriginBottomLeft}
	lines := extractPDFEmbeddedPictureLines(data, map[int64][]pdfImagePlacement{
		0: {{Name: "Im0", PixelWidth: 3562, PixelHeight: 851, BBox: bbox}},
	})
	if len(lines) != 1 || lines[0].Picture == nil || lines[0].Picture.URI == "" {
		t.Fatalf("extracted lines=%+v", lines)
	}
	uri := lines[0].Picture.URI
	if !strings.HasPrefix(uri, "data:image/png;base64,") {
		t.Fatalf("jbig2 image should be re-encoded as png, got %s", lines[0].Picture.Mimetype)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(uri, "data:image/png;base64,"))
	if err != nil {
		t.Fatalf("decode image data: %v", err)
	}
	config, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	if config.Width != 3562 || config.Height != 851 {
		t.Fatalf("png size = %dx%d, want 3562x851", config.Width, config.Height)
	}
}
