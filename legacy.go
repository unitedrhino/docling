// 本文件把旧版 Office 文档、电子书与帮助文件转换为统一的 DoclingDocument。
package docling

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"

	word "github.com/bytexlee/doc"
	"github.com/nkiri/xls"
	"github.com/raitucarp/epub"
	"github.com/richardlehane/mscfb"
	"github.com/unitedrhino/docling/internal/chm"
	"golang.org/x/net/html/charset"
)

// ParseDOC 提取二进制 Word 文档中的段落文字；不推测原文件的版式和表格。
func ParseDOC(data []byte) (*DoclingDocument, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("docling: DOC 文件为空")
	}
	reader, err := word.ParseDoc(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("docling: 解析 DOC 失败: %w", err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("docling: 读取 DOC 正文失败: %w", err)
	}
	doc := NewDoclingDocument("doc")
	for _, line := range strings.FieldsFunc(string(decoded), func(r rune) bool { return r == '\r' || r == '\n' || r == '\f' }) {
		if line = strings.TrimSpace(line); line != "" {
			doc.AddText(LabelParagraph, line, nil, nil)
		}
	}
	return doc, nil
}

// ParseXLS 读取 BIFF8 工作簿的工作表和缓存单元格值；不重新计算公式。
func ParseXLS(data []byte) (*DoclingDocument, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("docling: XLS 文件为空")
	}
	if err := validateBIFF8(data); err != nil {
		return nil, err
	}
	wb, err := xls.Read(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("docling: 解析 BIFF8 XLS 失败: %w", err)
	}
	doc := NewDoclingDocument("xls")
	for i, sheet := range wb.Sheets {
		if sheet == nil {
			continue
		}
		name := sheet.Name()
		group := doc.AddSectionGroup(name, nil)
		doc.Groups[group.Idx].Label = GroupLabelSheet
		maxRow, maxCol := 0, 0
		for row := 0; row < sheet.RowCount(); row++ {
			current := sheet.Row(row)
			if current == nil {
				continue
			}
			for col := 0; col < current.CellCount(); col++ {
				if strings.TrimSpace(legacyXLSCellValue(current.Cell(col))) != "" {
					if row+1 > maxRow {
						maxRow = row + 1
					}
					if col+1 > maxCol {
						maxCol = col + 1
					}
				}
			}
		}
		if maxRow > 0 && maxCol > 0 {
			if maxRow*maxCol > 100000 {
				return nil, fmt.Errorf("docling: XLS 工作表 %s 超出 100000 单元格解析上限", name)
			}
			cells := make([]DoclingTableCell, 0, maxRow*maxCol)
			for row := 0; row < maxRow; row++ {
				for col := 0; col < maxCol; col++ {
					value := legacyXLSCellValue(sheet.Row(row).Cell(col))
					cells = append(cells, DoclingTableCell{RowSpan: 1, ColSpan: 1, StartRowOffsetIdx: int64(row), EndRowOffsetIdx: int64(row + 1), StartColOffsetIdx: int64(col), EndColOffsetIdx: int64(col + 1), Text: value, ColumnHeader: row == 0})
				}
			}
			doc.AddTable(cells, int64(maxRow), int64(maxCol), nil, &group)
		}
		doc.AddPage(int64(i+1), float64(maxCol), float64(maxRow))
	}
	return doc, nil
}

// legacyXLSCellValue 输出单元格缓存值，并按 Excel 的 15 位有效数字去除浮点噪声。
func legacyXLSCellValue(cell *xls.Cell) string {
	if cell == nil {
		return ""
	}
	if cell.Type == xls.CellTypeNumber || cell.Type == xls.CellTypeFormula {
		if value := cell.Float(); value != 0 || cell.Value() == "0" {
			return strconv.FormatFloat(value, 'g', 15, 64)
		}
	}
	return cell.Value()
}

// validateBIFF8 核对 Workbook 流的 BOF 版本，并拒绝加密或更早的 Excel 文件。
func validateBIFF8(data []byte) error {
	container, err := mscfb.New(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("docling: XLS OLE 容器无效: %w", err)
	}
	var stream *mscfb.File
	for _, entry := range container.File {
		if entry.Name == "Workbook" {
			stream = entry
			break
		}
	}
	if stream == nil {
		return fmt.Errorf("docling: 仅支持 Excel 97–2003 BIFF8 XLS")
	}
	var header [8]byte
	if _, err := stream.ReadAt(header[:], 0); err != nil {
		return fmt.Errorf("docling: XLS Workbook 流不完整: %w", err)
	}
	if binary.LittleEndian.Uint16(header[:2]) != 0x0809 || binary.LittleEndian.Uint16(header[4:6]) != 0x0600 {
		return fmt.Errorf("docling: 仅支持 Excel 97–2003 BIFF8 XLS")
	}
	// FILEPASS 出现在工作簿全局记录内；加密内容不交给普通 BIFF8 解析器。
	for offset := int64(0); offset+4 <= stream.Size && offset < 1<<20; {
		var record [4]byte
		if _, err := stream.ReadAt(record[:], offset); err != nil {
			return fmt.Errorf("docling: XLS 记录头不完整: %w", err)
		}
		kind := binary.LittleEndian.Uint16(record[:2])
		size := int64(binary.LittleEndian.Uint16(record[2:]))
		if kind == 0x002f {
			return fmt.Errorf("docling: 不支持加密 XLS")
		}
		if kind == 0x000a {
			break
		}
		offset += 4 + size
		if offset > stream.Size {
			return fmt.Errorf("docling: XLS 记录越界")
		}
	}
	return nil
}

// ParseEPUB 按 EPUB spine 顺序提取 XHTML 章节及其标题和正文。
func ParseEPUB(data []byte) (*DoclingDocument, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("docling: EPUB 文件为空")
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("docling: EPUB ZIP 容器无效: %w", err)
	}
	var expanded uint64
	for _, file := range archive.File {
		expanded += file.UncompressedSize64
		if expanded > 64<<20 {
			return nil, fmt.Errorf("docling: EPUB 解压内容超过 64 MiB")
		}
	}
	reader, err := epub.NewReader(data)
	if err != nil {
		return nil, fmt.Errorf("docling: 解析 EPUB 失败: %w", err)
	}
	var sections []string
	for _, item := range reader.Spine() {
		if !strings.Contains(item.MIMEType, "html") || len(item.Content) == 0 {
			continue
		}
		section, parseErr := ParseHTML(item.Content)
		if parseErr != nil {
			return nil, fmt.Errorf("docling: 解析 EPUB 章节 %s 失败: %w", item.Href, parseErr)
		}
		if markdown := strings.TrimSpace(section.ToMarkdown()); markdown != "" {
			sections = append(sections, markdown)
		}
	}
	doc, err := ParseMarkdown([]byte(strings.Join(sections, "\n\n")))
	if err == nil {
		doc.Name = "epub"
	}
	return doc, err
}

// ParseCHM 按帮助文件主题目录提取 HTML 正文；缺少目录时按归档顺序读取主题页。
func ParseCHM(data []byte) (*DoclingDocument, error) {
	archive, err := chm.Open(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("docling: 解析 CHM 失败: %w", err)
	}
	paths := archive.Paths()
	ordered := make([]string, 0, len(paths))
	for _, name := range paths {
		if strings.HasSuffix(strings.ToLower(name), ".hhc") {
			contents, readErr := archive.ReadFile(name)
			if readErr == nil {
				ordered = append(ordered, chmTOCPaths(contents)...)
			}
		}
	}
	seen := make(map[string]bool)
	for _, name := range paths {
		if isCHMHTML(name) {
			ordered = append(ordered, name)
		}
	}
	sections := make([]string, 0, len(ordered))
	totalBytes := 0
	for _, name := range ordered {
		name = strings.TrimPrefix(path.Clean("/"+name), "/")
		if seen[strings.ToLower(name)] || !isCHMHTML(name) {
			continue
		}
		seen[strings.ToLower(name)] = true
		if len(seen) > 4096 {
			return nil, fmt.Errorf("docling: CHM 主题页超过 4096 页")
		}
		contents, readErr := archive.ReadFile(name)
		if readErr != nil {
			continue
		}
		totalBytes += len(contents)
		if totalBytes > 64<<20 {
			return nil, fmt.Errorf("docling: CHM 解压后的主题页超过 64 MiB")
		}
		decoded, decodeErr := charset.NewReader(bytes.NewReader(contents), "text/html")
		if decodeErr != nil {
			continue
		}
		buf := new(bytes.Buffer)
		if _, decodeErr = buf.ReadFrom(decoded); decodeErr != nil {
			continue
		}
		section, parseErr := ParseHTML(buf.Bytes())
		if parseErr != nil {
			continue
		}
		if markdown := strings.TrimSpace(section.ToMarkdown()); markdown != "" {
			sections = append(sections, markdown)
		}
	}
	doc, err := ParseMarkdown([]byte(trimCHMTrailingSpaces(strings.Join(sections, "\n\n"))))
	if err == nil {
		doc.Name = "chm"
	}
	return doc, err
}

// trimCHMTrailingSpaces 去掉帮助文件源码中不影响阅读的行尾空白。
func trimCHMTrailingSpaces(value string) string {
	lines := strings.Split(value, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return strings.Join(lines, "\n")
}

// isCHMHTML 判断条目是否为可检索的主题 HTML 页面。
func isCHMHTML(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".htm", ".html", ".xhtml", ".shtml":
		return true
	}
	return false
}

// chmTOCPaths 按 HHC 中 Local 参数的出现顺序读取主题路径。
func chmTOCPaths(data []byte) []string {
	// CHM 的 HHC 是宽松 HTML，提取 Local 参数无需依赖导航节点的嵌套形状。
	root, err := charset.NewReader(bytes.NewReader(data), "text/html")
	if err != nil {
		return nil
	}
	buf := new(bytes.Buffer)
	if _, err = buf.ReadFrom(root); err != nil {
		return nil
	}
	lower := strings.ToLower(buf.String())
	var result []string
	for pos := 0; pos < len(lower); {
		start := strings.Index(lower[pos:], "<param")
		if start < 0 {
			break
		}
		start += pos
		end := strings.IndexByte(lower[start:], '>')
		if end < 0 {
			break
		}
		end += start
		tag := buf.String()[start:end]
		if strings.Contains(strings.ToLower(tag), "local") {
			for _, quote := range []byte{'"', '\''} {
				key := "value=" + string(quote)
				if at := strings.Index(strings.ToLower(tag), key); at >= 0 {
					value := tag[at+len(key):]
					if stop := strings.IndexByte(value, quote); stop >= 0 {
						result = append(result, strings.SplitN(value[:stop], "#", 2)[0])
					}
					break
				}
			}
		}
		pos = end + 1
	}
	return result
}
