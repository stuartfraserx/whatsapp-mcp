package tools_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/fmflurry/whatsapp-mcp/internal/tools"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

type downloadResult struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	MimeType  string `json:"mime_type"`
}

func mediaMessage(id, chat, directPath string, media wa.Media) wa.Message {
	media.DirectPath = directPath
	if media.Type == "" {
		media.Type = wa.MediaDocument
	}
	return wa.Message{ID: id, ChatJID: chat, SenderJID: chat, Timestamp: epoch, Text: "caption", Media: &media}
}

func (e *env) downloadedFiles() []string {
	e.t.Helper()
	entries, err := os.ReadDir(e.downloads)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, en := range entries {
		names = append(names, en.Name())
	}
	return names
}

func TestDownloadMediaWritesOwnerOnlyFileInDownloadDir(t *testing.T) {
	e := newEnv(t)
	data := []byte("%PDF-1.7 binary \x00\x01\x02 content")
	e.fake.SetMedia("/v/t/doc", data)
	e.seedMessages(mediaMessage("D1", aliceJID, "/v/t/doc", wa.Media{Type: wa.MediaDocument, FileName: "report.pdf", MimeType: "application/pdf"}))

	got := callAs[downloadResult](e, "download_media", map[string]any{"message_id": "D1"})

	if want := filepath.Join(e.downloads, "report.pdf"); got.Path != want {
		t.Fatalf("path = %q, want %q", got.Path, want)
	}
	if got.SizeBytes != int64(len(data)) || got.MimeType != "application/pdf" {
		t.Fatalf("result = %+v", got)
	}
	onDisk, err := os.ReadFile(got.Path)
	if err != nil || string(onDisk) != string(data) {
		t.Fatalf("file content = %q (err=%v), want %q", onDisk, err, data)
	}
	assertPerm(t, got.Path, 0o600)
	assertPerm(t, e.downloads, 0o700)
}

func TestDownloadMediaNeverOverwritesExistingFiles(t *testing.T) {
	e := newEnv(t)
	e.fake.SetMedia("/v/t/doc", []byte("new content"))
	e.seedMessages(mediaMessage("D1", aliceJID, "/v/t/doc", wa.Media{Type: wa.MediaDocument, FileName: "report.pdf", MimeType: "application/pdf"}))
	original := e.writeFile(filepath.Join(e.downloads, "report.pdf"), []byte("ORIGINAL"))

	first := callAs[downloadResult](e, "download_media", map[string]any{"message_id": "D1"})
	second := callAs[downloadResult](e, "download_media", map[string]any{"message_id": "D1"})
	third := callAs[downloadResult](e, "download_media", map[string]any{"message_id": "D1"})

	for i, want := range []string{"report-1.pdf", "report-2.pdf", "report-3.pdf"} {
		got := []downloadResult{first, second, third}[i]
		if filepath.Base(got.Path) != want {
			t.Errorf("download %d saved as %q, want %q", i+1, filepath.Base(got.Path), want)
		}
		if b, _ := os.ReadFile(got.Path); string(b) != "new content" {
			t.Errorf("download %d content = %q", i+1, b)
		}
	}
	if b, _ := os.ReadFile(original); string(b) != "ORIGINAL" {
		t.Fatalf("pre-existing file was overwritten: %q", b)
	}
}

func TestDownloadMediaDoesNotFollowPlantedSymlinks(t *testing.T) {
	e := newEnv(t)
	e.fake.SetMedia("/v/t/doc", []byte("downloaded bytes"))
	e.seedMessages(mediaMessage("D1", aliceJID, "/v/t/doc", wa.Media{Type: wa.MediaDocument, FileName: "evil.txt", MimeType: "text/plain"}))
	outside := e.writeFile(filepath.Join(e.root, "victim.txt"), []byte("VICTIM"))
	if err := os.MkdirAll(e.downloads, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(e.downloads, "evil.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(e.root, "dangling-target"), filepath.Join(e.downloads, "evil-1.txt")); err != nil {
		t.Fatal(err)
	}

	got := callAs[downloadResult](e, "download_media", map[string]any{"message_id": "D1"})

	if b, _ := os.ReadFile(outside); string(b) != "VICTIM" {
		t.Fatalf("symlink target outside the download dir was written: %q", b)
	}
	if _, err := os.Stat(filepath.Join(e.root, "dangling-target")); !os.IsNotExist(err) {
		t.Fatalf("dangling symlink target was created (err=%v)", err)
	}
	if filepath.Base(got.Path) != "evil-2.txt" {
		t.Fatalf("saved as %q, want evil-2.txt", filepath.Base(got.Path))
	}
}

func TestDownloadMediaFileNaming(t *testing.T) {
	tests := []struct {
		name     string
		media    wa.Media
		id       string
		filename string
		want     string
	}{
		{"original name", wa.Media{FileName: "report.pdf", MimeType: "application/pdf"}, "ID1", "", "report.pdf"},
		{"explicit filename wins", wa.Media{FileName: "report.pdf", MimeType: "application/pdf"}, "ID1", "mine.bin", "mine.bin"},
		{"explicit filename with spaces and unicode", wa.Media{MimeType: "image/png"}, "ID1", "mon fichier é 😀.png", "mon fichier é 😀.png"},
		{"unix path in original name is stripped", wa.Media{FileName: "../../etc/evil.sh", MimeType: "text/x-sh"}, "ID1", "", "evil.sh"},
		{"windows path in original name is stripped", wa.Media{FileName: `C:\Users\bob\Desktop\doc.txt`, MimeType: "text/plain"}, "ID1", "", "doc.txt"},
		{"absolute path in original name is stripped", wa.Media{FileName: "/etc/passwd", MimeType: "text/plain"}, "ID1", "", "passwd"},
		{"control characters replaced", wa.Media{FileName: "a\x01b\nc.txt", MimeType: "text/plain"}, "ID1", "", "a_b_c.txt"},
		{"original name that is only dots falls back to id", wa.Media{FileName: "..", MimeType: "image/jpeg"}, "ID1", "", "ID1.jpg"},
		{"original name ending with slash falls back to id", wa.Media{FileName: "dir/", MimeType: "image/png"}, "ID2", "", "ID2.png"},
		{"jpeg without name", wa.Media{MimeType: "image/jpeg"}, "ID3", "", "ID3.jpg"},
		{"png without name", wa.Media{MimeType: "image/png"}, "ID4", "", "ID4.png"},
		{"ogg audio with codec parameter", wa.Media{MimeType: "audio/ogg; codecs=opus"}, "ID5", "", "ID5.ogg"},
		{"mp4 video", wa.Media{MimeType: "video/mp4"}, "ID6", "", "ID6.mp4"},
		{"unknown mime", wa.Media{MimeType: "application/x-totally-unknown"}, "ID7", "", "ID7.bin"},
		{"missing mime", wa.Media{}, "ID8", "", "ID8.bin"},
		{"hostile message id is sanitized", wa.Media{MimeType: "image/png"}, "../../ID9", "", "ID9.png"},
		{"original name with parent dir component only", wa.Media{FileName: "x/..", MimeType: "image/png"}, "ID10", "", "ID10.png"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.fake.SetMedia("/p", []byte("bytes"))
			e.seedMessages(mediaMessage(tt.id, aliceJID, "/p", tt.media))
			args := map[string]any{"message_id": tt.id}
			if tt.filename != "" {
				args["filename"] = tt.filename
			}

			got := callAs[downloadResult](e, "download_media", args)

			if filepath.Dir(got.Path) != e.downloads {
				t.Fatalf("file written to %q, outside the download dir %q", got.Path, e.downloads)
			}
			if base := filepath.Base(got.Path); base != tt.want {
				t.Fatalf("file name = %q, want %q", base, tt.want)
			}
			if files := e.downloadedFiles(); len(files) != 1 {
				t.Fatalf("download dir holds %v, want exactly the one file", files)
			}
		})
	}
}

func TestDownloadMediaTruncatesVeryLongOriginalNamesKeepingExtension(t *testing.T) {
	e := newEnv(t)
	e.fake.SetMedia("/p", []byte("bytes"))
	long := strings.Repeat("n", 400) + ".pdf"
	e.seedMessages(mediaMessage("L1", aliceJID, "/p", wa.Media{FileName: long, MimeType: "application/pdf"}))

	got := callAs[downloadResult](e, "download_media", map[string]any{"message_id": "L1"})

	base := filepath.Base(got.Path)
	if len(base) > 200 || !strings.HasSuffix(base, ".pdf") {
		t.Fatalf("name %q (len %d) must be <= 200 bytes and keep .pdf", base, len(base))
	}
}

func TestDownloadMediaTruncationNeverCutsARune(t *testing.T) {
	for _, tt := range []struct{ name, stem string }{
		{"two byte runes", "é"},
		{"three byte runes", "世"},
		{"four byte runes", "😀"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.fake.SetMedia("/p", []byte("bytes"))
			for _, pad := range []string{"", "a", "aa", "aaa"} {
				original := pad + strings.Repeat(tt.stem, 300) + ".pdf"
				e.seedMessages(mediaMessage("U"+pad, aliceJID, "/p", wa.Media{FileName: original, MimeType: "application/pdf"}))

				got := callAs[downloadResult](e, "download_media", map[string]any{"message_id": "U" + pad})

				base := filepath.Base(got.Path)
				if len(base) > 200 || !utf8.ValidString(base) || !strings.HasSuffix(base, ".pdf") {
					t.Fatalf("pad %q: name %q (len %d) must be <= 200 bytes of valid UTF-8 keeping .pdf", pad, base, len(base))
				}
			}
		})
	}
}

func TestDownloadMediaRefusesOversizedMedia(t *testing.T) {
	t.Run("declared length over the cap is refused before downloading", func(t *testing.T) {
		e := newEnv(t)
		e.fake.SetMedia("/big", []byte("small payload"))
		e.seedMessages(mediaMessage("B1", aliceJID, "/big", wa.Media{FileName: "big.bin", FileLength: tools.MaxDownloadBytes + 1}))

		e.fail("download_media", map[string]any{"message_id": "B1"}, "100 MiB download limit")
		if files := e.downloadedFiles(); len(files) != 0 {
			t.Fatalf("files written: %v", files)
		}
	})
	t.Run("declared length at the cap is allowed", func(t *testing.T) {
		e := newEnv(t)
		e.fake.SetMedia("/ok", []byte("payload"))
		e.seedMessages(mediaMessage("B2", aliceJID, "/ok", wa.Media{FileName: "ok.bin", FileLength: tools.MaxDownloadBytes}))

		e.ok("download_media", map[string]any{"message_id": "B2"})
	})
	t.Run("data larger than the cap despite a small declared length is refused", func(t *testing.T) {
		e := newEnv(t, withDownloadLimit(8))
		e.fake.SetMedia("/lie", []byte("123456789"))
		e.seedMessages(mediaMessage("B3", aliceJID, "/lie", wa.Media{FileName: "lie.bin", FileLength: 4}))

		e.fail("download_media", map[string]any{"message_id": "B3"}, "download limit")
		if files := e.downloadedFiles(); len(files) != 0 {
			t.Fatalf("files written: %v", files)
		}
	})
}

func TestDownloadMediaRejectsUnsafeFilenameParameter(t *testing.T) {
	tests := []struct {
		name     string
		filename string
	}{
		{"parent traversal", "../x"},
		{"deep traversal", "../../../../etc/cron.d/x"},
		{"subdirectory", "a/b"},
		{"absolute path", "/tmp/x"},
		{"backslash separator", `a\b`},
		{"windows traversal", `..\x`},
		{"dot dot", ".."},
		{"single dot", "."},
		{"nul byte", "a\x00b"},
		{"newline", "a\nb"},
		{"tab", "a\tb"},
		{"too long", strings.Repeat("x", 256)},
		{"trailing slash", "x/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.fake.SetMedia("/p", []byte("bytes"))
			e.seedMessages(mediaMessage("F1", aliceJID, "/p", wa.Media{MimeType: "image/png"}))

			e.fail("download_media", map[string]any{"message_id": "F1", "filename": tt.filename}, "filename must be a single file name")

			if files := e.downloadedFiles(); len(files) != 0 {
				t.Fatalf("download dir holds %v after a refused download", files)
			}
			parent, _ := os.ReadDir(e.root)
			for _, en := range parent {
				if en.Name() == "x" {
					t.Fatalf("traversal created %s outside the download dir", en.Name())
				}
			}
		})
	}
}

func TestDownloadMediaErrors(t *testing.T) {
	e := newEnv(t)
	e.fake.SetMedia("/have", []byte("data"))
	e.seedMessages(
		mediaMessage("HAS", aliceJID, "/have", wa.Media{MimeType: "image/png"}),
		mediaMessage("GONE", aliceJID, "/expired", wa.Media{MimeType: "image/png"}),
		msg("TXT", aliceJID, epoch, "just text"),
		mediaMessage("DUP", aliceJID, "/have", wa.Media{MimeType: "image/png"}),
		mediaMessage("DUP", bobJID, "/have", wa.Media{MimeType: "image/jpeg"}),
	)

	tests := []struct {
		name    string
		args    map[string]any
		wantErr string
	}{
		{"text message has no media", map[string]any{"message_id": "TXT"}, "no downloadable media"},
		{"unknown message", map[string]any{"message_id": "NOPE"}, "message not found"},
		{"message in a different chat", map[string]any{"message_id": "HAS", "chat_jid": bobJID}, "message not found"},
		{"media no longer available", map[string]any{"message_id": "GONE"}, "download media"},
		{"ambiguous id lists chats", map[string]any{"message_id": "DUP"}, aliceJID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e.fail("download_media", tt.args, tt.wantErr)
			if files := e.downloadedFiles(); len(files) != 0 {
				t.Fatalf("failed download left files behind: %v", files)
			}
		})
	}

	t.Run("ambiguous id resolved with chat_jid", func(t *testing.T) {
		got := callAs[downloadResult](e, "download_media", map[string]any{"message_id": "DUP", "chat_jid": bobJID})
		if got.MimeType != "image/jpeg" || filepath.Ext(got.Path) != ".jpg" {
			t.Fatalf("result = %+v", got)
		}
	})
}

func TestDownloadMediaRequiresConfiguredDownloadDir(t *testing.T) {
	e := newEnv(t, func(o *options) { o.noDownloadDir = true })
	e.fake.SetMedia("/p", []byte("x"))
	e.seedMessages(mediaMessage("M", aliceJID, "/p", wa.Media{MimeType: "image/png"}))

	e.fail("download_media", map[string]any{"message_id": "M"}, "download directory is not configured")
}

func TestDownloadMediaDownloadErrorDoesNotLeakPhoneNumbers(t *testing.T) {
	e := newEnv(t)
	e.seedMessages(mediaMessage("N", aliceJID, "/missing", wa.Media{MimeType: "image/png"}))

	text := e.fail("download_media", map[string]any{"message_id": "N"}, "download media")

	if strings.Contains(text, "33611111111") {
		t.Fatalf("error leaks a phone number: %s", text)
	}
}

func TestSentFileCanBeSentBackFromDownloadDirAfterDownload(t *testing.T) {
	e := newEnv(t, withRate(0, 1000))
	e.fake.SetMedia("/p", []byte("round trip"))
	e.seedMessages(mediaMessage("RT", aliceJID, "/p", wa.Media{FileName: "rt.png", MimeType: "image/png"}))

	dl := callAs[downloadResult](e, "download_media", map[string]any{"message_id": "RT"})
	e.ok("send_file", map[string]any{"recipient": bobJID, "path": dl.Path})

	sent := e.fake.Sent()
	if len(sent) != 1 || string(sent[0].File.Data) != "round trip" || sent[0].File.MimeType != "image/png" {
		t.Fatalf("sent = %+v", sent)
	}
}
