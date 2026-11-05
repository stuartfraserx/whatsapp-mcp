package wa

import "strings"

// OutgoingMediaType maps a MIME type to the WhatsApp attachment kind used when
// sending it: images are limited to JPEG, PNG and WebP, everything that is not
// image, video or audio is sent as a document.
func OutgoingMediaType(mime string) MediaType {
	m, _, _ := strings.Cut(mime, ";")
	m = strings.ToLower(strings.TrimSpace(m))
	switch {
	case m == "image/jpeg", m == "image/png", m == "image/webp":
		return MediaImage
	case strings.HasPrefix(m, "video/"):
		return MediaVideo
	case strings.HasPrefix(m, "audio/"):
		return MediaAudio
	default:
		return MediaDocument
	}
}
