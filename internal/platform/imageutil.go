package platform

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
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

func DecodeImageReader(reader io.Reader) (image.Image, string, error) {
	img, format, err := image.Decode(reader)
	return img, format, err
}

func CreateThumbnail(src image.Image, dstPath string, maxWidth int) error {
	thumb := ResizeToMaxWidth(src, maxWidth)
	if thumb == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
		return err
	}
	file, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer file.Close()
	return jpeg.Encode(file, thumb, &jpeg.Options{Quality: 82})
}

func CreateThumbnailJPEG(src image.Image, maxWidth int) ([]byte, error) {
	thumb := ResizeToMaxWidth(src, maxWidth)
	if thumb == nil {
		return nil, nil
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, thumb, &jpeg.Options{Quality: 82}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func CreateDetectionThumbnailJPEG(src image.Image, maxWidth int, detections []Detection) ([]byte, error) {
	thumb := ResizeToMaxWidth(src, maxWidth)
	if thumb == nil {
		return nil, nil
	}
	if len(detections) > 0 {
		thumb = DrawDetections(thumb, src.Bounds(), detections)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, thumb, &jpeg.Options{Quality: 86}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func DrawDetections(src image.Image, originalBounds image.Rectangle, detections []Detection) image.Image {
	bounds := src.Bounds()
	dst := image.NewRGBA(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			dst.Set(x, y, src.At(x, y))
		}
	}
	origWidth := float64(originalBounds.Dx())
	origHeight := float64(originalBounds.Dy())
	if origWidth <= 0 || origHeight <= 0 {
		return dst
	}
	scaleX := float64(bounds.Dx()) / origWidth
	scaleY := float64(bounds.Dy()) / origHeight
	red := color.RGBA{R: 255, A: 255}
	thickness := max(2, bounds.Dx()/180)
	for _, det := range detections {
		box := det.Box
		x1 := int(box.X * scaleX)
		y1 := int(box.Y * scaleY)
		x2 := int((box.X + box.Width) * scaleX)
		y2 := int((box.Y + box.Height) * scaleY)
		drawRect(dst, clamp(x1, 0, bounds.Dx()-1), clamp(y1, 0, bounds.Dy()-1), clamp(x2, 0, bounds.Dx()-1), clamp(y2, 0, bounds.Dy()-1), thickness, red)
	}
	return dst
}

func drawRect(img *image.RGBA, x1 int, y1 int, x2 int, y2 int, thickness int, c color.RGBA) {
	if x2 < x1 {
		x1, x2 = x2, x1
	}
	if y2 < y1 {
		y1, y2 = y2, y1
	}
	if x2-x1 < 2 || y2-y1 < 2 {
		return
	}
	for t := 0; t < thickness; t++ {
		for x := x1; x <= x2; x++ {
			img.SetRGBA(x, y1+t, c)
			img.SetRGBA(x, y2-t, c)
		}
		for y := y1; y <= y2; y++ {
			img.SetRGBA(x1+t, y, c)
			img.SetRGBA(x2-t, y, c)
		}
	}
}

func ResizeToMaxWidth(src image.Image, maxWidth int) image.Image {
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
	return dst
}

func clamp(value int, minValue int, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
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
