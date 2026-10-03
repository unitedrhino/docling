// pdf_unicode_recover.go 承接 Unicode 恢复的版面编排层：判断页面是否值得
// 恢复、执行恢复并与既有文本行按乱码率择优。字节级字符映射见 internal/pdfenc。
package docling

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"

	"github.com/unitedrhino/docling/internal/pdfenc"
)

// recoverPDFUnicodeLines 在现有坐标文本乱码明显时尝试第二条字体解码路径。
// 恢复成功时优先用定位 run 重建带坐标的文本行（可参与表格框线还原、
// 字号标题启发式与 XY-cut 版面分析），重建失败再退回无坐标兜底行。
func recoverPDFUnicodeLines(page pdf.Page, pageIdx int64, existing []pdfLine) []pdfLine {
	existingText := joinPDFLineText(existing)
	candidate := pdfenc.HasUnicodeRecoveryCandidate(page)
	if !candidate && strings.TrimSpace(existingText) != "" && textGarbageRatio(existingText) < 0.05 {
		return existing
	}
	recovered, ok := pdfenc.ExtractUnicodeText(page)
	if !ok || !pdfUnicodeRecoveryBetter(existingText, recovered, candidate) {
		return existing
	}
	if lines := assemblePDFUnicodeRunLines(page, pageIdx); len(lines) > 0 {
		return lines
	}
	var lines []pdfLine
	for _, text := range strings.Split(strings.ReplaceAll(recovered, "\r\n", "\n"), "\n") {
		text = normalizePDFExtractText(text)
		if text == "" {
			continue
		}
		lines = append(lines, pdfLine{Text: text, PageIdx: pageIdx, Fallback: true})
	}
	if len(lines) == 0 {
		return existing
	}
	return lines
}

// assemblePDFUnicodeRunLines 把恢复路径的定位 run 组装为坐标文本行：
// run 视为字符级条目复用 assemblePDFLines 的容差分行与词聚合；全部 run
// 缺有效坐标（畸形文本矩阵）时返回 nil，避免全零 bbox 混入版面计算。
func assemblePDFUnicodeRunLines(page pdf.Page, pageIdx int64) []pdfLine {
	runs, ok := pdfenc.ExtractUnicodeTextRuns(page)
	if !ok || len(runs) == 0 {
		return nil
	}
	chars := make([]pdf.Text, 0, len(runs))
	for _, run := range runs {
		text := strings.TrimSpace(run.Text)
		if text == "" {
			continue
		}
		chars = append(chars, pdf.Text{
			S:        text,
			X:        run.X,
			Y:        run.Y,
			W:        run.WidthEstimate,
			FontSize: run.FontSize,
		})
	}
	if len(chars) == 0 {
		return nil
	}
	lines := assemblePDFLines(chars, pageIdx)
	for _, line := range lines {
		if line.MinX != 0 || line.MaxX != 0 || line.MinY != 0 || line.MaxY != 0 {
			return lines
		}
	}
	return nil
}

// pdfUnicodeRecoveryBetter 只接受有足够正文且乱码率严格下降的恢复结果。
func pdfUnicodeRecoveryBetter(existing, recovered string, authoritative bool) bool {
	existing = strings.TrimSpace(sanitizeText(existing))
	recovered = strings.TrimSpace(sanitizeText(recovered))
	if recovered == "" || utf8.RuneCountInString(recovered) < 2 {
		return false
	}
	if recovered == existing {
		return false
	}
	existingRatio, recoveredRatio := textGarbageRatio(existing), textGarbageRatio(recovered)
	if existing == "" {
		return recoveredRatio < 0.2
	}
	if authoritative && recoveredRatio < 0.05 && pdfUnicodePrintableRatio(recovered) >= 0.8 &&
		utf8.RuneCountInString(recovered) >= utf8.RuneCountInString(existing) {
		return true
	}
	return existingRatio >= 0.05 && recoveredRatio+0.02 < existingRatio
}

// pdfUnicodePrintableRatio 统计可检索字符占比，用于拒绝错误字符集产生的控制串。
func pdfUnicodePrintableRatio(text string) float64 {
	total, printable := 0, 0
	for _, char := range text {
		if unicode.IsSpace(char) {
			continue
		}
		total++
		if !unicode.IsControl(char) && char != unicode.ReplacementChar && !pdfenc.IsUnicodePrivateUse(char) {
			printable++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(printable) / float64(total)
}
