package platform

import (
	"image"
	"path/filepath"
	"strings"
)

type GeneratedTag struct {
	Tag        string
	Confidence float64
}

func GenerateTags(img image.Image, originalName string) []GeneratedTag {
	tags := []GeneratedTag{}
	name := strings.ToLower(filepath.Base(originalName))
	for keyword, tag := range map[string]string{
		"car": "vehicle", "vehicle": "vehicle", "road": "road",
		"campus": "campus", "factory": "factory", "person": "person",
		"door": "entrance", "lab": "lab", "device": "device",
	} {
		if strings.Contains(name, keyword) {
			tags = append(tags, GeneratedTag{Tag: tag, Confidence: 0.92})
		}
	}
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	if width > height {
		tags = append(tags, GeneratedTag{Tag: "wide-view", Confidence: 0.76})
	} else {
		tags = append(tags, GeneratedTag{Tag: "portrait-view", Confidence: 0.74})
	}
	tags = append(tags, dominantColorTag(img))
	return dedupeTags(tags)
}

func dominantColorTag(img image.Image) GeneratedTag {
	bounds := img.Bounds()
	var r, g, b uint64
	var count uint64
	stepX := max(1, bounds.Dx()/40)
	stepY := max(1, bounds.Dy()/40)
	for y := bounds.Min.Y; y < bounds.Max.Y; y += stepY {
		for x := bounds.Min.X; x < bounds.Max.X; x += stepX {
			rr, gg, bb, _ := img.At(x, y).RGBA()
			r += uint64(rr >> 8)
			g += uint64(gg >> 8)
			b += uint64(bb >> 8)
			count++
		}
	}
	if count == 0 {
		return GeneratedTag{Tag: "unknown-color", Confidence: 0.5}
	}
	r /= count
	g /= count
	b /= count
	if g > r && g > b {
		return GeneratedTag{Tag: "green-scene", Confidence: 0.68}
	}
	if b > r && b > g {
		return GeneratedTag{Tag: "blue-scene", Confidence: 0.68}
	}
	if r > 170 && g > 150 && b > 120 {
		return GeneratedTag{Tag: "bright-scene", Confidence: 0.7}
	}
	return GeneratedTag{Tag: "mixed-scene", Confidence: 0.62}
}

func dedupeTags(tags []GeneratedTag) []GeneratedTag {
	seen := map[string]bool{}
	result := []GeneratedTag{}
	for _, tag := range tags {
		if seen[tag.Tag] {
			continue
		}
		seen[tag.Tag] = true
		result = append(result, tag)
	}
	return result
}
