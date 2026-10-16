// 本文件使用公开样本验证旧版文档和电子书的解析入口及失败边界。
package docling

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLegacyFormats 验证四种格式均提供可检索文本、统一来源和内容列表。
func TestLegacyFormats(t *testing.T) {
	cases := []struct {
		ext, want, mime string
	}{
		{"doc", "Heading 1", "application/msword"},
		{"xls", "Maryland", "application/vnd.ms-excel"},
		{"epub", "Hello", "application/epub+zip"},
		{"chm", "NSIS Users Manual", "application/vnd.ms-htmlhelp"},
	}
	for _, tc := range cases {
		t.Run(tc.ext, func(t *testing.T) {
			name := "sample." + tc.ext
			data, err := os.ReadFile(filepath.Join("examples", tc.ext, name))
			if err != nil {
				t.Fatal(err)
			}
			doc, err := ParseByExt(name, data)
			if err != nil {
				t.Fatal(err)
			}
			if doc.Origin == nil || doc.Origin.Mimetype != tc.mime {
				t.Fatalf("origin MIME: %+v", doc.Origin)
			}
			if !strings.Contains(doc.Text(), tc.want) {
				t.Fatalf("missing %q in document text", tc.want)
			}
			if len(ToContentList(doc, SourceGolight)) == 0 {
				t.Fatal("empty content_list")
			}
		})
	}
}

// TestLegacyXLSFormatting 验证内置百分比样式和日期格式进入可读单元格值。
func TestLegacyXLSFormatting(t *testing.T) {
	cases := []struct {
		name string
		want []string
	}{
		{"sample.xls", []string{"5%", "6%", "4%"}},
		{"DateFormats.xls", []string{"2007-08-10"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("examples/xls", tc.name))
			if err != nil {
				t.Fatal(err)
			}
			doc, err := ParseXLS(data)
			if err != nil {
				t.Fatal(err)
			}
			markdown := doc.ToMarkdown()
			for _, want := range tc.want {
				if !strings.Contains(markdown, want) {
					t.Errorf("formatted XLS output missing %q", want)
				}
			}
			if tc.name == "DateFormats.xls" && strings.Contains(markdown, "39304") {
				t.Fatal("date serial leaked instead of a readable date")
			}
		})
	}
}

// TestLegacyDOCExtractsTableText 验证旧版 DOC 表格单元格文字虽不保留网格结构但不会丢失。
func TestLegacyDOCExtractsTableText(t *testing.T) {
	data, err := os.ReadFile("examples/doc/simple-table.doc")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ParseDOC(data)
	if err != nil {
		t.Fatal(err)
	}
	text := doc.Text()
	for _, want := range []string{"Cell 1,1", "Cell 1,2", "Cell 1,3", "Cell 2,1", "Cell 2,2", "Cell 2,3", "This text is below the table."} {
		if !strings.Contains(text, want) {
			t.Errorf("DOC text missing table or trailing paragraph content %q", want)
		}
	}
}

// TestLegacyMalformedAndOldXLS 验证空文件、损坏文件和 Excel 95 的明确错误。
func TestLegacyMalformedAndOldXLS(t *testing.T) {
	for _, ext := range []string{"doc", "xls", "epub", "chm"} {
		t.Run(ext, func(t *testing.T) {
			if _, err := ParseByExt("empty."+ext, nil); err == nil {
				t.Fatal("empty file accepted")
			}
			if _, err := ParseByExt("bad."+ext, []byte("invalid data")); err == nil {
				t.Fatal("malformed file accepted")
			}
		})
	}
	data, err := os.ReadFile("testdata/legacy/biff5.xls")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseXLS(data); err == nil || !strings.Contains(err.Error(), "BIFF8") {
		t.Fatalf("Excel 95 should be rejected as unsupported: %v", err)
	}
	encrypted, err := os.ReadFile("testdata/legacy/encrypted.doc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseDOC(encrypted); err == nil || !strings.Contains(err.Error(), "加密 DOC") {
		t.Fatalf("encrypted Word document should have a clear error: %v", err)
	}
}
