package slackapi

import "context"

// Verify proves that the active bot token belongs to the configured workspace
// and bot, that the configured target is a writable member channel, that all
// required bot scopes are granted, and that configured trusted identities are
// active humans in that workspace. Returned errors contain no supplied IDs.
func Verify(ctx context.Context, web Web, request VerificationRequest) (VerificationResult, error) {
	if web == nil || ValidateTeamID(request.TeamID) != nil || ValidateUserID(request.BotUserID) != nil || ValidateChannelID(request.ChannelID) != nil {
		return VerificationResult{}, NewExternalError(ReasonRejected, DiagnosticInvalidRequest, "", nil)
	}
	if len(request.TrustedUserIDs) > MaxFiles {
		return VerificationResult{}, NewExternalError(ReasonRejected, DiagnosticInvalidRequest, "", nil)
	}
	seenUsers := make(map[string]struct{}, len(request.TrustedUserIDs))
	for _, userID := range request.TrustedUserIDs {
		if ValidateUserID(userID) != nil {
			return VerificationResult{}, NewExternalError(ReasonRejected, DiagnosticInvalidRequest, "", nil)
		}
		if _, exists := seenUsers[userID]; exists {
			return VerificationResult{}, NewExternalError(ReasonRejected, DiagnosticInvalidRequest, "", nil)
		}
		seenUsers[userID] = struct{}{}
	}

	identity, err := web.Auth(ctx)
	if err != nil {
		return VerificationResult{}, err
	}
	if identity.TeamID != request.TeamID {
		return VerificationResult{}, NewExternalError(ReasonRejected, DiagnosticTeamMismatch, "", nil)
	}
	if identity.BotUserID != request.BotUserID {
		return VerificationResult{}, NewExternalError(ReasonRejected, DiagnosticBotMismatch, "", nil)
	}

	team, err := web.Team(ctx, request.TeamID)
	if err != nil {
		return VerificationResult{}, err
	}
	if team.ID != request.TeamID {
		return VerificationResult{}, NewExternalError(ReasonRejected, DiagnosticTeamMismatch, "", nil)
	}

	channel, err := web.Channel(ctx, request.ChannelID)
	if err != nil {
		return VerificationResult{}, err
	}
	if channel.ID != request.ChannelID || channel.TeamID != request.TeamID {
		return VerificationResult{}, NewExternalError(ReasonRejected, DiagnosticTeamMismatch, "", nil)
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
		return VerificationResult{}, NewExternalError(ReasonRejected, DiagnosticMissingScope, "", nil)
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
		if user.ID != userID || user.TeamID != request.TeamID || user.Bot || user.Deactivated {
			return VerificationResult{}, NewExternalError(ReasonRejected, DiagnosticUserNotFound, "", nil)
		}
		result.Users = append(result.Users, user)
	}
	return result, nil
}

// VerifyChannel rejects every target on which Phase 1 cannot safely perform
// its required history and mutation operations.
func VerifyChannel(channel Channel) error {
	if !channel.Accessible {
		return NewExternalError(ReasonRejected, DiagnosticChannelNotFound, "", nil)
	}
	if channel.IM || channel.MPIM {
		return NewExternalError(ReasonRejected, DiagnosticUnsupportedChannel, "", nil)
	}
	if channel.Archived {
		return NewExternalError(ReasonRejected, DiagnosticChannelArchived, "", nil)
	}
	if channel.ReadOnly {
		return NewExternalError(ReasonRejected, DiagnosticChannelReadOnly, "", nil)
	}
	if !channel.Member {
		return NewExternalError(ReasonRejected, DiagnosticNotInChannel, "", nil)
	}
	return nil
}

func cloneAuthIdentity(identity AuthIdentity) AuthIdentity {
	identity.Scopes = append([]Scope(nil), identity.Scopes...)
	return identity
}
