package slackapi_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/stayradiated/mosscap/internal/slackapi"
	"github.com/stayradiated/mosscap/internal/slackapi/fake"
)

func TestVerifyProvesCompleteRuntimeAuthority(t *testing.T) {
	required := slackapi.RequiredBotScopes(false)
	web := fake.NewWeb(
		fake.Step{Operation: fake.Auth, Result: slackapi.AuthIdentity{TeamID: "T012ABC", BotUserID: "U012BOT"}},
		fake.Step{Operation: fake.Team, Request: "T012ABC", Result: slackapi.Team{ID: "T012ABC"}},
		fake.Step{Operation: fake.Channel, Request: "C012ABC", Result: slackapi.Channel{ID: "C012ABC", TeamID: "T012ABC", Member: true, Accessible: true}},
		fake.Step{Operation: fake.Scopes, Result: required},
		fake.Step{Operation: fake.User, Request: "U012USR", Result: slackapi.User{ID: "U012USR", TeamID: "T012ABC"}},
	)
	result, err := slackapi.Verify(context.Background(), web, slackapi.VerificationRequest{
		ChannelID: "C012ABC", TrustedUserIDs: []string{"U012USR"},
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if result.Identity.BotUserID != "U012BOT" || len(result.Scopes) != len(required) || len(result.Users) != 1 {
		t.Fatalf("result = %#v", result)
	}
	if err := web.AssertDone(); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRejectsIdentityScopeAndUserMismatch(t *testing.T) {
	tests := []struct {
		name string
		web  *fake.Web
		code slackapi.DiagnosticCode
	}{
		{
			name: "team", code: slackapi.DiagnosticTeamMismatch,
			web: fake.NewWeb(fake.Step{Operation: fake.Auth, Result: slackapi.AuthIdentity{TeamID: "T999BAD", BotUserID: "U012BOT"}}),
		},
		{
			name: "bot", code: slackapi.DiagnosticBotMismatch,
			web: fake.NewWeb(fake.Step{Operation: fake.Auth, Result: slackapi.AuthIdentity{TeamID: "T012ABC", BotUserID: "U999BAD"}}),
		},
		{
			name: "scope", code: slackapi.DiagnosticMissingScope,
			web: fake.NewWeb(
				fake.Step{Operation: fake.Auth, Result: slackapi.AuthIdentity{TeamID: "T012ABC", BotUserID: "U012BOT"}},
				fake.Step{Operation: fake.Team, Result: slackapi.Team{ID: "T012ABC"}},
				fake.Step{Operation: fake.Channel, Result: slackapi.Channel{ID: "C012ABC", TeamID: "T012ABC", Member: true, Accessible: true}},
				fake.Step{Operation: fake.Scopes, Result: []slackapi.Scope{slackapi.ScopeChatWrite}},
			),
		},
		{
			name: "trusted bot", code: slackapi.DiagnosticUserNotFound,
			web: fake.NewWeb(
				fake.Step{Operation: fake.Auth, Result: slackapi.AuthIdentity{TeamID: "T012ABC", BotUserID: "U012BOT"}},
				fake.Step{Operation: fake.Team, Result: slackapi.Team{ID: "T012ABC"}},
				fake.Step{Operation: fake.Channel, Result: slackapi.Channel{ID: "C012ABC", TeamID: "T012ABC", Member: true, Accessible: true}},
				fake.Step{Operation: fake.Scopes, Result: slackapi.RequiredBotScopes(false)},
				fake.Step{Operation: fake.User, Result: slackapi.User{ID: "U012USR", TeamID: "T012ABC", Bot: true}},
			),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := slackapi.Verify(context.Background(), test.web, slackapi.VerificationRequest{
				TeamID: "T012ABC", BotUserID: "U012BOT", ChannelID: "C012ABC", TrustedUserIDs: []string{"U012USR"},
			})
			failure := slackapi.Classify(err, slackapi.FailureRead)
			if failure.Class != slackapi.FailureConclusive || failure.Diagnostic != test.code {
				t.Fatalf("failure = %#v (%v)", failure, err)
			}
		})
	}
}

func TestVerifyReportsSortedRequiredMissingScopesSafely(t *testing.T) {
	tests := []struct {
		name    string
		private bool
		want    []slackapi.Scope
	}{
		{
			name: "public", want: []slackapi.Scope{
				slackapi.ScopeAppMentionsRead, slackapi.ScopeChannelsHistory, slackapi.ScopeChannelsRead,
				slackapi.ScopeFilesRead, slackapi.ScopeFilesWrite, slackapi.ScopeReactionsRead, slackapi.ScopeReactionsWrite, slackapi.ScopeTeamRead, slackapi.ScopeUsersRead,
			},
		},
		{
			name: "private", private: true, want: []slackapi.Scope{
				slackapi.ScopeAppMentionsRead, slackapi.ScopeFilesRead, slackapi.ScopeFilesWrite, slackapi.ScopeGroupsHistory,
				slackapi.ScopeGroupsRead, slackapi.ScopeReactionsRead, slackapi.ScopeReactionsWrite, slackapi.ScopeTeamRead, slackapi.ScopeUsersRead,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			web := fake.NewWeb(
				fake.Step{Operation: fake.Auth, Result: slackapi.AuthIdentity{TeamID: "T012ABC", BotUserID: "U012BOT"}},
				fake.Step{Operation: fake.Team, Result: slackapi.Team{ID: "T012ABC"}},
				fake.Step{Operation: fake.Channel, Result: slackapi.Channel{ID: "C012ABC", TeamID: "T012ABC", Private: test.private, Member: true, Accessible: true}},
				fake.Step{Operation: fake.Scopes, Result: []slackapi.Scope{slackapi.ScopeChatWrite}},
			)
			_, err := slackapi.Verify(context.Background(), web, slackapi.VerificationRequest{TeamID: "T012ABC", BotUserID: "U012BOT", ChannelID: "C012ABC"})
			got, ok := slackapi.MissingScopesFromError(err)
			if !ok || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("MissingScopesFromError(%v) = %v, %t; want %v, true", err, got, ok, test.want)
			}
			failure := slackapi.Classify(err, slackapi.FailureRead)
			if failure.Class != slackapi.FailureConclusive || failure.Diagnostic != slackapi.DiagnosticMissingScope {
				t.Fatalf("Classify = %#v", failure)
			}
			if strings.Contains(err.Error(), "T012ABC") || strings.Contains(err.Error(), "C012ABC") {
				t.Fatalf("missing-scope error leaked identifiers: %q", err)
			}
		})
	}
}

func TestVerifyChannelRejectsUnsafeKindsAndState(t *testing.T) {
	base := slackapi.Channel{ID: "C012ABC", TeamID: "T012ABC", Accessible: true, Member: true}
	tests := []struct {
		name string
		set  func(*slackapi.Channel)
		code slackapi.DiagnosticCode
	}{
		{"inaccessible", func(c *slackapi.Channel) { c.Accessible = false }, slackapi.DiagnosticChannelNotFound},
		{"archived", func(c *slackapi.Channel) { c.Archived = true }, slackapi.DiagnosticChannelArchived},
		{"read-only", func(c *slackapi.Channel) { c.ReadOnly = true }, slackapi.DiagnosticChannelReadOnly},
		{"im", func(c *slackapi.Channel) { c.IM = true }, slackapi.DiagnosticUnsupportedChannel},
		{"mpim", func(c *slackapi.Channel) { c.MPIM = true }, slackapi.DiagnosticUnsupportedChannel},
		{"nonmember", func(c *slackapi.Channel) { c.Member = false }, slackapi.DiagnosticNotInChannel},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			channel := base
			test.set(&channel)
			failure := slackapi.Classify(slackapi.VerifyChannel(channel), slackapi.FailureRead)
			if failure.Diagnostic != test.code || failure.Class != slackapi.FailureConclusive {
				t.Fatalf("failure = %#v", failure)
			}
		})
	}
}
