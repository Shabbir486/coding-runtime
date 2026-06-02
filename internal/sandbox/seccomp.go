package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// seccompProfile is a minimal, restrictive OCI seccomp profile that allows
// only the syscalls required for typical code-execution workloads.
// All syscalls not listed are denied (action: SCMP_ACT_ERRNO).
var seccompProfile = seccompProfileSpec{
	DefaultAction: "SCMP_ACT_ERRNO",
	Architectures: []string{
		"SCMP_ARCH_X86_64",
		"SCMP_ARCH_X86",
		"SCMP_ARCH_AARCH64",
		"SCMP_ARCH_ARM",
	},
	Syscalls: []seccompSyscall{
		// ── Process lifecycle ──────────────────────────────────────────────
		{Names: []string{"execve", "execveat"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"exit", "exit_group"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"wait4", "waitid"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"getpid", "getppid", "gettid"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"getuid", "getgid", "geteuid", "getegid"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"getgroups"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"arch_prctl"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"prctl"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"set_tid_address"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"set_robust_list"}, Action: "SCMP_ACT_ALLOW"},
		// fork/vfork/clone only for thread creation, not new user namespaces.
		// clone with CLONE_NEWUSER is blocked by keeping the default deny for
		// clone and only allowing it without CLONE_NEWUSER (0x10000000).
		{Names: []string{"clone"}, Action: "SCMP_ACT_ALLOW", Args: []seccompArg{
			{Index: 0, Value: 0x10000000, ValueTwo: 0, Op: "SCMP_CMP_MASKED_EQ"},
		}},
		{Names: []string{"clone3"}, Action: "SCMP_ACT_ERRNO"},
		{Names: []string{"fork", "vfork"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"kill", "tkill", "tgkill"}, Action: "SCMP_ACT_ALLOW"},

		// ── Memory management ─────────────────────────────────────────────
		{Names: []string{"brk"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"mmap", "mmap2"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"munmap"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"mprotect"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"mremap"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"madvise"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"mincore"}, Action: "SCMP_ACT_ALLOW"},

		// ── File I/O ──────────────────────────────────────────────────────
		{Names: []string{"read", "readv", "pread64"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"write", "writev", "pwrite64"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"open", "openat", "openat2", "creat"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"close", "close_range"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"lseek"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"dup", "dup2", "dup3"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"pipe", "pipe2"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"poll", "ppoll", "select", "pselect6"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"epoll_create", "epoll_create1", "epoll_ctl", "epoll_wait", "epoll_pwait"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"eventfd", "eventfd2"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"inotify_init", "inotify_init1", "inotify_add_watch", "inotify_rm_watch"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"fcntl", "fcntl64"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"flock"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"fsync", "fdatasync"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"truncate", "ftruncate"}, Action: "SCMP_ACT_ALLOW"},

		// ── Directory / filesystem metadata ───────────────────────────────
		{Names: []string{"stat", "fstat", "lstat", "newfstatat", "statx"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"getdents", "getdents64"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"getcwd"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"chdir", "fchdir"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"mkdir", "mkdirat"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"rmdir"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"unlink", "unlinkat"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"rename", "renameat", "renameat2"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"link", "linkat", "symlink", "symlinkat"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"readlink", "readlinkat"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"chmod", "fchmod", "fchmodat"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"chown", "fchown", "lchown", "fchownat"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"umask"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"access", "faccessat", "faccessat2"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"utime", "utimes", "futimesat", "utimensat"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"statfs", "fstatfs"}, Action: "SCMP_ACT_ALLOW"},

		// ── Signals ───────────────────────────────────────────────────────
		{Names: []string{"rt_sigaction", "rt_sigprocmask", "rt_sigreturn", "sigaltstack", "rt_sigsuspend", "rt_sigtimedwait", "rt_sigqueueinfo", "rt_tgsigqueueinfo", "sigprocmask", "sigreturn", "sgetmask", "ssetmask"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"pause"}, Action: "SCMP_ACT_ALLOW"},

		// ── Scheduling / timing ───────────────────────────────────────────
		{Names: []string{"sched_yield"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"sched_getaffinity", "sched_setaffinity"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"sched_getparam", "sched_setparam"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"sched_getscheduler", "sched_setscheduler"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"sched_get_priority_min", "sched_get_priority_max"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"getitimer", "setitimer"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"alarm"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"nanosleep", "clock_nanosleep"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"clock_gettime", "clock_getres", "gettimeofday", "time"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"timer_create", "timer_settime", "timer_gettime", "timer_getoverrun", "timer_delete"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"timerfd_create", "timerfd_settime", "timerfd_gettime"}, Action: "SCMP_ACT_ALLOW"},

		// ── IPC / synchronisation ─────────────────────────────────────────
		{Names: []string{"futex", "futex_time64"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"get_robust_list"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"socketpair"}, Action: "SCMP_ACT_ALLOW"},
		// Allow AF_UNIX sockets only (for language runtimes like Java that
		// create loopback sockets).  AF_INET blocked via NetworkMode none.
		{Names: []string{"socket"}, Action: "SCMP_ACT_ALLOW", Args: []seccompArg{
			{Index: 0, Value: 1, ValueTwo: 0, Op: "SCMP_CMP_EQ"}, // AF_UNIX = 1
		}},
		{Names: []string{"bind", "listen", "accept", "accept4", "connect", "getsockname", "getpeername", "setsockopt", "getsockopt", "sendto", "recvfrom", "sendmsg", "recvmsg", "shutdown"}, Action: "SCMP_ACT_ALLOW"},

		// ── Resource limits ───────────────────────────────────────────────
		{Names: []string{"getrlimit", "setrlimit", "prlimit64"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"getrusage"}, Action: "SCMP_ACT_ALLOW"},

		// ── System information ────────────────────────────────────────────
		{Names: []string{"uname"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"sysinfo"}, Action: "SCMP_ACT_ALLOW"},

		// ── Threads (pthread) ─────────────────────────────────────────────
		{Names: []string{"gettid"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"tgkill"}, Action: "SCMP_ACT_ALLOW"},

		// ── Miscellaneous safe syscalls ───────────────────────────────────
		{Names: []string{"getrandom"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"sendfile", "sendfile64"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"splice"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"copy_file_range"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"ioctl"}, Action: "SCMP_ACT_ALLOW"},
		{Names: []string{"tee"}, Action: "SCMP_ACT_ALLOW"},

		// ── Explicitly blocked dangerous syscalls (belt-and-suspenders) ───
		// These would be caught by the default SCMP_ACT_ERRNO but are listed
		// explicitly to make the intent clear and survive profile linting.
		{Names: []string{"mount", "umount2", "pivot_root"}, Action: "SCMP_ACT_ERRNO"},
		{Names: []string{"chroot"}, Action: "SCMP_ACT_ERRNO"},
		{Names: []string{"unshare"}, Action: "SCMP_ACT_ERRNO"},
		{Names: []string{"keyctl", "add_key", "request_key"}, Action: "SCMP_ACT_ERRNO"},
		{Names: []string{"ptrace"}, Action: "SCMP_ACT_ERRNO"},
		{Names: []string{"perf_event_open"}, Action: "SCMP_ACT_ERRNO"},
		{Names: []string{"acct"}, Action: "SCMP_ACT_ERRNO"},
		{Names: []string{"setns"}, Action: "SCMP_ACT_ERRNO"},
		{Names: []string{"kcmp"}, Action: "SCMP_ACT_ERRNO"},
		{Names: []string{"bpf"}, Action: "SCMP_ACT_ERRNO"},
		{Names: []string{"userfaultfd"}, Action: "SCMP_ACT_ERRNO"},
		{Names: []string{"fanotify_init", "fanotify_mark"}, Action: "SCMP_ACT_ERRNO"},
		{Names: []string{"io_uring_setup", "io_uring_enter", "io_uring_register"}, Action: "SCMP_ACT_ERRNO"},
		{Names: []string{"reboot", "kexec_load", "kexec_file_load"}, Action: "SCMP_ACT_ERRNO"},
		{Names: []string{"syslog"}, Action: "SCMP_ACT_ERRNO"},
		{Names: []string{"nfsservctl"}, Action: "SCMP_ACT_ERRNO"},
	},
}

// seccompProfileSpec is the top-level OCI seccomp profile structure.
type seccompProfileSpec struct {
	DefaultAction string           `json:"defaultAction"`
	Architectures []string         `json:"architectures"`
	Syscalls      []seccompSyscall `json:"syscalls"`
}

// seccompSyscall is a single syscall rule inside the OCI seccomp profile.
type seccompSyscall struct {
	Names  []string     `json:"names"`
	Action string       `json:"action"`
	Args   []seccompArg `json:"args,omitempty"`
}

// seccompArg is a filter argument for a seccomp rule.
type seccompArg struct {
	Index    uint   `json:"index"`
	Value    uint64 `json:"value"`
	ValueTwo uint64 `json:"valueTwo"`
	Op       string `json:"op"`
}

// GetSeccompProfile serialises the built-in seccomp profile to a JSON string.
// The returned string is suitable for passing directly to Docker's SecurityOpt.
func GetSeccompProfile() string {
	b, err := json.Marshal(seccompProfile)
	if err != nil {
		// This should never happen with a statically-defined struct.
		panic(fmt.Sprintf("sandbox: failed to marshal seccomp profile: %v", err))
	}
	return string(b)
}

// WriteSeccompProfile serialises the built-in profile and writes it to path.
// Intermediate directories are created as needed.
func WriteSeccompProfile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("sandbox: create seccomp dir %q: %w", filepath.Dir(path), err)
	}
	b, err := json.MarshalIndent(seccompProfile, "", "  ")
	if err != nil {
		return fmt.Errorf("sandbox: marshal seccomp profile: %w", err)
	}
	if err := os.WriteFile(path, b, 0o640); err != nil {
		return fmt.Errorf("sandbox: write seccomp profile %q: %w", path, err)
	}
	return nil
}
