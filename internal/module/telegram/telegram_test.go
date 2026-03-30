package telegram

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
)

func TestReplyPreservesThreadAndReplyTarget(t *testing.T) {
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("Reply() method = %q, want %q", req.Method, http.MethodPost)
			}
			if got, want := req.URL.Path, "/bottoken/sendMessage"; got != want {
				t.Fatalf("Reply() path = %q, want %q", got, want)
			}

			mediaType, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
			if err != nil {
				t.Fatalf("ParseMediaType(Content-Type) error = %v", err)
			}
			if mediaType != "multipart/form-data" {
				t.Fatalf("Reply() content type = %q, want multipart/form-data", mediaType)
			}

			reader := multipart.NewReader(req.Body, params["boundary"])
			values := make(map[string]string)
			for {
				part, err := reader.NextPart()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("NextPart() error = %v", err)
				}

				body, err := io.ReadAll(part)
				if err != nil {
					t.Fatalf("ReadAll(part) error = %v", err)
				}
				values[part.FormName()] = string(body)
			}

			if got := values["chat_id"]; got != "12345" {
				t.Fatalf("Reply() chat_id = %q, want %q", got, "12345")
			}
			if got := values["message_thread_id"]; got != "99" {
				t.Fatalf("Reply() message_thread_id = %q, want %q", got, "99")
			}
			if got := values["text"]; got != `<b>hi</b>` {
				t.Fatalf("Reply() text = %q, want %q", got, `<b>hi</b>`)
			}
			if got := values["parse_mode"]; got != "HTML" {
				t.Fatalf("Reply() parse_mode = %q, want %q", got, "HTML")
			}
			if !strings.Contains(values["reply_parameters"], `"message_id":77`) {
				t.Fatalf("Reply() reply_parameters = %q, want message_id", values["reply_parameters"])
			}
			if !strings.Contains(values["reply_parameters"], `"allow_sending_without_reply":true`) {
				t.Fatalf("Reply() reply_parameters = %q, want allow_sending_without_reply", values["reply_parameters"])
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":1,"date":1,"chat":{"id":12345,"type":"supergroup"}}}`)),
				Header:     make(http.Header),
			}, nil
		}),
	}

	botClient, err := newBot("token", client)
	if err != nil {
		t.Fatalf("newBot() error = %v", err)
	}

	module := &Module{
		cfg:            Config{ChatID: "unused"},
		client:         botClient,
		requestTimeout: sendTimeout,
	}

	err = module.Reply(context.Background(), ReplyTarget{
		ChatID:          12345,
		MessageID:       77,
		MessageThreadID: 99,
	}, `<b>hi</b>`)
	if err != nil {
		t.Fatalf("Reply() error = %v", err)
	}
}

func TestSendToThreadPreservesThreadWithoutReplyTarget(t *testing.T) {
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			mediaType, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
			if err != nil {
				t.Fatalf("ParseMediaType(Content-Type) error = %v", err)
			}
			if mediaType != "multipart/form-data" {
				t.Fatalf("SendToThread() content type = %q, want multipart/form-data", mediaType)
			}

			reader := multipart.NewReader(req.Body, params["boundary"])
			values := make(map[string]string)
			for {
				part, err := reader.NextPart()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("NextPart() error = %v", err)
				}

				body, err := io.ReadAll(part)
				if err != nil {
					t.Fatalf("ReadAll(part) error = %v", err)
				}
				values[part.FormName()] = string(body)
			}

			if got := values["chat_id"]; got != "12345" {
				t.Fatalf("SendToThread() chat_id = %q, want %q", got, "12345")
			}
			if got := values["message_thread_id"]; got != "55" {
				t.Fatalf("SendToThread() message_thread_id = %q, want %q", got, "55")
			}
			if got := values["text"]; got != `<b>next</b>` {
				t.Fatalf("SendToThread() text = %q, want %q", got, `<b>next</b>`)
			}
			if _, ok := values["reply_parameters"]; ok {
				t.Fatalf("SendToThread() reply_parameters present = %q, want omitted", values["reply_parameters"])
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":2,"date":1,"chat":{"id":12345,"type":"supergroup"}}}`)),
				Header:     make(http.Header),
			}, nil
		}),
	}

	botClient, err := newBot("token", client)
	if err != nil {
		t.Fatalf("newBot() error = %v", err)
	}

	module := &Module{
		client:         botClient,
		requestTimeout: sendTimeout,
	}

	err = module.SendToThread(context.Background(), ThreadTarget{
		ChatID:          12345,
		MessageThreadID: 55,
	}, `<b>next</b>`)
	if err != nil {
		t.Fatalf("SendToThread() error = %v", err)
	}
}

func TestSplitTextRespectsChunkLimitAndTruncatesOverflow(t *testing.T) {
	short := SplitText("hello", 2)
	if len(short) != 1 || short[0] != "hello" {
		t.Fatalf("SplitText(short) = %#v, want [\"hello\"]", short)
	}

	twoChunksInput := strings.Repeat("chunk ", 900)
	twoChunks := SplitText(twoChunksInput, 2)
	if len(twoChunks) != 2 {
		t.Fatalf("SplitText(twoChunks) len = %d, want 2", len(twoChunks))
	}
	for index, chunk := range twoChunks {
		if plainTextLength(chunk) > MessageLimit {
			t.Fatalf("chunk %d length = %d, want <= %d", index, plainTextLength(chunk), MessageLimit)
		}
	}

	overflowInput := strings.Repeat("overflow ", 1400)
	overflow := SplitText(overflowInput, 2)
	if len(overflow) != 2 {
		t.Fatalf("SplitText(overflow) len = %d, want 2", len(overflow))
	}
	if !strings.HasSuffix(overflow[1], TruncationNotice) {
		t.Fatalf("overflow last chunk = %q, want truncation notice suffix", overflow[1])
	}
	if plainTextLength(overflow[1]) > MessageLimit {
		t.Fatalf("overflow last chunk length = %d, want <= %d", plainTextLength(overflow[1]), MessageLimit)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
