package slackgo

import (
	"errors"
	"net/url"
	"unicode/utf8"

	"github.com/slack-go/slack"
	"github.com/stayradiated/mosscap/internal/slackapi"
)

func normalizeFiles(files []slack.File, maximum int) ([]slackapi.File, error) {
	if maximum < 1 || maximum > slackapi.MaxFiles || len(files) > maximum {
		return nil, errors.New("too many Slack files")
	}
	result := make([]slackapi.File, 0, len(files))
	for _, sdkFile := range files {
		if sdkFile.Size < 0 {
			return nil, errors.New("invalid Slack file size")
		}
		size := int64(sdkFile.Size)
		file := slackapi.File{
			ID:        sdkFile.ID,
			Name:      sdkFile.Name,
			MIMEType:  sdkFile.Mimetype,
			Permalink: sdkFile.Permalink,
			SizeBytes: &size,
		}
		if err := slackapi.ValidateFile(file); err != nil {
			return nil, err
		}
		result = append(result, file)
	}
	return result, nil
}

func validatePermalink(value string) error {
	if value == "" || len(value) > slackapi.MaxPermalinkBytes || !utf8.ValidString(value) {
		return errors.New("invalid Slack permalink")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return errors.New("invalid Slack permalink")
	}
	return nil
}
