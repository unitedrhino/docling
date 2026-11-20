// pdf_poppler.go 实现 PDF 文本提取的 poppler 降级后端：
// ledongthuc/pdf 对部分中文字体（方正等自定义 CMap）解析失败（整页空或
// 乱码），而 poppler 的 pdftotext 是 Linux 生态最成熟的提取器，中文支持
// 完善。降级链路：ledongthuc（字符级精确字号/坐标）→ 提取为空或乱码超限
// → pdftotext -bbox-layout（TOPLEFT 词盒转 BOTTOMLEFT，行高近似字号）
// → 外部 OCR 钩子。
// pdftotext 为可选外部依赖（PATH 检测，缺失时降级链自动禁用）。
package docling

import (
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/net/html"
)

// popplerFallbackGarbageThreshold 乱码率超过该值时尝试 poppler 降级提取
// （与 LLM OCR 钩子的阈值语义独立，此处只比较 ledongthuc 与 poppler 谁更干净）。
const popplerFallbackGarbageThreshold = 0.15

var (
	popplerOnce     sync.Once
	popplerExecPath string
)

// popplerAvailable 检测 pdftotext 是否可用（进程内缓存检测结果）。
func popplerAvailable() bool {
	popplerOnce.Do(func() {
		path, err := exec.LookPath("pdftotext")
		if err == nil {
			popplerExecPath = path
		}
	})
	return popplerExecPath != ""
}

// extractPDFLinesByPoppler 用 pdftotext -bbox-layout 一次性提取全文档：
// 返回 页号(0起) → 行序列（行内词按 X 坐标排序拼接，词间水平间隙足够时补
// 空格保英文分词；行高 yMax-yMin 近似字号供标题启发式；行盒坐标填入
// MinX/MaxX/MinY/MaxY；pdftotext 的 TOPLEFT Y 坐标使用页面高度转换为
// 与字符后端一致的 BOTTOMLEFT 语义）。
// 整文档一次 exec 是性能关键——按页调用会产生数百次进程/临时文件开销。
// 返回 false 表示 pdftotext 不可用、执行失败或产出为空。
func extractPDFLinesByPoppler(data []byte) (map[int64][]pdfLine, bool) {
	if !popplerAvailable() {
		return nil, false
	}
	tmp, err := os.CreateTemp("", "docling-poppler-*.pdf")
	if err != nil {
		return nil, false
	}
	tmpPath := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(data); err != nil {
		return nil, false
	}
	if err := tmp.Close(); err != nil {
		return nil, false
	}
	cmd := exec.Command(popplerExecPath, "-bbox-layout", tmpPath, "-")
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return nil, false
	}
	return parsePopplerPages(out)
}

// parsePopplerPages 解析 pdftotext 的 XHTML 输出，并按页面高度把 TOPLEFT
// 行盒和词盒转换为 BOTTOMLEFT；解析失败或没有有效文本行时返回 false。
func parsePopplerPages(data []byte) (map[int64][]pdfLine, bool) {
	// 解析 XHTML：用 golang.org/x/net/html（对 DOCTYPE/命名空间/实体完全
	// 容错；encoding/xml 对 pdftotext 的输出存在无声匹配失败）
	root, err := html.Parse(strings.NewReader(string(data)))
	if err != nil {
		return nil, false
	}
	pages := make(map[int64][]pdfLine)
	pageNo := int64(-1)
	pageHeight := float64(0)
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "page":
				pageNo++
				pageHeight, _ = popplerAttrF(n, "height")
			case "line":
				if pageNo >= 0 {
					ln := popplerLineFromNode(n)
					line, ok := popplerPDFLine(ln, pageNo, pageHeight)
					if ok {
						pages[pageNo] = append(pages[pageNo], line)
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	if len(pages) == 0 {
		return nil, false
	}
	return pages, true
}

// popplerLineXML 行盒坐标 + 行内词（从 XHTML 节点提取）。
type popplerLineXML struct {
	XMin  float64
	YMin  float64
	XMax  float64
	YMax  float64
	Words []popplerWord
}

// popplerWord 单词：bbox + 字面文本。
type popplerWord struct {
	XMin float64
	YMin float64
	XMax float64
	YMax float64
	Text string
}

// popplerAttrF 取节点浮点属性（缺失或非法返回 false）。
func popplerAttrF(n *html.Node, key string) (float64, bool) {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			v, err := strconv.ParseFloat(strings.TrimSpace(a.Val), 64)
			return v, err == nil
		}
	}
	return 0, false
}

// popplerLineFromNode 从 <line> 节点提取行盒与 <word> 子节点。
func popplerLineFromNode(n *html.Node) popplerLineXML {
	ln := popplerLineXML{}
	ln.XMin, _ = popplerAttrF(n, "xMin")
	ln.YMin, _ = popplerAttrF(n, "yMin")
	ln.XMax, _ = popplerAttrF(n, "xMax")
	ln.YMax, _ = popplerAttrF(n, "yMax")
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode || c.Data != "word" {
			continue
		}
		w := popplerWord{}
		w.XMin, _ = popplerAttrF(c, "xMin")
		w.YMin, _ = popplerAttrF(c, "yMin")
		w.XMax, _ = popplerAttrF(c, "xMax")
		w.YMax, _ = popplerAttrF(c, "yMax")
		var sb strings.Builder
		for t := c.FirstChild; t != nil; t = t.NextSibling {
			if t.Type == html.TextNode {
				sb.WriteString(t.Data)
			}
		}
		w.Text = sb.String()
		ln.Words = append(ln.Words, w)
	}
	return ln
}

// sortedPopplerWords 返回按 X 坐标排序的 Poppler 词副本，不修改 XML 解析结果。
func sortedPopplerWords(ln popplerLineXML) []popplerWord {
	words := make([]popplerWord, len(ln.Words))
	copy(words, ln.Words)
	sort.SliceStable(words, func(i, j int) bool { return words[i].XMin < words[j].XMin })
	return words
}

// popplerPDFWords 把 Poppler 行内词按 X 坐标排序后映射为统一词模型，
// 使用页面高度把 TOPLEFT 词盒转换为 BOTTOMLEFT，并以词盒高度近似字号。
func popplerPDFWords(ln popplerLineXML, pageHeight float64) []pdfWord {
	words := sortedPopplerWords(ln)
	result := make([]pdfWord, 0, len(words))
	for _, word := range words {
		result = append(result, pdfWord{
			Text:     sanitizeText(word.Text),
			MinX:     word.XMin,
			MaxX:     word.XMax,
			MinY:     pageHeight - word.YMax,
			MaxY:     pageHeight - word.YMin,
			FontSize: word.YMax - word.YMin,
		})
	}
	return result
}

// popplerPDFLine 把 Poppler 的 TOPLEFT 行盒转换为统一 BOTTOMLEFT pdfLine；
// 页面高度无效、无词或文本为空时返回 false，避免把未转换坐标混入后续版面计算。
func popplerPDFLine(ln popplerLineXML, pageIdx int64, pageHeight float64) (pdfLine, bool) {
	text, ok := joinPopplerWords(ln)
	if !ok || strings.TrimSpace(text) == "" || pageHeight <= 0 {
		return pdfLine{}, false
	}
	return pdfLine{
		Text:        sanitizeText(text),
		Words:       popplerPDFWords(ln, pageHeight),
		MaxFontSize: ln.YMax - ln.YMin,
		PageIdx:     pageIdx,
		MinX:        ln.XMin,
		MaxX:        ln.XMax,
		MinY:        pageHeight - ln.YMax,
		MaxY:        pageHeight - ln.YMin,
	}, true
}

// joinPopplerWords 把行内词按 X 坐标排序拼接：相邻词水平间隙超过词高 25%
// 时补空格（保留英文词间空格；中文逐字词天然连排）。
func joinPopplerWords(ln popplerLineXML) (string, bool) {
	if len(ln.Words) == 0 {
		return "", false
	}
	words := sortedPopplerWords(ln)
	var b strings.Builder
	gapThreshold := (ln.YMax - ln.YMin) * 0.25
	prevRight := float64(0)
	for i, w := range words {
		if i > 0 && w.XMin-prevRight > gapThreshold {
			b.WriteString(" ")
		}
		b.WriteString(w.Text)
		if w.XMax > prevRight {
			prevRight = w.XMax
		}
	}
	return b.String(), true
}

// popplerBackend 单文档的 poppler 降级提取器：惰性整文档提取 + 按页缓存
// （同一文档多页触发降级时只执行一次 pdftotext）。
type popplerBackend struct {
	data  []byte
	once  sync.Once
	pages map[int64][]pdfLine
	ok    bool
}

// linesFor 返回指定页（0 起）的 poppler 降级行；未提取过时执行整文档提取。
func (p *popplerBackend) linesFor(pageIdx int64) []pdfLine {
	p.once.Do(func() {
		p.pages, p.ok = extractPDFLinesByPoppler(p.data)
	})
	if !p.ok {
		return nil
	}
	return p.pages[pageIdx]
}

// tryPopplerFallback 返回 Poppler 的单页坐标行。少量坏字仅在原页
// 缺少坐标时尝试恢复，且备用结果须无坏字并保留可读字符数。
// backend 提供文档级缓存（多页降级只执行一次 pdftotext）。
func tryPopplerFallback(backend *popplerBackend, pageIdx int64, original []pdfLine) []pdfLine {
	pl := backend.linesFor(pageIdx)
	if len(pl) == 0 {
		return nil
	}
	if len(original) == 0 {
		return pl
	}
	sourceText, popplerText := joinPDFLineText(original), joinPDFLineText(pl)
	sourceRatio, popplerRatio := textGarbageRatio(sourceText), textGarbageRatio(popplerText)
	if popplerRatio >= sourceRatio {
		return nil
	}
	if sourceRatio > popplerFallbackGarbageThreshold ||
		(original[0].Fallback && popplerRatio == 0 &&
			pdfUnicodeReadableCount(pdfUnicodeNonSpace(popplerText), popplerRatio) >=
				pdfUnicodeReadableCount(pdfUnicodeNonSpace(sourceText), sourceRatio)) {
		return pl
	}
	return nil
}
