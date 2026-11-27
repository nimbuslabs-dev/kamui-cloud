package connect

import (
	"fmt"
	"strings"
)

// Pure-Go Byte-mode QR Code generator (Versions 1..8, Error Correction Level L).
// Supports payloads up to 192 bytes, sufficient for Kamui pairing URLs and tokens.

type qrVersion struct {
	ver        int
	size       int
	dataCW     int
	ecCW       int
	alignments []int
}

var qrVersions = []qrVersion{
	{1, 21, 19, 7, nil},
	{2, 25, 34, 10, []int{6, 18}},
	{3, 29, 55, 15, []int{6, 22}},
	{4, 33, 80, 20, []int{6, 26}},
	{5, 37, 108, 26, []int{6, 30}},
	{6, 41, 136, 18, []int{6, 34}}, // 2 blocks of (68 data, 18 ec) -> handle single-block versions or split cleanly
}

// Single-block EC-L versions up to Version 5 (106 byte payload) plus 2-block Version 6-8 if needed.
// Actually Version 5 EC-L holds 106 UTF-8 bytes; Version 7-L (single block? No, 2 blocks of 78) -> let's support 1-block AND multi-block cleanly!
type qrSpec struct {
	ver        int
	size       int
	blocks     int
	dataPerBlk int
	extraBlks  int // group 2 blocks with dataPerBlk+1
	ecPerBlk   int
	alignments []int
}

var qrSpecs = []qrSpec{
	{1, 21, 1, 19, 0, 7, nil},
	{2, 25, 1, 34, 0, 10, []int{6, 18}},
	{3, 29, 1, 55, 0, 15, []int{6, 22}},
	{4, 33, 1, 80, 0, 20, []int{6, 26}},
	{5, 37, 1, 108, 0, 26, []int{6, 30}},
	{6, 41, 2, 68, 0, 18, []int{6, 34}},
	{7, 45, 2, 78, 0, 20, []int{6, 22, 38}},
	{8, 49, 2, 97, 0, 24, []int{6, 24, 42}},
	{9, 53, 2, 116, 0, 30, []int{6, 26, 46}},
	{10, 57, 4, 68, 2, 18, []int{6, 28, 50}},
}

var gfExp [512]byte
var gfLog [256]byte

func init() {
	x := 1
	for i := range 255 {
		gfExp[i] = byte(x)
		gfLog[x] = byte(i)
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11d
		}
	}
	for i := 255; i < 512; i++ {
		gfExp[i] = gfExp[i-255]
	}
}

func gfMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[int(gfLog[a])+int(gfLog[b])]
}

func rsGenerator(degree int) []byte {
	poly := []byte{1}
	for i := range degree {
		next := make([]byte, len(poly)+1)
		factor := gfExp[i]
		for j, c := range poly {
			next[j] ^= c
			next[j+1] ^= gfMul(c, factor)
		}
		poly = next
	}
	return poly
}

func rsRemainder(data []byte, ecLen int) []byte {
	gen := rsGenerator(ecLen)
	rem := make([]byte, ecLen)
	for _, b := range data {
		factor := b ^ rem[0]
		copy(rem, rem[1:])
		rem[ecLen-1] = 0
		if factor != 0 {
			for j := range ecLen {
				rem[j] ^= gfMul(gen[j+1], factor)
			}
		}
	}
	return rem
}

func EncodeQR(payload string) ([][]bool, error) {
	raw := []byte(payload)
	var spec *qrSpec
	for i := range qrSpecs {
		s := &qrSpecs[i]
		totalData := (s.blocks-s.extraBlks)*s.dataPerBlk + s.extraBlks*(s.dataPerBlk+1)
		headerBits := 4 + 8
		if s.ver >= 10 {
			headerBits = 4 + 16
		}
		if (headerBits+len(raw)*8+7)/8 <= totalData {
			spec = s
			break
		}
	}
	if spec == nil {
		return nil, fmt.Errorf("QR payload too long (%d bytes)", len(raw))
	}

	totalData := (spec.blocks-spec.extraBlks)*spec.dataPerBlk + spec.extraBlks*(spec.dataPerBlk+1)
	bits := make([]bool, 0, totalData*8)
	appendBits := func(val, count int) {
		for i := count - 1; i >= 0; i-- {
			bits = append(bits, (val>>i)&1 == 1)
		}
	}
	appendBits(0b0100, 4) // Byte mode
	if spec.ver >= 10 {
		appendBits(len(raw), 16)
	} else {
		appendBits(len(raw), 8)
	}
	for _, b := range raw {
		appendBits(int(b), 8)
	}
	for i := 0; i < 4 && len(bits) < totalData*8; i++ {
		bits = append(bits, false)
	}
	for len(bits)%8 != 0 {
		bits = append(bits, false)
	}
	cw := make([]byte, len(bits)/8)
	for i, bit := range bits {
		if bit {
			cw[i/8] |= 1 << (7 - i%8)
		}
	}
	pad := []byte{0xEC, 0x11}
	for i := 0; len(cw) < totalData; i++ {
		cw = append(cw, pad[i%2])
	}

	g1 := spec.blocks - spec.extraBlks
	dataBlocks := make([][]byte, spec.blocks)
	ecBlocks := make([][]byte, spec.blocks)
	off := 0
	for i := range spec.blocks {
		blen := spec.dataPerBlk
		if i >= g1 {
			blen++
		}
		dataBlocks[i] = cw[off : off+blen]
		ecBlocks[i] = rsRemainder(dataBlocks[i], spec.ecPerBlk)
		off += blen
	}

	finalBytes := make([]byte, 0, totalData+spec.blocks*spec.ecPerBlk)
	maxDataLen := spec.dataPerBlk
	if spec.extraBlks > 0 {
		maxDataLen++
	}
	for c := range maxDataLen {
		for b := range spec.blocks {
			if c < len(dataBlocks[b]) {
				finalBytes = append(finalBytes, dataBlocks[b][c])
			}
		}
	}
	for c := range spec.ecPerBlk {
		for b := range spec.blocks {
			finalBytes = append(finalBytes, ecBlocks[b][c])
		}
	}

	n := spec.size
	modules := make([][]bool, n)
	reserved := make([][]bool, n)
	for i := range n {
		modules[i] = make([]bool, n)
		reserved[i] = make([]bool, n)
	}
	setFunc := func(r, c int, dark bool) {
		modules[r][c] = dark
		reserved[r][c] = true
	}
	drawFinder := func(top, left int) {
		for r := -1; r <= 7; r++ {
			for c := -1; c <= 7; c++ {
				rr, cc := top+r, left+c
				if rr < 0 || rr >= n || cc < 0 || cc >= n {
					continue
				}
				dark := r >= 0 && r <= 6 && c >= 0 && c <= 6 &&
					(r == 0 || r == 6 || c == 0 || c == 6 || (r >= 2 && r <= 4 && c >= 2 && c <= 4))
				setFunc(rr, cc, dark)
			}
		}
	}
	drawFinder(0, 0)
	drawFinder(0, n-7)
	drawFinder(n-7, 0)

	for _, r := range spec.alignments {
		for _, c := range spec.alignments {
			if reserved[r][c] {
				continue
			}
			for dr := -2; dr <= 2; dr++ {
				for dc := -2; dc <= 2; dc++ {
					dark := dr == -2 || dr == 2 || dc == -2 || dc == 2 || (dr == 0 && dc == 0)
					setFunc(r+dr, c+dc, dark)
				}
			}
		}
	}
	for i := 8; i < n-8; i++ {
		setFunc(6, i, i%2 == 0)
		setFunc(i, 6, i%2 == 0)
	}
	setFunc(n-8, 8, true) // Dark module
	for i := range 9 {
		if i != 6 {
			reserved[8][i] = true
			reserved[i][8] = true
		}
	}
	for i := range 8 {
		reserved[8][n-1-i] = true
		reserved[n-1-i][8] = true
	}
	if spec.ver >= 7 {
		vBits := versionInfoBits(spec.ver)
		for i := range 18 {
			bit := (vBits>>i)&1 == 1
			setFunc(i/3, n-11+i%3, bit)
			setFunc(n-11+i%3, i/3, bit)
		}
	}

	// Place data bits in upward/downward zigzag columns.
	bitIdx := 0
	totalBits := len(finalBytes) * 8
	upward := true
	for right := n - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for step := range n {
			r := step
			if upward {
				r = n - 1 - step
			}
			for _, c := range []int{right, right - 1} {
				if reserved[r][c] {
					continue
				}
				var dark bool
				if bitIdx < totalBits {
					dark = (finalBytes[bitIdx/8]>>(7-bitIdx%8))&1 == 1
				}
				modules[r][c] = dark
				bitIdx++
			}
		}
		upward = !upward
	}

	// Evaluate masks 0..7 and select lowest penalty.
	bestMask := 0
	bestPenalty := 1 << 30
	var best [][]bool
	for mask := range 8 {
		cand := make([][]bool, n)
		for r := range n {
			cand[r] = make([]bool, n)
			for c := range n {
				dark := modules[r][c]
				if !reserved[r][c] && qrMask(mask, r, c) {
					dark = !dark
				}
				cand[r][c] = dark
			}
		}
		applyFormatInfo(cand, mask)
		p := qrPenalty(cand)
		if p < bestPenalty {
			bestPenalty = p
			bestMask = mask
			best = cand
		}
	}
	_ = bestMask
	return best, nil
}

func qrMask(mask, r, c int) bool {
	switch mask {
	case 0:
		return (r+c)%2 == 0
	case 1:
		return r%2 == 0
	case 2:
		return c%3 == 0
	case 3:
		return (r+c)%3 == 0
	case 4:
		return (r/2+c/3)%2 == 0
	case 5:
		return (r*c)%2+(r*c)%3 == 0
	case 6:
		return ((r*c)%2+(r*c)%3)%2 == 0
	default:
		return ((r+c)%2+(r*c)%3)%2 == 0
	}
}

func applyFormatInfo(m [][]bool, mask int) {
	// EC level L = 01
	data := (0b01 << 3) | mask
	rem := data << 10
	for i := 14; i >= 10; i-- {
		if (rem>>i)&1 == 1 {
			rem ^= 0x537 << (i - 10)
		}
	}
	bits := ((data << 10) | rem) ^ 0x5412
	n := len(m)
	for i := range 15 {
		bit := (bits>>i)&1 == 1
		if i < 6 {
			m[8][i] = bit
		} else if i < 8 {
			m[8][i+1] = bit
		} else {
			m[8][n-15+i] = bit
		}
		if i < 8 {
			m[n-1-i][8] = bit
		} else if i < 9 {
			m[15-i][8] = bit
		} else {
			m[14-i][8] = bit
		}
	}
}

func versionInfoBits(ver int) int {
	rem := ver << 12
	for i := 17; i >= 12; i-- {
		if (rem>>i)&1 == 1 {
			rem ^= 0x1F25 << (i - 12)
		}
	}
	return (ver << 12) | rem
}

func qrPenalty(m [][]bool) int {
	n := len(m)
	penalty := 0
	for r := range n {
		run := 1
		for c := 1; c < n; c++ {
			if m[r][c] == m[r][c-1] {
				run++
				if run == 5 {
					penalty += 3
				} else if run > 5 {
					penalty++
				}
			} else {
				run = 1
			}
		}
	}
	for c := range n {
		run := 1
		for r := 1; r < n; r++ {
			if m[r][c] == m[r-1][c] {
				run++
				if run == 5 {
					penalty += 3
				} else if run > 5 {
					penalty++
				}
			} else {
				run = 1
			}
		}
	}
	return penalty
}

func RenderQRTerminal(matrix [][]bool) string {
	n := len(matrix)
	if n == 0 {
		return ""
	}
	isDark := func(r, c int) bool {
		if r < 0 || r >= n || c < 0 || c >= n {
			return false
		}
		return matrix[r][c]
	}
	var b strings.Builder
	for r := -2; r < n+2; r += 2 {
		for c := -2; c < n+2; c++ {
			top := isDark(r, c)
			bot := isDark(r+1, c)
			switch {
			case top && bot:
				b.WriteRune('█')
			case top && !bot:
				b.WriteRune('▀')
			case !top && bot:
				b.WriteRune('▄')
			default:
				b.WriteRune(' ')
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func RenderQRSVG(matrix [][]bool) string {
	n := len(matrix)
	if n == 0 {
		return ""
	}
	total := n + 4
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges"><rect width="100%%" height="100%%" fill="#fff"/><path fill="#111" d="`, total, total)
	for r := range n {
		for c := range n {
			if matrix[r][c] {
				fmt.Fprintf(&b, "M%d,%dh1v1h-1z", c+2, r+2)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String()
}
