// Command genicon rasterizes the Collie app icon (web/public/icon.svg) to the
// PNG sizes needed by the web manifest and iOS. Run: go run ./tools/genicon
package main

import (
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
)

type rgba struct{ r, g, b float64 }

var (
	bg    = rgba{0x1a, 0x1b, 0x1f}
	amber = rgba{0xe3, 0xa8, 0x57}
	cream = rgba{0xf2, 0xe6, 0xd3}
)

// shade returns the colour and coverage of the icon at (x, y) in 64-unit space.
func shade(x, y float64) (rgba, float64, bool) {
	for i, cx := range []float64{22, 32, 42} {
		if math.Hypot(x-cx, y-44) <= 4 {
			if i == 1 {
				return cream, 1, true
			}
			return amber, 1, true
		}
	}
	// Arch: upper half of an ellipse centred at (32,40), rx=18, ry=22, stroke 5, round caps.
	dx, dy := (x-32)/18, (y-40)/22
	d := (math.Sqrt(dx*dx+dy*dy) - 1) * 20
	if y <= 40 && math.Abs(d) <= 2.5 {
		return amber, 1, true
	}
	if math.Hypot(x-14, y-40) <= 2.5 || math.Hypot(x-50, y-40) <= 2.5 {
		return amber, 1, true
	}
	// Rounded background square (radius 14).
	qx := math.Max(math.Abs(x-32)-(32-14), 0)
	qy := math.Max(math.Abs(y-32)-(32-14), 0)
	if math.Hypot(qx, qy) <= 14 {
		return bg, 1, true
	}
	return rgba{}, 0, false
}

func render(size int, path string) {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	const ss = 4
	scale := 64 / float64(size)
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/ss) * scale
					y := (float64(py) + (float64(sy)+0.5)/ss) * scale
					c, cov, ok := shade(x, y)
					if ok {
						r += c.r * cov
						g += c.g * cov
						b += c.b * cov
						a += cov
					}
				}
			}
			n := float64(ss * ss)
			if a > 0 {
				img.Set(px, py, color.NRGBA{uint8(r / a), uint8(g / a), uint8(b / a), uint8(255 * a / n)})
			}
		}
	}
	f, err := os.Create(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		log.Fatal(err)
	}
}

func main() {
	out := "web/public"
	for _, s := range []int{192, 512} {
		render(s, filepath.Join(out, "icon-"+itoa(s)+".png"))
	}
	render(180, filepath.Join(out, "apple-touch-icon.png"))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
