package bee

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"strings"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	// DecodeConfig is checked before image.Decode. Keep this low enough that a
	// single RGBA decode remains bounded even though the compressed payload is
	// allowed up to the artifact limit. 3213x5712 is about 18.35MP.
	MaxSpecialistImageSourceDimension       = 32768
	MaxSpecialistImageSourcePixels    int64 = 25_000_000
)

// Image decoding and scaling is intentionally serialized separately from
// provider-call concurrency. The library decoders are not cancellable, so a
// single bounded worker is the safest CPU/RAM envelope.
var specialistImageProcessingSem = make(chan struct{}, 1)

func normalizeSpecialistImagePayload(ctx context.Context, source AttachmentArtifactResolver, ref AttachmentRef, maxBytes int64, maxDimension int, maxPixels ...int64) (Artifact, io.ReadCloser, error) {
	if source == nil {
		return Artifact{}, nil, errors.New("specialist attachment resolver is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if maxBytes <= 0 {
		maxBytes = int64(DefaultSpecialistImageMaxBytes)
	}
	if maxDimension <= 0 {
		maxDimension = DefaultSpecialistImageMaxDimension
	}
	outputPixels := int64(DefaultSpecialistImageMaxPixels)
	if len(maxPixels) > 0 && maxPixels[0] > 0 {
		outputPixels = maxPixels[0]
	}

	artifact, original, err := source.OpenAttachmentPayload(ctx, ref)
	if err != nil {
		if original != nil {
			err = errors.Join(err, original.Close())
		}
		return Artifact{}, nil, err
	}
	if original == nil {
		return Artifact{}, nil, errors.New("specialist attachment resolver returned a nil reader")
	}
	data, readErr := readSpecialistAttachment(ctx, original, maxBytes)
	closeErr := original.Close()
	if joined := errors.Join(readErr, closeErr); joined != nil {
		return Artifact{}, nil, joined
	}
	if int64(len(data)) > maxBytes {
		return Artifact{}, nil, fmt.Errorf("image attachment %q exceeds image_max_bytes limit of %d bytes", ref.Filename, maxBytes)
	}
	if !isImageAttachment(ref, artifact) {
		return transientSpecialistArtifact(artifact, artifact.MediaType, data), io.NopCloser(bytes.NewReader(data)), nil
	}

	normalized, mediaType, err := normalizeImageBytesContext(ctx, data, maxBytes, maxDimension, outputPixels)
	if err != nil {
		return Artifact{}, nil, fmt.Errorf("normalize image attachment %q: %w", ref.Filename, err)
	}
	return transientSpecialistArtifact(artifact, mediaType, normalized), io.NopCloser(bytes.NewReader(normalized)), nil
}

func readSpecialistAttachment(ctx context.Context, reader io.Reader, maxBytes int64) ([]byte, error) {
	if reader == nil {
		return nil, errors.New("image attachment payload reader is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, reader: reader}, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

func isImageAttachment(ref AttachmentRef, artifact Artifact) bool {
	return ref.Kind == "image" || artifact.Kind == "image" || strings.HasPrefix(strings.ToLower(strings.TrimSpace(ref.MediaType)), "image/") || strings.HasPrefix(strings.ToLower(strings.TrimSpace(artifact.MediaType)), "image/")
}

func transientSpecialistArtifact(source Artifact, mediaType string, data []byte) Artifact {
	digest := sha256.Sum256(data)
	originalSize := int64(len(data))
	if source.Size > originalSize {
		originalSize = source.Size
	}
	if source.OriginalSize > originalSize {
		originalSize = source.OriginalSize
	}
	result := source
	result.MediaType = mediaType
	if strings.HasPrefix(mediaType, "image/") {
		result.Kind = "image"
	}
	result.Size = int64(len(data))
	result.SHA256 = hex.EncodeToString(digest[:])
	result.OriginalSize = originalSize
	result.RetainedSize = int64(len(data))
	// These describe the transient provider-facing representation, not the
	// durable source artifact. The sidecar only returns after reading the full
	// bounded source, so neither truncation nor retention capping occurred.
	result.OriginalTruncated = false
	result.RetentionCapped = false
	return result
}

func normalizeImageBytes(data []byte, maxBytes int64, maxDimension int) ([]byte, string, error) {
	return normalizeImageBytesContext(context.Background(), data, maxBytes, maxDimension, int64(DefaultSpecialistImageMaxPixels))
}

func normalizeImageBytesContext(ctx context.Context, data []byte, maxBytes int64, maxDimension int, maxPixels int64) ([]byte, string, error) {
	if len(data) == 0 {
		return nil, "", errors.New("image attachment payload is empty")
	}
	if maxBytes <= 0 {
		return nil, "", errors.New("image_max_bytes must be positive")
	}
	if maxDimension <= 0 {
		maxDimension = DefaultSpecialistImageMaxDimension
	}
	if maxPixels <= 0 {
		maxPixels = int64(DefaultSpecialistImageMaxPixels)
	}
	if int64(len(data)) > maxBytes {
		return nil, "", fmt.Errorf("image attachment exceeds image_max_bytes limit of %d bytes", maxBytes)
	}
	if err := acquireImageProcessing(ctx); err != nil {
		return nil, "", err
	}
	defer releaseImageProcessing()

	if err := checkImageContext(ctx); err != nil {
		return nil, "", err
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("unsupported image format or invalid image metadata: %w", err)
	}
	if err := checkImageContext(ctx); err != nil {
		return nil, "", err
	}
	if config.Width <= 0 || config.Height <= 0 {
		return nil, "", errors.New("image has invalid dimensions")
	}
	if config.Width > MaxSpecialistImageSourceDimension || config.Height > MaxSpecialistImageSourceDimension {
		return nil, "", fmt.Errorf("image source dimensions %dx%d exceed the absolute limit of %d", config.Width, config.Height, MaxSpecialistImageSourceDimension)
	}
	pixels := int64(config.Width) * int64(config.Height)
	if pixels > MaxSpecialistImageSourcePixels {
		return nil, "", fmt.Errorf("image source dimensions %dx%d exceed the absolute pixel limit of %d", config.Width, config.Height, MaxSpecialistImageSourcePixels)
	}
	mediaType, err := imageFormatMediaType(format)
	if err != nil {
		return nil, "", err
	}

	orientation := 1
	if strings.EqualFold(format, "jpeg") || strings.EqualFold(format, "jpg") {
		orientation = jpegEXIFOrientation(data)
	}
	needsResize := config.Width > maxDimension || config.Height > maxDimension || pixels > maxPixels
	needsNormalization := needsResize || orientation != 1
	if !needsNormalization {
		return data, mediaType, nil
	}
	if strings.EqualFold(format, "gif") {
		animated, scanErr := gifHasMultipleFrames(data)
		if scanErr != nil {
			return nil, "", fmt.Errorf("inspect GIF animation: %w", scanErr)
		}
		if animated {
			return nil, "", errors.New("animated GIF resizing is unsupported; refusing to drop animation frames")
		}
	}
	if strings.EqualFold(format, "webp") && webpIsAnimated(data) {
		return nil, "", errors.New("animated WebP resizing is unsupported; refusing to drop animation frames")
	}
	if err := checkImageContext(ctx); err != nil {
		return nil, "", err
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("decode image: %w", err)
	}
	if err := checkImageContext(ctx); err != nil {
		return nil, "", err
	}
	if orientation != 1 {
		if err := checkImageContext(ctx); err != nil {
			return nil, "", err
		}
		decoded = applyJPEGOrientation(decoded, orientation)
		if err := checkImageContext(ctx); err != nil {
			return nil, "", err
		}
	}
	width, height := proportionalImageDimensionsWithPixels(decoded.Bounds().Dx(), decoded.Bounds().Dy(), maxDimension, maxPixels)
	if width != decoded.Bounds().Dx() || height != decoded.Bounds().Dy() {
		if err := checkImageContext(ctx); err != nil {
			return nil, "", err
		}
		decoded = resizeImage(decoded, width, height)
		if err := checkImageContext(ctx); err != nil {
			return nil, "", err
		}
	}
	for width >= 1 && height >= 1 {
		encoded, encodeErr := encodeBoundedImage(ctx, decoded, format, maxBytes)
		if encodeErr == nil {
			return encoded, resizedImageMediaType(format), nil
		}
		if width == 1 && height == 1 {
			return nil, "", encodeErr
		}
		width, height = reduceImageDimensions(width, height)
		if err := checkImageContext(ctx); err != nil {
			return nil, "", err
		}
		decoded = resizeImage(decoded, width, height)
		if err := checkImageContext(ctx); err != nil {
			return nil, "", err
		}
	}
	return nil, "", errors.New("image could not be resized within image_max_bytes")
}

func acquireImageProcessing(ctx context.Context) error {
	select {
	case specialistImageProcessingSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func releaseImageProcessing() { <-specialistImageProcessingSem }

func checkImageContext(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func imageFormatMediaType(format string) (string, error) {
	mediaType := encodedImageMediaType(format)
	if mediaType == "" {
		return "", fmt.Errorf("unsupported image format %q", format)
	}
	return mediaType, nil
}

func encodedImageMediaType(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "jpeg", "jpg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	default:
		return ""
	}
}

func resizedImageMediaType(format string) string {
	if strings.EqualFold(strings.TrimSpace(format), "jpeg") || strings.EqualFold(strings.TrimSpace(format), "jpg") {
		return "image/jpeg"
	}
	// Resized PNG, GIF, and WebP inputs are intentionally normalized to PNG.
	return "image/png"
}

func proportionalImageDimensions(width, height, maxDimension int) (int, int) {
	return proportionalImageDimensionsWithPixels(width, height, maxDimension, math.MaxInt64)
}

func proportionalImageDimensionsWithPixels(width, height, maxDimension int, maxPixels int64) (int, int) {
	if width <= 0 || height <= 0 {
		return 1, 1
	}
	if maxDimension <= 0 {
		maxDimension = DefaultSpecialistImageMaxDimension
	}
	if maxPixels <= 0 {
		maxPixels = math.MaxInt64
	}
	scale := 1.0
	if width > maxDimension || height > maxDimension {
		scale = math.Min(scale, math.Min(float64(maxDimension)/float64(width), float64(maxDimension)/float64(height)))
	}
	if area := int64(width) * int64(height); area > maxPixels {
		scale = math.Min(scale, math.Sqrt(float64(maxPixels)/float64(area)))
	}
	resultWidth := max(1, int(math.Floor(float64(width)*scale)))
	resultHeight := max(1, int(math.Floor(float64(height)*scale)))
	for int64(resultWidth)*int64(resultHeight) > maxPixels || resultWidth > maxDimension || resultHeight > maxDimension {
		if resultWidth >= resultHeight && resultWidth > 1 {
			resultWidth--
		} else if resultHeight > 1 {
			resultHeight--
		} else {
			break
		}
	}
	return resultWidth, resultHeight
}

func reduceImageDimensions(width, height int) (int, int) {
	return max(1, int(math.Floor(float64(width)*0.75))), max(1, int(math.Floor(float64(height)*0.75)))
}

func resizeImage(source image.Image, width, height int) image.Image {
	resized := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(resized, resized.Bounds(), source, source.Bounds(), draw.Over, nil)
	return resized
}

func encodeBoundedImage(ctx context.Context, img image.Image, format string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, errors.New("image_max_bytes must be positive")
	}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "jpeg", "jpg":
		for _, quality := range []int{90, 80, 70, 60, 50, 40, 30} {
			if err := checkImageContext(ctx); err != nil {
				return nil, err
			}
			var buffer bytes.Buffer
			if err := jpeg.Encode(&buffer, img, &jpeg.Options{Quality: quality}); err != nil {
				return nil, fmt.Errorf("encode JPEG: %w", err)
			}
			if err := checkImageContext(ctx); err != nil {
				return nil, err
			}
			data := buffer.Bytes()
			if int64(len(data)) <= maxBytes {
				return data, nil
			}
		}
		return nil, fmt.Errorf("resized JPEG payload exceeds image_max_bytes limit of %d bytes", maxBytes)
	default:
		if err := checkImageContext(ctx); err != nil {
			return nil, err
		}
		var buffer bytes.Buffer
		if err := png.Encode(&buffer, img); err != nil {
			return nil, fmt.Errorf("encode PNG: %w", err)
		}
		if err := checkImageContext(ctx); err != nil {
			return nil, err
		}
		data := buffer.Bytes()
		if int64(len(data)) > maxBytes {
			return nil, fmt.Errorf("resized PNG payload exceeds image_max_bytes limit of %d bytes", maxBytes)
		}
		return data, nil
	}
}

// jpegEXIFOrientation reads only the bounded APP1/TIFF metadata and returns a
// safe default for malformed or absent metadata.
func jpegEXIFOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return 1
	}
	for pos := 2; pos+1 < len(data); {
		if data[pos] != 0xff {
			return 1
		}
		for pos < len(data) && data[pos] == 0xff {
			pos++
		}
		if pos >= len(data) {
			return 1
		}
		marker := data[pos]
		pos++
		if marker == 0xd9 || marker == 0xda {
			return 1
		}
		if marker >= 0xd0 && marker <= 0xd7 || marker == 0x01 {
			continue
		}
		if pos+2 > len(data) {
			return 1
		}
		length := int(binary.BigEndian.Uint16(data[pos:]))
		if length < 2 || pos+length > len(data) {
			return 1
		}
		segment := data[pos+2 : pos+length]
		if marker == 0xe1 && len(segment) >= 8 && bytes.Equal(segment[:6], []byte("Exif\x00\x00")) {
			if orientation := parseTIFFOrientation(segment[6:]); orientation >= 1 && orientation <= 8 {
				return orientation
			}
		}
		pos += length
	}
	return 1
}

func parseTIFFOrientation(data []byte) int {
	if len(data) < 8 {
		return 1
	}
	var order binary.ByteOrder = binary.BigEndian
	if data[0] == 'I' && data[1] == 'I' {
		order = binary.LittleEndian
	} else if data[0] != 'M' || data[1] != 'M' {
		return 1
	}
	if order.Uint16(data[2:4]) != 42 {
		return 1
	}
	ifd := int(order.Uint32(data[4:8]))
	if ifd < 0 || ifd+2 > len(data) {
		return 1
	}
	count := int(order.Uint16(data[ifd : ifd+2]))
	entriesStart := ifd + 2
	if count > (len(data)-entriesStart)/12 {
		return 1
	}
	for i := 0; i < count; i++ {
		entry := data[entriesStart+i*12 : entriesStart+(i+1)*12]
		if order.Uint16(entry[0:2]) != 0x0112 || order.Uint16(entry[2:4]) != 3 || order.Uint32(entry[4:8]) < 1 {
			continue
		}
		return int(order.Uint16(entry[8:10]))
	}
	return 1
}

func applyJPEGOrientation(source image.Image, orientation int) image.Image {
	if orientation < 2 || orientation > 8 {
		return source
	}
	sw, sh := source.Bounds().Dx(), source.Bounds().Dy()
	ow, oh := sw, sh
	if orientation >= 5 {
		ow, oh = sh, sw
	}
	result := image.NewRGBA(image.Rect(0, 0, ow, oh))
	for y := 0; y < oh; y++ {
		for x := 0; x < ow; x++ {
			sx, sy := x, y
			switch orientation {
			case 2:
				sx = sw - 1 - x
			case 3:
				sx, sy = sw-1-x, sh-1-y
			case 4:
				sy = sh - 1 - y
			case 5:
				sx, sy = y, x
			case 6:
				sx, sy = y, sh-1-x
			case 7:
				sx, sy = sw-1-y, sh-1-x
			case 8:
				sx, sy = sw-1-y, x
			}
			result.Set(x, y, source.At(source.Bounds().Min.X+sx, source.Bounds().Min.Y+sy))
		}
	}
	return result
}

func gifHasMultipleFrames(data []byte) (bool, error) {
	if len(data) < 13 || string(data[:3]) != "GIF" {
		return false, errors.New("invalid GIF header")
	}
	pos := 13
	packed := data[10]
	if packed&0x80 != 0 {
		pos += 3 * (1 << ((packed & 0x07) + 1))
	}
	frames := 0
	for pos < len(data) {
		switch data[pos] {
		case 0x3b:
			return frames > 1, nil
		case 0x2c:
			frames++
			if frames > 1 {
				return true, nil
			}
			if pos+10 > len(data) {
				return false, errors.New("truncated GIF image descriptor")
			}
			pos += 10
			if data[pos-1]&0x80 != 0 {
				pos += 3 * (1 << ((data[pos-1] & 0x07) + 1))
			}
			if pos >= len(data) {
				return false, errors.New("truncated GIF image data")
			}
			pos++ // LZW minimum code size.
			if err := skipGIFSubBlocks(data, &pos); err != nil {
				return false, err
			}
		case 0x21:
			pos += 2 // extension introducer and label
			if pos > len(data) {
				return false, errors.New("truncated GIF extension")
			}
			if err := skipGIFSubBlocks(data, &pos); err != nil {
				return false, err
			}
		default:
			return false, errors.New("invalid GIF block")
		}
	}
	return false, errors.New("unterminated GIF")
}

func skipGIFSubBlocks(data []byte, pos *int) error {
	for {
		if *pos >= len(data) {
			return errors.New("truncated GIF sub-block")
		}
		size := int(data[*pos])
		*pos = *pos + 1
		if size == 0 {
			return nil
		}
		if size > len(data)-*pos {
			return errors.New("truncated GIF sub-block data")
		}
		*pos += size
	}
}

func webpIsAnimated(data []byte) bool {
	if len(data) < 16 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return false
	}
	for pos := 12; pos+8 <= len(data); {
		kind := string(data[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		pos += 8
		if size < 0 || size > len(data)-pos {
			return false
		}
		if kind == "ANIM" || kind == "ANMF" {
			return true
		}
		if kind == "VP8X" && size >= 10 && data[pos]&0x02 != 0 {
			return true
		}
		pos += size
		if size&1 != 0 {
			pos++
		}
	}
	return false
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
