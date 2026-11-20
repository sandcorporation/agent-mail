// Package winjob holds the Windows process plumbing the job runner needs:
// job objects that end a command's whole tree, starting a process inside
// one before it can start anything, and whether this process may leave its
// own job. It is empty on other systems.
package winjob
