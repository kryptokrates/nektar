package core

import (
	"fmt"
	"strings"
)

// Pure-Go ISO/IEC 18004 QR Code encoder (Byte mode, ECC level M, versions 1–6: up to 106 bytes).
// Zero external dependencies, zero CGO — used by CLI (`nektar wallet address --qr`, `nektar serve`,
// `nektar hub up`) and Dashboard (`GET /v1/wallet/qr`) to render scannable BSV funding QR codes.

type qrVersionSpec struct {
	version    int
	size       int
	dataBytes  int // total data codewords for ECC-M
	ecPerBlock int // EC codewords per block
	blocks     int // number of RS blocks (all equal size for v1-v4, v6-M)
	align      []int
}

// Versions 1–6 at ECC Level M (Byte mode capacity: 14, 26, 42, 62, 84, 106 bytes).
var qrSpecsM = []qrVersionSpec{
	{version: 1, size: 21, dataBytes: 16, ecPerBlock: 10, blocks: 1, align: nil},
	{version: 2, size: 25, dataBytes: 28, ecPerBlock: 16, blocks: 1, align: []int{6, 18}},
	{version: 3, size: 29, dataBytes: 44, ecPerBlock: 26, blocks: 1, align: []int{6, 22}},
	{version: 4, size: 33, dataBytes: 64, ecPerBlock: 18, blocks: 2, align: []int{6, 26}},
	{version: 5, size: 37, dataBytes: 86, ecPerBlock: 24, blocks: 2, align: []int{6, 30}},
	{version: 6, size: 41, dataBytes: 108, ecPerBlock: 16, blocks: 4, align: []int{6, 34}},
}

// EncodeQRMatrix encodes text into an ISO/IEC 18004 ECC-M QR matrix (including border quiet-zone modules).
// Returns a square matrix where true = dark module and false = light module.
func EncodeQRMatrix(text string, border int) ([][]bool, error) {
	raw := []byte(text)
	if len(raw) == 0 {
		return nil, fmt.Errorf("qr: empty payload")
	}
	if border < 0 {
		border = 0
	}
	var spec qrVersionSpec
	found := false
	for _, s := range qrSpecsM {
		// Byte mode overhead: 4-bit mode (0100) + 8-bit length + 4-bit terminator = 2 bytes.
		if len(raw)+2 <= s.dataBytes {
			spec = s
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("qr: payload length %d exceeds max 106 bytes", len(raw))
	}

	// 1. Build data bitstream: mode=0100, count=8-bit len, data bytes, terminator + pad bytes.
	dataCW := make([]byte, 0, spec.dataBytes)
	var bitBuf uint32
	bitLen := 0
	pushBits := func(val uint32, n int) {
		bitBuf = (bitBuf << n) | (val & ((1 << n) - 1))
		bitLen += n
		for bitLen >= 8 {
			bitLen -= 8
			dataCW = append(dataCW, byte(bitBuf>>bitLen))
		}
	}
	pushBits(0b0100, 4)
	pushBits(uint32(len(raw)), 8)
	for _, b := range raw {
		pushBits(uint32(b), 8)
	}
	// Terminator (up to 4 zero bits) + zero-pad to byte boundary.
	remBits := spec.dataBytes*8 - (len(dataCW)*8 + bitLen)
	term := 4
	if remBits < term {
		term = remBits
	}
	if term > 0 {
		pushBits(0, term)
	}
	if bitLen > 0 {
		pushBits(0, 8-bitLen)
	}
	padSeq := [2]byte{0xEC, 0x11}
	for i := 0; len(dataCW) < spec.dataBytes; i++ {
		dataCW = append(dataCW, padSeq[i&1])
	}

	// 2. Reed-Solomon ECC per block + interleave.
	blockDataLen := spec.dataBytes / spec.blocks
	dataBlocks := make([][]byte, spec.blocks)
	ecBlocks := make([][]byte, spec.blocks)
	gen := rsGeneratorPoly(spec.ecPerBlock)
	for b := 0; b < spec.blocks; b++ {
		blk := dataCW[b*blockDataLen : (b+1)*blockDataLen]
		dataBlocks[b] = blk
		ecBlocks[b] = rsRemainder(blk, gen)
	}
	finalCW := make([]byte, 0, spec.dataBytes+spec.blocks*spec.ecPerBlock)
	for i := 0; i < blockDataLen; i++ {
		for b := 0; b < spec.blocks; b++ {
			finalCW = append(finalCW, dataBlocks[b][i])
		}
	}
	for i := 0; i < spec.ecPerBlock; i++ {
		for b := 0; b < spec.blocks; b++ {
			finalCW = append(finalCW, ecBlocks[b][i])
		}
	}

	// 3. Place function patterns on base grid.
	n := spec.size
	mod := make([][]bool, n)
	fn := make([][]bool, n)
	for y := 0; y < n; y++ {
		mod[y] = make([]bool, n)
		fn[y] = make([]bool, n)
	}
	setFn := func(x, y int, dark bool) {
		if x >= 0 && x < n && y >= 0 && y < n {
			mod[y][x] = dark
			fn[y][x] = true
		}
	}
	drawFinder := func(cx, cy int) {
		for dy := -4; dy <= 4; dy++ {
			for dx := -4; dx <= 4; dx++ {
				x, y := cx+dx, cy+dy
				if x < 0 || x >= n || y < 0 || y >= n {
					continue
				}
				dist := max(absInt(dx), absInt(dy))
				dark := dist != 2 && dist != 4
				setFn(x, y, dark)
			}
		}
	}
	drawFinder(3, 3)
	drawFinder(n-4, 3)
	drawFinder(3, n-4)

	// Timing patterns.
	for i := 8; i < n-8; i++ {
		dark := i%2 == 0
		setFn(i, 6, dark)
		setFn(6, i, dark)
	}

	// Alignment patterns.
	for _, ay := range spec.align {
		for _, ax := range spec.align {
			if fn[ay][ax] {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					dist := max(absInt(dx), absInt(dy))
					setFn(ax+dx, ay+dy, dist != 1)
				}
			}
		}
	}

	// Reserve format info strips + dark module.
	for i := 0; i <= 8; i++ {
		if i != 6 {
			setFn(8, i, false)
			setFn(i, 8, false)
		}
	}
	for i := 0; i < 8; i++ {
		setFn(n-1-i, 8, false)
	}
	for i := 0; i < 7; i++ {
		setFn(8, n-1-i, false)
	}
	setFn(8, n-8, true)

	// 4. Zig-zag place codewords into non-function modules.
	bitIdx := 0
	totalBits := len(finalCW) * 8
	up := true
	for right := n - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < n; vert++ {
			y := vert
			if up {
				y = n - 1 - vert
			}
			for j := 0; j < 2; j++ {
				x := right - j
				if fn[y][x] {
					continue
				}
				dark := false
				if bitIdx < totalBits {
					dark = ((finalCW[bitIdx>>3] >> (7 - (bitIdx & 7))) & 1) != 0
					bitIdx++
				}
				mod[y][x] = dark
			}
		}
		up = !up
	}

	// 5. Evaluate all 8 standard masks and pick the lowest penalty.
	bestPenalty := int(^uint(0) >> 1)
	var bestGrid [][]bool
	for mask := 0; mask < 8; mask++ {
		cand := applyQRMaskAndFormat(mod, fn, mask)
		pen := qrPenalty(cand)
		if pen < bestPenalty {
			bestPenalty = pen
			bestGrid = cand
		}
	}

	// 6. Add quiet-zone border.
	outSize := n + 2*border
	out := make([][]bool, outSize)
	for y := 0; y < outSize; y++ {
		out[y] = make([]bool, outSize)
	}
	for y := 0; y < n; y++ {
		copy(out[y+border][border:border+n], bestGrid[y])
	}
	return out, nil
}

// RenderTerminalQR renders text as a compact scannable Unicode half-block QR code for terminals.
// Uses white-on-black ANSI contrast when invert=false (black modules on white quiet zone via inverse cells).
func RenderTerminalQR(text string) (string, error) {
	m, err := EncodeQRMatrix(text, 2)
	if err != nil {
		return "", err
	}
	// Render 2 rows per terminal line using Unicode upper/lower half blocks.
	// In dark terminals, light modules (false) are drawn as foreground '█' and dark modules (true) as space,
	// producing a crisp white quiet zone with dark QR modules that phone cameras scan immediately.
	var b strings.Builder
	h := len(m)
	w := len(m[0])
	for y := 0; y < h; y += 2 {
		for x := 0; x < w; x++ {
			topLight := !m[y][x]
			botLight := true
			if y+1 < h {
				botLight = !m[y+1][x]
			}
			switch {
			case topLight && botLight:
				b.WriteRune('█')
			case topLight && !botLight:
				b.WriteRune('▀')
			case !topLight && botLight:
				b.WriteRune('▄')
			default:
				b.WriteRune(' ')
			}
		}
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// RenderQRSVG renders text as a crisp self-contained SVG QR code with a 4-module white quiet zone.
func RenderQRSVG(text string) (string, error) {
	m, err := EncodeQRMatrix(text, 4)
	if err != nil {
		return "", err
	}
	size := len(m)
	var d strings.Builder
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if m[y][x] {
				fmt.Fprintf(&d, "M%d,%dh1v1h-1z", x, y)
			}
		}
	}
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges"><rect width="%d" height="%d" fill="#ffffff"/><path d="%s" fill="#000000"/></svg>`,
		size, size, size, size, d.String()), nil
}

func applyQRMaskAndFormat(mod, fn [][]bool, mask int) [][]bool {
	n := len(mod)
	out := make([][]bool, n)
	for y := 0; y < n; y++ {
		out[y] = make([]bool, n)
		for x := 0; x < n; x++ {
			v := mod[y][x]
			if !fn[y][x] && qrMaskBit(mask, x, y) {
				v = !v
			}
			out[y][x] = v
		}
	}
	// Format bits for ECC-M (indicator 00) + 3-bit mask -> 15-bit BCH(15,5) XOR 0x5412.
	data := (0b00 << 3) | mask
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	bits := ((data << 10) | rem) ^ 0x5412

	bit := func(i int) bool { return ((bits >> i) & 1) != 0 }
	for i := 0; i <= 5; i++ {
		out[i][8] = bit(i)
	}
	out[7][8] = bit(6)
	out[8][8] = bit(7)
	out[8][7] = bit(8)
	for i := 9; i < 15; i++ {
		out[8][14-i] = bit(i)
	}
	for i := 0; i < 8; i++ {
		out[8][n-1-i] = bit(i)
	}
	for i := 8; i < 15; i++ {
		out[n-15+i][8] = bit(i)
	}
	out[n-8][8] = true
	return out
}

func qrMaskBit(mask, x, y int) bool {
	switch mask {
	case 0:
		return (x+y)%2 == 0
	case 1:
		return y%2 == 0
	case 2:
		return x%3 == 0
	case 3:
		return (x+y)%3 == 0
	case 4:
		return (y/2+x/3)%2 == 0
	case 5:
		return (x*y)%2+(x*y)%3 == 0
	case 6:
		return ((x*y)%2+(x*y)%3)%2 == 0
	case 7:
		return ((x+y)%2+(x*y)%3)%2 == 0
	default:
		return false
	}
}

func qrPenalty(g [][]bool) int {
	n := len(g)
	pen := 0
	// Rule 1: adjacent modules in row/col with same color.
	for y := 0; y < n; y++ {
		run := 1
		for x := 1; x < n; x++ {
			if g[y][x] == g[y][x-1] {
				run++
				if run == 5 {
					pen += 3
				} else if run > 5 {
					pen++
				}
			} else {
				run = 1
			}
		}
	}
	for x := 0; x < n; x++ {
		run := 1
		for y := 1; y < n; y++ {
			if g[y][x] == g[y-1][x] {
				run++
				if run == 5 {
					pen += 3
				} else if run > 5 {
					pen++
				}
			} else {
				run = 1
			}
		}
	}
	// Rule 2: 2x2 blocks of same color.
	for y := 0; y < n-1; y++ {
		for x := 0; x < n-1; x++ {
			c := g[y][x]
			if g[y][x+1] == c && g[y+1][x] == c && g[y+1][x+1] == c {
				pen += 3
			}
		}
	}
	// Rule 3: finder-like 1:1:3:1:1 patterns.
	hasPattern := func(get func(int) bool) int {
		hits := 0
		for i := 0; i <= n-11; i++ {
			// 10111010000 or 00001011101
			if get(i) && !get(i+1) && get(i+2) && get(i+3) && get(i+4) && !get(i+5) && get(i+6) && !get(i+7) && !get(i+8) && !get(i+9) && !get(i+10) {
				hits += 40
			}
			if !get(i) && !get(i+1) && !get(i+2) && !get(i+3) && get(i+4) && !get(i+5) && get(i+6) && get(i+7) && get(i+8) && !get(i+9) && get(i+10) {
				hits += 40
			}
		}
		return hits
	}
	for y := 0; y < n; y++ {
		row := g[y]
		pen += hasPattern(func(i int) bool { return row[i] })
	}
	for x := 0; x < n; x++ {
		col := x
		pen += hasPattern(func(i int) bool { return g[i][col] })
	}
	// Rule 4: balance of dark modules.
	dark := 0
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if g[y][x] {
				dark++
			}
		}
	}
	total := n * n
	k := absInt(dark*20-total*10) / total
	pen += k * 10
	return pen
}

func gfMul(x, y byte) byte {
	var z byte
	for i := 7; i >= 0; i-- {
		z = (z << 1) ^ ((z >> 7) * 0x1D)
		z ^= ((y >> i) & 1) * x
	}
	return z
}

func rsGeneratorPoly(degree int) []byte {
	res := make([]byte, degree)
	res[degree-1] = 1
	root := byte(1)
	for i := 0; i < degree; i++ {
		for j := 0; j < degree; j++ {
			res[j] = gfMul(res[j], root)
			if j+1 < degree {
				res[j] ^= res[j+1]
			}
		}
		root = gfMul(root, 2)
	}
	return res
}

func rsRemainder(data, gen []byte) []byte {
	res := make([]byte, len(gen))
	for _, b := range data {
		factor := b ^ res[0]
		copy(res, res[1:])
		res[len(res)-1] = 0
		for i, coef := range gen {
			res[i] ^= gfMul(coef, factor)
		}
	}
	return res
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
