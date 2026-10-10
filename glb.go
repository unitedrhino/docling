// glb.go 将 GLB 2.0 的场景、节点、网格和已有属性组织为知识库文档。
// 只读取容器中的 JSON 元数据，不加载二进制几何、外部资源或生成预览图片。
// 先校验容器长度和节点引用，再构建带 JSON 指针的正文；extras 使用原始
// JSON 保留大整数与嵌套字段，避免浮点转换破坏设备编号等业务属性。
package docling

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/qmuntal/gltf"
)

// GLB 读取上限限制元数据解析的内存与文档树规模，不涉及几何解压。
const (
	glbMaxFileBytes = 128 << 20
	glbMaxJSONBytes = 16 << 20
	glbMaxNodes     = 10000
	glbJSONChunk    = 0x4E4F534A
	glbBINChunk     = 0x004E4942
)

// ParseGLB 提取 GLB 2.0 字节流中实际存在的结构与属性，返回统一文档。
// 损坏容器、非法引用或循环节点返回错误；几何和纹理不进入输出文档，
// 不根据网格推测型号、工程材料、尺寸或 BOM。
func ParseGLB(data []byte) (*DoclingDocument, error) {
	metadata, err := glbMetadataJSON(data)
	if err != nil {
		return nil, err
	}
	var model gltf.Document
	if err := json.Unmarshal(metadata, &model); err != nil {
		return nil, fmt.Errorf("docling: GLB 元数据解析失败: %w", err)
	}
	if model.Asset.Version != "2.0" || (model.Asset.MinVersion != "" && model.Asset.MinVersion != "2.0") {
		return nil, fmt.Errorf("docling: GLB 仅支持 glTF 2.0 元数据")
	}
	parents, err := glbNodeParents(&model)
	if err != nil {
		return nil, err
	}
	// 原始对象与类型化模型分别读取，避免依赖库的 any 字段丢失整数精度。
	var raw struct {
		Asset     json.RawMessage   `json:"asset"`
		Scenes    []json.RawMessage `json:"scenes"`
		Nodes     []json.RawMessage `json:"nodes"`
		Meshes    []json.RawMessage `json:"meshes"`
		Materials []json.RawMessage `json:"materials"`
	}
	if err := json.Unmarshal(metadata, &raw); err != nil {
		return nil, err
	}
	doc := NewDoclingDocument("glb")
	doc.AddHeading(1, "三维模型信息", nil, nil)
	doc.AddText(LabelText, fmt.Sprintf("格式：GLB / glTF %s；场景：%d；节点：%d；网格：%d；渲染材质：%d", model.Asset.Version, len(model.Scenes), len(model.Nodes), len(model.Meshes), len(model.Materials)), nil, nil)
	if model.Asset.Generator != "" {
		doc.AddText(LabelText, "生成工具："+model.Asset.Generator, nil, nil)
	}
	if model.Asset.Copyright != "" {
		doc.AddText(LabelText, "版权信息："+model.Asset.Copyright, nil, nil)
	}
	doc.AddText(LabelText, "本解析结果提取模型结构与已有属性，不包含几何渲染或工程材料推断。", nil, nil)
	if len(model.ExtensionsRequired) > 0 {
		doc.AddText(LabelText, "模型声明必需扩展："+strings.Join(model.ExtensionsRequired, "、")+"；此处仅保留元数据，未解码扩展几何。", nil, nil)
	}
	glbAddProperties(doc, metadata, "#", nil)
	glbAddProperties(doc, raw.Asset, "#/asset", nil)

	for i, scene := range model.Scenes {
		group := glbAddSection(doc, glbObjectName("场景", i, scene.Name), fmt.Sprintf("#/scenes/%d", i))
		doc.AddText(LabelText, "根节点："+glbNodeNames(&model, scene.Nodes), nil, &group)
		glbAddProperties(doc, raw.Scenes[i], fmt.Sprintf("#/scenes/%d", i), &group)
	}
	for i, node := range model.Nodes {
		pointer := fmt.Sprintf("#/nodes/%d", i)
		group := glbAddSection(doc, glbObjectName("节点", i, node.Name), pointer)
		if parents[i] >= 0 {
			doc.AddText(LabelText, "父节点："+glbNodeNames(&model, []int{parents[i]}), nil, &group)
		}
		if len(node.Children) > 0 {
			doc.AddText(LabelText, "子节点："+glbNodeNames(&model, node.Children), nil, &group)
		}
		if node.Mesh != nil {
			doc.AddText(LabelText, "引用网格："+glbObjectName("网格", *node.Mesh, model.Meshes[*node.Mesh].Name), nil, &group)
		}
		glbAddProperties(doc, raw.Nodes[i], pointer, &group)
	}
	for i, mesh := range model.Meshes {
		pointer := fmt.Sprintf("#/meshes/%d", i)
		group := glbAddSection(doc, glbObjectName("网格", i, mesh.Name), pointer)
		doc.AddText(LabelText, fmt.Sprintf("图元数量：%d", len(mesh.Primitives)), nil, &group)
		for j, primitive := range mesh.Primitives {
			if primitive.Material != nil {
				doc.AddText(LabelText, fmt.Sprintf("图元[%d] 渲染材质：%s", j, glbObjectName("渲染材质", *primitive.Material, model.Materials[*primitive.Material].Name)), nil, &group)
			}
		}
		glbAddProperties(doc, raw.Meshes[i], pointer, &group)
	}
	for i, material := range model.Materials {
		pointer := fmt.Sprintf("#/materials/%d", i)
		group := glbAddSection(doc, glbObjectName("渲染材质", i, material.Name), pointer)
		glbAddProperties(doc, raw.Materials[i], pointer, &group)
	}
	return doc, nil
}

// glbMetadataJSON 校验完整 GLB 容器并返回首个 JSON 块，未知块按标准跳过。
// 使用文件实际长度检查每个块，不依据不可信的 buffer.byteLength 分配内存。
func glbMetadataJSON(data []byte) ([]byte, error) {
	if len(data) > glbMaxFileBytes {
		return nil, fmt.Errorf("docling: GLB 文件超过 %d 字节上限", glbMaxFileBytes)
	}
	if len(data) < 20 || !bytes.Equal(data[:4], []byte("glTF")) || binary.LittleEndian.Uint32(data[4:8]) != 2 {
		return nil, fmt.Errorf("docling: 无效的 GLB 2.0 文件头")
	}
	if len(data)%4 != 0 || uint64(binary.LittleEndian.Uint32(data[8:12])) != uint64(len(data)) {
		return nil, fmt.Errorf("docling: GLB 容器长度不匹配")
	}
	var metadata []byte
	for offset, chunkIndex := 12, 0; offset < len(data); chunkIndex++ {
		if len(data)-offset < 8 {
			return nil, fmt.Errorf("docling: GLB 块头截断")
		}
		length := uint64(binary.LittleEndian.Uint32(data[offset : offset+4]))
		kind := binary.LittleEndian.Uint32(data[offset+4 : offset+8])
		offset += 8
		if length%4 != 0 || length > uint64(len(data)-offset) {
			return nil, fmt.Errorf("docling: GLB 块长度无效")
		}
		if chunkIndex == 0 && kind != glbJSONChunk {
			return nil, fmt.Errorf("docling: GLB 首块必须为 JSON")
		}
		if kind == glbJSONChunk {
			if chunkIndex != 0 || length > glbMaxJSONBytes {
				return nil, fmt.Errorf("docling: GLB JSON 块重复或超过上限")
			}
			metadata = data[offset : offset+int(length)]
		}
		if kind == glbBINChunk && chunkIndex != 1 {
			return nil, fmt.Errorf("docling: GLB BIN 块位置无效")
		}
		offset += int(length)
	}
	return metadata, nil
}

// glbNodeParents 校验节点森林、场景及网格/材质引用，返回每个节点的父索引。
// 采用拓扑队列检测循环，避免递归访问不可信节点树导致栈溢出。
func glbNodeParents(model *gltf.Document) ([]int, error) {
	if len(model.Nodes) > glbMaxNodes {
		return nil, fmt.Errorf("docling: GLB 节点数量超过 %d", glbMaxNodes)
	}
	parents := make([]int, len(model.Nodes))
	for i := range parents {
		parents[i] = -1
	}
	for i, node := range model.Nodes {
		if node == nil || (node.Mesh != nil && (*node.Mesh < 0 || *node.Mesh >= len(model.Meshes))) {
			return nil, fmt.Errorf("docling: GLB 节点[%d] 或网格引用无效", i)
		}
		for _, child := range node.Children {
			if child < 0 || child >= len(parents) || parents[child] != -1 {
				return nil, fmt.Errorf("docling: GLB 节点[%d] 子节点引用无效或具有多个父级", i)
			}
			parents[child] = i
		}
	}
	queue := make([]int, 0, len(parents))
	for i, parent := range parents {
		if parent == -1 {
			queue = append(queue, i)
		}
	}
	for i := 0; i < len(queue); i++ {
		queue = append(queue, model.Nodes[queue[i]].Children...)
	}
	if len(queue) != len(parents) {
		return nil, fmt.Errorf("docling: GLB 节点存在循环引用")
	}
	if model.Scene != nil && (*model.Scene < 0 || *model.Scene >= len(model.Scenes)) {
		return nil, fmt.Errorf("docling: GLB 默认场景引用无效")
	}
	for i, scene := range model.Scenes {
		if scene == nil {
			return nil, fmt.Errorf("docling: GLB 场景[%d] 无效", i)
		}
		for _, root := range scene.Nodes {
			if root < 0 || root >= len(parents) || parents[root] != -1 {
				return nil, fmt.Errorf("docling: GLB 场景[%d] 根节点引用无效", i)
			}
		}
	}
	for i, material := range model.Materials {
		if material == nil {
			return nil, fmt.Errorf("docling: GLB 渲染材质[%d] 无效", i)
		}
	}
	for i, mesh := range model.Meshes {
		if mesh == nil {
			return nil, fmt.Errorf("docling: GLB 网格[%d] 无效", i)
		}
		for _, primitive := range mesh.Primitives {
			if primitive == nil || (primitive.Material != nil && (*primitive.Material < 0 || *primitive.Material >= len(model.Materials))) {
				return nil, fmt.Errorf("docling: GLB 网格[%d] 图元或材质引用无效", i)
			}
		}
	}
	return parents, nil
}

// glbObjectName 用真实名称与索引标识对象，缺失名称时只保留索引，避免伪造名称。
func glbObjectName(kind string, index int, name string) string {
	return strings.TrimSpace(fmt.Sprintf("%s[%d] %s", kind, index, name))
}

// glbNodeNames 返回已验证节点引用的可读列表，名称重复时索引仍可区分来源。
func glbNodeNames(model *gltf.Document, indices []int) string {
	names := make([]string, 0, len(indices))
	for _, index := range indices {
		names = append(names, glbObjectName("节点", index, model.Nodes[index].Name))
	}
	return strings.Join(names, "、")
}

// glbAddSection 创建对象章节，将 JSON 指针同时写入正文和官方命名空间元数据。
func glbAddSection(doc *DoclingDocument, name, pointer string) RefItem {
	// 属性挂在标题子树内，使统一简化器和切片器保留对象章节来源。
	group := doc.AddHeading(2, name, nil, nil)
	ref := doc.AddText(LabelText, "模型来源："+pointer, nil, &group)
	source, _ := json.Marshal(map[string]string{"pointer": pointer})
	doc.Texts[ref.Idx].Meta = BaseMeta{"glb:source": source}
	return group
}

// glbAddProperties 保留对象的 extras 与扩展数据，并把属性写入可检索正文。
// 不解读属性的工程含义；嵌套 JSON 和整数保持原始精度。
func glbAddProperties(doc *DoclingDocument, raw json.RawMessage, pointer string, parent *RefItem) {
	var object map[string]json.RawMessage
	_ = json.Unmarshal(raw, &object)
	for _, field := range []string{"extras", "extensions"} {
		value := bytes.TrimSpace(object[field])
		if len(value) == 0 || bytes.Equal(value, []byte("null")) {
			continue
		}
		var properties map[string]json.RawMessage
		if json.Unmarshal(value, &properties) != nil || properties == nil {
			ref := doc.AddText(LabelText, "自定义属性（"+pointer+"/"+field+"）："+string(value), nil, parent)
			doc.Texts[ref.Idx].Meta = BaseMeta{"glb:" + field: value}
			continue
		}
		keys := make([]string, 0, len(properties))
		for key := range properties {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			ref := doc.AddText(LabelText, "自定义属性（"+pointer+"/"+field+"）："+key+" = "+string(properties[key]), nil, parent)
			doc.Texts[ref.Idx].Meta = BaseMeta{"glb:property": properties[key]}
		}
	}
}
