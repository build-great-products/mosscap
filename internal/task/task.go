// Package task defines task domain values and text normalization.
package task

import (
	"errors"
	"strings"
	"unicode/utf8"
)

type State string

const (
	Open    State = "open"
	Working State = "working"
	Done    State = "done"
)

func (s State) Valid() bool { return s == Open || s == Working || s == Done }

func CanTransition(from, to State) bool {
	return from.Valid() && to.Valid() && from != to
}

// NormalizeText makes line endings canonical and removes trailing spaces and
// tabs from each line without otherwise changing user content.
func NormalizeText(text string) (string, error) {
	if !utf8.ValidString(text) {
		return "", errors.New("task text is not valid UTF-8")
	}
	if strings.IndexByte(text, 0) >= 0 {
		return "", errors.New("task text contains NUL")
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	text = strings.Join(lines, "\n")
	if text == "" {
		return "", errors.New("task text is empty after normalization")
	}
	return text, nil
}
