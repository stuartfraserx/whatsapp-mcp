package watest_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/wa"
	"github.com/fmflurry/whatsapp-mcp/internal/wa/watest"
)

const watchdog = 5 * time.Second

func TestSendTextRecordsMessagesInOrder(t *testing.T) {
	t.Parallel()
	f := watest.NewFake()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	f.SetClock(func() time.Time { return at })

	first, err := f.SendText(context.Background(), "111@s.whatsapp.net", "hello")
	if err != nil {
		t.Fatalf("SendText: %v", err)
	}
	second, err := f.SendText(context.Background(), "222@g.us", "world")
	if err != nil {
		t.Fatalf("SendText: %v", err)
	}

	if first.ID == "" || first.ID == second.ID {
		t.Errorf("message IDs must be non-empty and unique: %q, %q", first.ID, second.ID)
	}
	if first.ChatJID != "111@s.whatsapp.net" || !first.Timestamp.Equal(at) {
		t.Errorf("SentMessage = %+v, want chat 111@s.whatsapp.net at %v", first, at)
	}
	got := f.Sent()
	if len(got) != 2 {
		t.Fatalf("Sent() has %d records, want 2", len(got))
	}
	if got[0].ChatJID != "111@s.whatsapp.net" || got[0].Text != "hello" || got[0].File != nil {
		t.Errorf("record 0 = %+v", got[0])
	}
	if got[1].ChatJID != "222@g.us" || got[1].Text != "world" {
		t.Errorf("record 1 = %+v", got[1])
	}
	if !got[0].At.Equal(at) {
		t.Errorf("record 0 At = %v, want %v", got[0].At, at)
	}
}

func TestSentReturnsACopy(t *testing.T) {
	t.Parallel()
	f := watest.NewFake()
	if _, err := f.SendText(context.Background(), "1@s.whatsapp.net", "a"); err != nil {
		t.Fatal(err)
	}
	snapshot := f.Sent()
	snapshot[0].Text = "tampered"
	if _, err := f.SendText(context.Background(), "1@s.whatsapp.net", "b"); err != nil {
		t.Fatal(err)
	}
	if got := f.Sent(); len(got) != 2 || got[0].Text != "a" {
		t.Errorf("Sent() = %+v; mutating a snapshot must not change recorded sends", got)
	}
	if len(snapshot) != 1 {
		t.Errorf("an earlier snapshot must not grow, len = %d", len(snapshot))
	}
}

func TestSendFileRecordsFileAndCaption(t *testing.T) {
	t.Parallel()
	f := watest.NewFake()
	data := []byte("pdf-bytes")
	sent, err := f.SendFile(context.Background(), "111@s.whatsapp.net", wa.OutgoingFile{
		Data: data, FileName: "a.pdf", MimeType: "application/pdf", Caption: "see attached",
	})
	if err != nil {
		t.Fatalf("SendFile: %v", err)
	}
	data[0] = 'X'

	rec := f.Sent()
	if len(rec) != 1 {
		t.Fatalf("Sent() has %d records, want 1", len(rec))
	}
	r := rec[0]
	if r.File == nil {
		t.Fatal("record has no File")
	}
	if r.File.FileName != "a.pdf" || r.File.MimeType != "application/pdf" || r.File.Caption != "see attached" {
		t.Errorf("File = %+v", *r.File)
	}
	if string(r.File.Data) != "pdf-bytes" {
		t.Errorf("File.Data = %q; the fake must keep its own copy of the bytes", r.File.Data)
	}
	if r.Text != "see attached" {
		t.Errorf("record Text = %q, want the caption", r.Text)
	}
	if sent.ChatJID != "111@s.whatsapp.net" || sent.ID == "" {
		t.Errorf("SentMessage = %+v", sent)
	}
}

func TestSetSendErrorFailsSendsWithoutRecording(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	f := watest.NewFake()
	f.SetSendError(boom)

	if _, err := f.SendText(context.Background(), "1@s.whatsapp.net", "x"); !errors.Is(err, boom) {
		t.Errorf("SendText error = %v, want %v", err, boom)
	}
	if _, err := f.SendFile(context.Background(), "1@s.whatsapp.net", wa.OutgoingFile{Data: []byte("x")}); !errors.Is(err, boom) {
		t.Errorf("SendFile error = %v, want %v", err, boom)
	}
	if got := f.Sent(); len(got) != 0 {
		t.Errorf("failed sends must not be recorded, got %+v", got)
	}

	f.SetSendError(nil)
	if _, err := f.SendText(context.Background(), "1@s.whatsapp.net", "x"); err != nil {
		t.Errorf("SendText after clearing the error: %v", err)
	}
	if got := f.Sent(); len(got) != 1 {
		t.Errorf("Sent() has %d records after recovery, want 1", len(got))
	}
}

func TestEmitDeliversToHandlerSynchronously(t *testing.T) {
	t.Parallel()
	f := watest.NewFake()
	var got []wa.Event
	f.SetEventHandler(func(ev wa.Event) { got = append(got, ev) })

	want := []wa.Event{
		wa.ConnectedEvent{},
		wa.MessageEvent{Message: wa.Message{ID: "m1", ChatJID: "1@s.whatsapp.net", Text: "hi"}},
		wa.LoggedOutEvent{Reason: "revoked"},
	}
	for _, ev := range want {
		f.Emit(ev)
	}

	if len(got) != len(want) {
		t.Fatalf("handler saw %d events synchronously, want %d", len(got), len(want))
	}
	if _, ok := got[0].(wa.ConnectedEvent); !ok {
		t.Errorf("event 0 = %T", got[0])
	}
	if m, ok := got[1].(wa.MessageEvent); !ok || m.Message.ID != "m1" || m.Message.Text != "hi" {
		t.Errorf("event 1 = %#v", got[1])
	}
	if l, ok := got[2].(wa.LoggedOutEvent); !ok || l.Reason != "revoked" {
		t.Errorf("event 2 = %#v", got[2])
	}
}

func TestEmitWithoutHandlerIsANoOp(t *testing.T) {
	t.Parallel()
	watest.NewFake().Emit(wa.ConnectedEvent{})
}

func TestSetEventHandlerReplacesPreviousHandler(t *testing.T) {
	t.Parallel()
	f := watest.NewFake()
	var first, second int
	f.SetEventHandler(func(wa.Event) { first++ })
	f.SetEventHandler(func(wa.Event) { second++ })
	f.Emit(wa.ConnectedEvent{})
	if first != 0 || second != 1 {
		t.Errorf("first=%d second=%d, want 0 and 1", first, second)
	}
}

func TestHandlerMayCallBackIntoTheFake(t *testing.T) {
	t.Parallel()
	f := watest.NewFake()
	f.SetStatus(wa.Status{State: wa.StateConnected, Paired: true})
	done := make(chan wa.ConnState, 1)
	f.SetEventHandler(func(wa.Event) {
		_, _ = f.SendText(context.Background(), "1@s.whatsapp.net", "from handler")
		done <- f.Status().State
	})

	go f.Emit(wa.ConnectedEvent{})

	select {
	case state := <-done:
		if state != wa.StateConnected {
			t.Errorf("Status inside handler = %q", state)
		}
	case <-time.After(watchdog):
		t.Fatal("Emit deadlocked when the handler called back into the Fake")
	}
	if got := f.Sent(); len(got) != 1 || got[0].Text != "from handler" {
		t.Errorf("Sent() = %+v", got)
	}
}

func TestConnectOnUnpairedEmitsConfiguredQRCodes(t *testing.T) {
	t.Parallel()
	f := watest.NewFake()
	f.SetQRCodesOnConnect("code-1", "code-2")
	var codes []string
	f.SetEventHandler(func(ev wa.Event) {
		if q, ok := ev.(wa.QRCodeEvent); ok {
			if q.Timeout <= 0 {
				t.Errorf("QRCodeEvent.Timeout = %v, want positive", q.Timeout)
			}
			codes = append(codes, q.Code)
		}
	})

	if err := f.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if len(codes) != 2 || codes[0] != "code-1" || codes[1] != "code-2" {
		t.Errorf("emitted codes = %v, want [code-1 code-2]", codes)
	}
	if f.ConnectCalls() != 1 {
		t.Errorf("ConnectCalls = %d, want 1", f.ConnectCalls())
	}
	_ = f.Connect(context.Background())
	if f.ConnectCalls() != 2 {
		t.Errorf("ConnectCalls = %d, want 2", f.ConnectCalls())
	}
}

func TestConnectOnPairedEmitsNothing(t *testing.T) {
	t.Parallel()
	f := watest.NewFake()
	f.SetQRCodesOnConnect("code-1")
	f.SetStatus(wa.Status{State: wa.StateDisconnected, Paired: true, AccountJID: "1@s.whatsapp.net"})
	events := 0
	f.SetEventHandler(func(wa.Event) { events++ })

	if err := f.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if events != 0 {
		t.Errorf("paired Connect emitted %d events, want 0", events)
	}
	if f.ConnectCalls() != 1 {
		t.Errorf("ConnectCalls = %d, want 1", f.ConnectCalls())
	}
}

func TestConnectOnUnpairedWithoutCodesDoesNothing(t *testing.T) {
	t.Parallel()
	f := watest.NewFake()
	events := 0
	f.SetEventHandler(func(wa.Event) { events++ })
	if err := f.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if events != 0 || f.ConnectCalls() != 1 {
		t.Errorf("events=%d connectCalls=%d, want 0 and 1", events, f.ConnectCalls())
	}
}

func TestStatusAndDisconnect(t *testing.T) {
	t.Parallel()
	f := watest.NewFake()
	if st := f.Status(); st.State != wa.StateDisconnected || st.Paired {
		t.Errorf("initial status = %+v, want disconnected and unpaired", st)
	}
	want := wa.Status{State: wa.StateConnected, AccountJID: "1@s.whatsapp.net", Paired: true}
	f.SetStatus(want)
	if st := f.Status(); st != want {
		t.Errorf("Status = %+v, want %+v", st, want)
	}
	f.Disconnect()
	st := f.Status()
	if st.State != wa.StateDisconnected {
		t.Errorf("State after Disconnect = %q", st.State)
	}
	if !st.Paired || st.AccountJID != "1@s.whatsapp.net" {
		t.Errorf("Disconnect must keep pairing, got %+v", st)
	}
}

func TestLogoutRecordsCallAndUnpairs(t *testing.T) {
	t.Parallel()
	f := watest.NewFake()
	f.SetStatus(wa.Status{State: wa.StateConnected, AccountJID: "1@s.whatsapp.net", Paired: true})
	if err := f.Logout(context.Background()); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if f.LogoutCalls() != 1 {
		t.Errorf("LogoutCalls = %d, want 1", f.LogoutCalls())
	}
	if st := f.Status(); st.State != wa.StateLoggedOut || st.Paired || st.AccountJID != "" {
		t.Errorf("status after logout = %+v", st)
	}
}

func TestDownloadMediaByDirectPath(t *testing.T) {
	t.Parallel()
	f := watest.NewFake()
	payload := []byte("jpeg-bytes")
	f.SetMedia("/v/t62/abc", payload)
	payload[0] = 'X'

	got, err := f.DownloadMedia(context.Background(), wa.Media{DirectPath: "/v/t62/abc"})
	if err != nil {
		t.Fatalf("DownloadMedia: %v", err)
	}
	if string(got) != "jpeg-bytes" {
		t.Errorf("DownloadMedia = %q; fake must keep its own copy", got)
	}
	got[0] = 'Y'
	again, _ := f.DownloadMedia(context.Background(), wa.Media{DirectPath: "/v/t62/abc"})
	if string(again) != "jpeg-bytes" {
		t.Errorf("mutating a returned slice changed stored media: %q", again)
	}

	if _, err := f.DownloadMedia(context.Background(), wa.Media{DirectPath: "/other"}); err == nil {
		t.Error("DownloadMedia for an unknown DirectPath must fail")
	}
	if _, err := f.DownloadMedia(context.Background(), wa.Media{}); err == nil {
		t.Error("DownloadMedia for an empty DirectPath must fail")
	}
}

func TestContactsReturnsACopy(t *testing.T) {
	t.Parallel()
	f := watest.NewFake()
	in := []wa.Contact{{JID: "1@s.whatsapp.net", Name: "Alice"}, {JID: "2@s.whatsapp.net", PushName: "Bob"}}
	f.SetContacts(in)
	in[0].Name = "mutated-input"

	got, err := f.Contacts(context.Background())
	if err != nil {
		t.Fatalf("Contacts: %v", err)
	}
	if len(got) != 2 || got[0].Name != "Alice" || got[1].PushName != "Bob" {
		t.Fatalf("Contacts = %+v", got)
	}
	got[0].Name = "mutated-output"
	again, _ := f.Contacts(context.Background())
	if again[0].Name != "Alice" {
		t.Errorf("mutating a returned slice changed stored contacts: %+v", again)
	}
}

func TestConcurrentUseIsRaceFree(t *testing.T) {
	t.Parallel()
	const workers, perWorker = 16, 25
	f := watest.NewFake()
	var seen sync.Map
	f.SetEventHandler(func(wa.Event) {})

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWorker {
				sent, err := f.SendText(context.Background(), "1@s.whatsapp.net", fmt.Sprintf("w%d-%d", w, i))
				if err != nil {
					t.Errorf("SendText: %v", err)
					return
				}
				if _, dup := seen.LoadOrStore(sent.ID, true); dup {
					t.Errorf("duplicate message ID %q", sent.ID)
				}
				f.Emit(wa.ConnectedEvent{})
				_ = f.Status()
				_ = f.Sent()
				f.SetStatus(wa.Status{State: wa.StateConnected})
				f.SetEventHandler(func(wa.Event) {})
				_, _ = f.Contacts(context.Background())
				_ = f.Connect(context.Background())
			}
		}()
	}
	wg.Wait()

	if got := len(f.Sent()); got != workers*perWorker {
		t.Errorf("recorded %d sends, want %d", got, workers*perWorker)
	}
	if f.ConnectCalls() != workers*perWorker {
		t.Errorf("ConnectCalls = %d, want %d", f.ConnectCalls(), workers*perWorker)
	}
}
