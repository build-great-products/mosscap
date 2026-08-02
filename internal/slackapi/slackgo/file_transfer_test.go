package slackgo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stayradiated/mosscap/internal/slackapi"
)

type transferDoer struct {
	mu       sync.Mutex
	handlers []func(*http.Request) (*http.Response, error)
}

func (doer *transferDoer) Do(request *http.Request) (*http.Response, error) {
	doer.mu.Lock()
	defer doer.mu.Unlock()
	if len(doer.handlers) == 0 {
		return nil, io.ErrUnexpectedEOF
	}
	handler := doer.handlers[0]
	doer.handlers = doer.handlers[1:]
	return handler(request)
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestDownloadFollowsOnlyApprovedRedirectAndStripsToken(t *testing.T) {
	const token = "xoxb-download-token-canary"
	doer := &transferDoer{handlers: []func(*http.Request) (*http.Response, error){
		func(request *http.Request) (*http.Response, error) {
			if request.URL.Path != "/api/files.info" {
				t.Fatalf("files.info path = %q", request.URL.Path)
			}
			return jsonResponse(http.StatusOK, `{"ok":true,"file":{"id":"F012ABC","name":"note.txt","mimetype":"text/plain","size":3,"url_private_download":"https://files.slack.com/private"}}`), nil
		},
		func(request *http.Request) (*http.Response, error) {
			if got := request.Header.Get("Authorization"); got != "Bearer "+token {
				t.Fatalf("initial authorization = %q", got)
			}
			response := jsonResponse(http.StatusFound, "")
			response.Header.Set("Location", "https://uploads.slack-edge.com/content")
			return response, nil
		},
		func(request *http.Request) (*http.Response, error) {
			if got := request.Header.Get("Authorization"); got != "" {
				t.Fatalf("cross-origin authorization = %q", got)
			}
			return jsonResponse(http.StatusOK, "abc"), nil
		},
	}}
	web := NewWeb(token, WebAPIURL("https://api.slack.com/api/"), WebHTTPClient(doer))
	var content bytes.Buffer
	file, err := web.Download(context.Background(), slackapi.DownloadRequest{FileID: "F012ABC", MaximumBytes: 3}, &content)
	if err != nil || file.ID != "F012ABC" || content.String() != "abc" {
		t.Fatalf("Download = %#v, %q, %v", file, content.String(), err)
	}
	if len(doer.handlers) != 0 {
		t.Fatalf("unconsumed exchanges = %d", len(doer.handlers))
	}
}

func TestDownloadHTTPSRedirectCanaryNeverForwardsToken(t *testing.T) {
	const token = "xoxb-httptest-token-canary"
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Host {
		case "api.slack.com":
			if request.URL.Path != "/api/files.info" {
				t.Fatalf("API path = %q", request.URL.Path)
			}
			_, _ = writer.Write([]byte(`{"ok":true,"file":{"id":"F012ABC","name":"a","size":1,"url_private_download":"https://files.slack.com/private"}}`))
		case "files.slack.com":
			if got := request.Header.Get("Authorization"); got != "Bearer "+token {
				t.Fatalf("initial token = %q", got)
			}
			writer.Header().Set("Location", "https://uploads.slack-edge.com/content")
			writer.WriteHeader(http.StatusFound)
		case "uploads.slack-edge.com":
			if got := request.Header.Get("Authorization"); got != "" {
				t.Fatalf("redirect token = %q", got)
			}
			_, _ = writer.Write([]byte("a"))
		default:
			t.Fatalf("unexpected host %q", request.Host)
		}
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := server.Client()
	transport := client.Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.InsecureSkipVerify = true // test-only host routing
	transport.DialContext = func(context.Context, string, string) (net.Conn, error) { return net.Dial("tcp", serverURL.Host) }
	client.Transport = transport
	web := NewWeb(token, WebAPIURL("https://api.slack.com/api/"), WebHTTPClient(client))
	var content bytes.Buffer
	if _, err := web.Download(context.Background(), slackapi.DownloadRequest{FileID: "F012ABC", MaximumBytes: 1}, &content); err != nil || content.String() != "a" {
		t.Fatalf("Download content=%q err=%v", content.String(), err)
	}
}

func TestDownloadRejectsUnsafeOrOversizeBeforeWritingUnboundedBytes(t *testing.T) {
	tests := []struct {
		name, url string
		length    int64
		want      slackapi.DiagnosticCode
	}{
		{"ip", "https://127.0.0.1/private", 0, slackapi.DiagnosticUnsafeFileRedirect},
		{"port", "https://files.slack.com:444/private", 0, slackapi.DiagnosticUnsafeFileRedirect},
		{"oversize", "https://files.slack.com/private", 4, slackapi.DiagnosticFileTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doer := &transferDoer{handlers: []func(*http.Request) (*http.Response, error){
				func(*http.Request) (*http.Response, error) {
					return jsonResponse(http.StatusOK, `{"ok":true,"file":{"id":"F012ABC","name":"note.txt","size":3,"url_private_download":"`+test.url+`"}}`), nil
				},
			}}
			if test.length > 0 {
				doer.handlers = append(doer.handlers, func(*http.Request) (*http.Response, error) {
					response := jsonResponse(http.StatusOK, "abcd")
					response.ContentLength = test.length
					return response, nil
				})
			}
			web := NewWeb("xoxb-test", WebAPIURL("https://api.slack.com/api/"), WebHTTPClient(doer))
			var content bytes.Buffer
			_, err := web.Download(context.Background(), slackapi.DownloadRequest{FileID: "F012ABC", MaximumBytes: 3}, &content)
			failure := slackapi.Classify(err, slackapi.FailureRead)
			if failure.Diagnostic != test.want {
				t.Fatalf("failure = %#v (%v)", failure, err)
			}
			if content.Len() != 0 {
				t.Fatalf("wrote %d bytes", content.Len())
			}
		})
	}
}

func TestUploadStagesZeroAndMultipleFilesThenProvesCompletion(t *testing.T) {
	const token = "xoxb-upload-token-canary"
	var validationCalls int
	doer := &transferDoer{handlers: []func(*http.Request) (*http.Response, error){
		func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"ok":true,"file_id":"F001","upload_url":"https://uploads.slack-edge.com/one"}`), nil
		},
		func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"ok":true,"file_id":"F002","upload_url":"https://uploads.slack-edge.com/two"}`), nil
		},
		func(request *http.Request) (*http.Response, error) {
			if request.Header.Get("Authorization") != "" {
				t.Fatal("token forwarded to staging host")
			}
			_, _ = io.ReadAll(request.Body)
			return jsonResponse(http.StatusOK, ""), nil
		},
		func(request *http.Request) (*http.Response, error) {
			if request.Header.Get("Authorization") != "" {
				t.Fatal("token forwarded to staging host")
			}
			body, _ := io.ReadAll(request.Body)
			if !bytes.Contains(body, []byte("abc")) {
				t.Fatal("staged body missing bytes")
			}
			return jsonResponse(http.StatusOK, ""), nil
		},
		func(request *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(request.Body)
			if !bytes.Contains(body, []byte("F001")) || !bytes.Contains(body, []byte("F002")) {
				t.Fatal("completion did not carry reservations")
			}
			return jsonResponse(http.StatusOK, `{"ok":true,"files":[{"id":"F001","permalink":"https://files.slack.com/F001"},{"id":"F002","permalink":"https://files.slack.com/F002"}]}`), nil
		},
	}}
	web := NewWeb(token, WebAPIURL("https://api.slack.com/api/"), WebHTTPClient(doer))
	result, err := web.Upload(context.Background(), slackapi.UploadRequest{ChannelID: "C012ABC", InitialComment: "comment", Files: []slackapi.UploadSource{
		{Name: "zero", SizeBytes: 0, Reader: strings.NewReader(""), ValidateAfter: func(context.Context) error { validationCalls++; return nil }},
		{Name: "three", SizeBytes: 3, Reader: strings.NewReader("abc"), ValidateAfter: func(context.Context) error { validationCalls++; return nil }},
	}})
	if err != nil || len(result.Files) != 2 || validationCalls != 2 {
		t.Fatalf("Upload = %#v, validations=%d, err=%v", result, validationCalls, err)
	}
}

func TestUploadValidatorPreventsCompletion(t *testing.T) {
	doer := &transferDoer{handlers: []func(*http.Request) (*http.Response, error){
		func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"ok":true,"file_id":"F001","upload_url":"https://uploads.slack-edge.com/one"}`), nil
		},
		func(request *http.Request) (*http.Response, error) {
			_, _ = io.ReadAll(request.Body)
			return jsonResponse(http.StatusOK, ""), nil
		},
	}}
	web := NewWeb("xoxb-test", WebAPIURL("https://api.slack.com/api/"), WebHTTPClient(doer))
	_, err := web.Upload(context.Background(), slackapi.UploadRequest{ChannelID: "C012ABC", Files: []slackapi.UploadSource{{Name: "one", SizeBytes: 1, Reader: strings.NewReader("a"), ValidateAfter: func(context.Context) error { return io.ErrUnexpectedEOF }}}})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Upload error = %v", err)
	}
	if len(doer.handlers) != 0 {
		t.Fatalf("completion was called")
	}
}
