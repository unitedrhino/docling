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
