package wire

import "time"

// PermissionView is the JSON of one request.
type PermissionView struct {
	ID            string     `json:"id"`
	Requester     string     `json:"requester"`
	Tool          string     `json:"tool"`
	Summary       string     `json:"summary"`
	Parent        string     `json:"parent,omitempty"`         // trusted parent fixed at request time
	IgnoredParent string     `json:"ignored_parent,omitempty"` // reported parent that was not trusted
	Status        string     `json:"status"`                   // pending | allowed | denied | expired
	Decision      string     `json:"decision,omitempty"`       // allow | deny
	Note          string     `json:"note,omitempty"`
	AnsweredBy    string     `json:"answered_by,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	AnsweredAt    *time.Time `json:"answered_at,omitempty"`
	CanAnswer     bool       `json:"can_answer"` // the caller may answer it now

	RequesterBase, ParentBase string `json:"-"` // server-side only
}

type PermissionList struct {
	Permissions []PermissionView `json:"permissions"`
}
