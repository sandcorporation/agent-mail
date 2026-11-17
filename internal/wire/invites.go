package wire

// Invite is one row as the admin API shows it (never the code or its hash).
type Invite struct {
	ID          int64   `json:"id"`
	Prefix      string  `json:"prefix"`
	Role        *string `json:"role"`
	ExpiresAt   string  `json:"expires_at"`
	UsedAt      *string `json:"used_at"`
	UsedBy      *string `json:"used_by"`
	CreatedBy   string  `json:"created_by"`
	CreatedAt   string  `json:"created_at"`
	CancelledAt *string `json:"cancelled_at"`
}

// InviteCreated answers a create: the code, shown this once.
type InviteCreated struct {
	Invite
	Code         string `json:"code"`
	GroupCreated bool   `json:"group_created"`
}

type InviteRequest struct {
	Prefix string `json:"prefix"`
	Role   string `json:"role"`
	TTL    string `json:"ttl"`
}

type JoinRequest struct {
	Code       string `json:"code"`
	Role       string `json:"role"`
	Credential string `json:"credential"`
}
