package platform

import (
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"

	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

func DecodeImage(path string) (image.Image, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	img, format, err := image.Decode(file)
	return img, format, err
}

func CreateThumbnail(src image.Image, dstPath string, maxWidth int) error {
	bounds := src.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	if width <= 0 || height <= 0 {
		return nil
	}
	targetWidth := maxWidth
	if width < targetWidth {
		targetWidth = width
	}
	targetHeight := height * targetWidth / width
	if targetHeight < 1 {
		targetHeight = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	for y := 0; y < targetHeight; y++ {
		for x := 0; x < targetWidth; x++ {
			srcX := bounds.Min.X + x*width/targetWidth
			srcY := bounds.Min.Y + y*height/targetHeight
			dst.Set(x, y, src.At(srcX, srcY))
		}
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
		return err
	}
	file, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer file.Close()
	return jpeg.Encode(file, dst, &jpeg.Options{Quality: 82})
}

func DetectFormatFromName(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".jpg", ".jpeg":
		return "jpeg"
	case ".png":
		return "png"
	default:
		return strings.TrimPrefix(ext, ".")
	}
}

func EncodeSamplePNG(path string, c color.RGBA) error {
	img := image.NewRGBA(image.Rect(0, 0, 640, 420))
	for y := 0; y < 420; y++ {
		for x := 0; x < 640; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8((int(c.R) + x/5) % 255),
				G: uint8((int(c.G) + y/4) % 255),
				B: uint8((int(c.B) + (x+y)/9) % 255),
				A: 255,
			})
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return png.Encode(file, img)
}
