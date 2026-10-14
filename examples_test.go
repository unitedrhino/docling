// examples_test.go 维护 examples/ 样例对照集：每个受支持格式一个子目录，
// 存放 sample.<ext> 样例源文件与 sample.expected.md 转换期望输出。
//
// 常规回归：go test ./... -run TestExamplesGolden
//
//	逐格式读取磁盘样例，调用 ParseByExtToMarkdown 后与期望输出严格逐字对比，
//	任何解析行为变化都会在此暴露。
//
// 重建对照集：go test ./... -run TestExamplesGolden -update
//
//	解析能力演进后，一键重建全部样例源文件与期望输出。
package docling

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

// updateExamples 由 -update 打开：重建样例源文件与期望输出。
var updateExamples = flag.Bool("update", false, "重建 examples/ 样例源文件与期望输出")

// examplesSource 描述一个格式的样例：子目录、源文件名与样例字节构造器。
type examplesSource struct {
	dir      string
	filename string
	build    func(t *testing.T) []byte
}

// examplesSources 返回全部受支持格式的样例构造清单。图片格式不收录：
// 其 Markdown 输出依赖外部 OCR/视觉钩子，无法给出确定性期望输出。
func examplesSources() []examplesSource {
	return []examplesSource{
		{dir: "txt", filename: "sample.txt", build: buildExampleTXT},
		{dir: "markdown", filename: "sample.md", build: buildExampleMarkdown},
		{dir: "csv", filename: "sample.csv", build: buildExampleCSV},
		{dir: "html", filename: "sample.html", build: buildExampleHTML},
		{dir: "adoc", filename: "sample.adoc", build: buildExampleAsciiDoc},
		{dir: "eml", filename: "sample.eml", build: buildExampleEML},
		{dir: "pdf", filename: "sample.pdf", build: buildExamplePDF},
		{dir: "docx", filename: "sample.docx", build: buildExampleDocx},
		{dir: "docx", filename: "rich.docx", build: buildRichExampleDocx},
		{dir: "xlsx", filename: "sample.xlsx", build: buildExampleXLSX},
		{dir: "xlsx", filename: "rich-chart.xlsx", build: buildRichExampleXLSX},
		{dir: "pptx", filename: "sample.pptx", build: buildExamplePPTXGolden},
		{dir: "pptx", filename: "rich.pptx", build: buildRichExamplePPTX},
		{dir: "doc", filename: "sample.doc"},
		{dir: "xls", filename: "sample.xls"},
		{dir: "epub", filename: "sample.epub"},
		{dir: "chm", filename: "sample.chm"},
	}
}

// TestExamplesGolden 对 examples/ 对照集做端到端回归：目录中每个样例源
// 文件（sample.<ext> 及其他真实样本）经 ParseByExtToMarkdown 的输出必须与
// 同名 <name>.expected.md 逐字一致。canonical 的 sample.<ext> 在 -update
// 模式下由代码构造器重建；随后追加的真实样本（如研究论文、业务工作簿）
// 只重建期望输出，源文件保持原样以展示真实文档的转换效果。
func TestExamplesGolden(t *testing.T) {
	for _, source := range examplesSources() {
		t.Run(source.dir, func(t *testing.T) {
			dir := filepath.Join("examples", source.dir)
			entries, err := os.ReadDir(dir)
			if err != nil {
				if os.IsNotExist(err) && *updateExamples {
					if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
						t.Fatalf("create examples dir: %v", mkErr)
					}
					entries = nil
				} else {
					t.Fatalf("read examples dir: %v", err)
				}
			}
			// -update 时确保合成样例源文件存在:canonical sample.* 每次重建,
			// 其余合成样例(rich-*)仅首次生成、之后与真实样本同等对待。
			if *updateExamples {
				path := filepath.Join(dir, source.filename)
				_, statErr := os.Stat(path)
				isCanonical := strings.HasPrefix(source.filename, "sample.")
				if source.build != nil && (isCanonical || os.IsNotExist(statErr)) {
					if err := os.WriteFile(path, source.build(t), 0o644); err != nil {
						t.Fatalf("write sample source: %v", err)
					}
				}
				entries, err = os.ReadDir(dir)
				if err != nil {
					t.Fatalf("re-read examples dir: %v", err)
				}
			}
			sourceExt := filepath.Ext(source.filename)
			for _, entry := range entries {
				name := entry.Name()
				if entry.IsDir() || strings.HasSuffix(name, ".expected.md") ||
					filepath.Ext(name) != sourceExt {
					continue
				}
				expectedPath := filepath.Join(dir, strings.TrimSuffix(name, sourceExt)+".expected.md")
				data, readErr := os.ReadFile(filepath.Join(dir, name))
				if readErr != nil {
					t.Fatalf("读取样例失败: %v", readErr)
				}
				if *updateExamples {
					// canonical sample.* 已在上方重建;此处 data 重新读取以保证
					// 与磁盘内容一致(rich-*/真实样本源文件不变)。
					if refreshed, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
						data = refreshed
					}
					markdown, parseErr := ParseByExtToMarkdown(name, data)
					if parseErr != nil {
						t.Fatalf("解析 %s: %v", name, parseErr)
					}
					if err := os.WriteFile(expectedPath, []byte(markdown), 0o644); err != nil {
						t.Fatalf("write expected markdown: %v", err)
					}
					continue
				}
				markdown, parseErr := ParseByExtToMarkdown(name, data)
				if parseErr != nil {
					t.Fatalf("解析样例 %s: %v", name, parseErr)
				}
				expected, readErr := os.ReadFile(expectedPath)
				if readErr != nil {
					t.Fatalf("读取期望输出失败: %v", readErr)
				}
				if string(markdown) != string(expected) {
					t.Fatalf("%s 的 Markdown 输出与期望不一致，解析器行为可能发生变化；\n确认无误后运行 go test -run TestExamplesGolden -update 刷新对照集。\n--- 首处差异 ---\n%s",
						name, firstLineDiff(string(expected), string(markdown)))
				}
			}
		})
	}
}

// firstLineDiff 返回两段文本的第一处逐行差异，便于定位回归原因。
func firstLineDiff(expected, actual string) string {
	expectedLines := strings.Split(expected, "\n")
	actualLines := strings.Split(actual, "\n")
	for i := 0; i < len(expectedLines) || i < len(actualLines); i++ {
		var want, got string
		switch {
		case i >= len(expectedLines):
			want = "<缺失>"
		case i >= len(actualLines):
			got = "<缺失>"
		default:
			want, got = expectedLines[i], actualLines[i]
		}
		if want != got {
			return fmt.Sprintf("第 %d 行:\n  期望: %q\n  实际: %q", i+1, want, got)
		}
	}
	return "无差异（请检查换行风格）"
}

// buildExampleTXT 构造纯文本样例：无结构标记，逐段落进入 Markdown。
func buildExampleTXT(t *testing.T) []byte {
	t.Helper()
	return []byte(`网关部署说明

本网关支持 Modbus 与 MQTT 两类接入协议，默认监听 1883 端口。
部署前请确认设备时间已通过 NTP 同步，否则证书校验会失败。

升级时先在控制台导出配置快照，再执行在线升级；升级过程约持续两分钟，
期间设备会自动重启一次。
`)
}

// buildExampleMarkdown 构造 Markdown 样例：标题层级、列表与围栏代码块。
func buildExampleMarkdown(t *testing.T) []byte {
	t.Helper()
	return []byte(`# 采集任务配置

定时采集任务按分钟粒度下发，支持断点续传。

## 参数说明

- interval：采集间隔，默认 60 秒
- timeout：单次请求超时
- retry：失败重试次数

## 下发示例

` + "```" + `bash
curl -X POST /api/v1/tasks -d '{"interval": 60}'
` + "```" + `
`)
}

// buildExampleCSV 构造 CSV 样例：表头、数据行与引号内逗号。
func buildExampleCSV(t *testing.T) []byte {
	t.Helper()
	return []byte(`设备编号,设备名称,安装位置,状态
GW-001,边缘网关,配电室A,在线
GW-002,采集终端,"机房,三楼",离线
GW-003,温感探头,锅炉房,在线
`)
}

// buildExampleHTML 构造 HTML 样例：标题、段落、列表与表格。
func buildExampleHTML(t *testing.T) []byte {
	t.Helper()
	return []byte(`<!DOCTYPE html>
<html>
<head><title>运维手册</title></head>
<body>
<h1>运维手册</h1>
<p>本章描述日常巡检项与告警处置流程。</p>
<h2>巡检项</h2>
<ul>
<li>检查磁盘占用率</li>
<li>检查服务健康状态</li>
</ul>
<h2>告警阈值</h2>
<table>
<tr><th>指标</th><th>警告</th><th>严重</th></tr>
<tr><td>CPU 使用率</td><td>80%</td><td>95%</td></tr>
<tr><td>内存使用率</td><td>85%</td><td>97%</td></tr>
</table>
</body>
</html>
`)
}

// buildExampleAsciiDoc 构造 AsciiDoc 样例：文档标题、小节与列表。
func buildExampleAsciiDoc(t *testing.T) []byte {
	t.Helper()
	return []byte(`= 接入指南

本章说明如何将设备接入平台。

== 准备工作

. 获取接入凭证
. 配置接入地址
. 校验网络连通性

接入完成后可在设备列表查看上报数据。
`)
}

// buildExampleEML 构造邮件样例：标准头部与 multipart/alternative 正文。
func buildExampleEML(t *testing.T) []byte {
	t.Helper()
	return []byte(`From: ops@example.com
To: team@example.com
Subject: =?utf-8?B?5beh5qOA5ZGo5oql?=
Date: Mon, 8 Sep 2026 10:00:00 +0800
MIME-Version: 1.0
Content-Type: multipart/alternative; boundary="boundary-42"

--boundary-42
Content-Type: text/plain; charset=utf-8

本周巡检已完成，所有网关运行正常，磁盘占用率低于 60%。

--boundary-42
Content-Type: text/html; charset=utf-8

<html><body><p>本周巡检已完成，所有网关运行正常。</p></body></html>
--boundary-42--
`)
}

// buildExamplePDF 用最小 PDF 1.4 语法构造两页文本型样例：封面标题与
// 参数表格行。受手工构造器限制（标准 Helvetica 字体、无嵌入中文字体），
// 样例使用英文内容；真实中文 PDF 的字体编码恢复由库的 CMap/Unicode 链路
// 处理，见 README 能力说明。
func buildExamplePDF(t *testing.T) []byte {
	t.Helper()
	return buildTestPDF(t, []testPDFPage{
		{content: "BT /F1 18 Tf 72 720 Td (Gateway Product Specification) Tj ET\n" +
			"BT /F1 11 Tf 72 680 Td (This specification covers deployment and acceptance of the edge gateway series.) Tj ET\n" +
			"BT /F1 11 Tf 72 660 Td (Supported protocols: Modbus, MQTT and OPC UA.) Tj ET"},
		{content: "BT /F1 14 Tf 72 720 Td (Network Parameters) Tj ET\n" +
			"BT /F1 11 Tf 72 690 Td (Model            Protocol        Max Points) Tj ET\n" +
			"BT /F1 11 Tf 72 670 Td (GW-100           Modbus          512) Tj ET\n" +
			"BT /F1 11 Tf 72 650 Td (GW-200           MQTT            2048) Tj ET"},
	})
}

// buildExampleDocx 构造 DOCX 样例：标题层级、正文段落与数据表格。
func buildExampleDocx(t *testing.T) []byte {
	t.Helper()
	documentXML := buildStructuredDocxXML(
		`<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>部署指南</w:t></w:r></w:p>` +
			`<w:p><w:r><w:t>本指南描述边缘网关的标准部署流程与验收要求。</w:t></w:r></w:p>` +
			`<w:p><w:pPr><w:pStyle w:val="Heading2"/></w:pPr><w:r><w:t>环境要求</w:t></w:r></w:p>` +
			`<w:tbl><w:tr><w:tc><w:p><w:r><w:t>组件</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>端口</w:t></w:r></w:p></w:tc></w:tr>` +
			`<w:tr><w:tc><w:p><w:r><w:t>接入服务</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>1883</w:t></w:r></w:p></w:tc></w:tr>` +
			`<w:tr><w:tc><w:p><w:r><w:t>管理台</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>443</w:t></w:r></w:p></w:tc></w:tr></w:tbl>`,
	)
	return mustZipDocxParts(t, documentXML, "", "")
}

// buildExampleXLSX 用 excelize 构造工作表样例：表头与库存数据行。
func buildExampleXLSX(t *testing.T) []byte {
	t.Helper()
	book := excelize.NewFile()
	defer func() { _ = book.Close() }()
	if err := book.SetSheetName("Sheet1", "库存"); err != nil {
		t.Fatalf("rename sheet: %v", err)
	}
	for cell, value := range map[string]any{
		"A1": "物料编码", "B1": "物料名称", "C1": "库存数量",
		"A2": "M-1001", "B2": "温湿度传感器", "C2": 120,
		"A3": "M-1002", "B3": "边缘网关", "C3": 35,
		"A4": "M-1003", "B4": "电流互感器", "C4": 480,
	} {
		if err := book.SetCellValue("库存", cell, value); err != nil {
			t.Fatalf("set cell %s: %v", cell, err)
		}
	}
	var buf bytes.Buffer
	if _, err := book.WriteTo(&buf); err != nil {
		t.Fatalf("write xlsx: %v", err)
	}
	return buf.Bytes()
}

// buildExamplePPTXGolden 复用演示文稿测试构造器作为 PPTX 样例来源。
func buildExamplePPTXGolden(t *testing.T) []byte {
	t.Helper()
	return buildTestPPTX(t)
}
