// 本文件解压 CHM 的 LZX 内容流，改编自 github.com/gen2brain/folio/html（Apache-2.0）。
package chm

import "fmt"

const (
	lzxMinWindow = 15
	lzxMaxWindow = 21
	lzxFrame     = 1 << 15
	lzxLengths   = 249
	lzxAligned   = 8
	lzxPretree   = 20
)

// The kinds of block an LZX stream is made of.
const (
	lzxVerbatim = 1
	lzxAlignedB = 2
	lzxRaw      = 3
)

// lzxFooter is how many bits of an offset a position slot leaves to be read
// verbatim, and lzxBase where the slot starts.
var lzxFooter, lzxBase = func() ([]uint8, []uint32) {
	const n = 290
	f := make([]uint8, n)
	b := make([]uint32, n+1)
	for i := range f {
		switch {
		case i < 4:
			f[i] = 0
		case i >= 36:
			f[i] = 17
		default:
			f[i] = uint8(i-2) / 2
		}
		b[i+1] = b[i] + 1<<f[i]
	}
	return f, b
}()

func lzxSlots(window int) int {
	switch window {
	case 20:
		return 42
	case 21:
		return 50
	}
	return window * 2
}

// lzxBits reads the stream as 16 bit little endian words, high bit first.
type lzxBits struct {
	buf  []byte
	n    uint16
	left uint8
}

func (b *lzxBits) word() bool {
	if len(b.buf) < 2 {
		b.buf = nil
		return false
	}
	b.n = uint16(b.buf[0]) | uint16(b.buf[1])<<8
	b.buf = b.buf[2:]
	b.left = 16
	return true
}

func (b *lzxBits) read(n uint8) (uint32, bool) {
	if n > 16 {
		hi, ok := b.read(16)
		if !ok {
			return 0, false
		}
		lo, ok := b.read(n - 16)
		if !ok {
			return 0, false
		}
		return hi<<(n-16) | lo, true
	}
	if n <= b.left {
		b.left -= n
		b.n = rol16(b.n, n)
		return uint32(b.n) & (1<<n - 1), true
	}
	hi := uint32(rol16(b.n, b.left)) & (1<<b.left - 1)
	n -= b.left
	if !b.word() {
		return 0, false
	}
	b.left -= n
	b.n = rol16(b.n, n)
	return hi<<n | uint32(b.n)&(1<<n-1), true
}

// peek reads zeros past the end of the stream.
func (b *lzxBits) peek(n uint8) uint32 {
	if n <= b.left {
		return uint32(rol16(b.n, n)) & (1<<n - 1)
	}
	hi := uint32(rol16(b.n, b.left)) & (1<<b.left - 1)
	n -= b.left
	var w uint16
	if len(b.buf) >= 2 {
		w = uint16(b.buf[0]) | uint16(b.buf[1])<<8
	}
	return hi<<n | uint32(rol16(w, n))&(1<<n-1)
}

func (b *lzxBits) align() { b.left = 0 }

// bytes hands back n bytes of the stream, valid on a word boundary only.
func (b *lzxBits) bytes(n int) ([]byte, bool) {
	if n > len(b.buf) {
		return nil, false
	}
	out := b.buf[:n]
	b.buf = b.buf[n:]
	return out, true
}

func rol16(v uint16, n uint8) uint16 { return v<<n | v>>(16-n)&(1<<n-1) }

// lzxTree is a Huffman tree as a flat lookup, indexed by the widest code.
type lzxTree struct {
	lens  []uint8
	table []uint16
	width uint8
}

func (t *lzxTree) build(lens []uint8) error {
	t.lens = lens
	width := uint8(0)
	for _, v := range lens {
		if v > width {
			width = v
		}
	}
	t.width = width
	if width == 0 {
		t.table = nil
		return nil
	}
	if n := 1 << width; cap(t.table) < n {
		t.table = make([]uint16, n)
	} else {
		t.table = t.table[:n]
	}
	at := 0
	for bit := uint8(1); bit <= width; bit++ {
		run := 1 << (width - bit)
		for code, v := range lens {
			if v != bit {
				continue
			}
			if at+run > len(t.table) {
				return fmt.Errorf("%w: LZX path lengths overflow the tree", ErrInvalid)
			}
			for i := at; i < at+run; i++ {
				t.table[i] = uint16(code)
			}
			at += run
		}
	}
	if at != len(t.table) {
		return fmt.Errorf("%w: LZX path lengths do not fill the tree", ErrInvalid)
	}
	return nil
}

func (t *lzxTree) empty() bool { return t.width == 0 }

func (t *lzxTree) decode(b *lzxBits) (uint16, bool) {
	if t.width == 0 {
		return 0, false
	}
	code := t.table[b.peek(t.width)]
	if _, ok := b.read(t.lens[code]); !ok {
		return 0, false
	}
	return code, true
}

// lzx decodes one LZX stream a frame at a time. The trees, the window and the
// recent offsets carry from one frame to the next.
type lzx struct {
	window []byte
	pos    int
	mask   int
	slots  int

	r [3]uint32

	mainLens []uint8
	lenLens  []uint8
	main     lzxTree
	length   lzxTree
	aligned  lzxTree

	remaining uint32
	blockLen  uint32
	kind      uint8

	headerRead bool
	e8size     int32
	e8start    bool
	frames     uint32
	at         int
}

func newLZX(window int) (*lzx, error) {
	if window < lzxMinWindow || window > lzxMaxWindow {
		return nil, fmt.Errorf("%w: LZX window of 2^%d", ErrInvalid, window)
	}
	d := &lzx{
		window: make([]byte, 1<<window),
		mask:   1<<window - 1,
		slots:  lzxSlots(window),
	}
	d.mainLens = make([]uint8, 256+8*d.slots)
	d.lenLens = make([]uint8, lzxLengths)
	d.reset()
	return d, nil
}

func (d *lzx) reset() {
	d.r = [3]uint32{1, 1, 1}
	clear(d.mainLens)
	clear(d.lenLens)
	d.main = lzxTree{}
	d.length = lzxTree{}
	d.aligned = lzxTree{}
	d.remaining, d.blockLen, d.kind = 0, 0, 0
	d.headerRead, d.e8size, d.e8start = false, 0, false
	d.frames, d.at, d.pos = 0, 0, 0
	clear(d.window)
}

var errLZXShort = fmt.Errorf("%w: LZX stream ends inside a frame", ErrInvalid)

// frame decodes one frame into out, which is lzxFrame bytes but for the last.
func (d *lzx) frame(in, out []byte) error {
	b := &lzxBits{buf: in}
	if !d.headerRead {
		d.headerRead = true
		v, ok := b.read(1)
		if !ok {
			return errLZXShort
		}
		if v != 0 {
			hi, ok1 := b.read(16)
			lo, ok2 := b.read(16)
			if !ok1 || !ok2 {
				return errLZXShort
			}
			d.e8size = int32(hi<<16 | lo)
		}
	}

	togo := len(out)
	for togo > 0 {
		if d.remaining == 0 {
			if d.kind == lzxRaw && d.blockLen&1 != 0 {
				if _, ok := b.bytes(1); !ok {
					return errLZXShort
				}
			}
			if err := d.readBlock(b); err != nil {
				return err
			}
		}
		n, err := d.decodeInto(b, togo)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: LZX made no progress", ErrInvalid)
		}
		d.remaining -= uint32(n)
		togo -= n
	}

	d.past(out)
	d.translate(out)
	return nil
}

// past copies what the frame wrote out of the window it may have wrapped.
func (d *lzx) past(out []byte) {
	n := len(out)
	from := (d.pos - n) & d.mask
	if from+n <= len(d.window) {
		copy(out, d.window[from:from+n])
		return
	}
	k := copy(out, d.window[from:])
	copy(out[k:], d.window[:n-k])
}

func (d *lzx) readBlock(b *lzxBits) error {
	kind, ok := b.read(3)
	if !ok {
		return errLZXShort
	}
	hi, ok1 := b.read(16)
	lo, ok2 := b.read(8)
	if !ok1 || !ok2 {
		return errLZXShort
	}
	size := hi<<8 | lo
	if size == 0 {
		return fmt.Errorf("%w: LZX block of no length", ErrInvalid)
	}
	d.kind, d.remaining, d.blockLen = uint8(kind), size, size

	switch d.kind {
	case lzxAlignedB:
		lens := make([]uint8, lzxAligned)
		for i := range lens {
			v, ok := b.read(3)
			if !ok {
				return errLZXShort
			}
			lens[i] = uint8(v)
		}
		if err := d.aligned.build(lens); err != nil {
			return err
		}
		fallthrough
	case lzxVerbatim:
		if err := d.readLengths(b, d.mainLens, 0, 256); err != nil {
			return err
		}
		if err := d.readLengths(b, d.mainLens, 256, len(d.mainLens)); err != nil {
			return err
		}
		if err := d.main.build(d.mainLens); err != nil {
			return err
		}
		if d.main.empty() {
			return fmt.Errorf("%w: LZX main tree is empty", ErrInvalid)
		}
		if d.mainLens[0xE8] != 0 {
			d.e8start = true
		}
		if err := d.readLengths(b, d.lenLens, 0, lzxLengths); err != nil {
			return err
		}
		if err := d.length.build(d.lenLens); err != nil {
			return err
		}
	case lzxRaw:
		d.e8start = true
		b.align()
		for i := range d.r {
			lo, ok1 := b.read(16)
			hi, ok2 := b.read(16)
			if !ok1 || !ok2 {
				return errLZXShort
			}
			d.r[i] = hi<<16 | lo
		}
	default:
		return fmt.Errorf("%w: LZX block of kind %d", ErrInvalid, kind)
	}
	return nil
}

// readLengths applies to one range of a tree the delta a block writes it as,
// which is itself coded with a pretree written out in full.
func (d *lzx) readLengths(b *lzxBits, lens []uint8, from, to int) error {
	var pre lzxTree
	p := make([]uint8, lzxPretree)
	for i := range p {
		v, ok := b.read(4)
		if !ok {
			return errLZXShort
		}
		p[i] = uint8(v)
	}
	if err := pre.build(p); err != nil {
		return err
	}
	if pre.empty() {
		return fmt.Errorf("%w: LZX pretree is empty", ErrInvalid)
	}

	for i := from; i < to; {
		code, ok := pre.decode(b)
		if !ok {
			return errLZXShort
		}
		switch {
		case code <= 16:
			lens[i] = uint8((17 + int(lens[i]) - int(code)) % 17)
			i++
		case code == 17, code == 18:
			bits, base := uint8(4), 4
			if code == 18 {
				bits, base = 5, 20
			}
			v, ok := b.read(bits)
			if !ok {
				return errLZXShort
			}
			n := int(v) + base
			if i+n > to {
				return fmt.Errorf("%w: LZX run of %d lengths overruns the tree", ErrInvalid, n)
			}
			clear(lens[i : i+n])
			i += n
		case code == 19:
			same, ok := b.read(1)
			if !ok {
				return errLZXShort
			}
			next, ok := pre.decode(b)
			if !ok {
				return errLZXShort
			}
			if next > 16 {
				return fmt.Errorf("%w: LZX pretree code %d in a run", ErrInvalid, next)
			}
			v := uint8((17 + int(lens[i]) - int(next)) % 17)
			n := int(same) + 4
			if i+n > to {
				return fmt.Errorf("%w: LZX run of %d lengths overruns the tree", ErrInvalid, n)
			}
			for k := i; k < i+n; k++ {
				lens[k] = v
			}
			i += n
		default:
			return fmt.Errorf("%w: LZX pretree code %d", ErrInvalid, code)
		}
	}
	return nil
}

// decodeInto writes at most n bytes of the current block and returns how many.
func (d *lzx) decodeInto(b *lzxBits, n int) (int, error) {
	if d.kind == lzxRaw {
		want := min(int(d.remaining), n, len(b.buf))
		if want == 0 {
			return 0, errLZXShort
		}
		src, ok := b.bytes(want)
		if !ok {
			return 0, errLZXShort
		}
		for _, v := range src {
			d.window[d.pos] = v
			d.pos = (d.pos + 1) & d.mask
		}
		return want, nil
	}

	elem, ok := d.main.decode(b)
	if !ok {
		return 0, errLZXShort
	}
	if elem < 256 {
		d.window[d.pos] = byte(elem)
		d.pos = (d.pos + 1) & d.mask
		return 1, nil
	}

	header := int(elem-256) & 7
	length := header + 2
	if header == 7 {
		if d.length.empty() {
			return 0, fmt.Errorf("%w: LZX match needs an empty length tree", ErrInvalid)
		}
		v, ok := d.length.decode(b)
		if !ok {
			return 0, errLZXShort
		}
		length = int(v) + 9
	}

	slot := int(elem-256) >> 3
	var offset uint32
	switch slot {
	case 0:
		offset = d.r[0]
	case 1:
		offset = d.r[1]
		d.r[0], d.r[1] = d.r[1], d.r[0]
	case 2:
		offset = d.r[2]
		d.r[0], d.r[2] = d.r[2], d.r[0]
	default:
		if slot >= len(lzxFooter) {
			return 0, fmt.Errorf("%w: LZX position slot %d", ErrInvalid, slot)
		}
		bits := lzxFooter[slot]
		var formatted uint32
		switch {
		case d.kind == lzxAlignedB && bits >= 3:
			v, ok := b.read(bits - 3)
			if !ok {
				return 0, errLZXShort
			}
			a, ok := d.aligned.decode(b)
			if !ok {
				return 0, errLZXShort
			}
			formatted = lzxBase[slot] + v<<3 + uint32(a)
		default:
			v, ok := b.read(bits)
			if !ok {
				return 0, errLZXShort
			}
			formatted = lzxBase[slot] + v
		}
		if formatted < 2 {
			return 0, fmt.Errorf("%w: LZX match offset %d", ErrInvalid, formatted)
		}
		offset = formatted - 2
		d.r[2], d.r[1], d.r[0] = d.r[1], d.r[0], offset
	}
	if offset == 0 || int(offset) > len(d.window) {
		return 0, fmt.Errorf("%w: LZX match reaches %d back", ErrInvalid, offset)
	}
	if length > n {
		length = n
	}
	src := (d.pos - int(offset)) & d.mask
	for i := 0; i < length; i++ {
		d.window[d.pos] = d.window[(src+i)&d.mask]
		d.pos = (d.pos + 1) & d.mask
	}
	return length, nil
}

// translate undoes what the compressor did to the operand of every x86 call
// it found, for the first 32768 frames and once a 0xE8 became codable.
func (d *lzx) translate(out []byte) {
	frame := d.frames
	d.frames++
	if frame >= 32768 || d.e8size == 0 || len(out) <= 6 || !d.e8start {
		d.at += len(out)
		return
	}
	at := int32(d.at)
	d.at += len(out)
	end := len(out) - 10
	for i := 0; i < end; {
		if out[i] != 0xE8 {
			i++
			at++
			continue
		}
		abs := int32(out[i+1]) | int32(out[i+2])<<8 | int32(out[i+3])<<16 | int32(out[i+4])<<24
		if abs >= -at && abs < d.e8size {
			rel := abs + d.e8size
			if abs >= 0 {
				rel = abs - at
			}
			out[i+1] = byte(rel)
			out[i+2] = byte(rel >> 8)
			out[i+3] = byte(rel >> 16)
			out[i+4] = byte(rel >> 24)
		}
		i += 5
		at += 5
	}
}
