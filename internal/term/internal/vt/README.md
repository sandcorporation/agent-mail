# vt

A copy of `github.com/charmbracelet/x/vt` at tag `vt/v0.1.0` (MIT, `LICENSE`),
non-test files only, owned here so the terminal emulator can be patched.
`internal/term.Emulator` is the only user; nothing else imports this package.

Changes from the tag (each marked `switchboard patch` in the source):

- `utf8.go`: a wide character that does not fit before the right edge wraps
  to the next line instead of being dropped, and a wide character ending on
  the last column leaves a pending wrap instead of letting the next character
  overwrite its second half (Korean text wrapping at the edge was lost).
