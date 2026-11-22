package bee

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type sidecarPayloadResolver struct {
	artifact Artifact
	reader   io.ReadCloser
	err      error
}

func (resolver sidecarPayloadResolver) OpenAttachmentPayload(context.Context, AttachmentRef) (Artifact, io.ReadCloser, error) {
	return resolver.artifact, resolver.reader, resolver.err
}

type closeCountingReader struct {
	reader io.Reader
	mu     sync.Mutex
	closes int
}

func (reader *closeCountingReader) Read(p []byte) (int, error) { return reader.reader.Read(p) }
func (reader *closeCountingReader) Close() error {
	reader.mu.Lock()
	reader.closes++
	reader.mu.Unlock()
	return nil
}
func (reader *closeCountingReader) closeCount() int {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return reader.closes
}

type failingImageReader struct{}

func (failingImageReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (failingImageReader) Close() error             { return nil }

type errorCloseReader struct {
	io.Reader
	closes int
}

func (reader *errorCloseReader) Close() error { reader.closes++; return errors.New("close failed") }

func makePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var buffer bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 251), G: uint8(y % 251), B: 90, A: 255})
		}
	}
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func makeJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	var buffer bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: uint8((x + y) % 251), G: uint8(x % 251), B: uint8(y % 251), A: 255})
		}
	}
	if err := jpeg.Encode(&buffer, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestSpecialistImageConfigDimensionDefaultsValidationAndJSON(t *testing.T) {
	defaults := DefaultSpecialistConfig()
	if defaults.ImageMaxDimension != DefaultSpecialistImageMaxDimension {
		t.Fatalf("image dimension default=%d", defaults.ImageMaxDimension)
	}
	if defaults.ImageMaxPixels != DefaultSpecialistImageMaxPixels {
		t.Fatalf("image pixel default=%d", defaults.ImageMaxPixels)
	}
	if err := (SpecialistConfig{ImageMaxDimension: MaxSpecialistImageMaxDimension}).Validate(); err != nil {
		t.Fatalf("maximum dimension rejected: %v", err)
	}
	if err := (SpecialistConfig{ImageMaxDimension: MaxSpecialistImageMaxDimension + 1}).Validate(); err == nil {
		t.Fatal("dimension above hard cap accepted")
	}
	if err := (SpecialistConfig{ImageMaxPixels: MaxSpecialistImageMaxPixels}).Validate(); err != nil {
		t.Fatalf("maximum pixel cap rejected: %v", err)
	}
	if err := (SpecialistConfig{ImageMaxPixels: MaxSpecialistImageMaxPixels + 1}).Validate(); err == nil {
		t.Fatal("pixel cap above hard cap accepted")
	}
	data, err := json.Marshal(defaults)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip SpecialistConfig
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.ImageMaxDimension != defaults.ImageMaxDimension {
		t.Fatalf("JSON round trip dimension=%d want %d", roundTrip.ImageMaxDimension, defaults.ImageMaxDimension)
	}
	if roundTrip.ImageMaxPixels != defaults.ImageMaxPixels {
		t.Fatalf("JSON round trip pixels=%d want %d", roundTrip.ImageMaxPixels, defaults.ImageMaxPixels)
	}
}

func TestSpecialistImageOutputPixelCap(t *testing.T) {
	data := makePNG(t, 2000, 1000)
	got, _, err := normalizeImageBytesContext(context.Background(), data, 2<<20, 2000, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(got))
	if err != nil {
		t.Fatal(err)
	}
	if area := int64(config.Width) * int64(config.Height); area > 10_000 {
		t.Fatalf("output area=%d exceeds cap", area)
	}
}

func TestSpecialistOrdinaryJPEGNoOpPreservesBytes(t *testing.T) {
	data := makeJPEG(t, 12, 8)
	got, mediaType, err := normalizeImageBytesContext(context.Background(), data, 1<<20, 1536, 1_500_000)
	if err != nil || mediaType != "image/jpeg" || !bytes.Equal(got, data) {
		t.Fatalf("err=%v media=%q bytes_equal=%v", err, mediaType, bytes.Equal(got, data))
	}
}

func TestSpecialistUnsupportedImageFormatIsExplicit(t *testing.T) {
	_, _, err := normalizeImageBytesContext(context.Background(), []byte("BMnot-a-supported-image"), 1<<20, 1536, 1_500_000)
	if err == nil || !strings.Contains(err.Error(), "unsupported image format") {
		t.Fatalf("unsupported format error=%v", err)
	}
}

func TestSpecialistImageContextAwareSemaphore(t *testing.T) {
	specialistImageProcessingSem <- struct{}{}
	defer func() { <-specialistImageProcessingSem }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, _, err := normalizeImageBytesContext(ctx, makePNG(t, 2, 2), 1<<20, 1536, 100)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("semaphore cancellation error=%v", err)
	}
}

func TestSpecialistImageExactByteLimitRejectsBeforeDecode(t *testing.T) {
	limit := int64(8)
	reader := &closeCountingReader{reader: strings.NewReader("123456789")}
	_, output, err := normalizeSpecialistImagePayload(context.Background(), sidecarPayloadResolver{artifact: Artifact{ID: "artifact", Kind: "image", MediaType: "image/png", Size: 9}, reader: reader}, AttachmentRef{ID: "att", Kind: "image", Filename: "image.png"}, limit, 1536)
	if err == nil || !strings.Contains(err.Error(), "image_max_bytes") {
		t.Fatalf("over-limit error=%v", err)
	}
	if output != nil || reader.closeCount() != 1 {
		t.Fatalf("output=%v closes=%d", output, reader.closeCount())
	}
}

func TestSpecialistImageSmallPayloadIsNoOpAndMetadataIsTransient(t *testing.T) {
	data := makePNG(t, 12, 8)
	artifact := Artifact{ID: "artifact-original", Kind: "image", MediaType: "image/png", Size: int64(len(data)), SHA256: "original"}
	ref := AttachmentRef{ID: "att-original", ArtifactID: artifact.ID, Kind: "image", MediaType: "image/png", Filename: "small.png"}
	reader := &closeCountingReader{reader: bytes.NewReader(data)}
	gotArtifact, output, err := normalizeSpecialistImagePayload(context.Background(), sidecarPayloadResolver{artifact: artifact, reader: reader}, ref, int64(len(data)), 1536)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(output)
	if err != nil {
		t.Fatal(err)
	}
	_ = output.Close()
	if !bytes.Equal(got, data) || gotArtifact.ID != artifact.ID || gotArtifact.Size != int64(len(data)) || gotArtifact.SHA256 == artifact.SHA256 || gotArtifact.RetainedSize != int64(len(data)) || reader.closeCount() != 1 {
		t.Fatalf("artifact=%#v bytes_equal=%v closes=%d", gotArtifact, bytes.Equal(got, data), reader.closeCount())
	}
	if ref.ArtifactID != artifact.ID || ref.ID != "att-original" {
		t.Fatalf("reference changed: %#v", ref)
	}
}

func TestSpecialistImageLargeJPEGIsResizedWithAspectAndBound(t *testing.T) {
	data := makeJPEG(t, 3213, 5712)
	maxBytes := int64(8 << 20)
	gotArtifact, output, err := normalizeSpecialistImagePayload(context.Background(), sidecarPayloadResolver{artifact: Artifact{ID: "artifact-jpeg", Kind: "image", MediaType: "image/jpeg", Size: int64(len(data))}, reader: io.NopCloser(bytes.NewReader(data))}, AttachmentRef{ID: "att-jpeg", Kind: "image", MediaType: "image/jpeg", Filename: "large.jpg"}, maxBytes, 1536)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(output)
	if err != nil {
		t.Fatal(err)
	}
	_ = output.Close()
	config, format, err := image.DecodeConfig(bytes.NewReader(got))
	if err != nil {
		t.Fatal(err)
	}
	if format != "jpeg" || config.Width > 1536 || config.Height > 1536 || int64(len(got)) > maxBytes {
		t.Fatalf("format=%s config=%+v bytes=%d", format, config, len(got))
	}
	if ratio := float64(config.Width) / float64(config.Height); ratio < 0.55 || ratio > 0.59 {
		t.Fatalf("aspect ratio=%f", ratio)
	}
	if gotArtifact.MediaType != "image/jpeg" || gotArtifact.Size != int64(len(got)) || gotArtifact.RetainedSize != int64(len(got)) || gotArtifact.OriginalSize != int64(len(data)) {
		t.Fatalf("transient artifact=%#v", gotArtifact)
	}
}

func TestSpecialistImagePNGWebPAndResizedGIFSupport(t *testing.T) {
	pngData := makePNG(t, 2200, 1000)
	got, mediaType, err := normalizeImageBytes(pngData, 2<<20, 1536)
	if err != nil || mediaType != "image/png" {
		t.Fatalf("PNG err=%v media=%q", err, mediaType)
	}
	pngConfig, _, err := image.DecodeConfig(bytes.NewReader(got))
	if err != nil || pngConfig.Width > 1536 || pngConfig.Height > 1536 {
		t.Fatalf("PNG config=%+v err=%v", pngConfig, err)
	}
	webpData, err := base64.StdEncoding.DecodeString("UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AA/vuUAAA=")
	if err != nil {
		t.Fatal(err)
	}
	got, mediaType, err = normalizeImageBytes(webpData, 1024, 1536)
	if err != nil || mediaType != "image/webp" || !bytes.Equal(got, webpData) {
		t.Fatalf("WebP err=%v media=%q equal=%v", err, mediaType, bytes.Equal(got, webpData))
	}
	var gifBuffer bytes.Buffer
	gifImage := image.NewPaletted(image.Rect(0, 0, 20, 10), color.Palette{color.Black, color.White})
	if err := gif.Encode(&gifBuffer, gifImage, nil); err != nil {
		t.Fatal(err)
	}
	got, mediaType, err = normalizeImageBytes(gifBuffer.Bytes(), 1024, 8)
	if err != nil || mediaType != "image/png" {
		t.Fatalf("resized GIF err=%v media=%q", err, mediaType)
	}
	gifConfig, format, err := image.DecodeConfig(bytes.NewReader(got))
	if err != nil || format != "png" || gifConfig.Width > 8 || gifConfig.Height > 8 {
		t.Fatalf("resized GIF format=%q config=%+v err=%v", format, gifConfig, err)
	}
}

func TestSpecialistImageRejectsAnimatedGIFWhenResizing(t *testing.T) {
	first := image.NewPaletted(image.Rect(0, 0, 20, 10), color.Palette{color.Black, color.White})
	second := image.NewPaletted(image.Rect(0, 0, 20, 10), color.Palette{color.Black, color.White})
	second.SetColorIndex(0, 0, 1)
	var buffer bytes.Buffer
	if err := gif.EncodeAll(&buffer, &gif.GIF{Image: []*image.Paletted{first, second}, Delay: []int{1, 1}}); err != nil {
		t.Fatal(err)
	}
	_, _, err := normalizeImageBytes(buffer.Bytes(), 1<<20, 8)
	if err == nil || !strings.Contains(err.Error(), "animated GIF") {
		t.Fatalf("animated GIF error=%v", err)
	}
}

func TestSpecialistJPEGEXIFOrientationsAreNormalized(t *testing.T) {
	for _, test := range []struct {
		orientation byte
		width       int
		height      int
	}{
		{orientation: 3, width: 4, height: 2},
		{orientation: 6, width: 2, height: 4},
		{orientation: 8, width: 2, height: 4},
	} {
		data := jpegWithOrientation(t, makeJPEG(t, 4, 2), test.orientation)
		got, mediaType, err := normalizeImageBytesContext(context.Background(), data, 1<<20, 100, 100)
		if err != nil {
			t.Fatalf("orientation %d: %v", test.orientation, err)
		}
		config, format, err := image.DecodeConfig(bytes.NewReader(got))
		if err != nil || format != "jpeg" || mediaType != "image/jpeg" || config.Width != test.width || config.Height != test.height {
			t.Fatalf("orientation %d format=%q media=%q config=%+v err=%v", test.orientation, format, mediaType, config, err)
		}
	}
}

func jpegWithOrientation(t *testing.T, jpegData []byte, orientation byte) []byte {
	t.Helper()
	tiff := make([]byte, 26)
	tiff[0], tiff[1] = 'I', 'I'
	binary.LittleEndian.PutUint16(tiff[2:4], 42)
	binary.LittleEndian.PutUint32(tiff[4:8], 8)
	binary.LittleEndian.PutUint16(tiff[8:10], 1)
	binary.LittleEndian.PutUint16(tiff[10:12], 0x0112)
	binary.LittleEndian.PutUint16(tiff[12:14], 3)
	binary.LittleEndian.PutUint32(tiff[14:18], 1)
	binary.LittleEndian.PutUint16(tiff[18:20], uint16(orientation))
	exif := append([]byte("Exif\x00\x00"), tiff...)
	segment := make([]byte, 4+len(exif))
	segment[0], segment[1] = 0xff, 0xe1
	binary.BigEndian.PutUint16(segment[2:4], uint16(len(exif)+2))
	copy(segment[4:], exif)
	return append(append(append([]byte{}, jpegData[:2]...), segment...), jpegData[2:]...)
}

func TestSpecialistImageRejectsAbsurdSourcePixelsBeforeDecode(t *testing.T) {
	data := oversizedPNGHeader(10000, 10000)
	_, _, err := normalizeImageBytes(data, 8<<20, 1536)
	if err == nil || !strings.Contains(err.Error(), "pixel limit") {
		t.Fatalf("oversized image error=%v", err)
	}
}

func oversizedPNGHeader(width, height uint32) []byte {
	data := make([]byte, 8+4+4+13+4)
	copy(data, []byte("\x89PNG\r\n\x1a\n"))
	binary.BigEndian.PutUint32(data[8:], 13)
	copy(data[12:], []byte("IHDR"))
	binary.BigEndian.PutUint32(data[16:], width)
	binary.BigEndian.PutUint32(data[20:], height)
	data[24] = 8
	data[25] = 6
	binary.BigEndian.PutUint32(data[8+4+4+13:], crc32.ChecksumIEEE(data[12:12+4+13]))
	return data
}

func TestSpecialistImageResolverClosesOriginalExactlyOnceOnError(t *testing.T) {
	openErrorReader := &closeCountingReader{reader: strings.NewReader("ignored")}
	_, output, err := normalizeSpecialistImagePayload(context.Background(), sidecarPayloadResolver{reader: openErrorReader, err: errors.New("open failed")}, AttachmentRef{Kind: "image"}, 1024, 1536)
	if err == nil || output != nil || openErrorReader.closeCount() != 1 {
		t.Fatalf("open err=%v output=%v closes=%d", err, output, openErrorReader.closeCount())
	}

	reader := &closeCountingReader{reader: failingImageReader{}}
	_, output, err = normalizeSpecialistImagePayload(context.Background(), sidecarPayloadResolver{artifact: Artifact{Kind: "image", MediaType: "image/png"}, reader: reader}, AttachmentRef{Kind: "image", Filename: "broken.png"}, 1024, 1536)
	if err == nil || output != nil || reader.closeCount() != 1 {
		t.Fatalf("err=%v output=%v closes=%d", err, output, reader.closeCount())
	}
	closeErrorReader := &errorCloseReader{Reader: bytes.NewReader([]byte("x"))}
	_, _, err = normalizeSpecialistImagePayload(context.Background(), sidecarPayloadResolver{artifact: Artifact{Kind: "binary", MediaType: "application/octet-stream"}, reader: closeErrorReader}, AttachmentRef{Kind: "text"}, 1024, 1536)
	if err == nil || closeErrorReader.closes != 1 {
		t.Fatalf("close error=%v closes=%d", err, closeErrorReader.closes)
	}
}

func TestSpecialistImageTransientMetadataUsesObservedBounds(t *testing.T) {
	data := makePNG(t, 2, 2)
	artifact, output, err := normalizeSpecialistImagePayload(context.Background(), sidecarPayloadResolver{artifact: Artifact{Kind: "image", MediaType: "image/png", Size: 1, OriginalSize: 2, OriginalTruncated: true, RetentionCapped: true}, reader: io.NopCloser(bytes.NewReader(data))}, AttachmentRef{Kind: "image", MediaType: "image/png"}, 1<<20, 1536)
	if err != nil {
		t.Fatal(err)
	}
	_ = output.Close()
	if artifact.OriginalSize < int64(len(data)) || artifact.OriginalSize < artifact.RetainedSize || artifact.OriginalTruncated || artifact.RetentionCapped {
		t.Fatalf("transient metadata=%#v data=%d", artifact, len(data))
	}
}

func TestSpecialistImageReadAndCloseErrorsAreJoined(t *testing.T) {
	readErr := errors.New("read failed")
	closeErr := errors.New("close failed")
	reader := &readCloseErrorReader{readErr: readErr, closeErr: closeErr}
	_, _, err := normalizeSpecialistImagePayload(context.Background(), sidecarPayloadResolver{artifact: Artifact{Kind: "image"}, reader: reader}, AttachmentRef{Kind: "image"}, 1024, 1536)
	if !errors.Is(err, readErr) || !errors.Is(err, closeErr) || reader.closes != 1 {
		t.Fatalf("joined err=%v closes=%d", err, reader.closes)
	}
}

type readCloseErrorReader struct {
	readErr  error
	closeErr error
	closes   int
}

func (reader *readCloseErrorReader) Read([]byte) (int, error) { return 0, reader.readErr }
func (reader *readCloseErrorReader) Close() error {
	reader.closes++
	return reader.closeErr
}

func TestSpecialistJSONPayloadAcceptsBareAndSingleFenceOnly(t *testing.T) {
	bare := `{"summary":"ok","claims":["one"]}`
	payload, err := specialistJSONPayload(bare)
	if err != nil || string(payload) != bare {
		t.Fatalf("bare payload=%q err=%v", payload, err)
	}
	fenced := "```json\n" + bare + "\n```"
	payload, err = specialistJSONPayload(fenced)
	if err != nil || string(payload) != bare {
		t.Fatalf("fenced payload=%q err=%v", payload, err)
	}
	for _, invalid := range []string{
		"Here is the JSON:\n```json\n" + bare + "\n```",
		"```yaml\nsummary: ok\n```",
		"```json\n" + bare,
		"```json\n" + bare + "\n```\nextra",
	} {
		if _, err := specialistJSONPayload(invalid); err == nil {
			t.Fatalf("invalid specialist JSON accepted: %q", invalid)
		}
	}
}

func TestProviderReceivesNormalizedImageDataURI(t *testing.T) {
	data := makeJPEG(t, 3213, 5712)
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ = io.ReadAll(request.Body)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"model":"local"}`))
	}))
	defer server.Close()
	provider, err := NewOpenAICompatibleProvider(ProviderConfig{Type: DefaultProviderType, BaseURL: server.URL, Model: "local", ImageInput: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	provider.SetAttachmentResolver(boundedSpecialistResolver{resolver: sidecarPayloadResolver{artifact: Artifact{ID: "artifact", Kind: "image", MediaType: "image/jpeg", Size: int64(len(data))}, reader: io.NopCloser(bytes.NewReader(data))}, maxBytes: 8 << 20, maxDimension: 1536})
	_, err = provider.Complete(context.Background(), Request{Model: "local", Messages: []Message{{Role: RoleUser, Content: "describe", Attachments: []AttachmentRef{{ID: "att", ArtifactID: "artifact", Kind: "image", MediaType: "image/jpeg", Filename: "large.jpg"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Messages []struct {
			Content []struct {
				Type     string `json:"type"`
				ImageURL struct {
					URL string `json:"url"`
				} `json:"image_url"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	url := decoded.Messages[0].Content[1].ImageURL.URL
	if !strings.HasPrefix(url, "data:image/jpeg;base64,") {
		t.Fatalf("image URL=%q", url[:minInt(len(url), 40)])
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, "data:image/jpeg;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(payload))
	if err != nil || format != "jpeg" || config.Width > 1536 || config.Height > 1536 {
		t.Fatalf("normalized wire image format=%s config=%+v err=%v", format, config, err)
	}
}
