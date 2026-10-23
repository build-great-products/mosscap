package slackapi

import "context"

// Verify proves that the active bot token belongs to the configured workspace
// and bot, that every allowlisted channel is a writable member channel, and
// that all required bot scopes are granted. Returned errors contain no
// supplied IDs.
func Verify(ctx context.Context, web Web, request VerificationRequest) (VerificationResult, error) {
	if web == nil || len(request.ChannelIDs) > MaxFiles {
		return VerificationResult{}, NewVerificationError(DiagnosticInvalidRequest)
	}
	seenChannels := make(map[string]struct{}, len(request.ChannelIDs))
	for _, channelID := range request.ChannelIDs {
		if ValidateChannelID(channelID) != nil {
			return VerificationResult{}, NewVerificationError(DiagnosticInvalidRequest)
		}
		if _, exists := seenChannels[channelID]; exists {
			return VerificationResult{}, NewVerificationError(DiagnosticInvalidRequest)
		}
		seenChannels[channelID] = struct{}{}
	}
	if (request.TeamID != "" && ValidateTeamID(request.TeamID) != nil) || (request.BotUserID != "" && ValidateUserID(request.BotUserID) != nil) {
		return VerificationResult{}, NewVerificationError(DiagnosticInvalidRequest)
	}

	identity, err := web.Auth(ctx)
	if err != nil {
		return VerificationResult{}, err
	}
	teamID := identity.TeamID
	botUserID := identity.BotUserID
	if request.TeamID != "" && identity.TeamID != request.TeamID {
		return VerificationResult{}, NewVerificationError(DiagnosticTeamMismatch)
	}
	if request.BotUserID != "" && identity.BotUserID != request.BotUserID {
		return VerificationResult{}, NewVerificationError(DiagnosticBotMismatch)
	}
	if ValidateTeamID(teamID) != nil || ValidateUserID(botUserID) != nil {
		return VerificationResult{}, NewExternalError(ReasonInvalidResponse, DiagnosticMalformedResponse, "", nil)
	}

	team, err := web.Team(ctx, teamID)
	if err != nil {
		return VerificationResult{}, err
	}
	if team.ID != teamID {
		return VerificationResult{}, NewVerificationError(DiagnosticTeamMismatch)
	}

	channels := make([]Channel, 0, len(request.ChannelIDs))
	for _, channelID := range request.ChannelIDs {
		channel, err := web.Channel(ctx, channelID)
		if err != nil {
			return VerificationResult{}, err
		}
		if channel.ID != channelID || channel.TeamID != teamID {
			return VerificationResult{}, NewVerificationError(DiagnosticTeamMismatch)
		}
		if err := VerifyChannel(channel); err != nil {
			return VerificationResult{}, err
		}
		channels = append(channels, channel)
	}

	scopes, err := web.Scopes(ctx)
	if err != nil {
		return VerificationResult{}, err
	}
	missing, err := MissingScopes(scopes, RequiredBotScopes())
	if err != nil {
		return VerificationResult{}, NewExternalError(ReasonInvalidResponse, DiagnosticMalformedResponse, "", nil)
	}
	if len(missing) != 0 {
		return VerificationResult{}, NewMissingScopesError(missing)
	}

	result := VerificationResult{
		Identity: cloneAuthIdentity(identity),
		Team:     team,
		Channels: channels,
		Scopes:   append([]Scope(nil), scopes...),
	}
	return result, nil
}

// VerifyChannel rejects every target on which Mosscap cannot safely perform
// its required history and mutation operations.
func VerifyChannel(channel Channel) error {
	if !channel.Accessible {
		return NewVerificationError(DiagnosticChannelNotFound)
	}
	if channel.IM || channel.MPIM {
		return NewVerificationError(DiagnosticUnsupportedChannel)
	}
	if channel.Archived {
		return NewVerificationError(DiagnosticChannelArchived)
	}
	if channel.ReadOnly {
		return NewVerificationError(DiagnosticChannelReadOnly)
	}
	if !channel.Member {
		return NewVerificationError(DiagnosticNotInChannel)
	}
	return nil
}

func cloneAuthIdentity(identity AuthIdentity) AuthIdentity {
	identity.Scopes = append([]Scope(nil), identity.Scopes...)
	return identity
}
