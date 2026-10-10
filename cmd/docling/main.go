// docling CLI：把文档解析为 outline / markdown / content-list / Docling JSON
// 四种形态。设计为通用原语——只做"转换"，查询交给 jq 等通用工具；配合
// --ocr 可选启用大模型 OCR（OpenAI 兼容协议，凭据从环境变量读取）。
//
// 用法：
//
//	docling parse <file|-> [--format outline|md|content-list|json]
//	                   [--section 前缀] [--sheet 名] [--layers body,furniture]
//	                   [--out 文件] [--ocr] [--name stdin文件名]
//	docling formats
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/unitedrhino/docling"
	"github.com/unitedrhino/docling/cli"
	"github.com/unitedrhino/docling/llmocr"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run 是 CLI 主入口，参数与输出注入便于测试。
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	switch args[0] {
	case "parse":
		return runParse(args[1:], stdout, stderr)
	case "formats":
		for _, e := range []string{"pdf", "docx", "pptx", "xlsx", "csv", "html", "md", "adoc", "txt", "eml", "png", "jpg", "jpeg", "bmp", "webp", "dwg", "dxf", "dxfb", "glb"} {
			fmt.Fprintln(stdout, e)
		}
		return 0
	case "-h", "--help", "help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "未知命令 %q\n", args[0])
		printUsage(stderr)
		return 2
	}
}

// parseFlags 收集 parse 子命令的参数。
type parseFlags struct {
	format     string
	section    string
	sheet      string
	layers     string
	out        string
	name       string
	ocr        bool
	ocrModel   string
	ocrBaseURL string
	maxPages   int
}

// runParse 执行解析与格式化输出。
func runParse(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("parse", flag.ContinueOnError)
	fs.SetOutput(stderr)
	opts := &parseFlags{}
	fs.StringVar(&opts.format, "format", "outline", "输出格式: outline|md|content-list|json")
	fs.StringVar(&opts.section, "section", "", "按章节路径前缀过滤(md/content-list)")
	fs.StringVar(&opts.sheet, "sheet", "", "按首级分组名(如工作表)过滤(md/content-list)")
	fs.StringVar(&opts.layers, "layers", "body", "markdown 内容层,逗号分隔: body,furniture,notes,invisible,background")
	fs.StringVar(&opts.out, "out", "", "输出到文件(默认 stdout)")
	fs.StringVar(&opts.name, "name", "", "从 stdin 读取时的文件名(决定解析器)")
	fs.BoolVar(&opts.ocr, "ocr", false, "启用大模型 OCR/结构化视觉(OpenAI 兼容 env)")
	fs.StringVar(&opts.ocrModel, "ocr-model", "", "OCR 模型名(默认 gpt-4o,env DOCLING_OCR_MODEL)")
	fs.StringVar(&opts.ocrBaseURL, "ocr-base-url", "", "OCR API 地址(默认 env OPENAI_BASE_URL)")
	fs.IntVar(&opts.maxPages, "ocr-max-pages", 0, "OCR/视觉最大页数预算(0=不限)")
	// flag 包遇到首个位置参数即停止,先重排:flag 在前、位置参数在后。
	boolFlags := map[string]bool{"--ocr": true, "-ocr": true}
	var flagArgs, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") && arg != "-" {
			flagArgs = append(flagArgs, arg)
			name := strings.SplitN(strings.TrimLeft(arg, "-"), "=", 2)[0]
			if !boolFlags["--"+name] && !strings.Contains(arg, "=") && i+1 < len(args) {
				i++
				flagArgs = append(flagArgs, args[i])
			}
			continue
		}
		positional = append(positional, arg)
	}
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	rest := positional
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "用法: docling parse <file|-> [flags]")
		return 2
	}
	if err := parseAndWrite(rest[0], opts, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "docling: %v\n", err)
		return 1
	}
	return 0
}

// parseAndWrite 读取输入、解析并按格式写出。
func parseAndWrite(source string, opts *parseFlags, stdout, stderr io.Writer) error {
	name, data, err := readSource(source, opts.name)
	if err != nil {
		return err
	}
	parseOpts := docling.ParseOptions{}
	if opts.ocr {
		cfg := llmocr.OpenAIConfig{Model: opts.ocrModel, BaseURL: opts.ocrBaseURL}
		client, err := llmocr.NewOpenAIClient(cfg)
		if err != nil {
			return err
		}
		budget := llmocr.Options{MaxPages: opts.maxPages}
		parseOpts.OCRHook = llmocr.NewOCRHook(client, budget)
		parseOpts.PDFVisualHook = llmocr.NewPDFVisualHook(client, budget)
	}
	doc, err := docling.ParseByExtWithOptions(name, data, parseOpts)
	if err != nil {
		return err
	}

	var output []byte
	switch opts.format {
	case "outline":
		output = []byte(cli.Outline(doc))
	case "md":
		if opts.section == "" && opts.sheet == "" {
			output = []byte(doc.ToMarkdownWithOptions(docling.ExportOptions{Layers: cli.ParseLayers(opts.layers)}))
		} else {
			items := cli.FilterItems(docling.ToContentList(doc, docling.SourceGolight), opts.section, opts.sheet)
			output = []byte(cli.RenderItemsMarkdown(items))
		}
	case "content-list":
		items := docling.ToContentList(doc, docling.SourceGolight)
		if opts.section != "" || opts.sheet != "" {
			items = cli.FilterItems(items, opts.section, opts.sheet)
		}
		output, err = marshalIndent(items)
		if err != nil {
			return err
		}
	case "json":
		// json 始终输出完整无损文档；--section/--sheet 不生效,用 jq 钻取。
		if opts.section != "" || opts.sheet != "" {
			fmt.Fprintln(stderr, "提示: json 格式不受 --section/--sheet 影响,请用 jq 查询")
		}
		output, err = marshalIndent(doc)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("未知格式 %q(可选 outline|md|content-list|json)", opts.format)
	}

	if opts.out == "" {
		_, err = stdout.Write(output)
		if !strings.HasSuffix(string(output), "\n") {
			stdout.Write([]byte("\n"))
		}
		return err
	}
	return os.WriteFile(opts.out, output, 0o644)
}

// readSource 读取文件或 stdin;stdin 需通过 --name 提供带扩展名的文件名。
func readSource(source, name string) (string, []byte, error) {
	if source == "-" {
		if name == "" {
			return "", nil, fmt.Errorf("stdin 模式需要 --name 提供文件名(如 report.xlsx)")
		}
		data, err := io.ReadAll(os.Stdin)
		return name, data, err
	}
	data, err := os.ReadFile(source)
	return source, data, err
}

// marshalIndent 带尾行换行的缩进 JSON 输出。
func marshalIndent(v any) ([]byte, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// printUsage 输出帮助。
func printUsage(w io.Writer) {
	fmt.Fprint(w, `docling — 纯 Go 文档解析 CLI

用法:
  docling parse <file|-> [flags]
      --format outline|md|content-list|json   输出格式(默认 outline)
      --section 前缀                          按章节路径过滤(md/content-list)
      --sheet 名                              按首级分组过滤(md/content-list)
      --layers body,furniture                 markdown 内容层
      --out 文件                              写文件(默认 stdout)
      --name 文件名                           stdin 模式的文件名
      --ocr                                   启用大模型 OCR(OPENAI_API_KEY 等 env)
      --ocr-model 名 / --ocr-base-url / --ocr-max-pages N
  docling formats                             列出支持的格式

示例:
  docling parse report.pdf --format outline
  docling parse report.xlsx --format json --out doc.json && jq '.texts' doc.json
  docling parse book.pdf --format md --section 第四章
`)
}
