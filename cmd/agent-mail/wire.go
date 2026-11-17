package main

import "github.com/sandcorporation/agent-mail/internal/wire"

// The HTTP API shapes live in internal/wire, shared with the client.
type (
	Message           = wire.Message
	aliasUse          = wire.AliasUse
	currentState      = wire.CurrentState
	decision          = wire.Decision
	decisionItem      = wire.DecisionItem
	decisionList      = wire.DecisionList
	firstReplyMetrics = wire.FirstReplyMetrics
	invite            = wire.Invite
	inviteCreated     = wire.InviteCreated
	inviteRequest     = wire.InviteRequest
	joinRequest       = wire.JoinRequest
	kindTime          = wire.KindTime
	latencyBucket     = wire.LatencyBucket
	latencyStats      = wire.LatencyStats
	mailEvent         = wire.MailEvent
	metricsReport     = wire.MetricsReport
	metricsWindow     = wire.MetricsWindow
	pendingAge        = wire.PendingAge
	permissionList    = wire.PermissionList
	permissionView    = wire.PermissionView
	presenceList      = wire.PresenceList
	presenceView      = wire.PresenceView
	progressDirective = wire.ProgressDirective
	progressMail      = wire.ProgressMail
	progressReport    = wire.ProgressReport
	progressSession   = wire.ProgressSession
	progressTask      = wire.ProgressTask
	questionAnswer    = wire.QuestionAnswer
	questionItem      = wire.QuestionItem
	questionList      = wire.QuestionList
	questionOption    = wire.QuestionOption
	questionView      = wire.QuestionView
	recipientLatency  = wire.RecipientLatency
	roleStateTime     = wire.RoleStateTime
	serviceProgress   = wire.ServiceProgress
	sessionMetrics    = wire.SessionMetrics
	slotGrant         = wire.SlotGrant
	slotRequest       = wire.SlotRequest
	slotView          = wire.SlotView
	threadMetrics     = wire.ThreadMetrics
	threadRounds      = wire.ThreadRounds
	thresholdSeconds  = wire.ThresholdSeconds
)
