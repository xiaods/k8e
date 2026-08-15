const std = @import("std");
const main = @import("main.zig");

/// Directory watch (E2B Filesystem/WatchDir family — CreateWatcher,
/// GetWatcherEvents, RemoveWatcher). KIP-18 P1 last gap.
///
/// inotify is a single fd; events for all watches arrive there, so a
/// background thread drains it and appends each event to the owning
/// watcher's buffer (fixed-size ring per watcher, oldest dropped when full).
/// GetWatcherEvents returns the events since the last call (per-watcher
/// cursor), matching the SDK's WatchHandle.get_new_events semantics.
///
/// Watchers are created with a globally-incrementing id. The buffers are
/// plain memory: a daemon restart drops them, matching the process table's
/// honest non-persistence.

const WATCHER_MAX = 32;
const EVENT_BUF_MAX = 64;

const Watcher = struct {
    active: bool = false,
    id: u32 = 0,
    wd: i32 = -1, // inotify watch descriptor
    path: [512]u8 = .{0} ** 512,
    // Ring of event names (paths) + types.
    names: [EVENT_BUF_MAX][256]u8 = .{.{0} ** 256} ** EVENT_BUF_MAX,
    types: [EVENT_BUF_MAX]u8 = .{0} ** EVENT_BUF_MAX,
    head: usize = 0,
    len: usize = 0,
    cursor: usize = 0, // index of the next event to deliver
};

var watchers: [WATCHER_MAX]Watcher = undefined;
var watchers_init = false;
var next_id: u32 = 1;
var inotify_fd: i32 = -1;
var initialized = false;

var lock: std.atomic.Value(u32) = .init(0);

fn lockTable() void {
    while (lock.cmpxchgStrong(0, 1, .acquire, .monotonic) != null) {
        std.atomic.spinLoopHint();
    }
}

fn unlockTable() void {
    _ = lock.cmpxchgStrong(1, 0, .release, .monotonic);
}

pub fn eventTypeOf(mask: u32) u8 {
    // Map inotify masks to E2B EventType names (proto enum values).
    if ((mask & std.os.linux.IN.CREATE) != 0) return 1; // EVENT_TYPE_CREATE
    if ((mask & std.os.linux.IN.MODIFY) != 0) return 2; // EVENT_TYPE_WRITE
    if ((mask & std.os.linux.IN.DELETE) != 0) return 3; // EVENT_TYPE_REMOVE
    if ((mask & std.os.linux.IN.MOVED_FROM) != 0 or (mask & std.os.linux.IN.MOVED_TO) != 0) return 4; // EVENT_TYPE_RENAME
    if ((mask & std.os.linux.IN.ATTRIB) != 0) return 5; // EVENT_TYPE_CHMOD
    return 0;
}

fn findWatcherByWd(wd: i32) ?*Watcher {
    for (&watchers) |*w| {
        if (w.active and w.wd == wd) return w;
    }
    return null;
}

/// drainInotify reads events from the inotify fd and appends them to the
/// matching watcher's buffer. Runs on the background thread.
fn drainInotify() void {
    var buf: [4096]u8 align(@alignOf(std.os.linux.inotify_event)) = undefined;
    while (true) {
        const n = std.os.linux.read(inotify_fd, &buf, buf.len);
        if (n <= 0) break;
        var pos: usize = 0;
        while (pos + @sizeOf(std.os.linux.inotify_event) <= @as(usize, @intCast(n))) {
            const ev = @as(*align(1) const std.os.linux.inotify_event, @ptrCast(&buf[pos]));
            const rec_len = ev.len;
            if (rec_len == 0) break;
            const name = if (rec_len > @sizeOf(std.os.linux.inotify_event))
                @as([*]const u8, @ptrCast(&buf[pos + @sizeOf(std.os.linux.inotify_event)]))[0 .. rec_len - @sizeOf(std.os.linux.inotify_event) - 1]
            else
                "";
            lockTable();
            if (findWatcherByWd(ev.wd)) |w| {
                const t = eventTypeOf(ev.mask);
                if (t != 0) {
                    const slot = (w.head + w.len) % EVENT_BUF_MAX;
                    var path_buf: [256]u8 = undefined;
                    const full = std.fmt.bufPrint(&path_buf, "{s}/{s}", .{ std.mem.sliceTo(&w.path, 0), name }) catch "";
                    const clen = @min(full.len, 255);
                    @memcpy(w.names[slot][0..clen], full[0..clen]);
                    w.names[slot][clen] = 0;
                    w.types[slot] = t;
                    if (w.len < EVENT_BUF_MAX) {
                        w.len += 1;
                    } else {
                        w.head = (w.head + 1) % EVENT_BUF_MAX;
                        if (w.cursor > 0) w.cursor -= 1;
                    }
                }
            }
            unlockTable();
            pos += rec_len;
        }
    }
}

/// init creates the inotify fd and starts the background drain thread.
pub fn init() void {
    lockTable();
    defer unlockTable();
    if (initialized) return;
    if (!watchers_init) {
        for (&watchers) |*w| w.* = .{};
        watchers_init = true;
    }
    // inotify_init1 takes raw flags (IN_NONBLOCK | IN_CLOEXEC).
    const raw = std.os.linux.inotify_init1(std.os.linux.IN.NONBLOCK | std.os.linux.IN.CLOEXEC);
    const fd: isize = @bitCast(raw);
    if (fd < 0) return;
    inotify_fd = @intCast(fd);
    initialized = true;
    const thread = std.Thread.spawn(.{}, drainInotify, .{}) catch null;
    if (thread) |t| t.detach();
}

/// POST /watch/create {"path": "...", "recursive": false}
/// → 200 {"watcher_id": N}
pub fn handleCreate(allocator: std.mem.Allocator, client_fd: i32, body: []const u8) !void {
    const parsed = std.json.parseFromSlice(CreateRequest, allocator, body, .{ .ignore_unknown_fields = true, .allocate = .alloc_always }) catch {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"invalid json\"}");
        return;
    };
    defer parsed.deinit();
    const req = parsed.value;
    if (req.path.len == 0) {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"path required\"}");
        return;
    }

    init();

    // Watch the path (non-recursive; recursive would need per-dir watches
    // for existing subdirs — documented limitation).
    var path_buf: [512]u8 = undefined;
    const path_z = std.fmt.bufPrintZ(&path_buf, "{s}", .{req.path}) catch {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"path too long\"}");
        return;
    };
    const wd_raw = std.os.linux.inotify_add_watch(inotify_fd, path_z,
        std.os.linux.IN.CREATE | std.os.linux.IN.MODIFY | std.os.linux.IN.DELETE |
            std.os.linux.IN.MOVED_FROM | std.os.linux.IN.MOVED_TO | std.os.linux.IN.ATTRIB);
    const wd: isize = @bitCast(wd_raw);
    if (wd < 0) {
        try main.writeResponse(client_fd, "404 Not Found", "application/json", "{\"error\":\"path not found\"}");
        return;
    }

    lockTable();
    var assigned: u32 = 0;
    for (&watchers) |*w| {
        if (!w.active) {
            w.* = .{};
            w.active = true;
            w.id = next_id;
            w.wd = @intCast(wd);
            const clen = @min(req.path.len, 511);
            @memcpy(w.path[0..clen], req.path[0..clen]);
            w.path[clen] = 0;
            assigned = w.id;
            next_id +%= 1;
            break;
        }
    }
    unlockTable();

    if (assigned == 0) {
        _ = std.os.linux.inotify_rm_watch(inotify_fd, @intCast(wd));
        try main.writeResponse(client_fd, "503 Service Unavailable", "application/json", "{\"error\":\"no watcher slots\"}");
        return;
    }
    const resp = try std.fmt.allocPrint(allocator, "{{\"watcher_id\":{d}}}", .{assigned});
    defer allocator.free(resp);
    try main.writeResponse(client_fd, "200 OK", "application/json", resp);
}

/// GET /watch/events?watcher_id=N → {"events":[{"name":"...","type":N}]}
/// Returns events since the last call (per-watcher cursor).
pub fn handleEvents(allocator: std.mem.Allocator, client_fd: i32, query: []const u8) !void {
    const raw = parseWatcherIdQuery(query) orelse {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"watcher_id required\"}");
        return;
    };
    const id = std.fmt.parseInt(u32, raw, 10) catch {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"invalid watcher_id\"}");
        return;
    };

    lockTable();
    var out = std.array_list.Managed(u8).init(allocator);
    defer out.deinit();
    try out.appendSlice("{\"events\":[");
    var wrote_any = false;
    var found = false;
    for (&watchers) |*w| {
        if (w.active and w.id == id) {
            found = true;
            // Deliver from cursor to current len (new events only).
            var idx = w.cursor;
            while (idx < w.len) : (idx += 1) {
                const slot = (w.head + idx) % EVENT_BUF_MAX;
                const name = std.mem.sliceTo(&w.names[slot], 0);
                if (wrote_any) try out.append(',');
                wrote_any = true;
                var entry_buf: [320]u8 = undefined;
                const entry = try std.fmt.bufPrint(&entry_buf, "{{\"name\":\"{s}\",\"type\":{d}}}",
                    .{ name, w.types[slot] });
                try out.appendSlice(entry);
            }
            w.cursor = w.len;
            break;
        }
    }
    unlockTable();
    try out.appendSlice("]}");
    if (!found) {
        try main.writeResponse(client_fd, "404 Not Found", "application/json", "{\"error\":\"watcher not found\"}");
        return;
    }
    try main.writeResponse(client_fd, "200 OK", "application/json", out.items);
}

/// POST /watch/remove {"watcher_id": N} → 200 {"ok":true}
pub fn handleRemove(allocator: std.mem.Allocator, client_fd: i32, body: []const u8) !void {
    const parsed = std.json.parseFromSlice(RemoveRequest, allocator, body, .{ .ignore_unknown_fields = true }) catch {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"invalid json\"}");
        return;
    };
    defer parsed.deinit();
    const req = parsed.value;

    lockTable();
    var removed = false;
    for (&watchers) |*w| {
        if (w.active and w.id == req.watcher_id) {
            _ = std.os.linux.inotify_rm_watch(inotify_fd, w.wd);
            w.active = false;
            removed = true;
            break;
        }
    }
    unlockTable();
    if (!removed) {
        try main.writeResponse(client_fd, "404 Not Found", "application/json", "{\"error\":\"watcher not found\"}");
        return;
    }
    try main.writeResponse(client_fd, "200 OK", "application/json", "{\"ok\":true}");
}

/// parseWatcherIdQuery extracts the "watcher_id=" query value.
pub fn parseWatcherIdQuery(query: []const u8) ?[]const u8 {
    const prefix = "watcher_id=";
    if (std.mem.startsWith(u8, query, prefix)) {
        return query[prefix.len..];
    }
    return null;
}

const CreateRequest = struct {
    path: []const u8 = "",
    recursive: bool = false,
};

const RemoveRequest = struct {
    watcher_id: u32 = 0,
};
