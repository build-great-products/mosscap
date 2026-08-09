package slackapi

import "context"

// Verify proves that the active bot token belongs to the configured workspace
// and bot, that the configured target is a writable member channel, that all
// required bot scopes are granted, and that configured trusted identities are
// active humans in that workspace. Returned errors contain no supplied IDs.
func Verify(ctx context.Context, web Web, request VerificationRequest) (VerificationResult, error) {
	if web == nil || ValidateChannelID(request.ChannelID) != nil {
		return VerificationResult{}, NewVerificationError(DiagnosticInvalidRequest)
	}
	if (request.TeamID != "" && ValidateTeamID(request.TeamID) != nil) || (request.BotUserID != "" && ValidateUserID(request.BotUserID) != nil) {
		return VerificationResult{}, NewVerificationError(DiagnosticInvalidRequest)
	}
	if len(request.TrustedUserIDs) > MaxFiles {
		return VerificationResult{}, NewVerificationError(DiagnosticInvalidRequest)
	}
	seenUsers := make(map[string]struct{}, len(request.TrustedUserIDs))
	for _, userID := range request.TrustedUserIDs {
		if ValidateUserID(userID) != nil {
			return VerificationResult{}, NewVerificationError(DiagnosticInvalidRequest)
		}
		if _, exists := seenUsers[userID]; exists {
			return VerificationResult{}, NewVerificationError(DiagnosticInvalidRequest)
		}
		seenUsers[userID] = struct{}{}
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

	channel, err := web.Channel(ctx, request.ChannelID)
	if err != nil {
		return VerificationResult{}, err
	}
	if channel.ID != request.ChannelID || channel.TeamID != teamID {
		return VerificationResult{}, NewVerificationError(DiagnosticTeamMismatch)
	}
	if err := VerifyChannel(channel); err != nil {
		return VerificationResult{}, err
	}

	scopes, err := web.Scopes(ctx)
	if err != nil {
		return VerificationResult{}, err
	}
	missing, err := MissingScopes(scopes, RequiredBotScopes(channel.Private))
	if err != nil {
		return VerificationResult{}, NewExternalError(ReasonInvalidResponse, DiagnosticMalformedResponse, "", nil)
	}
	if len(missing) != 0 {
		return VerificationResult{}, NewMissingScopesError(missing)
	}

	result := VerificationResult{
		Identity: cloneAuthIdentity(identity),
		Team:     team,
		Channel:  channel,
		Scopes:   append([]Scope(nil), scopes...),
		Users:    make([]User, 0, len(request.TrustedUserIDs)),
	}
	for _, userID := range request.TrustedUserIDs {
		user, err := web.User(ctx, userID)
		if err != nil {
			return VerificationResult{}, err
		}
		if user.ID != userID || user.TeamID != teamID || user.Bot || user.Deactivated {
			return VerificationResult{}, NewVerificationError(DiagnosticUserNotFound)
		}
		result.Users = append(result.Users, user)
	}
	return result, nil
}

// VerifyChannel rejects every target on which Phase 1 cannot safely perform
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
