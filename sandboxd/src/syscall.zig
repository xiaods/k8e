const std = @import("std");

/// Decode a raw Linux syscall return value into an errno.
///
/// A failing syscall returns the NEGATED errno as a usize (ENOENT is
/// 0xFFFF_FFFF_FFFF_FFFE). Recovering it must therefore reinterpret the
/// whole word, never narrow it: `@intCast(rc)` to isize overflows on exactly
/// the error values we care about, and in ReleaseSafe that overflow is a
/// panic — which killed the sandboxd process (PID 1) and took the whole Pod
/// with it (see KIP-18 §8.1 "sandboxd-owned process table" era; the mkdir
/// path hit it on every EEXIST/ENOENT).
///
/// std.os.linux.errno bitcasts without narrowing, so it is safe for every
/// return value. Use this instead of hand-rolling the conversion.
pub fn errnoFromSyscall(rc: usize) std.posix.E {
    return std.os.linux.errno(rc);
}

/// The number of bytes a directory read returned, or null when the read
/// failed.
///
/// getdents64 (like every raw Linux syscall) returns the NEGATED errno as a
/// usize on failure, so a plain `n <= 0` test never fires — the error then
/// reached `@intCast(n)` and panicked in ReleaseSafe, killing the sandbox.
/// A read of a directory that vanishes mid-walk (or an unreadable one) hit
/// that path.
///
/// Returns null for both "end of directory" and "the read failed"; the
/// caller cannot tell them apart and does not need to: either way the walk
/// of that directory is over.
pub fn dirReadCount(rc: usize) ?usize {
    const signed: isize = @bitCast(rc);
    if (signed <= 0) return null;
    return @intCast(signed);
}
