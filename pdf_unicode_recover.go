// pdf_unicode_recover.go 承接 Unicode 恢复的版面编排层：判断页面是否值得
// 恢复、执行恢复并与既有文本行按乱码率择优。字节级字符映射见 internal/pdfenc。
package docling

import (
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"

	"github.com/unitedrhino/docling/internal/pdfenc"
)

// recoverPDFUnicodeLines 用字体解码结果修复单页文本；字符序列可核对时保留
// chars 中的原始坐标，否则按可读文本覆盖与乱码率决定是否采用无坐标结果。
func recoverPDFUnicodeLines(page pdf.Page, pageIdx int64, existing []pdfLine, chars []pdf.Text) []pdfLine {
	existingText := joinPDFLineText(existing)
	candidate := pdfenc.HasUnicodeRecoveryCandidate(page)
	if !candidate && strings.TrimSpace(existingText) != "" && textGarbageRatio(existingText) < 0.05 {
		return existing
	}
	recovered, ok := pdfenc.ExtractUnicodeText(page)
	if !ok {
		return existing
	}
	if repaired := removePDFSyntheticReplacements(chars, recovered); repaired != nil {
		return assemblePDFLines(repaired, pageIdx)
	}
	if !pdfUnicodeRecoveryBetter(existingText, recovered, candidate) {
		return existing
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

// removePDFSyntheticReplacements 移除坐标提取器把 TJ 末尾的合成换行
// 当作字体代码产生的无宽度替换符。仅在其余非空白字符按绘制顺序与
// 独立解码完全一致时返回修复后的坐标字符，避免丢掉真实的未解码字形。
func removePDFSyntheticReplacements(chars []pdf.Text, recovered string) []pdf.Text {
	removed := 0
	for _, char := range chars {
		if char.W == 0 && char.S == "\uFFFD" {
			removed++
		}
	}
	if removed == 0 {
		return nil
	}
	var text strings.Builder
	filtered := make([]pdf.Text, 0, len(chars)-removed)
	for _, char := range chars {
		if char.W != 0 || char.S != "\uFFFD" {
			text.WriteString(char.S)
			filtered = append(filtered, char)
		}
	}
	originalText := pdfUnicodeNonSpace(text.String())
	if originalText == "" || originalText != pdfUnicodeNonSpace(recovered) {
		return nil
	}
	return filtered
}

// pdfUnicodeNonSpace 去掉提取器按各自布局生成的空白，以便核对字形内容。
func pdfUnicodeNonSpace(text string) string {
	return strings.Map(func(char rune) rune {
		if unicode.IsSpace(char) {
			return -1
		}
		return char
	}, text)
}

// pdfUnicodeRecoveryBetter 比较乱码率和可读内容覆盖，允许恢复少量坏字，
// 并避免用无坐标解码结果覆盖正常的坐标文本。
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
	if authoritative && recoveredRatio < 0.05 {
		originalGlyphs, recoveredGlyphs := pdfUnicodeNonSpace(existing), pdfUnicodeNonSpace(recovered)
		if existingRatio > 0 && recoveredRatio <= existingRatio/2 &&
			pdfUnicodeReadableCount(recoveredGlyphs, recoveredRatio) >= pdfUnicodeReadableCount(originalGlyphs, existingRatio) {
			return true
		}
		if originalGlyphs != recoveredGlyphs && pdfUnicodePrintableRatio(recovered) >= 0.8 &&
			utf8.RuneCountInString(recoveredGlyphs) >= utf8.RuneCountInString(originalGlyphs) {
			return true
		}
	}
	return existingRatio >= 0.05 && recoveredRatio+0.02 < existingRatio
}

// pdfUnicodeReadableCount 根据非空白文本及其乱码率还原可读字符数。
func pdfUnicodeReadableCount(glyphs string, garbageRatio float64) int {
	return int(math.Round(float64(utf8.RuneCountInString(glyphs)) * (1 - garbageRatio)))
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
