// pdf_image_codecs.go 是 PDF 内嵌图片补充解码的编排层：JPEG 2000 与
// JBIG2 的字节级解码由 internal/pdfenc 提供，这里负责从 pdfcpu 提取结果
// 中识别对应资产并接入提取流程。
//
// pdfcpu 不解码这两类编码：JPEG 2000 原始 codestream 被透传为 image/jp2
// 资产，JBIG2 段流被透传为 image/jbig2 资产，此前均无法进入检索与多模态
// 链路。任何解码失败都由后续统一图片校验跳过该资源，正文解析不受影响。
package docling

import (
	pdfcpumodel "github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/unitedrhino/docling/internal/pdfenc"
)

// decodePDFJPXImage 把 pdfcpu 透传的 JPEG 2000 图片解码为 PNG。仅处理
// image/jp2 资产；解码失败时原样返回，交由统一图片校验跳过。
func decodePDFJPXImage(decoded pdfExtractedImage) pdfExtractedImage {
	if decoded.MIMEType != "image/jp2" || len(decoded.Data) == 0 {
		return decoded
	}
	pngData, ok := pdfenc.DecodeJPEG2000(decoded.Data, pdfMaxExtractDimension)
	if !ok {
		return decoded
	}
	decoded.Data = pngData
	decoded.MIMEType = "image/png"
	return decoded
}

// decodePDFJBIG2Image 把 pdfcpu 透传的 JBIG2 图片解码为 PNG：段流即提取
// 资产字节，/JBIG2Globals 全局段从图片对象字典解引用。仅处理 image/jbig2
// 资产；解码失败时原样返回，交由统一图片校验跳过。
func decodePDFJBIG2Image(context *pdfcpumodel.Context, objectNumber int, decoded pdfExtractedImage) pdfExtractedImage {
	if decoded.MIMEType != "image/jbig2" || len(decoded.Data) == 0 {
		return decoded
	}
	// 第三方解码链路可能 panic，兜底只增强不破坏。
	defer func() { _ = recover() }()
	var globals []byte
	if context != nil && context.Optimize != nil {
		if object := context.Optimize.ImageObjects[objectNumber]; object != nil && object.ImageDict != nil {
			if entry, _ := object.ImageDict.Find("JBIG2Globals"); entry != nil {
				if stream, _, err := context.DereferenceStreamDict(entry); err == nil && stream != nil {
					globals = stream.Raw
				}
			}
		}
	}
	pngData, ok := pdfenc.DecodeJBIG2(decoded.Data, globals, pdfMaxExtractDimension)
	if !ok {
		return decoded
	}
	decoded.Data = pngData
	decoded.MIMEType = "image/png"
	return decoded
}
