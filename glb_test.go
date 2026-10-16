// glb_test.go 验证合成 GLB 的结构提取、属性精度、来源、统一输出和损坏输入拒绝。
// 合成数据只用于契约测试，不代表客户模型或完整几何兼容性验证。
package docling

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

// glbTestData 将给定 JSON 包装为仅含元数据块的 GLB 2.0 测试容器。
func glbTestData(metadata string) []byte {
	payload := []byte(metadata)
	for len(payload)%4 != 0 {
		payload = append(payload, ' ')
	}
	data := make([]byte, 20+len(payload))
	copy(data, "glTF")
	binary.LittleEndian.PutUint32(data[4:8], 2)
	binary.LittleEndian.PutUint32(data[8:12], uint32(len(data)))
	binary.LittleEndian.PutUint32(data[12:16], uint32(len(payload)))
	binary.LittleEndian.PutUint32(data[16:20], glbJSONChunk)
	copy(data[20:], payload)
	return data
}

// TestParseGLBStructured 验证真实字段组织、重复名称索引、整数精度和输出回读。
func TestParseGLBStructured(t *testing.T) {
	data := glbTestData(`{
		"asset":{"version":"2.0","generator":"联犀测试","extras":{"作者":"测试"}},
		"extras":{"用途":"合成水泵模型"},
		"scene":0,"scenes":[{"name":"泵站","nodes":[0]}],
		"nodes":[
			{"name":"水泵","children":[1],"extras":{"设备编号":9007199254740993,"型号":"P-100","参数":{"功率":15}}},
			{"name":"水泵","mesh":0}
		],
		"accessors":[{"componentType":5126,"count":1,"type":"VEC3","min":[0,0,0],"max":[0,0,0]}],
		"meshes":[{"name":"泵体网格","primitives":[{"attributes":{"POSITION":0},"material":0}]}],
		"materials":[{"name":"蓝色外观","extras":{"说明":"仅表示外观"}}]
	}`)
	doc, err := ParseByExtWithOptions("模型/泵站.GLB", data, ParseOptions{OriginURI: "models/pump.glb"})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Name != "泵站" || doc.Origin == nil || doc.Origin.Mimetype != "model/gltf-binary" || doc.Origin.URI != "models/pump.glb" {
		t.Fatalf("来源不正确：%+v", doc.Origin)
	}
	validateDocumentWithDoclingCore110ForTest(t, "泵站.glb", doc)
	markdown := doc.ToMarkdown()
	for _, text := range []string{"节点[0] 水泵", "节点[1] 水泵", "父节点：节点[0] 水泵", "子节点：节点[1] 水泵", "#/nodes/0", "9007199254740993", "P-100", "功率", "蓝色外观", "泵体网格", "合成水泵模型"} {
		if !strings.Contains(markdown, text) {
			t.Fatalf("输出缺少 %q：%s", text, markdown)
		}
	}
	if len(doc.Pages) != 0 || len(doc.Pictures) != 0 {
		t.Fatal("结构解析不得伪造页面或模型截图")
	}
	items := ToContentList(doc, SourceGolight)
	var propertiesSeen bool
	for _, item := range items {
		if strings.Contains(item.Text, "型号") && strings.Contains(item.Text, "P-100") {
			propertiesSeen = true
			if !strings.Contains(strings.Join(item.SectionPath, ">"), "节点[0] 水泵") {
				t.Fatal("属性缺少章节来源")
			}
		}
	}
	if !propertiesSeen {
		t.Fatal("属性没有进入可检索 content_list")
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"glb:source"`)) || !bytes.Contains(raw, []byte("9007199254740993")) {
		t.Fatal("JSON 丢失来源或整数精度")
	}
	restored, err := ParseDoclingDocument(raw)
	if err != nil || restored.ToMarkdown() != markdown {
		t.Fatalf("JSON 回读不一致：%v", err)
	}
}

// TestParseGLBMetadataOnly 验证几何未解码时仍保留已有属性和压缩扩展声明。
func TestParseGLBMetadataOnly(t *testing.T) {
	data := glbTestData(`{"asset":{"version":"2.0"},"extensionsRequired":["KHR_draco_mesh_compression"],"buffers":[{"uri":"https://example.invalid/geometry.bin","byteLength":2147483647}],"nodes":[{"extras":["设备A",42]}]}`)
	doc, err := ParseGLB(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"节点[0]", "设备A", "KHR_draco_mesh_compression", "未解码"} {
		if !strings.Contains(doc.Text(), text) {
			t.Fatalf("缺少元数据 %q", text)
		}
	}
}

// TestParseGLBRejectsInvalid 验证容器、类型和关系损坏时返回错误而不是成功占位。
func TestParseGLBRejectsInvalid(t *testing.T) {
	valid := glbTestData(`{"asset":{"version":"2.0"}}`)
	wrongLength := bytes.Clone(valid)
	binary.LittleEndian.PutUint32(wrongLength[8:12], uint32(len(valid)+4))
	hugeChunk := bytes.Clone(valid)
	binary.LittleEndian.PutUint32(hugeChunk[12:16], 0xfffffffc)
	wrongFirst := bytes.Clone(valid)
	binary.LittleEndian.PutUint32(wrongFirst[16:20], glbBINChunk)
	cases := map[string][]byte{
		"空文件":     nil,
		"普通JSON":  []byte(`{"asset":{"version":"2.0"}}`),
		"容器截断":    valid[:len(valid)-1],
		"总长度错误":   wrongLength,
		"超大块声明":   hugeChunk,
		"首块非JSON": wrongFirst,
		"JSON无效":  glbTestData(`{"asset":`),
		"缺少asset": glbTestData(`{}`),
		"版本不支持":   glbTestData(`{"asset":{"version":"1.0"}}`),
		"空节点":     glbTestData(`{"asset":{"version":"2.0"},"nodes":[null]}`),
		"负子节点":    glbTestData(`{"asset":{"version":"2.0"},"nodes":[{"children":[-1]}]}`),
		"子节点越界":   glbTestData(`{"asset":{"version":"2.0"},"nodes":[{"children":[1]}]}`),
		"节点循环":    glbTestData(`{"asset":{"version":"2.0"},"nodes":[{"children":[1]},{"children":[0]}]}`),
		"多个父级":    glbTestData(`{"asset":{"version":"2.0"},"nodes":[{"children":[2]},{"children":[2]},{}]}`),
		"非法默认场景":  glbTestData(`{"asset":{"version":"2.0"},"scene":-1}`),
		"空场景":     glbTestData(`{"asset":{"version":"2.0"},"scenes":[null]}`),
		"场景引用越界":  glbTestData(`{"asset":{"version":"2.0"},"scenes":[{"nodes":[0]}]}`),
		"网格引用越界":  glbTestData(`{"asset":{"version":"2.0"},"nodes":[{"mesh":0}]}`),
		"空网格":     glbTestData(`{"asset":{"version":"2.0"},"meshes":[null]}`),
		"空材质":     glbTestData(`{"asset":{"version":"2.0"},"materials":[null]}`),
		"材质引用越界":  glbTestData(`{"asset":{"version":"2.0"},"meshes":[{"primitives":[{"attributes":{"POSITION":0},"material":0}]}]}`),
		"节点超过上限":  glbTestData(`{"asset":{"version":"2.0"},"nodes":[` + strings.Repeat(`{},`, glbMaxNodes) + `{}]}`),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseByExt("损坏模型.glb", data); err == nil {
				t.Fatal("损坏输入不应被接受")
			}
		})
	}
}

// FuzzParseGLB 验证任意容器和 JSON 字节不会触发 panic。
func FuzzParseGLB(f *testing.F) {
	f.Add(glbTestData(`{"asset":{"version":"2.0"},"nodes":[{}]}`))
	f.Add([]byte("glTF"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseGLB(data)
	})
}
