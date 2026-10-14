# examples — 样例与转换输出对照集

每个子目录对应一种受支持格式，包含两个文件：

| 文件 | 说明 |
|------|------|
| `sample.<ext>` | 最小典型样例源文件（可下载后直接投喂解析器） |
| `sample.expected.md` | `ParseByExtToMarkdown` 对该样例的完整期望输出 |

并排对照两个文件，即可直观了解该格式的转换效果。

## 样例类型

- `sample.<ext>`：最小典型样例，展示基础结构映射；
- `rich.docx` / `rich.pptx` / `rich-chart.xlsx`：复杂对象样例 —— SmartArt 流程图、
  艺术字、OLE 嵌入对象、OMML 公式（LaTeX 输出）、原生图表（数据表格 + SVG 语义预览）、
  母版继承与组合形状。全部为代码构造的完整合法 OOXML 包，可直接用 Office/LibreOffice
  打开与 `*.expected.md` 并排对照。
- 真实世界样本（源文件不参与 `-update` 重建，仅刷新期望输出）：
  - `doc/sample.doc`、`doc/simple-table.doc`、`xls/sample.xls`、`xls/DateFormats.xls`、`epub/sample.epub`、`chm/sample.chm`：在线取得的旧版 Office、EPUB 与 CHM 文件；覆盖常见段落、DOC 表格文本、XLS 百分比和日期格式，以及章节和主题页。
  - `pdf/design-patterns-thesis.pdf`（196 页硕士论文）、`pdf/gohotdraw-paper.pdf`
    （10 页双栏会议论文）——真实学术 PDF，覆盖长文档、双栏阅读顺序与真实字体编码；
  - `docx/real-charts.docx`、`pptx/real-3dpie.pptx`、`xlsx/real-ole.xlsx`——微软
    Open XML SDK 官方测试资产（MIT），真实 Office 生成的原生图表与 OLE 对象；
  - `eml/real-bounce.eml`——CPython 标准库的真实 MIME 多段邮件；
  - `txt/tom-sawyer.txt`——《汤姆·索亚历险记》全书（380KB 公版书）；
  - `csv/x86-instructions.csv`——x86 指令集描述表（3700+ 行）；
  - `markdown/goldmark-readme.md`、`adoc/lzip-changelog.adoc`、`html/go-homepage.html`
    ——真实开源项目文档与 Go 官方包文档页。

## 回归测试

```bash
go test ./... -run TestExamplesGolden            # 严格逐字对比（CI/日常回归）
go test ./... -run TestExamplesGolden -update    # 解析行为变化后重建样例与期望输出
```

- 样例源文件由 [`examples_test.go`](../examples_test.go) 中的构造器生成（二进制格式
  pdf/docx/xlsx/pptx 由代码构造，保证可再生产），`-update` 会同时重建样例与期望输出。
- 常规模式下测试读取磁盘上的 `sample.<ext>` 解析后与 `sample.expected.md` 逐字对比；
  解析器输出发生任何变化都会在此暴露。
- DOC、XLS、EPUB、CHM 的源文件是外部真实样本，`-update` 只刷新对应的 `*.expected.md`，不会重建或覆盖源文件。

## 新增真实样本来源

| 文件 | 来源与固定版本 | SHA-256 | 许可依据 |
|------|----------------|---------|----------|
| `doc/sample.doc` | [Apache POI `Bug49820.doc`](https://github.com/apache/poi/blob/18d4128d956f9436bfe06c10cfdd457fca6a61de/test-data/document/Bug49820.doc) | `7f9900a5689f1368f5dad718a433dd64b2ec57a47d4c1d9ca3ffc8771ceca040` | Apache POI 仓库 `legal/LICENSE`（Apache-2.0） |
| `doc/simple-table.doc` | [Apache POI `simple-table.doc`](https://github.com/apache/poi/blob/18d4128d956f9436bfe06c10cfdd457fca6a61de/test-data/document/simple-table.doc) | `67d7fe8e0555c16516354498a5935f24eeef90beb5efbf526f93a1d6c734da43` | Apache POI 仓库 `legal/LICENSE`（Apache-2.0） |
| `xls/sample.xls` | [Apache POI `15375.xls`](https://github.com/apache/poi/blob/18d4128d956f9436bfe06c10cfdd457fca6a61de/test-data/spreadsheet/15375.xls) | `fe3cf3887dd99024231ff103f221f6bb0cc4c5cc65cf46959dd1fcb73e12b4d5` | 同上 |
| `xls/DateFormats.xls` | [Apache POI `DateFormats.xls`](https://github.com/apache/poi/blob/18d4128d956f9436bfe06c10cfdd457fca6a61de/test-data/spreadsheet/DateFormats.xls) | `f3ac7e6f0f23e638cfeb0d34543e18f3b07cd93edd1e7a71be907c73a767310c` | 同上 |
| `epub/sample.epub` | [W3C EPUB 测试 `nav-spine_in-spine.epub`](https://w3c.github.io/epub-tests/tests/nav-spine_in-spine.epub)，源仓版本 `54092b4233253e9aac80e93ec4782b380b4b3403` | `1a0adf78e0a79fe9079a4c8f70ac239e5f98fb2ff7ba994eec3bea0e70c569da` | [W3C Software and Document License](https://github.com/w3c/epub-tests/blob/main/LICENSE.md) |
| `chm/sample.chm` | [pylibmspack `sample.chm`](https://github.com/bwhitn/pylibmspack/blob/3cf1091492467ec100f0435e786eb29136ffc080/tests/fixtures/sample.chm) | `3ab384cdc93ff8fab8eb518f39af50d0cf1d5784a31d05e31a6328a72cf435e7` | NSIS 文档，zlib/libpng 许可，见来源仓 README |
| `../testdata/legacy/biff5.xls` | [Apache POI `testEXCEL_95.xls`](https://github.com/apache/poi/blob/18d4128d956f9436bfe06c10cfdd457fca6a61de/test-data/spreadsheet/testEXCEL_95.xls) | `ac0ce3a2eff43e32e56d6b789df6aaa1645ae846d22fa3a2fe3057391d0f1cae` | Apache POI 仓库 `legal/LICENSE`（Apache-2.0） |
| `../testdata/legacy/encrypted.doc` | [Apache POI `PasswordProtected.doc`](https://github.com/apache/poi/blob/18d4128d956f9436bfe06c10cfdd457fca6a61de/test-data/document/PasswordProtected.doc) | `863d2216a6f5bb4cec0ea4131a05ae0822dbce347e2940155ef638c6d64935fe` | Apache POI 仓库 `legal/LICENSE`（Apache-2.0） |

## 新增格式样例

1. 在 `examples_test.go` 的 `examplesSources()` 中追加一行（目录名、源文件名、构造器），
   代码构造的合成样例同时登记到 `syntheticExamplesFor`（真实样本不需要）；
2. 实现对应的构造函数；
3. 运行 `-update` 生成对照文件，检查 `sample.expected.md` 内容符合预期后一并提交。

图片格式（PNG/JPEG/BMP/WEBP）未收录：其 Markdown 输出依赖外部 OCR/视觉钩子，
无法给出确定性期望输出，见主 [README](../README.md) 的钩子章节。
