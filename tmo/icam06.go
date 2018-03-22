package tmo

import (
	"image"
	"image/color"
	"math"
	"sort"

	colorful "github.com/lucasb-eyer/go-colorful"
	"github.com/mdouchement/hdr"
	"github.com/mdouchement/hdr/filter"
	"github.com/mdouchement/hdr/hdrcolor"
	"github.com/mdouchement/hdr/util"
)

const (
	maxLum         = 20000 // maximum luminance(cd/m2)
	surroundFactor = 1     // F=1 in an average surround
)

var (
	xyzD65   = []float64{96.047, 100, 108.883} // D65 white point in XYZ
	cat02D65 []float64                         // D65 white point in CAT02
)

func init() {
	l, m, s := hdrcolor.XyzToLmsMcat02(xyzD65[0], xyzD65[1], xyzD65[2])
	cat02D65 = []float64{l, m, s}
}

// A ICam06 is a TMO implementation based on
// Mark D. Fairchild, Jiangtao Kuang and Garrett M. Johnson's 2006 white paper.
//
// Reference:
// iCAM for high-dynamic-range image rendering.
// Mark D. Fairchild, Jiangtao Kuang and Garrett M. Johnson.
// In SIGGRAPH '06 ACM SIGGRAPH 2006 Research posters. Article No. 185
// http://rit-mcsl.org/fairchild/PDFs/PAP26.pdf
type ICam06 struct {
	HDRImage       hdr.Image
	Contrast       float64
	MinClipping    float64
	MaxClipping    float64
	width          int
	height         int
	maxLum         float64
	normalized     hdr.Image
	baseLayer      hdr.Image
	white          hdr.Image
	detailCombined hdr.Image
}

// NewDefaultICam06 instanciates a new ICam06 TMO with default parameters.
func NewDefaultICam06(m hdr.Image) *ICam06 {
	return NewICam06(m, 0.75, 0.01, 0.99)
}

// NewICam06 instanciates a new ICam06 TMO.
func NewICam06(m hdr.Image, contrast, minClipping, maxClipping float64) *ICam06 {
	return &ICam06{
		HDRImage:    m,
		Contrast:    util.Clamp(0.6, 0.85, contrast),
		MinClipping: util.Clamp(0, 1, minClipping),
		MaxClipping: util.Clamp(0, 1, maxClipping),
		width:       m.Bounds().Dx(),
		height:      m.Bounds().Dy(),
	}
}

// Perform runs the TMO mapping.
func (t *ICam06) Perform() image.Image {
	// Note: Section & Equation numbers come from the PDF paper.
	//
	// Input normalization
	t.luminance()
	t.normalized = filter.NewApply1(t.HDRImage, func(c1 hdrcolor.Color, _ hdrcolor.Color) hdrcolor.Color {
		x, y, z, _ := c1.HDRXYZA()
		return hdrcolor.XYZ{
			X: util.Clamp(0.00000001, maxLum, (x/t.maxLum)*maxLum),
			Y: util.Clamp(0.00000001, maxLum, (y/t.maxLum)*maxLum),
			Z: util.Clamp(0.00000001, maxLum, (z/t.maxLum)*maxLum),
		}
	})
	//
	// Decomposing the image into base layer  - Section 2.2
	log := filter.NewLog10(t.normalized)
	bilateral := filter.NewYFastBilateralAuto(log) // Better blur with log10 values
	bilateral.SigmaSpace = float64(t.minDim()) * 0.02
	bilateral.Perform()
	t.baseLayer = filter.NewPow10(bilateral) // bilateral filter + un-log10 values
	//
	//
	// Chromatic adaptation (White adaptation) - Section 2.3
	t.white = filter.FastGaussian(t.normalized, (t.maxDim() / 2))
	//
	//
	// Non-linear tone compression - Section 2.4
	toneCompressed := t.toneCompression() // with chromatic adaptation included
	//
	//
	// Decomposing the image into detail layer - Section 2.2 & 2.6
	detailLayer := filter.NewApply2(t.normalized, t.baseLayer, func(c1, c2 hdrcolor.Color) hdrcolor.Color {
		x1, y1, z1, _ := c1.HDRXYZA()
		x2, y2, z2, _ := c2.HDRXYZA()

		// Details Layer
		X := t.clampToZero(x1 / x2)
		Y := t.clampToZero(y1 / y2)
		Z := t.clampToZero(z1 / z2)

		// Stevenson Detail Enhancement - Equation 24
		La := 0.2 * y2                                                                                // Luminance Adaptation
		k := 1.0 / (5*La + 1.0)                                                                       // Equation 14
		fl := 0.2*math.Pow(k, 4)*(5*La) + 0.1*math.Pow(1.0-math.Pow(k, 4), 2)*math.Pow(5*La, 1.0/3.0) // Equation 13
		exponent := math.Pow(fl+0.8, 0.25)
		return hdrcolor.XYZ{
			X: math.Pow(X, exponent),
			Y: math.Pow(Y, exponent),
			Z: math.Pow(Z, exponent),
		}
	})
	//
	//
	// Image attribute adjustments - Section 2.6
	t.detailCombined = filter.NewApply2(toneCompressed, detailLayer, func(c1, c2 hdrcolor.Color) hdrcolor.Color {
		x1, y1, z1, _ := c1.HDRXYZA()
		x2, y2, z2, _ := c2.HDRXYZA()

		return hdrcolor.XYZ{
			X: x1 * x2,
			Y: y1 * y2,
			Z: z1 * z2,
		}
	})
	//
	//
	m := image.NewRGBA64(t.HDRImage.Bounds())
	t.normalize(m) // colorfullnessXsurround + ldr scale
	return m
}

func (t *ICam06) luminance() {
	maxCh := make(chan float64)

	completed := util.ParallelR(t.HDRImage.Bounds(), func(x1, y1, x2, y2 int) {
		var max float64

		for y := y1; y < y2; y++ {
			for x := x1; x < x2; x++ {
				_, lum, _, _ := t.HDRImage.HDRAt(x, y).HDRXYZA()

				max = math.Max(t.maxLum, lum)
			}
		}

		maxCh <- max
	})

	for {
		select {
		case <-completed:
			return
		case max := <-maxCh:
			t.maxLum = math.Max(t.maxLum, max)
		}
	}
}

func (t *ICam06) chromaticAdaptation(x, y int) (float64, float64, float64) {
	l1, m1, s1, _ := hdr.NewLMSCAT02w(t.baseLayer).HDRAt(x, y).HDRPixel()

	// FIXME StackBlur seems to have some side effects here (halo effect and red-ish image)
	l2, m2, s2, _ := hdr.NewLMSCAT02w(t.white).HDRAt(x, y).HDRPixel()
	la := 0.2 * m2

	// Use generic White adaptation values
	// l2, m2, s2 := 1.0, 1.0, 1.0
	// la := 1.0

	D := surroundFactor * (1.0 - (math.Exp(-(la+42)/92) / 3.6))

	return hdrcolor.LmsMcat02ToXyz(
		l1*(cat02D65[0]*D/l2+(1.0-D)),
		m1*(cat02D65[1]*D/m2+(1.0-D)),
		s1*(cat02D65[2]*D/s2+(1.0-D)),
	)
}

// Aka tonemap.
func (t *ICam06) toneCompression() hdr.Image {
	toneCompressed := t.white.(hdr.ImageSet) // Reuse memory allocation by updating in-place the raster

	Sw := math.Inf(-1) // Global scale from the maximum value of the local adapted white point image
	for y := 0; y < t.height; y++ {
		for x := 0; x < t.width; x++ {
			_, Yw, _, _ := t.white.HDRAt(x, y).HDRXYZA()
			Sw = math.Max(Sw, Yw)
		}
	}

	completed := util.ParallelR(t.HDRImage.Bounds(), func(x1, y1, x2, y2 int) {
		for y := y1; y < y2; y++ {
			for x := x1; x < x2; x++ {
				_, Yw, _, _ := t.white.HDRAt(x, y).HDRXYZA() // Yw is the luminance of the local adapted white image

				//
				// Cone response
				//

				La := 0.2 * Yw                                                                                // Luminance Adaptation
				k := 1.0 / (5*La + 1.0)                                                                       // Equation 14
				fl := 0.2*math.Pow(k, 4)*(5*La) + 0.1*math.Pow(1.0-math.Pow(k, 4), 2)*math.Pow(5*La, 1.0/3.0) // Equation 13

				//
				// compression
				//

				Xca, Yca, Zca := t.chromaticAdaptation(x, y)
				l, m, s := hdrcolor.XyzToLmsMhpe(Xca, Yca, Zca) // Equation 9

				S := math.Abs(Yca) // Luminance of each pixel in the chromatic adapted image - FIXME Should be Yca according to the paper

				pow := math.Pow(fl*l/Yw, t.Contrast)
				l = ((400 * pow) / (27.13 + pow)) + 0.1 // Equation 10

				pow = math.Pow(fl*m/Yw, t.Contrast)
				m = ((400 * pow) / (27.13 + pow)) + 0.1 // Equation 11

				pow = math.Pow(fl*s/Yw, t.Contrast)
				s = ((400 * pow) / (27.13 + pow)) + 0.1 // Equation 12

				//
				// Make a neutral As Rod response
				//

				// Las := 2.26 * La                    // Equation 17, scotopic luminance factor
				// Let say: Lls = 5*Las/2.26
				Lls := 5 * La

				j := 0.00001 / (Lls + 0.00001)                                          // Equation 18
				FLS := 3800*(j*j)*Lls + 0.2*math.Pow(1-(j*j), 4)*math.Pow(Lls, 1.0/6.0) // Equation 16, scotopic luminance level adaptation factor

				St := S / Sw
				// BS is the rod pigment bleach or satruration factor
				Bs := 0.5/(1+0.3*math.Pow(Lls*St, 0.3)) + 0.5/(1+5*Lls) // Equation 19
				// Noise term in Rod response is 1 / 3 of that in Cone response because Rods are more sensitive
				pow = math.Pow(FLS*St, t.Contrast)
				As := 3.05*Bs*(400*pow/(27.13+pow)) + 0.3 // Equation 15, The rod response after adaptation

				//
				// Combine Cone and Rod response
				//
				Xtc, Ytc, Ztc := hdrcolor.LmsMhpeToXyz(l+As, m+As, s+As) // Equation 20 + HPE->XYZ

				toneCompressed.Set(x, y, hdrcolor.XYZ{X: Xtc, Y: Ytc, Z: Ztc})
			}
		}
	})

	<-completed

	t.white = nil // No longer used and was replaced by `toneCompressed` raster
	return toneCompressed.(hdr.Image)
}

func (t *ICam06) iptColor(x, y int) (float64, float64, float64) {
	X, Y, Z, _ := t.detailCombined.HDRAt(x, y).HDRXYZA()

	L, M, S := hdrcolor.XyzToLms(X, Y, Z) // D65 LMS - Equation 21

	// Apply gamma - Equation 22
	gamma := func(v float64) float64 {
		return math.Pow(math.Abs(v), 0.43)
	}

	return hdrcolor.LmsToIpt(gamma(L), gamma(M), gamma(S)) // Convert LMS to IPT color space - Equation 23
}

// aka Inverse CAT
func (t *ICam06) reverseIptColor(I, P, T float64) hdrcolor.Color {
	Lp, Mp, Sp := hdrcolor.IptToLms(I, P, T)

	// reverse gamma
	rGamma := func(v float64) float64 {
		return math.Pow(math.Abs(v), 1/0.43)
	}

	// D65 LMS - Equation 21
	X, Y, Z := hdrcolor.LmsToXyz(rGamma(Lp), rGamma(Mp), rGamma(Sp))

	return hdrcolor.XYZ{X: X, Y: Y, Z: Z}
}

func (t *ICam06) colorfullnessXsurround(x, y int) hdrcolor.Color {
	I, P, T := t.iptColor(x, y)

	_, Y, _, _ := t.baseLayer.HDRAt(x, y).HDRXYZA()
	La := Y * 0.2
	k := 1.0 / (5*La + 1.0)
	fl := 0.2*math.Pow(k, 4)*(5*La) + 0.1*math.Pow(1.0-math.Pow(k, 4), 2)*math.Pow(5*La, 1.0/3.0) // Equation 13

	// Hunt effect - Equation 25 & 26
	Chroma := math.Sqrt(P*P + T*T)
	scale := math.Pow(fl+1, 0.2) * (1.29*Chroma*Chroma - 0.27*Chroma + 0.42) / (Chroma*Chroma - 0.31*Chroma + 0.42)
	P *= scale
	T *= scale

	// Bartleson surround adjustment - Equation 27
	// To simplify/speedup the algorithm, the gamma is the average 1.0 so there is no need to compute the new I.

	return t.reverseIptColor(I, P, T) // Inverse CAT
}

func (t *ICam06) normalize(m *image.RGBA64) {
	norMaxLum := math.Inf(-1)
	maxCh := make(chan float64)

	completed := util.ParallelR(t.HDRImage.Bounds(), func(x1, y1, x2, y2 int) {
		var max float64

		for y := y1; y < y2; y++ {
			for x := x1; x < x2; x++ {
				_, lum, _, _ := t.colorfullnessXsurround(x, y).HDRXYZA() // FIXME perf-1

				max = math.Max(t.maxLum, lum)
			}
		}

		maxCh <- max
	})

	for {
		select {
		case <-completed:
			goto NEXT
		case max := <-maxCh:
			norMaxLum = math.Max(norMaxLum, max)
		}
	}
NEXT:

	normLum := func(x, y int) (r, g, b float64) {
		X, Y, Z, _ := t.colorfullnessXsurround(x, y).HDRXYZA() // FIXME perf-1

		// XYZ normalization
		X /= norMaxLum
		Y /= norMaxLum
		Z /= norMaxLum

		// RGB-space conversion
		return colorful.XyzToLinearRgb(X, Y, Z)
	}

	// Percentile
	size := t.HDRImage.Size()
	perc := make(percentiles, size*3) // FIXME high memory consumption => only 2 values are needed minRGB && maxRGB

	completed = util.ParallelR(t.HDRImage.Bounds(), func(x1, y1, x2, y2 int) {
		for y := y1; y < y2; y++ {
			for x := x1; x < x2; x++ {
				r, g, b := normLum(x, y)

				// Clipping, first part
				i := x * y
				perc[i] = r
				perc[size+i] = g
				perc[size*2+i] = b
			}
		}
	})

	<-completed

	perc.sort()
	minRGB := math.Min(perc.percentile(t.MinClipping), 0)
	maxRGB := perc.percentile(t.MaxClipping)

	completed = util.ParallelR(t.HDRImage.Bounds(), func(x1, y1, x2, y2 int) {
		for y := y1; y < y2; y++ {
			for x := x1; x < x2; x++ {
				r, g, b := normLum(x, y)

				// Clipping, second part
				r = util.Clamp(0, 1, (r-minRGB)/(maxRGB-minRGB))
				g = util.Clamp(0, 1, (g-minRGB)/(maxRGB-minRGB))
				b = util.Clamp(0, 1, (b-minRGB)/(maxRGB-minRGB))

				// RGB normalization
				m.SetRGBA64(x, y, color.RGBA64{
					R: t.normalizeC(r),
					G: t.normalizeC(g),
					B: t.normalizeC(b),
					A: RangeMax,
				})
			}
		}
	})

	<-completed
}

func (t *ICam06) normalizeC(channel float64) uint16 {
	// c := WoB(channel < -0.0031308) * (math.Pow(-1.055*-channel, 1/2.4) + 0.055)
	c := WoB((channel >= -0.0031308) && (channel <= 0.0031308)) * channel * 12.92
	c += WoB(channel > 0.0031308) * (math.Pow(channel, 1/2.4)*1.055 - 0.055)
	return uint16(RangeMax * c)
}

func (t *ICam06) clampToZero(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, -1) || math.IsInf(x, 1) {
		return 0
	}
	return x
}

func (t *ICam06) minDim() int {
	if t.width < t.height {
		return t.width
	}
	return t.height
}

func (t *ICam06) maxDim() int {
	if t.width > t.height {
		return t.width
	}
	return t.height
}

//-----------------//
// Percentile      //
//-----------------//

type percentiles []float64

func (p percentiles) sort() {
	sort.Sort(p)
}

func (p percentiles) percentile(clipping float64) float64 {
	n := float64(len(p))
	i := int(clipping * n)
	return float64(p[i])
}

func (p percentiles) Len() int {
	return len(p)
}

func (p percentiles) Less(i, j int) bool {
	return p[i] < p[j]
}

func (p percentiles) Swap(i, j int) {
	p[i], p[j] = p[j], p[i]
}
