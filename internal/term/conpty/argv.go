package conpty

import (
	"path/filepath"
	"strings"
)

// CommandLine is how a session's child is created on Windows: the program
// to run and the full command line handed to CreateProcess.
type CommandLine struct {
	Program string
	Line    string
}

// ResolveCommand turns a Spec's argv into a CommandLine. argv[0] is
// resolved with lookPath (exec.LookPath, which honours PATHEXT). A .cmd or
// .bat shim (npm installs claude as one) cannot be created directly, so it
// runs as `<comspec> /d /s /c "<shim> <args>"`: /d skips AutoRun, and /s
// makes cmd strip exactly the outer quotes, so the quoted arguments inside
// reach the shim as composed. quote composes one argument
// (windows.EscapeArg on Windows).
func ResolveCommand(argv []string, lookPath func(string) (string, error), comspec string, quote func(string) string) (CommandLine, error) {
	prog, err := lookPath(argv[0])
	if err != nil {
		return CommandLine{}, err
	}
	parts := make([]string, len(argv))
	parts[0] = quote(prog)
	for i, a := range argv[1:] {
		parts[i+1] = quote(a)
	}
	line := strings.Join(parts, " ")
	switch strings.ToLower(filepath.Ext(prog)) {
	case ".cmd", ".bat":
		if comspec == "" {
			comspec = `C:\Windows\System32\cmd.exe`
		}
		return CommandLine{Program: comspec, Line: quote(comspec) + ` /d /s /c "` + line + `"`}, nil
	}
	return CommandLine{Program: prog, Line: line}, nil
}
