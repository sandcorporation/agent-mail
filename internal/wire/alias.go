package wire

// AliasUse is the "aliases" block of the admin metrics.
type AliasUse struct {
	OldHeaders map[string]int64 `json:"old_headers"`
	OldEnv     []string         `json:"old_env"`
}
