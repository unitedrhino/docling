// pdf_ruled.go 基于 PDF 矢量框线还原有线表格（lattice 算法），服务 Word/WPS
// 等"表格边框以矢量线段绘制"的 PDF：此类文档的单元格文字基线随垂直居中
// 错开、空单元格整行无文字，按行聚合 + 大留白切列的坐标启发式
// （pdf_table.go）无法对齐网格，必须用框线重建。
//
// 模块分两层：
//  1. 图形提取：extractPDFPageRuleEdges 解释页面内容流（含 Form XObject），
//     在绘制算子处把描边/填充路径展开为线段，仅保留水平/垂直段，并经统一
//     页面几何变换为显示坐标（与文本行同系）；
//  2. 网格还原：detectPDFRuledTables 把同页框线按"相交连通"分组件（一组件
//     对应一张表格，表格间的标题文字与无关下划线不牵连），组件内线段聚类成
//     横纵边界，按"边覆盖度"对初等网格做并查集合并出含行/列跨度的单元格
//     区域（合并单元格即覆盖多个网格位的区域），再按词坐标装填文字。
//
// 识别保持保守：候选组件必须围成闭合框（四边全程覆盖）、至少 2 行 2 列；
// 文字内的填空下划线等短线段因不构成完整分隔边界会自然并回所属单元格。
package docling

import (
	"math"
	"sort"
	"strings"

	"github.com/ledongthuc/pdf"
)

const (
	// pdfRuleAxisTolerancePt 判定线段为水平/垂直的端点坐标差上限（pt）。
	pdfRuleAxisTolerancePt = 0.5
	// pdfRuleClusterTolerancePt 框线聚类的位置容差：同侧框线（如细矩形描出的
	// 边框两条长边）位置差在此范围内合并为一条网格边界。
	pdfRuleClusterTolerancePt = 3.0
	// pdfRuleIntersectTolerancePt 判定横纵线段相交（连通分组件）的端点容差。
	pdfRuleIntersectTolerancePt = 1.5
	// pdfRuleCoverageGapTolerancePt 判定一条边界的跨度被"覆盖"时允许的线段
	// 间隙上限（pt）。
	pdfRuleCoverageGapTolerancePt = 2.0
	// pdfRuleMinCellSizePt 相邻网格边界的最小间距，过滤聚类残渣与病态网格。
	pdfRuleMinCellSizePt = 3.0
	// pdfRuleWordSnapPt 词中心未落入任何单元格时吸附到最近区域的距离上限。
	pdfRuleWordSnapPt = 6.0
	// pdfRuleMaxBackgroundRatio 单个填充路径两条边均超过页面该比例时视为
	// 背景底色，不产生框线（整页/整版底色不是表格框线）。
	pdfRuleMaxBackgroundRatio = 0.6
	// pdfRuleMaxEdgesPerPage 单页参与的框线段上限，防御畸形文档的路径爆炸。
	pdfRuleMaxEdgesPerPage = 20000
)

// pdfRuleEdge 一条显示坐标下的水平或垂直框线段（BOTTOMLEFT，坐标与文本行一致）。
type pdfRuleEdge struct {
	Horizontal bool    // true: 水平线（Pos 为 Y，区间为 X）
	Pos        float64 // 水平线的 Y / 垂直线的 X
	From       float64 // 区间起点（恒 <= To）
	To         float64 // 区间终点
}

// pdfPathPoint 路径构造点（已经 CTM 变换到默认用户空间）。
type pdfPathPoint struct {
	X, Y float64
}

// pdfPathSegment 当前路径中的一条线段（矩形已展开为四边）。
type pdfPathSegment struct {
	From, To pdfPathPoint
}

// extractPDFPageRuleEdges 提取单页矢量框线段并变换到显示坐标。
// 内容流解释失败时吞掉异常返回已收集结果（尽力增强，不阻断主解析）。
func extractPDFPageRuleEdges(page pdf.Page, geometry pdfPageGeometry) []pdfRuleEdge {
	if page.V.IsNull() || page.V.Key("Contents").IsNull() {
		return nil
	}
	edges := collectPDFRuleEdges(page.V.Key("Contents"), page.Resources(), geometry.transform, geometry, 0)
	if len(edges) > pdfRuleMaxEdgesPerPage {
		edges = edges[:pdfRuleMaxEdgesPerPage]
	}
	return edges
}

// collectPDFRuleEdges 解释一个页面或 Form 内容流：跟踪 CTM 与路径构造，
// 在绘制算子（描边/填充）处把当前路径展开为线段，并递归 Form XObject。
func collectPDFRuleEdges(stream, resources pdf.Value, initial pdfAffine, geometry pdfPageGeometry, depth int) (edges []pdfRuleEdge) {
	if stream.IsNull() || depth > pdfMaxFormDepth {
		return nil
	}
	// ledongthuc 的解释器对畸形算子/栈使用 panic；框线感知是尽力增强，
	// 必须吞掉该局部异常并保留已收集结果，不影响 PDF 主解析。
	defer func() {
		if recover() != nil {
			edges = edges[:min(len(edges), pdfRuleMaxEdgesPerPage)]
		}
	}()
	current := initial
	stack := make([]pdfAffine, 0, 4)
	var path []pdfPathPoint       // 当前子路径折线点
	var closed bool               // 当前子路径是否已 h 闭合
	var segments []pdfPathSegment // 当前路径（可含多个子路径）累计的线段
	appendSegment := func(from, to pdfPathPoint) {
		if len(segments) < pdfRuleMaxEdgesPerPage {
			segments = append(segments, pdfPathSegment{From: from, To: to})
		}
	}
	moveTo := func(x, y float64) {
		px, py := transformPDFPoint(current, x, y)
		path = append(path[:0], pdfPathPoint{X: px, Y: py})
	}
	lineTo := func(x, y float64) {
		px, py := transformPDFPoint(current, x, y)
		point := pdfPathPoint{X: px, Y: py}
		if len(path) == 0 {
			path = append(path, point)
			return
		}
		appendSegment(path[len(path)-1], point)
		path = append(path, point)
	}
	resetPath := func() {
		path = path[:0]
		closed = false
		segments = segments[:0]
	}
	paint := func(fill bool) {
		if (closed || fill) && len(path) > 2 {
			// h 闭合的描边路径与填充路径（PDF 语义自动闭合）补上末段
			appendSegment(path[len(path)-1], path[0])
		}
		pageEdges := make([]pdfRuleEdge, 0, len(segments))
		for _, segment := range segments {
			if edge, ok := pdfRuleEdgeFromSegment(segment, geometry); ok {
				pageEdges = append(pageEdges, edge)
			}
		}
		if !fill || !pdfRuleIsBackground(pageEdges, geometry) {
			edges = append(edges, pageEdges...)
		}
		resetPath()
	}
	pdf.Interpret(stream, func(operand *pdf.Stack, operator string) {
		values := popPDFOperands(operand)
		switch operator {
		case "q":
			stack = append(stack, current)
		case "Q":
			if len(stack) > 0 {
				current = stack[len(stack)-1]
				stack = stack[:len(stack)-1]
			}
		case "cm":
			if matrix, ok := pdfAffineFromValues(values); ok {
				current = mulPDFAffine(matrix, current)
			}
		case "m":
			if len(values) >= 2 {
				moveTo(values[0].Float64(), values[1].Float64())
			}
		case "l":
			if len(values) >= 2 {
				lineTo(values[0].Float64(), values[1].Float64())
			}
		case "c", "v", "y":
			// 贝塞尔曲线以终点近似（曲线路径非表格框线，仅保持路径连续性）
			if len(values) >= 2 {
				lineTo(values[len(values)-2].Float64(), values[len(values)-1].Float64())
			}
		case "h":
			closed = true
		case "re":
			if len(values) != 4 {
				panic("bad re")
			}
			x, y := values[0].Float64(), values[1].Float64()
			w, hgt := values[2].Float64(), values[3].Float64()
			corners := [4]pdfPathPoint{}
			for i, point := range [][2]float64{{x, y}, {x + w, y}, {x + w, y + hgt}, {x, y + hgt}} {
				px, py := transformPDFPoint(current, point[0], point[1])
				corners[i] = pdfPathPoint{X: px, Y: py}
			}
			for i := 0; i < 4; i++ {
				appendSegment(corners[i], corners[(i+1)%4])
			}
		case "S", "s":
			paint(false)
		case "f", "F", "f*", "B", "B*", "b", "b*":
			paint(true)
		case "n":
			// 裁剪组合的收尾（re W* n）：路径不绘制，仅丢弃
			resetPath()
		case "Do":
			if len(values) != 1 {
				return
			}
			xObject := resources.Key("XObject").Key(values[0].Name())
			if xObject.Key("Subtype").Name() != "Form" {
				return
			}
			formMatrix := identityPDFAffine()
			if raw := xObject.Key("Matrix"); raw.Len() == 6 {
				matrixValues := make([]pdf.Value, 6)
				for i := range matrixValues {
					matrixValues[i] = raw.Index(i)
				}
				if parsed, ok := pdfAffineFromValues(matrixValues); ok {
					formMatrix = parsed
				}
			}
			formResources := xObject.Key("Resources")
			if formResources.IsNull() {
				formResources = resources
			}
			edges = append(edges, collectPDFRuleEdges(
				xObject, formResources, mulPDFAffine(formMatrix, current), geometry, depth+1,
			)...)
		}
	})
	return edges
}

// pdfRuleIsBackground 判断一组线段是否来自整页/整版背景填充：
// 外接框两边均超过页面尺寸的 pdfRuleMaxBackgroundRatio 时视为底色。
func pdfRuleIsBackground(edges []pdfRuleEdge, geometry pdfPageGeometry) bool {
	if geometry.width <= 0 || geometry.height <= 0 || len(edges) == 0 {
		return false
	}
	minX, maxX := math.Inf(1), math.Inf(-1)
	minY, maxY := math.Inf(1), math.Inf(-1)
	for _, edge := range edges {
		if edge.Horizontal {
			minX, maxX = math.Min(minX, edge.From), math.Max(maxX, edge.To)
			minY, maxY = math.Min(minY, edge.Pos), math.Max(maxY, edge.Pos)
			continue
		}
		minY, maxY = math.Min(minY, edge.From), math.Max(maxY, edge.To)
		minX, maxX = math.Min(minX, edge.Pos), math.Max(maxX, edge.Pos)
	}
	return maxX-minX >= geometry.width*pdfRuleMaxBackgroundRatio &&
		maxY-minY >= geometry.height*pdfRuleMaxBackgroundRatio
}

// pdfRuleEdgeFromSegment 把任意路径线段归一化为水平/垂直框线段：
// 经页面几何变换到显示坐标后，端点坐标差在轴容差内才保留。
func pdfRuleEdgeFromSegment(segment pdfPathSegment, geometry pdfPageGeometry) (pdfRuleEdge, bool) {
	x0, y0 := transformPDFPoint(geometry.transform, segment.From.X, segment.From.Y)
	x1, y1 := transformPDFPoint(geometry.transform, segment.To.X, segment.To.Y)
	edge := pdfRuleEdge{}
	switch {
	case math.Abs(y0-y1) <= pdfRuleAxisTolerancePt && math.Abs(x0-x1) > pdfRuleAxisTolerancePt:
		edge = pdfRuleEdge{Horizontal: true, Pos: (y0 + y1) / 2, From: math.Min(x0, x1), To: math.Max(x0, x1)}
	case math.Abs(x0-x1) <= pdfRuleAxisTolerancePt && math.Abs(y0-y1) > pdfRuleAxisTolerancePt:
		edge = pdfRuleEdge{Horizontal: false, Pos: (x0 + x1) / 2, From: math.Min(y0, y1), To: math.Max(y0, y1)}
	default:
		return pdfRuleEdge{}, false
	}
	return edge, true
}

// pdfRuleLineGroup 位置对齐的一组框线段（水平：Y 相同；垂直：X 相同）。
type pdfRuleLineGroup struct {
	pos      float64
	segments [][2]float64 // 区间 [from, to]
}

// groupPDFRuleEdges 把同向框线段按位置聚类（与相邻组位置差 <= 容差合并，
// 组位置取均值），返回按位置升序的组序列。
func groupPDFRuleEdges(edges []pdfRuleEdge, horizontal bool) []pdfRuleLineGroup {
	sorted := make([]pdfRuleEdge, 0, len(edges))
	for _, edge := range edges {
		if edge.Horizontal == horizontal {
			sorted = append(sorted, edge)
		}
	}
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Pos < sorted[j].Pos })
	groups := make([]pdfRuleLineGroup, 0, len(sorted))
	for _, edge := range sorted {
		if len(groups) > 0 && edge.Pos-groups[len(groups)-1].pos <= pdfRuleClusterTolerancePt {
			group := &groups[len(groups)-1]
			group.pos = (group.pos*float64(len(group.segments)) + edge.Pos) / float64(len(group.segments)+1)
			group.segments = append(group.segments, [2]float64{edge.From, edge.To})
			continue
		}
		groups = append(groups, pdfRuleLineGroup{pos: edge.Pos, segments: [][2]float64{{edge.From, edge.To}}})
	}
	return groups
}

// pdfRuleGroupCovers 判断组内线段区间的并集是否覆盖 [from, to]
// （允许 pdfRuleCoverageGapTolerancePt 内的间隙）。
func pdfRuleGroupCovers(group pdfRuleLineGroup, from, to float64) bool {
	if to-from <= pdfRuleCoverageGapTolerancePt {
		return false
	}
	segments := append([][2]float64(nil), group.segments...)
	sort.Slice(segments, func(i, j int) bool { return segments[i][0] < segments[j][0] })
	cursor := from
	for _, segment := range segments {
		if segment[0] > cursor+pdfRuleCoverageGapTolerancePt {
			return false
		}
		if segment[1] > cursor {
			cursor = segment[1]
			if cursor >= to-pdfRuleCoverageGapTolerancePt {
				return true
			}
		}
	}
	return false
}

// pdfRuledGrid 一个连通组件的候选网格：横纵边界与各边界段的覆盖度。
type pdfRuledGrid struct {
	xs     []float64 // 垂直边界位置（升序，len = cols+1）
	ys     []float64 // 水平边界位置（升序，len = rows+1）
	hCover [][]bool  // hCover[i][j]: 第 i 条水平边界与列 j 交叉段被覆盖
	vCover [][]bool  // vCover[i][j]: 第 j 条垂直边界与行 i 交叉段被覆盖
}

// pdfRuledRegion 一个还原出的单元格区域：覆盖 [row0,row1) × [col0,col1)
// 的网格位（含合并跨度），bbox 为显示坐标。
type pdfRuledRegion struct {
	row0, row1, col0, col1 int
	bbox                   DoclingBBox
}

// buildPDFRuledGrid 由组件内的横纵线组构建候选网格并计算边覆盖度；
// 不满足"至少 2 行 2 列、网格间距健康、外框闭合"时返回 false。
func buildPDFRuledGrid(hGroups, vGroups []pdfRuleLineGroup) (pdfRuledGrid, bool) {
	if len(hGroups) < 3 || len(vGroups) < 3 {
		return pdfRuledGrid{}, false
	}
	grid := pdfRuledGrid{}
	for _, group := range vGroups {
		grid.xs = append(grid.xs, group.pos)
	}
	for _, group := range hGroups {
		grid.ys = append(grid.ys, group.pos)
	}
	for i := 1; i < len(grid.xs); i++ {
		if grid.xs[i]-grid.xs[i-1] < pdfRuleMinCellSizePt {
			return pdfRuledGrid{}, false
		}
	}
	for i := 1; i < len(grid.ys); i++ {
		if grid.ys[i]-grid.ys[i-1] < pdfRuleMinCellSizePt {
			return pdfRuledGrid{}, false
		}
	}
	rows, cols := len(grid.ys)-1, len(grid.xs)-1
	grid.hCover = make([][]bool, len(grid.ys))
	for i, group := range hGroups {
		grid.hCover[i] = make([]bool, cols)
		for j := 0; j < cols; j++ {
			grid.hCover[i][j] = pdfRuleGroupCovers(group, grid.xs[j], grid.xs[j+1])
		}
	}
	grid.vCover = make([][]bool, rows)
	for i := 0; i < rows; i++ {
		grid.vCover[i] = make([]bool, len(vGroups))
		for j, group := range vGroups {
			grid.vCover[i][j] = pdfRuleGroupCovers(group, grid.ys[i], grid.ys[i+1])
		}
	}
	// 闭合框校验：外框四边必须全程覆盖，排除下划线、花括号等开放图形
	outerFrom, outerTo := grid.xs[0], grid.xs[cols]
	if !pdfRuleGroupCovers(hGroups[0], outerFrom, outerTo) ||
		!pdfRuleGroupCovers(hGroups[len(hGroups)-1], outerFrom, outerTo) {
		return pdfRuledGrid{}, false
	}
	outerBottom, outerTop := grid.ys[0], grid.ys[rows]
	leftFound, rightFound := false, false
	for _, group := range vGroups {
		if !pdfRuleGroupCovers(group, outerBottom, outerTop) {
			continue
		}
		if math.Abs(group.pos-grid.xs[0]) <= pdfRuleClusterTolerancePt {
			leftFound = true
		}
		if math.Abs(group.pos-grid.xs[cols]) <= pdfRuleClusterTolerancePt {
			rightFound = true
		}
	}
	if !leftFound || !rightFound {
		return pdfRuledGrid{}, false
	}
	return grid, true
}

// pdfRuledComponents 把同页框线按"横纵线段相交连通"分成组件：
// 一条水平组与垂直组在某线段交叉处接触即归入同组件；返回各组件的
// （横线组下标集, 纵线组下标集）。
func pdfRuledComponents(hGroups, vGroups []pdfRuleLineGroup) [][2][]int {
	hCount := len(hGroups)
	parent := make([]int, hCount+len(vGroups))
	for i := range parent {
		parent[i] = i
	}
	var find func(x int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, b int) { parent[find(a)] = find(b) }
	intersects := func(h pdfRuleLineGroup, v pdfRuleLineGroup) bool {
		for _, hs := range h.segments {
			if v.pos < hs[0]-pdfRuleIntersectTolerancePt || v.pos > hs[1]+pdfRuleIntersectTolerancePt {
				continue
			}
			for _, vs := range v.segments {
				if h.pos >= vs[0]-pdfRuleIntersectTolerancePt && h.pos <= vs[1]+pdfRuleIntersectTolerancePt {
					return true
				}
			}
		}
		return false
	}
	for i, h := range hGroups {
		for j, v := range vGroups {
			if intersects(h, v) {
				union(i, hCount+j)
			}
		}
	}
	members := map[int][2][]int{}
	for i := range hGroups {
		root := find(i)
		m := members[root]
		m[0] = append(m[0], i)
		members[root] = m
	}
	for j := range vGroups {
		root := find(hCount + j)
		m := members[root]
		m[1] = append(m[1], j)
		members[root] = m
	}
	components := make([][2][]int, 0, len(members))
	for _, m := range members {
		components = append(components, m)
	}
	// 稳定输出顺序：按组件最底部的水平线位置排序
	sort.SliceStable(components, func(a, b int) bool {
		return hGroups[components[a][0][0]].pos < hGroups[components[b][0][0]].pos
	})
	return components
}

// pdfGridUnionFind 初等网格的并查集。
type pdfGridUnionFind []int

func newPDFGridUnionFind(rows, cols int) pdfGridUnionFind {
	parent := make(pdfGridUnionFind, rows*cols)
	for i := range parent {
		parent[i] = i
	}
	return parent
}

func (uf pdfGridUnionFind) find(x int) int {
	for uf[x] != x {
		uf[x] = uf[uf[x]]
		x = uf[x]
	}
	return x
}

func (uf pdfGridUnionFind) union(a, b int) { uf[uf.find(a)] = uf.find(b) }

// regionsPDFRuledGrid 按边覆盖度合并初等网格：相邻两格之间无覆盖边界则
// 归并为同一单元格区域（即合并单元格），随后收敛真实边界——从不是任何
// 区域顶/底的边界为区域内部伪边界（如填空下划线），予以坍缩并重排行列号。
func regionsPDFRuledGrid(grid pdfRuledGrid) []pdfRuledRegion {
	rows, cols := len(grid.ys)-1, len(grid.xs)-1
	uf := newPDFGridUnionFind(rows, cols)
	index := func(i, j int) int { return i*cols + j }
	for i := 0; i < rows; i++ {
		for j := 0; j < cols; j++ {
			if i > 0 && !grid.hCover[i][j] {
				uf.union(index(i, j), index(i-1, j))
			}
			if j > 0 && !grid.vCover[i][j] {
				uf.union(index(i, j), index(i, j-1))
			}
		}
	}
	type span struct {
		row0, row1, col0, col1 int
	}
	spans := map[int]*span{}
	for i := 0; i < rows; i++ {
		for j := 0; j < cols; j++ {
			root := uf.find(index(i, j))
			if spans[root] == nil {
				spans[root] = &span{row0: i, row1: i + 1, col0: j, col1: j + 1}
				continue
			}
			s := spans[root]
			s.row0, s.row1 = min(s.row0, i), max(s.row1, i+1)
			s.col0, s.col1 = min(s.col0, j), max(s.col1, j+1)
		}
	}
	if len(spans) == 0 {
		return nil
	}
	// 真实边界收敛：只有作为某区域顶/底的边界才是真实行/列边界
	realRows := map[int]bool{}
	realCols := map[int]bool{}
	for _, s := range spans {
		realRows[s.row0], realRows[s.row1] = true, true
		realCols[s.col0], realCols[s.col1] = true, true
	}
	rowRank := map[int]int{}
	rank := 0
	for i := 0; i <= rows; i++ {
		if realRows[i] {
			rowRank[i] = rank
			rank++
		}
	}
	colRank := map[int]int{}
	rank = 0
	for j := 0; j <= cols; j++ {
		if realCols[j] {
			colRank[j] = rank
			rank++
		}
	}
	regions := make([]pdfRuledRegion, 0, len(spans))
	for _, s := range spans {
		// 行号转阅读序（从顶部起）：库内 Y 向上递增，网格行 0 在页面底部
		displayRow0 := len(realRows) - 1 - rowRank[s.row1]
		displayRow1 := len(realRows) - 1 - rowRank[s.row0]
		regions = append(regions, pdfRuledRegion{
			row0: displayRow0, row1: displayRow1,
			col0: colRank[s.col0], col1: colRank[s.col1],
			bbox: DoclingBBox{
				L: grid.xs[s.col0], R: grid.xs[s.col1],
				B: grid.ys[s.row0], T: grid.ys[s.row1],
				CoordOrigin: CoordOriginBottomLeft,
			},
		})
	}
	sort.Slice(regions, func(i, j int) bool {
		if regions[i].row0 != regions[j].row0 {
			return regions[i].row0 < regions[j].row0
		}
		return regions[i].col0 < regions[j].col0
	})
	return regions
}

// detectPDFRuledTables 在全文档行上还原各页的框线表格：
// 每页按连通组件独立建网格（组件间标题文字不牵连），行内词按中心点装入
// 区域；行包围盒中心落入表格 bbox 即视为被表格消费。
// 返回表格（按 StartLine 升序）与行级消费标记（无表格时为 nil），
// 消费行集合供调用方从正文流移除并用于表格 Lines 的保序回填。
func detectPDFRuledTables(lines []pdfLine, pageEdges map[int64][]pdfRuleEdge) ([]pdfDetectedTable, []bool) {
	if len(pageEdges) == 0 || len(lines) == 0 {
		return nil, nil
	}
	pageNumbers := make([]int64, 0, len(pageEdges))
	for pageIdx := range pageEdges {
		if len(pageEdges[pageIdx]) > 0 {
			pageNumbers = append(pageNumbers, pageIdx)
		}
	}
	sort.Slice(pageNumbers, func(i, j int) bool { return pageNumbers[i] < pageNumbers[j] })

	var tables []pdfDetectedTable
	consumed := make([]bool, len(lines))
	for _, pageIdx := range pageNumbers {
		edges := pageEdges[pageIdx]
		hGroups := groupPDFRuleEdges(edges, true)
		vGroups := groupPDFRuleEdges(edges, false)
		for _, component := range pdfRuledComponents(hGroups, vGroups) {
			hMembers := make([]pdfRuleLineGroup, 0, len(component[0]))
			for _, i := range component[0] {
				hMembers = append(hMembers, hGroups[i])
			}
			vMembers := make([]pdfRuleLineGroup, 0, len(component[1]))
			for _, j := range component[1] {
				vMembers = append(vMembers, vGroups[j])
			}
			grid, ok := buildPDFRuledGrid(hMembers, vMembers)
			if !ok {
				continue
			}
			regions := regionsPDFRuledGrid(grid)
			if len(regions) < 3 || len(grid.ys)-1 < 2 || len(grid.xs)-1 < 2 {
				continue
			}
			if table := assemblePDFRuledTable(pageIdx, grid, regions, lines, consumed); table != nil {
				tables = append(tables, *table)
			}
		}
	}
	if len(tables) == 0 {
		return nil, nil
	}
	sort.SliceStable(tables, func(i, j int) bool { return tables[i].StartLine < tables[j].StartLine })
	return tables, consumed
}

// assemblePDFRuledTable 把一页候选网格的区域转成官方 TableData：
// 消费中心落入表格 bbox 的行；词按中心点装填单元格（未命中的吸附到最近
// 区域）；首行均为非数字文本且数据行含数字时标记列表头。
// 返回 nil 表示该组件内没有可消费文本（纯空网格）。
func assemblePDFRuledTable(pageIdx int64, grid pdfRuledGrid, regions []pdfRuledRegion, lines []pdfLine, consumed []bool) *pdfDetectedTable {
	firstIdx, lastIdx := -1, -1
	type cellText struct {
		words   []pdfWord
		lineIDs []int // 词所属行的原始下标（用于同视觉行/跨行拼接）
	}
	cells := make([]*cellText, len(regions))
	locate := func(word pdfWord) int {
		cx, cy := (word.MinX+word.MaxX)/2, (word.MinY+word.MaxY)/2
		best, bestDistance := -1, math.MaxFloat64
		for i := range regions {
			region := &regions[i]
			if cx >= region.bbox.L && cx <= region.bbox.R && cy >= region.bbox.B && cy <= region.bbox.T {
				return i
			}
			distance := pdfRulePointDistance(region.bbox, cx, cy)
			if distance < bestDistance {
				best, bestDistance = i, distance
			}
		}
		if bestDistance <= pdfRuleWordSnapPt {
			return best
		}
		return -1
	}
	for index, line := range lines {
		if line.PageIdx != pageIdx || line.Fallback || line.OCRSub != nil ||
			line.Table != nil || line.Furniture != "" || line.InTOC || consumed[index] {
			continue
		}
		cx, cy := (line.MinX+line.MaxX)/2, (line.MinY+line.MaxY)/2
		bboxL, bboxT := grid.xs[0], grid.ys[len(grid.ys)-1]
		bboxR, bboxB := grid.xs[len(grid.xs)-1], grid.ys[0]
		if cx < bboxL || cx > bboxR || cy < bboxB || cy > bboxT {
			continue
		}
		if firstIdx < 0 {
			firstIdx = index
		}
		lastIdx = index
		consumed[index] = true
		for _, word := range line.Words {
			if strings.TrimSpace(word.Text) == "" {
				continue
			}
			regionIdx := locate(word)
			if regionIdx < 0 {
				continue
			}
			if cells[regionIdx] == nil {
				cells[regionIdx] = &cellText{}
			}
			cells[regionIdx].words = append(cells[regionIdx].words, word)
			cells[regionIdx].lineIDs = append(cells[regionIdx].lineIDs, index)
		}
	}
	if firstIdx < 0 {
		return nil
	}

	tableCells := make([]DoclingTableCell, 0, len(regions))
	for i, region := range regions {
		text := ""
		if cells[i] != nil {
			text = joinPDFRuledCellText(cells[i].words, cells[i].lineIDs, lines)
		}
		tableCells = append(tableCells, DoclingTableCell{
			BBox:              &DoclingBBox{L: region.bbox.L, R: region.bbox.R, B: region.bbox.B, T: region.bbox.T, CoordOrigin: CoordOriginBottomLeft},
			RowSpan:           int64(region.row1 - region.row0),
			ColSpan:           int64(region.col1 - region.col0),
			StartRowOffsetIdx: int64(region.row0), EndRowOffsetIdx: int64(region.row1),
			StartColOffsetIdx: int64(region.col0), EndColOffsetIdx: int64(region.col1),
			Text: text,
		})
	}
	markPDFRuledHeader(tableCells)
	tableLines := make([]pdfLine, 0, lastIdx-firstIdx+1)
	for index := firstIdx; index <= lastIdx; index++ {
		if consumed[index] {
			tableLines = append(tableLines, lines[index])
		}
	}
	var bbox *DoclingBBox
	for i := range tableCells {
		bbox = unionPDFTableBBox(bbox, tableCells[i].BBox)
	}
	numRows, numCols := int64(0), int64(0)
	for i := range tableCells {
		numRows = max(numRows, tableCells[i].EndRowOffsetIdx)
		numCols = max(numCols, tableCells[i].EndColOffsetIdx)
	}
	return &pdfDetectedTable{
		PageIdx:   pageIdx,
		Data:      TableData{TableCells: tableCells, NumRows: numRows, NumCols: numCols, Orientation: TableOrientation0},
		BBox:      bbox,
		Lines:     tableLines,
		StartLine: firstIdx, EndLine: lastIdx + 1,
	}
}

// markPDFRuledHeader 首行单元格均为非数字文本且数据行出现数字时，
// 把首行单元格（含起始于首行的合并单元格不在此列）标记为列表头。
func markPDFRuledHeader(cells []DoclingTableCell) {
	hasHeaderRow := false
	digitsBelow := false
	for i := range cells {
		cell := &cells[i]
		switch {
		case cell.StartRowOffsetIdx == 0 && cell.EndRowOffsetIdx == 1:
			hasHeaderRow = true
			if pdfTableTextHasDigit(cell.Text) {
				return
			}
		case cell.StartRowOffsetIdx > 0:
			if !digitsBelow && pdfTableTextHasDigit(cell.Text) {
				digitsBelow = true
			}
		}
	}
	if !hasHeaderRow || !digitsBelow {
		return
	}
	for i := range cells {
		if cells[i].StartRowOffsetIdx == 0 && cells[i].EndRowOffsetIdx == 1 {
			cells[i].ColumnHeader = true
		}
	}
}

// joinPDFRuledCellText 按阅读顺序拼接单元格文本：行按消费顺序（全文档
// 自上而下）、行内词按 X 升序；同视觉行内词按 CJK 邻接规则拼接（CJK 相邻
// 不补空格），单元格内不同行以换行连接（Markdown 渲染层会压平）。
func joinPDFRuledCellText(words []pdfWord, lineIDs []int, lines []pdfLine) string {
	if len(words) == 0 {
		return ""
	}
	order := make([]int, len(words))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		lineA, lineB := lineIDs[order[a]], lineIDs[order[b]]
		if lineA != lineB {
			// 库内 Y 向上递增：阅读顺序靠前的行 MaxY 更大
			return lines[lineA].MaxY > lines[lineB].MaxY
		}
		return words[order[a]].MinX < words[order[b]].MinX
	})
	var lineTexts []string
	var lineText strings.Builder
	currentLine := -1
	for _, i := range order {
		word := strings.TrimSpace(words[i].Text)
		if word == "" {
			continue
		}
		if lineIDs[i] != currentLine {
			if lineText.Len() > 0 {
				lineTexts = append(lineTexts, lineText.String())
				lineText.Reset()
			}
			currentLine = lineIDs[i]
		} else if lineText.Len() > 0 && !endsWithCJK(lineText.String()) && !startsWithCJK(word) {
			lineText.WriteString(" ")
		}
		lineText.WriteString(word)
	}
	if lineText.Len() > 0 {
		lineTexts = append(lineTexts, lineText.String())
	}
	return strings.Join(lineTexts, "\n")
}

// pdfRulePointDistance 计算点到 bbox 的最近距离（内部点为 0）。
func pdfRulePointDistance(bbox DoclingBBox, x, y float64) float64 {
	dx := math.Max(bbox.L-x, math.Max(0, x-bbox.R))
	dy := math.Max(bbox.B-y, math.Max(0, y-bbox.T))
	return math.Hypot(dx, dy)
}
