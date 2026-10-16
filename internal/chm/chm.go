// 本包读取 CHM 容器与 LZX 内容流，供文档解析器提取主题页。
// CHM 容器与解压算法改编自 github.com/gen2brain/folio/html 的
// 3a9e9d3e0cc54d2c230aa6820c8ff77ebabc1711 版本（Apache-2.0）。
package chm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

var (
	// ErrInvalid 表示 CHM 容器或内容流损坏。
	ErrInvalid = errors.New("invalid CHM")
	// ErrNotFound 表示归档中不存在指定条目。
	ErrNotFound = errors.New("CHM entry not found")
	// ErrUnsupported 表示 CHM 使用尚未支持的版本或压缩方式。
	ErrUnsupported = errors.New("unsupported CHM")
)

// maxPartBytes 限制单个 CHM 条目解压后的最大字节数。
const maxPartBytes = 1 << 26

const (
	chmITSFv2   = 0x58
	chmITSFv3   = 0x60
	chmITSPLen  = 0x54
	chmPMGLLen  = 0x14
	chmMaxPath  = 1024
	chmMaxFiles = 1 << 17
	chmCache    = 4
)

// The files the compressed section describes itself with.
const (
	chmContent = "::DataSpace/Storage/MSCompressed/Content"
	chmControl = "::DataSpace/Storage/MSCompressed/ControlData"
	chmResets  = "::DataSpace/Storage/MSCompressed/Transform/" +
		"{7FC28940-9D31-11D0-9B27-00A0C91E9C7C}/InstanceData/ResetTable"
)

// chmEntry is one file of the archive: which section it lives in, and where.
type chmEntry struct {
	path    string
	section uint64
	start   int64
	length  int64
}

// chmArchive reads the files an ITSF container holds.
type chmArchive struct {
	r    io.ReaderAt
	data int64
	ent  map[string]chmEntry

	list []chmEntry

	mu       sync.Mutex
	lzx      *lzx
	blockLen int64
	total    int64
	resets   []uint64
	cmpAt    int64
	cmpLen   int64
	perReset int
	at       int
	cache    [chmCache]struct {
		block int
		buf   []byte
	}
}

func readCHM(r io.ReaderAt, size int64) (*chmArchive, error) {
	var head [chmITSFv3]byte
	n, _ := r.ReadAt(head[:], 0)
	if n < chmITSFv2 || string(head[:4]) != "ITSF" {
		return nil, fmt.Errorf("%w: not an ITSF archive", ErrInvalid)
	}
	version := int32(le32(head[4:]))
	dirOff := int64(le64(head[0x48:]))
	dirLen := int64(le64(head[0x50:]))
	dataOff := dirOff + dirLen
	switch {
	case version == 2:
	case version == 3 && n >= chmITSFv3:
		dataOff = int64(le64(head[0x58:]))
	default:
		return nil, fmt.Errorf("%w: ITSF version %d", ErrUnsupported, version)
	}
	if dirOff < 0 || dirLen < chmITSPLen || dirLen > size || dirOff > size-dirLen ||
		dataOff < 0 || dataOff > size {
		return nil, fmt.Errorf("%w: ITSF header points outside the file", ErrInvalid)
	}

	dir := make([]byte, dirLen)
	if _, err := r.ReadAt(dir, dirOff); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if string(dir[:4]) != "ITSP" {
		return nil, fmt.Errorf("%w: no ITSP directory", ErrInvalid)
	}
	headLen := int64(int32(le32(dir[8:])))
	blockLen := int64(le32(dir[0x10:]))
	if headLen < chmITSPLen || blockLen < chmPMGLLen || headLen > dirLen {
		return nil, fmt.Errorf("%w: ITSP directory is malformed", ErrInvalid)
	}

	a := &chmArchive{r: r, data: dataOff, ent: make(map[string]chmEntry)}
	var order []chmEntry
	for off := headLen; off+blockLen <= dirLen; off += blockLen {
		chunk := dir[off : off+blockLen]
		if string(chunk[:4]) != "PMGL" {
			continue
		}
		free := int64(le32(chunk[4:]))
		end := blockLen - free
		if free > blockLen-chmPMGLLen {
			continue
		}
		for i := int64(chmPMGLLen); i < end; {
			e, next, ok := chmParse(chunk, i, end)
			if !ok {
				break
			}
			i = next
			if len(a.ent) >= chmMaxFiles {
				break
			}
			key := chmKey(e.path)
			if _, dup := a.ent[key]; !dup {
				a.ent[key] = e
				order = append(order, e)
			}
		}
	}
	if len(order) == 0 {
		return nil, fmt.Errorf("%w: the directory is empty", ErrInvalid)
	}
	a.list = order
	if err := a.readSection(size); err != nil {
		return nil, err
	}
	return a, nil
}

// chmParse reads one directory entry, whose numbers are big endian with the
// top bit of each byte saying more follows.
func chmParse(chunk []byte, i, end int64) (chmEntry, int64, bool) {
	n, i, ok := chmWord(chunk, i, end)
	if !ok || n == 0 || n > chmMaxPath || i+int64(n) > end {
		return chmEntry{}, 0, false
	}
	e := chmEntry{path: string(chunk[i : i+int64(n)])}
	i += int64(n)
	sec, i, ok1 := chmWord(chunk, i, end)
	start, i, ok2 := chmWord(chunk, i, end)
	length, i, ok3 := chmWord(chunk, i, end)
	if !ok1 || !ok2 || !ok3 || start > 1<<62 || length > 1<<40 {
		return chmEntry{}, 0, false
	}
	e.section, e.start, e.length = sec, int64(start), int64(length)
	return e, i, true
}

func chmWord(b []byte, i, end int64) (uint64, int64, bool) {
	var v uint64
	for n := 0; i < end; n++ {
		if n > 9 {
			return 0, i, false
		}
		c := b[i]
		i++
		v = v<<7 | uint64(c&0x7f)
		if c < 0x80 {
			return v, i, true
		}
	}
	return 0, i, false
}

// chmKey is the name a path is looked up by. An archive is rooted and its
// names are case insensitive, so a link that climbs past the root stops there.
func chmKey(p string) string {
	p = strings.TrimPrefix(p, "/")
	for strings.HasPrefix(p, "../") {
		p = p[3:]
	}
	return strings.ToLower(p)
}

// readSection reads what the compressed section says about itself: the window
// the stream was coded with, how often it resets, and where each block begins.
func (a *chmArchive) readSection(size int64) error {
	content, ok := a.ent[chmKey(chmContent)]
	if !ok {
		return nil
	}
	control, err := a.raw(a.ent[chmKey(chmControl)])
	if err != nil || len(control) < 0x18 {
		return nil
	}
	if string(control[4:8]) != "LZXC" {
		return fmt.Errorf("%w: the section is not LZX", ErrUnsupported)
	}
	version := le32(control[8:])
	interval, window, perReset := le32(control[12:]), le32(control[16:]), le32(control[20:])
	if version == 2 {
		interval *= lzxFrame
		window *= lzxFrame
	}
	if window < 1<<lzxMinWindow || window > 1<<lzxMaxWindow || window&(window-1) != 0 {
		return fmt.Errorf("%w: LZX window of %d bytes", ErrUnsupported, window)
	}
	if perReset == 0 {
		perReset = 1
	}
	if interval == 0 || interval%(window/2) != 0 {
		return fmt.Errorf("%w: LZX resets every %d bytes", ErrUnsupported, interval)
	}

	table, err := a.raw(a.ent[chmKey(chmResets)])
	if err != nil || len(table) < 0x28 {
		return fmt.Errorf("%w: no LZX reset table", ErrInvalid)
	}
	blocks := int(le32(table[4:]))
	tableAt := int(le32(table[12:]))
	total := int64(le64(table[16:]))
	cmpLen := int64(le64(table[24:]))
	blockLen := int64(le64(table[32:]))
	if blocks <= 0 || blockLen <= 0 || blockLen > lzxFrame || total < 0 ||
		tableAt < 0 || tableAt+8*blocks > len(table) {
		return fmt.Errorf("%w: the LZX reset table is malformed", ErrInvalid)
	}
	resets := make([]uint64, blocks)
	for i := range resets {
		resets[i] = le64(table[tableAt+8*i:])
	}
	if a.data+content.start+cmpLen > size || cmpLen > content.length {
		return fmt.Errorf("%w: the LZX stream runs past the file", ErrInvalid)
	}

	d, err := newLZX(log2(window))
	if err != nil {
		return err
	}
	a.lzx, a.blockLen, a.total = d, blockLen, total
	a.resets, a.cmpAt, a.cmpLen = resets, a.data+content.start, cmpLen
	a.perReset = int(interval / (window / 2) * perReset)
	if a.perReset <= 0 {
		a.perReset = 1
	}
	a.at = -1
	return nil
}

func log2(v uint32) int {
	n := 0
	for v > 1 {
		v >>= 1
		n++
	}
	return n
}

// file returns the bytes of one path, whichever section holds it.
func (a *chmArchive) file(p string) ([]byte, error) {
	e, ok := a.ent[chmKey(p)]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, p)
	}
	if e.length > maxPartBytes {
		return nil, fmt.Errorf("%w: %s is %d bytes", ErrUnsupported, p, e.length)
	}
	if e.section == 0 {
		return a.raw(e)
	}
	return a.decompress(e)
}

func (a *chmArchive) raw(e chmEntry) ([]byte, error) {
	if e.length == 0 {
		return nil, nil
	}
	if e.length > maxPartBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrUnsupported, e.length)
	}
	b := make([]byte, e.length)
	if _, err := a.r.ReadAt(b, a.data+e.start); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return b, nil
}

func (a *chmArchive) decompress(e chmEntry) ([]byte, error) {
	if a.lzx == nil {
		return nil, fmt.Errorf("%w: the archive has no LZX section", ErrUnsupported)
	}
	if e.start+e.length > a.total {
		return nil, fmt.Errorf("%w: a file runs past the section", ErrInvalid)
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	out := make([]byte, e.length)
	for done := int64(0); done < e.length; {
		at := e.start + done
		i := int(at / a.blockLen)
		b, err := a.block(i)
		if err != nil {
			return nil, err
		}
		off := at % a.blockLen
		if off >= int64(len(b)) {
			return nil, fmt.Errorf("%w: block %d is short", ErrInvalid, i)
		}
		done += int64(copy(out[done:], b[off:]))
	}
	return out, nil
}

// block decodes one block of the compressed section, from the last reset
// before it or from wherever the decoder was left.
func (a *chmArchive) block(i int) ([]byte, error) {
	if i < 0 || i >= len(a.resets) {
		return nil, fmt.Errorf("%w: block %d of %d", ErrInvalid, i, len(a.resets))
	}
	for _, c := range a.cache {
		if c.buf != nil && c.block == i {
			return c.buf, nil
		}
	}
	from := i - i%a.perReset
	if a.at >= from && a.at < i {
		from = a.at + 1
	} else {
		a.lzx.reset()
	}
	var out []byte
	for j := from; j <= i; j++ {
		b, err := a.decode(j)
		if err != nil {
			a.at = -1
			return nil, err
		}
		a.at = j
		a.cache[j%chmCache].block, a.cache[j%chmCache].buf = j, b
		out = b
	}
	return out, nil
}

func (a *chmArchive) decode(i int) ([]byte, error) {
	start := int64(a.resets[i])
	end := a.cmpLen
	if i+1 < len(a.resets) {
		end = int64(a.resets[i+1])
	}
	if start < 0 || end < start || end > a.cmpLen {
		return nil, fmt.Errorf("%w: block %d spans %d to %d", ErrInvalid, i, start, end)
	}
	in := make([]byte, end-start)
	if _, err := a.r.ReadAt(in, a.cmpAt+start); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	n := a.blockLen
	if rest := a.total - int64(i)*a.blockLen; rest < n {
		n = rest
	}
	if n <= 0 {
		return nil, fmt.Errorf("%w: block %d is empty", ErrInvalid, i)
	}
	out := make([]byte, n)
	if err := a.lzx.frame(in, out); err != nil {
		return nil, err
	}
	return out, nil
}

// readSystem reads what the archive says about itself.
func le32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
func le64(b []byte) uint64 { return binary.LittleEndian.Uint64(b) }

// Open 读取 CHM 目录，返回可按主题路径读取的归档。
func Open(r io.ReaderAt, size int64) (*Archive, error) { return readCHM(r, size) }

// Archive 是 CHM 归档读取器。
type Archive = chmArchive

// Paths 返回归档条目的原始路径，顺序与 CHM 目录一致。
func (a *chmArchive) Paths() []string {
	paths := make([]string, 0, len(a.list))
	for _, e := range a.list {
		paths = append(paths, e.path)
	}
	return paths
}

// ReadFile 按 CHM 内部路径读取条目字节。
func (a *chmArchive) ReadFile(p string) ([]byte, error) { return a.file(p) }
