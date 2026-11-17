package main

import "github.com/sandcorporation/agent-mail/internal/wire"

// The HTTP API shapes live in internal/wire, shared with the client.
type (
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
	metricsReport     = wire.MetricsReport
	metricsWindow     = wire.MetricsWindow
	pendingAge        = wire.PendingAge
	permissionList    = wire.PermissionList
	permissionView    = wire.PermissionView
	questionAnswer    = wire.QuestionAnswer
	questionItem      = wire.QuestionItem
	questionList      = wire.QuestionList
	questionOption    = wire.QuestionOption
	questionView      = wire.QuestionView
	recipientLatency  = wire.RecipientLatency
	roleStateTime     = wire.RoleStateTime
	sessionMetrics    = wire.SessionMetrics
	threadMetrics     = wire.ThreadMetrics
	threadRounds      = wire.ThreadRounds
	thresholdSeconds  = wire.ThresholdSeconds
)
