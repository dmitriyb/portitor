package check

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

// TrampolineMain is the body of the `portitor internal-check-exec` re-exec
// trampoline: it enters the private workdir, applies the address-space rlimit
// to itself, and execs the operator-configured command with a minimal
// environment. It exists because Go cannot set a child's rlimits directly (no
// prlimit in the stdlib); it carries no knowledge of what fills the seam.
// Every diagnostic it prints starts with TrampolineSentinel so Records can
// tell "the contract could not be run" from "the command ran and rejected the
// content". Exposed as a function so test binaries can intercept the re-exec
// in their TestMain.
//
// Ordering is load-bearing: a Go process holds large virtual reservations, so
// ANY allocation after RLIMIT_AS is applied can abort the runtime. Everything
// that allocates — path lookup, argv/env pointer arrays, even the failure
// message — is prepared first; after Setrlimit the only calls are raw
// syscalls.
func TrampolineMain(args []string) int {
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(os.Stderr, TrampolineSentinel+" "+format+"\n", a...)
		return code
	}
	if len(args) < 2 {
		return fail(2, "usage: <workdir> <argv...>")
	}
	dir, argv := args[0], args[1:]
	if err := os.Chdir(dir); err != nil {
		return fail(1, "%v", err)
	}
	path0, err := exec.LookPath(argv[0])
	if err != nil {
		return fail(127, "%v", err)
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}

	// Pre-allocate everything execve needs, plus the failure report.
	p0, err := syscall.BytePtrFromString(path0)
	if err != nil {
		return fail(1, "argv: %v", err)
	}
	argvp, err := syscall.SlicePtrFromStrings(argv)
	if err != nil {
		return fail(1, "argv: %v", err)
	}
	envp, err := syscall.SlicePtrFromStrings(env)
	if err != nil {
		return fail(1, "env: %v", err)
	}
	execFailed := []byte(TrampolineSentinel + " execve " + path0 + " failed\n")

	rl := syscall.Rlimit{Cur: MemLimit, Max: MemLimit}
	if err := syscall.Setrlimit(syscall.RLIMIT_AS, &rl); err != nil {
		return fail(1, "setrlimit: %v", err)
	}
	// From here on: raw syscalls only (no allocation under the fresh limit).
	syscall.RawSyscall(syscall.SYS_EXECVE,
		uintptr(unsafe.Pointer(p0)),
		uintptr(unsafe.Pointer(&argvp[0])),
		uintptr(unsafe.Pointer(&envp[0])))
	// execve only returns on failure.
	syscall.Write(2, execFailed)
	return 1
}
