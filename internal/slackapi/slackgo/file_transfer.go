package slackgo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/slack-go/slack"
	"github.com/stayradiated/mosscap/internal/slackapi"
)

const (
	maxFileRedirects = 5
	defaultWebAPIURL = "https://slack.com/api/"
)

// Download obtains fresh Slack metadata, then streams the private URL through
// the frozen redirect policy. It never returns the private URL.
func (web *Web) Download(ctx context.Context, request slackapi.DownloadRequest, writer io.Writer) (slackapi.File, error) {
	if web.optionErr != nil || writer == nil || request.MaximumBytes < 0 || slackapi.ValidateFileID(request.FileID) != nil {
		return slackapi.File{}, invalidRequest()
	}
	file, _, _, err := web.client.GetFileInfoContext(ctx, request.FileID, 0, 0)
	if err != nil {
		return slackapi.File{}, sanitizeError(err)
	}
	if file == nil || file.ID != request.FileID {
		return slackapi.File{}, invalidResponse()
	}
	metadata, err := normalizeFiles([]slack.File{*file}, 1)
	if err != nil {
		return slackapi.File{}, invalidResponse()
	}
	if metadata[0].SizeBytes != nil && *metadata[0].SizeBytes > request.MaximumBytes {
		return slackapi.File{}, fileTooLarge()
	}
	privateURL := file.URLPrivateDownload
	if privateURL == "" {
		privateURL = file.URLPrivate
	}
	if err := web.downloadPrivate(ctx, privateURL, request.MaximumBytes, writer); err != nil {
		return slackapi.File{}, err
	}
	return metadata[0], nil
}

// Upload performs Slack's reserve, stage, validate, complete sequence without
// slack-go's zero-byte convenience guard. A source validator is called only
// after all staging succeeds and before the one completion call.
func (web *Web) Upload(ctx context.Context, request slackapi.UploadRequest) (slackapi.UploadResult, error) {
	if web.optionErr != nil || validateUploadRequest(request) != nil {
		return slackapi.UploadResult{}, invalidRequest()
	}
	reservations := make([]uploadReservation, 0, len(request.Files))
	for _, source := range request.Files {
		reservation, err := web.reserveUpload(ctx, source)
		if err != nil {
			return slackapi.UploadResult{}, err
		}
		reservations = append(reservations, reservation)
	}
	for index, source := range request.Files {
		if err := web.stageUpload(ctx, reservations[index], source); err != nil {
			return slackapi.UploadResult{}, err
		}
	}
	for _, source := range request.Files {
		if source.ValidateAfter != nil {
			if err := source.ValidateAfter(ctx); err != nil {
				// This was a local post-stage proof failure. Completion has not
				// happened, so preserve the caller's typed local error.
				return slackapi.UploadResult{}, err
			}
		}
	}
	return web.completeUpload(ctx, request, reservations)
}

type uploadReservation struct {
	fileID    string
	uploadURL string
}

func (web *Web) reserveUpload(ctx context.Context, source slackapi.UploadSource) (uploadReservation, error) {
	values := url.Values{
		"token":    {web.token},
		"filename": {source.Name},
		"length":   {strconv.FormatInt(source.SizeBytes, 10)},
	}
	var response struct {
		OK        bool   `json:"ok"`
		Error     string `json:"error"`
		UploadURL string `json:"upload_url"`
		FileID    string `json:"file_id"`
	}
	if err := web.apiForm(ctx, "files.getUploadURLExternal", values, &response); err != nil {
		return uploadReservation{}, err
	}
	if !response.OK {
		reason, diagnostic := slackAPIError(response.Error)
		return uploadReservation{}, slackapi.NewExternalError(reason, diagnostic, "", nil)
	}
	if slackapi.ValidateFileID(response.FileID) != nil || validateTransferURL(response.UploadURL) != nil {
		return uploadReservation{}, invalidResponse()
	}
	return uploadReservation{fileID: response.FileID, uploadURL: response.UploadURL}, nil
}

func (web *Web) stageUpload(ctx context.Context, reservation uploadReservation, source slackapi.UploadSource) error {
	if err := validateTransferURL(reservation.uploadURL); err != nil {
		return unsafeRedirect()
	}
	pipeReader, pipeWriter := io.Pipe()
	defer pipeReader.Close()
	multipartWriter := multipart.NewWriter(pipeWriter)
	writeDone := make(chan error, 1)
	go func() {
		part, err := multipartWriter.CreateFormFile("file", source.Name)
		if err == nil {
			var written int64
			written, err = io.Copy(part, io.LimitReader(source.Reader, source.SizeBytes))
			if err == nil && written != source.SizeBytes {
				err = io.ErrUnexpectedEOF
			}
			if err == nil {
				// The adapter never silently stages a different byte count.
				// Read exactly one more byte after the declared amount.
				var extra [1]byte
				count, readErr := source.Reader.Read(extra[:])
				if count != 0 || readErr == nil {
					err = errors.New("source exceeds declared size")
				} else if readErr != io.EOF {
					err = readErr
				}
			}
		}
		if closeErr := multipartWriter.Close(); err == nil {
			err = closeErr
		}
		_ = pipeWriter.CloseWithError(err)
		writeDone <- err
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reservation.uploadURL, pipeReader)
	if err != nil {
		return invalidRequest()
	}
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	// Staging URLs are frequently a different presigned origin. The bot token
	// is never forwarded there. It is attached only for the original API origin.
	if sameOrigin(req.URL, web.apiOrigin) {
		req.Header.Set("Authorization", "Bearer "+web.token)
	}
	response, err := web.fileHTTP.Do(req)
	if err != nil {
		return sanitizeError(err)
	}
	if response == nil {
		return slackapi.NewExternalError(slackapi.ReasonUnavailable, slackapi.DiagnosticUnavailable, "", nil)
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return unsafeRedirect()
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return statusError(response.StatusCode)
	}
	responseBytes, readErr := io.ReadAll(io.LimitReader(response.Body, maxWebResponseBytes+1))
	if readErr != nil || int64(len(responseBytes)) > maxWebResponseBytes {
		return invalidResponse()
	}
	if err := <-writeDone; err != nil {
		return slackapi.NewExternalError(slackapi.ReasonRejected, slackapi.DiagnosticInvalidRequest, "", nil)
	}
	return nil
}

func (web *Web) completeUpload(ctx context.Context, request slackapi.UploadRequest, reservations []uploadReservation) (slackapi.UploadResult, error) {
	files := make([]map[string]string, 0, len(reservations))
	for _, reservation := range reservations {
		files = append(files, map[string]string{"id": reservation.fileID})
	}
	encoded, err := json.Marshal(files)
	if err != nil {
		return slackapi.UploadResult{}, invalidRequest()
	}
	values := url.Values{"token": {web.token}, "files": {string(encoded)}, "channel_id": {request.ChannelID}}
	if request.ThreadTS != "" {
		values.Set("thread_ts", string(request.ThreadTS))
	}
	if request.InitialComment != "" {
		values.Set("initial_comment", request.InitialComment)
	}
	var response struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Files []struct {
			ID        string `json:"id"`
			Permalink string `json:"permalink"`
		} `json:"files"`
	}
	if err := web.apiForm(ctx, "files.completeUploadExternal", values, &response); err != nil {
		return slackapi.UploadResult{}, err
	}
	if !response.OK {
		reason, diagnostic := slackAPIError(response.Error)
		return slackapi.UploadResult{}, slackapi.NewExternalError(reason, diagnostic, "", nil)
	}
	if len(response.Files) != len(reservations) {
		return slackapi.UploadResult{}, invalidResponse()
	}
	result := slackapi.UploadResult{Files: make([]slackapi.UploadedFile, 0, len(response.Files))}
	for index, file := range response.Files {
		if file.ID != reservations[index].fileID || slackapi.ValidateFileID(file.ID) != nil {
			return slackapi.UploadResult{}, invalidResponse()
		}
		if file.Permalink != "" && validatePermalink(file.Permalink) != nil {
			return slackapi.UploadResult{}, invalidResponse()
		}
		confirmed := slackapi.UploadedFile{ID: file.ID, Permalink: file.Permalink}
		if confirmed.Permalink == "" {
			// Completion already proves the upload. A later metadata read cannot
			// make that applied operation ambiguous.
			confirmed.Permalink, confirmed.Diagnostic = web.confirmedPermalink(ctx, file.ID)
		}
		result.Files = append(result.Files, confirmed)
	}
	return result, nil
}

func (web *Web) confirmedPermalink(ctx context.Context, fileID string) (string, slackapi.DiagnosticCode) {
	file, _, _, err := web.client.GetFileInfoContext(ctx, fileID, 0, 0)
	if err != nil || file == nil || file.ID != fileID || validatePermalink(file.Permalink) != nil {
		return "", slackapi.DiagnosticPermalinkUnavailable
	}
	return file.Permalink, ""
}

func (web *Web) apiForm(ctx context.Context, method string, values url.Values, destination any) error {
	endpoint := strings.TrimRight(web.apiURL, "/") + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return invalidRequest()
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := web.apiHTTP.Do(req)
	if err != nil {
		return sanitizeError(err)
	}
	if response == nil || response.Body == nil {
		return invalidResponse()
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return statusError(response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxWebResponseBytes+1))
	if err != nil || int64(len(body)) > maxWebResponseBytes || json.Unmarshal(body, destination) != nil {
		return invalidResponse()
	}
	return nil
}

func (web *Web) downloadPrivate(ctx context.Context, rawURL string, maximum int64, destination io.Writer) error {
	current, err := parseTransferURL(rawURL)
	if err != nil {
		return unsafeRedirect()
	}
	original := *current
	crossedOrigin := false
	seen := make(map[string]struct{}, maxFileRedirects+1)
	for hop := 0; ; hop++ {
		canonical := current.String()
		if _, duplicate := seen[canonical]; duplicate {
			return unsafeRedirect()
		}
		seen[canonical] = struct{}{}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current.String(), nil)
		if err != nil {
			return unsafeRedirect()
		}
		if !crossedOrigin && sameOrigin(current, &original) {
			req.Header.Set("Authorization", "Bearer "+web.token)
		}
		response, err := web.fileHTTP.Do(req)
		if err != nil {
			return sanitizeError(err)
		}
		if response == nil || response.Body == nil {
			return slackapi.NewExternalError(slackapi.ReasonUnavailable, slackapi.DiagnosticUnavailable, "", nil)
		}
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			_ = response.Body.Close()
			if hop >= maxFileRedirects {
				return unsafeRedirect()
			}
			next, err := current.Parse(response.Header.Get("Location"))
			if err != nil || validateTransferURL(next.String()) != nil {
				return unsafeRedirect()
			}
			if !sameOrigin(next, current) {
				crossedOrigin = true
			}
			current = next
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			_ = response.Body.Close()
			return downloadStatusError(response.StatusCode)
		}
		if response.ContentLength > maximum {
			_ = response.Body.Close()
			return fileTooLarge()
		}
		// Keep the caller's writer at its admitted limit while retaining one
		// extra streamed byte solely to prove an overrun. In particular, a
		// bounded destination may reject that extra byte with its own local
		// error; that must not erase the conclusive size classification.
		count, copyErr := io.Copy(&downloadLimitWriter{destination: destination, remaining: maximum}, io.LimitReader(response.Body, downloadReadLimit(maximum)))
		closeErr := response.Body.Close()
		if errors.Is(copyErr, errDownloadLimitExceeded) {
			return fileTooLarge()
		}
		if copyErr != nil || closeErr != nil {
			return sanitizeError(firstError(copyErr, closeErr))
		}
		if count > maximum {
			return fileTooLarge()
		}
		return nil
	}
}

var errDownloadLimitExceeded = errors.New("download limit exceeded")

type downloadLimitWriter struct {
	destination io.Writer
	remaining   int64
}

func (writer *downloadLimitWriter) Write(bytes []byte) (int, error) {
	allowed := len(bytes)
	if int64(allowed) > writer.remaining {
		allowed = int(writer.remaining)
	}
	if allowed > 0 {
		written, err := writer.destination.Write(bytes[:allowed])
		writer.remaining -= int64(written)
		if err != nil {
			return written, err
		}
		if written != allowed {
			return written, io.ErrShortWrite
		}
	}
	if allowed != len(bytes) {
		return allowed, errDownloadLimitExceeded
	}
	return allowed, nil
}

func downloadReadLimit(maximum int64) int64 {
	if maximum == math.MaxInt64 {
		return maximum
	}
	return maximum + 1
}

func validateUploadRequest(request slackapi.UploadRequest) error {
	if slackapi.ValidateChannelID(request.ChannelID) != nil || !request.ThreadTS.Valid() && request.ThreadTS != "" || len(request.InitialComment) > slackapi.MaxTextBytes || !utf8.ValidString(request.InitialComment) || strings.IndexByte(request.InitialComment, 0) >= 0 || len(request.Files) == 0 || len(request.Files) > slackapi.MaxFiles {
		return errors.New("invalid upload request")
	}
	for _, source := range request.Files {
		if source.Reader == nil || source.SizeBytes < 0 || len(source.Name) == 0 || len(source.Name) > slackapi.MaxFileNameBytes || !utf8.ValidString(source.Name) || strings.IndexByte(source.Name, 0) >= 0 {
			return errors.New("invalid upload source")
		}
	}
	return nil
}

func parseTransferURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || validateTransferURL(parsed.String()) != nil {
		return nil, errors.New("unsafe transfer URL")
	}
	return parsed, nil
}

func validateTransferURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Port() != "" && parsed.Port() != "443" {
		return errors.New("unsafe transfer URL")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" || net.ParseIP(host) != nil || !asciiHostname(host) || !allowedFileHost(host) {
		return errors.New("unsafe transfer URL")
	}
	return nil
}

func asciiHostname(host string) bool {
	if len(host) == 0 || len(host) > 253 || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if !(character >= 'a' && character <= 'z') && !(character >= '0' && character <= '9') && character != '-' {
				return false
			}
		}
	}
	return true
}

func allowedFileHost(host string) bool {
	return host == "files.slack.com" || strings.HasSuffix(host, ".slack.com") || strings.HasSuffix(host, ".slack-edge.com")
}

func sameOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Hostname(), right.Hostname()) && effectivePort(left) == effectivePort(right)
}

func effectivePort(value *url.URL) string {
	if port := value.Port(); port != "" {
		return port
	}
	if value.Scheme == "https" {
		return "443"
	}
	return ""
}

func unsafeRedirect() error {
	return slackapi.NewExternalError(slackapi.ReasonRejected, slackapi.DiagnosticUnsafeFileRedirect, "", nil)
}

func fileTooLarge() error {
	return slackapi.NewExternalError(slackapi.ReasonRejected, slackapi.DiagnosticFileTooLarge, "", nil)
}

func downloadStatusError(status int) error {
	if status == http.StatusNotFound || status == http.StatusGone || status == http.StatusForbidden {
		return slackapi.NewExternalError(slackapi.ReasonRejected, slackapi.DiagnosticSlackFileUnavailable, "", nil)
	}
	return statusError(status)
}

func statusError(status int) error {
	if status == http.StatusRequestTimeout {
		return slackapi.NewExternalError(slackapi.ReasonTimeout, slackapi.DiagnosticTimeout, "", nil)
	}
	if status == http.StatusTooManyRequests {
		return slackapi.NewExternalError(slackapi.ReasonUnavailable, slackapi.DiagnosticRateLimited, "", nil)
	}
	if status >= 400 && status < 500 {
		return slackapi.NewExternalError(slackapi.ReasonRejected, slackapi.DiagnosticRejected, "", nil)
	}
	return slackapi.NewExternalError(slackapi.ReasonUnavailable, slackapi.DiagnosticUnavailable, "", nil)
}

func firstError(first, second error) error {
	if first != nil {
		return first
	}
	return second
}

// noAutoRedirect preserves redirect responses for the explicit transfer
// walker. Non-*http.Client doers are test seams and must themselves perform
// one exchange, as documented by HTTPDoer.
func noAutoRedirect(client HTTPDoer) HTTPDoer {
	if httpClient, ok := client.(*http.Client); ok {
		clone := *httpClient
		clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		return &clone
	}
	return client
}
