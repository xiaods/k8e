// rqlite HTTP client
// Communicates with rqlite's HTTP API for SQL execution and queries.
// Reference: https://rqlite.io/docs/api/
//
// Depends on the transport layer in server/backend.zig for HTTP/TCP.

const std = @import("std");
test "rqlite transaction JSON preserves semicolons inside literals" {
    const request = try buildExecuteRequest(testing.allocator, "BEGIN TRANSACTION;INSERT INTO kv(key) VALUES('a;''b');COMMIT;");
    defer testing.allocator.free(request);
    const parsed = try std.json.parseFromSlice([][]const u8, testing.allocator, request, .{});
    defer parsed.deinit();
    try testing.expectEqual(@as(usize, 1), parsed.value.len);
    try testing.expectEqualStrings("INSERT INTO kv(key) VALUES('a;''b')", parsed.value[0]);
}

test "rqlite surfaces SQL errors rather than reporting success" {
    try testing.expectError(error.RqliteError, parseExecuteResponse(testing.allocator, "{\"results\":[{}, {\"error\":\"constraint failed\"}]}"));
    try testing.expectError(error.RqliteError, parseQueryResponse(testing.allocator, "{\"results\":[{\"error\":\"no such table\"}]}"));
}

test "rqlite query owns escaped text and decodes binary columns" {
    const result = try parseQueryResponse(testing.allocator, "{\"results\":[{\"columns\":[\"key\",\"value\",\"revision\"],\"types\":[\"text\",\"blob\",\"integer\"],\"values\":[[\"a,\\\"b\",\"AP8=\",17]]}]}");
    defer result.deinit(testing.allocator);
    try testing.expectEqualStrings("a,\"b", result.values[0][0].text);
    try testing.expectEqualSlices(u8, &.{ 0, 255 }, result.values[0][1].blob);
    try testing.expectEqual(@as(i64, 17), result.values[0][2].integer);
}
const Allocator = std.mem.Allocator;
const HttpClient = @import("backend").HttpClient;
const JsonBuilder = @import("backend").JsonBuilder;
const JsonParser = @import("backend").JsonParser;
const HttpRequest = @import("backend").HttpRequest;
const HttpResponse = @import("backend").HttpResponse;

/// rqlite node connection
pub const RqliteClient = struct {
    allocator: Allocator,
    base_url: []const u8,
    http_client: HttpClient,

    pub fn init(allocator: Allocator, base_url: []const u8) !RqliteClient {
        const parsed = try HttpClient.parseUrl(allocator, base_url);
        errdefer {
            allocator.free(parsed.host);
            allocator.free(parsed.path);
        }
        const client = HttpClient.init(allocator, parsed.host, parsed.port);
        const base = try allocator.dupe(u8, base_url);
        // The transport keeps the parsed host; the path is not needed because
        // requests construct their endpoint URLs from base_url.
        allocator.free(parsed.path);
        return .{
            .allocator = allocator,
            .base_url = base,
            .http_client = client,
        };
    }

    pub fn deinit(self: *RqliteClient) void {
        self.allocator.free(self.base_url);
        self.allocator.free(self.http_client.host);
        self.http_client.deinit();
    }

    /// Wait until rqlite has a leader and can serve requests.
    ///
    /// A freshly started node answers HTTP 200 with an error in the body while
    /// it is still bootstrapping or holding an election. Writes cannot be
    /// retried blindly, so callers that must issue one on a cold node wait here
    /// first instead. Returns once a trivial linearizable read succeeds.
    pub fn waitForLeader(self: *RqliteClient) !void {
        var attempt: usize = 0;
        while (attempt < 150) : (attempt += 1) {
            const body = try buildQueryRequest(self.allocator, "SELECT 1");
            defer self.allocator.free(body);
            const url = try std.fmt.allocPrint(self.allocator, "{s}/db/query?level=linearizable", .{self.base_url});
            defer self.allocator.free(url);
            if (self.http_client.post(url, body)) |resp| {
                var owned = resp;
                defer owned.deinit();
                if (!isLeadershipPending(owned.body)) return;
            } else |_| {}
            _ = std.posix.poll(&.{}, 100) catch {};
        }
        return error.NoLeader;
    }

    // Only reads can be retried: a lost write response does not imply rollback.
    // Only reads can be retried: a lost write response does not imply rollback.
    fn queryWithRetry(self: *RqliteClient, url: []const u8, body: []const u8) !HttpResponse {
        var attempt: usize = 0;
        while (attempt < 29) : (attempt += 1) {
            // A linearizable read is answered with HTTP 200 and an error in the
            // body while the node has not elected a leader yet. That is a
            // transient startup condition rather than a failed query, so it is
            // retried the same way a refused connection is.
            var resp = self.http_client.post(url, body) catch {
                _ = std.posix.poll(&.{}, 100) catch {};
                continue;
            };
            if (isLeadershipPending(resp.body)) {
                resp.deinit();
                _ = std.posix.poll(&.{}, 100) catch {};
                continue;
            }
            return resp;
        }
        // The final attempt surfaces rqlite's real answer, error included.
        return self.http_client.post(url, body);
    }

    /// Execute a single SQL statement (write operation, goes through Raft)
    pub fn execute(self: *RqliteClient, sql: []const u8) !ExecuteResult {
        const body = try buildExecuteRequest(self.allocator, sql);
        defer self.allocator.free(body);

        const url = try std.fmt.allocPrint(self.allocator, "{s}/db/execute?transaction", .{self.base_url});
        defer self.allocator.free(url);
        var response = try self.http_client.post(url, body);
        defer response.deinit();
        reportError(response.body);

        return try parseExecuteResponse(self.allocator, response.body);
    }

    /// Return the result within the same Raft transaction as its writes.
    /// A later /db/query cannot see connection-local TEMP tables and may race
    /// another transaction even if the scratch table were persistent.
    pub fn transaction(self: *RqliteClient, sql: []const u8) !QueryResult {
        const sets = try self.transactionSets(sql);
        defer self.allocator.free(sets);
        if (sets.len == 0) return error.InvalidResponse;
        for (sets[1..]) |*set| set.deinit(self.allocator);
        return sets[0];
    }

    /// Like `transaction`, but returns every result set the script produced.
    /// A Txn that answers a Range needs both the per-operation result rows and
    /// the captured range rows, and both have to come from this one request:
    /// a follow-up query could observe a later commit.
    pub fn transactionSets(self: *RqliteClient, sql: []const u8) ![]QueryResult {
        const body = try buildExecuteRequest(self.allocator, sql);
        defer self.allocator.free(body);
        const url = try std.fmt.allocPrint(self.allocator, "{s}/db/request?transaction", .{self.base_url});
        defer self.allocator.free(url);
        var response = try self.http_client.post(url, body);
        defer response.deinit();
        reportError(response.body);
        const parsed = try std.json.parseFromSlice(std.json.Value, self.allocator, response.body, .{});
        defer parsed.deinit();
        const results = try responseResults(parsed.value);
        var sets: std.ArrayList(QueryResult) = .empty;
        errdefer {
            for (sets.items) |*set| set.deinit(self.allocator);
            sets.deinit(self.allocator);
        }
        for (results) |entry| {
            if (entry.object.get("columns") == null) continue;
            try sets.append(self.allocator, try parseQueryEntry(self.allocator, entry));
        }
        return sets.toOwnedSlice(self.allocator);
    }

    /// Execute multiple SQL statements in a single Raft transaction
    pub fn executeMany(self: *RqliteClient, statements: []const []const u8) ![]ExecuteResult {
        const body = try buildExecuteManyRequest(self.allocator, statements);
        defer self.allocator.free(body);

        const url = try std.fmt.allocPrint(self.allocator, "{s}/db/execute?transaction", .{self.base_url});
        defer self.allocator.free(url);
        var response = try self.http_client.post(url, body);
        defer response.deinit();

        return try parseExecuteManyResponse(self.allocator, response.body);
    }

    /// Query (read operation, can be served by any node)
    pub fn query(self: *RqliteClient, sql: []const u8) !QueryResult {
        const body = try buildQueryRequest(self.allocator, sql);
        defer self.allocator.free(body);

        // KIP-29 §7 requires `linearizable` for every read: rqlite resolves it
        // through a Raft quorum check and an applied-index wait, so a read can
        // never be served from a stale follower. The level must be explicit
        // because rqlite's default (`weak`) is not linearizable.
        const url = try std.fmt.allocPrint(self.allocator, "{s}/db/query?level=linearizable", .{self.base_url});
        defer self.allocator.free(url);
        var response = try self.queryWithRetry(url, body);
        defer response.deinit();

        return try parseQueryResponse(self.allocator, response.body);
    }

    /// Join this node to an existing rqlite cluster
    pub fn join(self: *RqliteClient, node_id: []const u8, address: []const u8) !void {
        var buf = std.ArrayList(u8).init(self.allocator);
        defer buf.deinit();

        var jb = JsonBuilder.init(self.allocator);
        try jb.startObject();
        try jb.addString("node_id", node_id);
        try jb.addString("address", address);
        try jb.endObject();
        const body = try jb.finish();
        defer self.allocator.free(body);

        const response = try self.http_client.post(self.base_url ++ "/db/join", body);
        defer response.deinit();
    }

    /// Get rqlite node status
    pub fn status(self: *RqliteClient) !NodeStatus {
        // `++` cannot concatenate onto a slice it does not own; base_url is
        // owned by the client, so this needs a real buffer.
        var buf: [512]u8 = undefined;
        const path = std.fmt.bufPrint(&buf, "{s}/status", .{self.base_url}) catch return error.InvalidUrl;
        var response = try self.http_client.get(path);
        defer response.deinit();

        return try parseStatusResponse(self.allocator, response.body);
    }
};

// ─── JSON request builders ──────────────────────────────────────────

// Split SQL outside quoted strings. rqlite executes each entry in a single
// transaction; explicit BEGIN/COMMIT wrappers must not be nested.
fn buildExecuteRequest(allocator: Allocator, sql: []const u8) ![]u8 {
    var statements: std.ArrayList([]const u8) = .empty;
    defer statements.deinit(allocator);
    var quoted = false;
    var start: usize = 0;
    for (sql, 0..) |byte, index| {
        if (byte == '\'') quoted = !quoted;
        if (byte != ';' or quoted) continue;
        const statement = std.mem.trim(u8, sql[start..index], " \n\r\t");
        start = index + 1;
        if (isTransactionWrapper(statement) or statement.len == 0) continue;
        try statements.append(allocator, statement);
    }
    const rest = std.mem.trim(u8, sql[start..], " \n\r\t");
    if (rest.len > 0 and !isTransactionWrapper(rest)) try statements.append(allocator, rest);
    return buildExecuteManyRequest(allocator, statements.items);
}

fn isTransactionWrapper(statement: []const u8) bool {
    return std.ascii.eqlIgnoreCase(statement, "BEGIN") or
        std.ascii.eqlIgnoreCase(statement, "BEGIN TRANSACTION") or
        std.ascii.eqlIgnoreCase(statement, "COMMIT");
}

fn buildExecuteManyRequest(allocator: Allocator, statements: []const []const u8) ![]u8 {
    return std.json.Stringify.valueAlloc(allocator, statements, .{});
}

fn buildQueryRequest(allocator: Allocator, sql: []const u8) ![]u8 {
    return std.json.Stringify.valueAlloc(allocator, &[_][]const u8{sql}, .{});
}

// ─── JSON response parsers ──────────────────────────────────────────

pub const ExecuteResult = struct {
    last_insert_id: i64,
    rows_affected: i64,
    err_msg: ?[]const u8,
};

pub const QueryResult = struct {
    columns: [][]const u8,
    types: [][]const u8,
    values: [][]Value,

    pub fn deinit(self: QueryResult, allocator: Allocator) void {
        for (self.columns) |column| allocator.free(column);
        allocator.free(self.columns);
        for (self.types) |kind| allocator.free(kind);
        allocator.free(self.types);
        for (self.values) |row| {
            for (row) |value| switch (value) {
                .text, .blob => |bytes| allocator.free(bytes),
                else => {},
            };
            allocator.free(row);
        }
        allocator.free(self.values);
    }
};

pub const Value = union(enum) {
    null: void,
    integer: i64,
    real: f64,
    text: []const u8,
    blob: []const u8,
};

pub const NodeStatus = struct {
    leader_address: []const u8,
    leader_id: []const u8,
    node_id: []const u8,
    version: []const u8,
    /// store.raft.applied_index — the last log index this node applied.
    raft_applied_index: i64 = 0,
    /// store.raft.term — the Raft term this node has seen.
    raft_term: i64 = 0,
};

fn responseResults(value: std.json.Value) ![]std.json.Value {
    if (value != .object) return error.InvalidResponse;
    if (value.object.get("error") != null) return error.RqliteError;
    const results = value.object.get("results") orelse return error.InvalidResponse;
    if (results != .array) return error.InvalidResponse;
    for (results.array.items) |result| {
        if (result != .object) return error.InvalidResponse;
        if (result.object.get("error") != null) return error.RqliteError;
    }
    return results.array.items;
}

/// rqlite's own text for a failed request. KIP-29 requires a write path to
/// check for database errors rather than trust HTTP 200. Discarding the text
/// satisfied that only formally: an operator saw a bare `RqliteError` with no
/// indication of what rqlite rejected. Reported at the HTTP boundary, where
/// the failing request is known, rather than in the parser.
fn reportError(body: []const u8) void {
    const parsed = std.json.parseFromSlice(std.json.Value, std.heap.page_allocator, body, .{}) catch return;
    defer parsed.deinit();
    if (parsed.value != .object) return;
    const object = parsed.value.object;
    const message = object.get("error") orelse blk: {
        const results = object.get("results") orelse return;
        if (results != .array) return;
        for (results.array.items) |entry| {
            if (entry != .object) continue;
            if (entry.object.get("error")) |e| break :blk e;
        }
        return;
    };
    std.log.err("rqlite rejected the request: {s}", .{switch (message) {
        .string => |text| text,
        else => "unreported error",
    }});
}

fn parseExecuteResponse(allocator: Allocator, body: []const u8) !ExecuteResult {
    const parsed = try std.json.parseFromSlice(std.json.Value, allocator, body, .{});
    defer parsed.deinit();
    const results = try responseResults(parsed.value);
    var result: ExecuteResult = .{ .last_insert_id = 0, .rows_affected = 0, .err_msg = null };
    for (results) |entry| {
        if (entry.object.get("rows_affected")) |v| result.rows_affected += v.integer;
        if (entry.object.get("last_insert_id")) |v| result.last_insert_id = v.integer;
    }
    return result;
}

fn parseExecuteManyResponse(allocator: Allocator, body: []const u8) ![]ExecuteResult {
    const result = try parseExecuteResponse(allocator, body);
    const results = try allocator.alloc(ExecuteResult, 1);
    results[0] = result;
    return results;
}

fn jsonStrings(allocator: Allocator, value: ?std.json.Value) ![][]const u8 {
    const array = value orelse return allocator.alloc([]const u8, 0);
    if (array != .array) return error.InvalidResponse;
    const strings = try allocator.alloc([]const u8, array.array.items.len);
    var initialized: usize = 0;
    errdefer {
        for (strings[0..initialized]) |s| allocator.free(s);
        allocator.free(strings);
    }
    for (array.array.items, 0..) |entry, i| {
        if (entry != .string) return error.InvalidResponse;
        strings[i] = try allocator.dupe(u8, entry.string);
        initialized += 1;
    }
    return strings;
}

fn parseQueryResponse(allocator: Allocator, body: []const u8) !QueryResult {
    const parsed = try std.json.parseFromSlice(std.json.Value, allocator, body, .{});
    defer parsed.deinit();
    const results = try responseResults(parsed.value);
    if (results.len != 1) return error.InvalidResponse;
    return parseQueryEntry(allocator, results[0]);
}

fn parseQueryEntry(allocator: Allocator, value: std.json.Value) !QueryResult {
    const entry = value.object;
    var result: QueryResult = .{ .columns = &.{}, .types = &.{}, .values = &.{} };
    errdefer result.deinit(allocator);
    result.columns = try jsonStrings(allocator, entry.get("columns"));
    result.types = try jsonStrings(allocator, entry.get("types"));
    if (entry.get("values")) |rows| {
        if (rows != .array) return error.InvalidResponse;
        result.values = try allocator.alloc([]Value, rows.array.items.len);
        for (result.values) |*row| row.* = &.{};
        for (rows.array.items, 0..) |row, i| {
            if (row != .array) return error.InvalidResponse;
            result.values[i] = try allocator.alloc(Value, row.array.items.len);
            for (result.values[i]) |*v| v.* = .{ .null = {} };
            for (row.array.items, 0..) |v, j| result.values[i][j] = switch (v) {
                .null => .{ .null = {} },
                .integer => |number| .{ .integer = number },
                .float => |number| .{ .real = number },
                .string => |bytes| blk: {
                    if (j < result.types.len and std.mem.eql(u8, result.types[j], "blob")) {
                        const decoder = std.base64.standard.Decoder;
                        const decoded = try allocator.alloc(u8, try decoder.calcSizeForSlice(bytes));
                        errdefer allocator.free(decoded);
                        try decoder.decode(decoded, bytes);
                        break :blk .{ .blob = decoded };
                    }
                    break :blk .{ .text = try allocator.dupe(u8, bytes) };
                },
                // rqlite returns BLOBs as arrays of byte values.
                .array => |bytes| blk: {
                    const data = try allocator.alloc(u8, bytes.items.len);
                    errdefer allocator.free(data);
                    for (bytes.items, 0..) |byte, k| {
                        if (byte != .integer or byte.integer < 0 or byte.integer > 255) return error.InvalidResponse;
                        data[k] = @intCast(byte.integer);
                    }
                    break :blk .{ .blob = data };
                },
                else => return error.InvalidResponse,
            };
        }
    }
    return result;
}

/// Parse the parts of rqlite's /status that Maintenance.Status reports.
///
/// This walks the parsed JSON rather than scanning for `"key":"` patterns.
/// rqlite nests `leader` and `raft` inside `store`, and both carry a `node_id`
/// and a `term`, so a textual search for the first occurrence of either name
/// returns whichever one happens to appear first in the document rather than
/// the one that was asked for.
fn parseStatusResponse(allocator: Allocator, body: []const u8) !NodeStatus {
    const parsed = std.json.parseFromSlice(std.json.Value, allocator, body, .{}) catch
        return NodeStatus{
            .leader_address = try allocator.dupe(u8, ""),
            .leader_id = try allocator.dupe(u8, ""),
            .node_id = try allocator.dupe(u8, ""),
            .version = try allocator.dupe(u8, ""),
        };
    defer parsed.deinit();

    const root = parsed.value;
    if (root != .object) {
        return NodeStatus{
            .leader_address = try allocator.dupe(u8, ""),
            .leader_id = try allocator.dupe(u8, ""),
            .node_id = try allocator.dupe(u8, ""),
            .version = try allocator.dupe(u8, ""),
        };
    }
    const store = root.object.get("store");
    const store_obj: ?std.json.ObjectMap = if (store != null and store.? == .object) store.?.object else null;

    const node_id = if (store_obj) |o| jsonString(o.get("node_id")) else "";
    const leader_addr = blk: {
        const leader = if (store_obj) |o| o.get("leader") else null;
        if (leader == null or leader.? != .object) break :blk "";
        break :blk jsonString(leader.?.object.get("addr"));
    };
    const leader_id = blk: {
        const leader = if (store_obj) |o| o.get("leader") else null;
        if (leader == null or leader.? != .object) break :blk "";
        break :blk jsonString(leader.?.object.get("node_id"));
    };
    const applied_index = blk: {
        const raft = if (store_obj) |o| o.get("raft") else null;
        if (raft == null or raft.? != .object) break :blk @as(i64, 0);
        break :blk jsonInt(raft.?.object.get("applied_index"));
    };
    const term = blk: {
        const raft = if (store_obj) |o| o.get("raft") else null;
        if (raft == null or raft.? != .object) break :blk @as(i64, 0);
        break :blk jsonInt(raft.?.object.get("term"));
    };

    return NodeStatus{
        .leader_address = try allocator.dupe(u8, leader_addr),
        .leader_id = try allocator.dupe(u8, leader_id),
        .node_id = try allocator.dupe(u8, node_id),
        .version = try allocator.dupe(u8, extractStringField(body, "version") orelse ""),
        .raft_applied_index = applied_index,
        .raft_term = term,
    };
}

fn jsonString(value: ?std.json.Value) []const u8 {
    const v = value orelse return "";
    return if (v == .string) v.string else "";
}

fn jsonInt(value: ?std.json.Value) i64 {
    const v = value orelse return 0;
    return switch (v) {
        .integer => |n| n,
        .float => |f| @intFromFloat(f),
        else => 0,
    };
}

// ─── JSON helpers ───────────────────────────────────────────────────

fn extractIntField(json: []const u8, key: []const u8) ?i64 {
    var pattern_buf: [256]u8 = undefined;
    const pattern = std.fmt.bufPrint(&pattern_buf, "\"{s}\":", .{key}) catch return null;
    const start = std.mem.indexOf(u8, json, pattern) orelse return null;
    const value_start = start + pattern.len;
    var pos = value_start;
    while (pos < json.len and (json[pos] == ' ' or json[pos] == '\t')) : (pos += 1) {}
    var negative = false;
    if (pos < json.len and json[pos] == '-') {
        negative = true;
        pos += 1;
    }
    var value: i64 = 0;
    while (pos < json.len) {
        const c = json[pos];
        if (c >= '0' and c <= '9') {
            value = value * 10 + @as(i64, @intCast(c - '0'));
            pos += 1;
        } else break;
    }
    return if (negative) -value else value;
}

fn extractStringField(json: []const u8, key: []const u8) ?[]const u8 {
    var pattern_buf: [256]u8 = undefined;
    const pattern = std.fmt.bufPrint(&pattern_buf, "\"{s}\":\"", .{key}) catch return null;
    const start = std.mem.indexOf(u8, json, pattern) orelse return null;
    const value_start = start + pattern.len;
    const value_end = std.mem.indexOfScalarPos(u8, json, value_start, '"') orelse return null;
    return json[value_start..value_end];
}

fn parseStringArrayField(allocator: Allocator, json: []const u8, key: []const u8) ![][]const u8 {
    var pattern_buf: [256]u8 = undefined;
    const pattern = std.fmt.bufPrint(&pattern_buf, "\"{s}\":[", .{key}) catch return &[_][]const u8{};
    const start = std.mem.indexOf(u8, json, pattern) orelse return &[_][]const u8{};
    const array_start = start + pattern.len;
    const array_end = std.mem.indexOfScalarPos(u8, json, array_start, ']') orelse return &[_][]const u8{};
    const array_content = json[array_start..array_end];

    var result = std.ArrayList([]const u8).empty;
    var it = std.mem.splitScalar(u8, array_content, ',');
    while (it.next()) |item| {
        const trimmed = std.mem.trim(u8, item, " \"");
        if (trimmed.len > 0) {
            try result.append(allocator, try allocator.dupe(u8, trimmed));
        }
    }
    return result.toOwnedSlice(allocator);
}

fn parseValuesArray(allocator: Allocator, json: []const u8, types: [][]const u8) ![][]Value {
    _ = types;
    // Find "values":[[...],[...]]
    const values_key = "\"values\":[";
    const start = std.mem.indexOf(u8, json, values_key) orelse return &[_][]Value{};
    const array_start = start + values_key.len;

    // Find matching closing bracket at top level
    var depth: u32 = 1;
    var pos = array_start;
    while (pos < json.len and depth > 0) {
        switch (json[pos]) {
            '[' => depth += 1,
            ']' => depth -= 1,
            else => {},
        }
        pos += 1;
    }
    if (depth != 0) return &[_][]Value{};
    const array_end = pos - 1;

    // Parse each row
    var result = std.ArrayList([]Value).empty;
    // Split rows by ],[
    var row_start = array_start;
    var row_end = array_start;
    var row_depth: u32 = 1;

    while (row_end < array_end) {
        if (json[row_end] == '[') row_depth += 1;
        if (json[row_end] == ']') row_depth -= 1;
        if (row_depth == 1 and json[row_end] == ']') {
            // End of a row
            const row_content = json[row_start..row_end];
            const values = try parseRowValues(allocator, row_content);
            try result.append(allocator, values);
            row_end += 2; // skip ],
            row_start = row_end;
            row_depth = 1;
        } else {
            row_end += 1;
        }
    }

    return result.toOwnedSlice(allocator);
}

fn parseRowValues(allocator: Allocator, row: []const u8) ![]Value {
    var result = std.ArrayList(Value).empty;
    var depth: u32 = 0;
    var field_start: usize = 0;
    var i: usize = 0;

    while (i <= row.len) {
        const c = if (i < row.len) row[i] else ',';
        if (c == '[' or c == '(') {
            depth += 1;
        } else if (c == ']' or c == ')') {
            depth -= 1;
        } else if (c == ',' and depth == 0) {
            const field = row[field_start..i];
            try result.append(allocator, try parseValue(allocator, field));
            field_start = i + 1;
        }
        i += 1;
    }

    return result.toOwnedSlice(allocator);
}

fn parseValue(allocator: Allocator, field: []const u8) !Value {
    const trimmed = std.mem.trim(u8, field, " ");
    if (trimmed.len == 0 or std.mem.eql(u8, trimmed, "null")) {
        return Value{ .null = {} };
    }
    if (trimmed[0] == '"' and trimmed[trimmed.len - 1] == '"') {
        return Value{ .text = try allocator.dupe(u8, trimmed[1 .. trimmed.len - 1]) };
    }
    if (std.mem.indexOf(u8, trimmed, ".")) |_| {
        const f = std.fmt.parseFloat(f64, trimmed) catch 0;
        return Value{ .real = f };
    }
    const i = std.fmt.parseInt(i64, trimmed, 10) catch 0;
    return Value{ .integer = i };
}

// ─── SQL Statement Builder ──────────────────────────────────────────

pub const SqlBuilder = struct {
    pub fn schemaSql() []const u8 {
        return
        \\CREATE TABLE IF NOT EXISTS kv (
        \\  key BLOB PRIMARY KEY,
        \\  value BLOB NOT NULL,
        \\  create_revision INTEGER NOT NULL,
        \\  mod_revision INTEGER NOT NULL,
        \\  version INTEGER NOT NULL DEFAULT 1,
        \\  lease INTEGER NOT NULL DEFAULT 0
        \\);
        \\CREATE TABLE IF NOT EXISTS kv_history (
        \\  id INTEGER PRIMARY KEY AUTOINCREMENT,
        \\  key BLOB NOT NULL,
        \\  value BLOB,
        \\  create_revision INTEGER NOT NULL,
        \\  mod_revision INTEGER NOT NULL,
        \\  version INTEGER NOT NULL,
        \\  lease INTEGER NOT NULL DEFAULT 0,
        \\  deleted INTEGER NOT NULL DEFAULT 0
        \\);
        \\CREATE INDEX IF NOT EXISTS idx_kv_history_key_rev ON kv_history(key, mod_revision);
        \\CREATE TABLE IF NOT EXISTS revision (
        \\  id INTEGER PRIMARY KEY,
        \\  current_revision INTEGER NOT NULL DEFAULT 1,
        \\  raft_term INTEGER NOT NULL DEFAULT 0
        \\);
        \\CREATE TABLE IF NOT EXISTS leases (
        \\  id INTEGER PRIMARY KEY,
        \\  ttl INTEGER NOT NULL,
        \\  created_at INTEGER NOT NULL,
        \\  expires_at INTEGER NOT NULL,
        \\  owner TEXT NOT NULL DEFAULT '',
        \\  owner_expires_at INTEGER NOT NULL DEFAULT 0
        \\);
        \\CREATE TABLE IF NOT EXISTS lease_sequence (id INTEGER PRIMARY KEY, last_id INTEGER NOT NULL);
        \\INSERT OR IGNORE INTO lease_sequence(id,last_id) SELECT 1,MAX(1000,COALESCE(MAX(id),1000)) FROM leases;
        \\CREATE INDEX IF NOT EXISTS idx_leases_expiry ON leases(expires_at);
        \\CREATE TABLE IF NOT EXISTS lease_keys (
        \\  lease_id INTEGER NOT NULL,
        \\  key BLOB NOT NULL,
        \\  PRIMARY KEY (lease_id, key)
        \\);
        \\CREATE TABLE IF NOT EXISTS compaction (
        \\  id INTEGER PRIMARY KEY,
        \\  compact_revision INTEGER NOT NULL DEFAULT 0
        \\);
        \\INSERT OR IGNORE INTO revision (id, current_revision) VALUES (1, 1);
        \\INSERT OR IGNORE INTO compaction (id, compact_revision) VALUES (1, 0);
        ;
    }

    /// Additive migrations for data directories written by an earlier build.
    /// `CREATE TABLE IF NOT EXISTS` leaves an existing table's columns alone,
    /// so a new column has to be added separately or a restart on an existing
    /// data dir would fail every statement that mentions it.
    pub fn migrationSql() []const u8 {
        return
        \\ALTER TABLE leases ADD COLUMN owner TEXT NOT NULL DEFAULT '';
        \\CREATE INDEX IF NOT EXISTS idx_leases_expiry ON leases(expires_at);
        ;
    }

    /// True when the lease table predates the owner column, so the ALTER would
    /// fail with "duplicate column name". Checked before running the migration.
    pub fn leasesOwnerColumnSql() []const u8 {
        return "SELECT COUNT(*) FROM pragma_table_info('leases') WHERE name='owner'";
    }

    pub fn leasesOwnerDeadlineColumnSql() []const u8 {
        return "SELECT COUNT(*) FROM pragma_table_info('leases') WHERE name='owner_expires_at'";
    }

    pub fn ownerDeadlineMigrationSql() []const u8 {
        return "ALTER TABLE leases ADD COLUMN owner_expires_at INTEGER NOT NULL DEFAULT 0";
    }

    fn keyPredicate(allocator: Allocator, key: []const u8, end: []const u8) ![]u8 {
        const start_hex = try toHex(allocator, key);
        defer allocator.free(start_hex);
        if (end.len == 0) return std.fmt.allocPrint(allocator, "key=X'{s}'", .{start_hex});
        if (std.mem.eql(u8, end, "\x00")) return std.fmt.allocPrint(allocator, "key>=X'{s}'", .{start_hex});
        const end_hex = try toHex(allocator, end);
        defer allocator.free(end_hex);
        return std.fmt.allocPrint(allocator, "key>=X'{s}' AND key<X'{s}'", .{ start_hex, end_hex });
    }

    pub fn rangeSql(allocator: Allocator, key: []const u8, range_end: []const u8, limit: i64, revision: i64, keys_only: bool, count_only: bool) ![]u8 {
        const predicate = try keyPredicate(allocator, key, range_end);
        defer allocator.free(predicate);
        // Count before LIMIT in the same SQL snapshot as the returned page.
        const selection = if (count_only) "COUNT(*)" else if (keys_only) "key,'' AS value,create_revision,mod_revision,version,lease,COUNT(*) OVER()" else "key,value,create_revision,mod_revision,version,lease,COUNT(*) OVER()";
        const source = if (revision > 0)
            try std.fmt.allocPrint(allocator, "(SELECT * FROM kv_history WHERE id IN (SELECT MAX(id) FROM kv_history WHERE mod_revision<={d} GROUP BY key) AND deleted=0)", .{revision})
        else
            try allocator.dupe(u8, "kv");
        defer allocator.free(source);
        const suffix = if (limit > 0 and !count_only) try std.fmt.allocPrint(allocator, " LIMIT {d}", .{limit}) else try allocator.dupe(u8, "");
        defer allocator.free(suffix);
        return std.fmt.allocPrint(allocator, "SELECT {s} FROM {s} WHERE {s} ORDER BY key{s}", .{ selection, source, predicate, suffix });
    }

    pub fn putSql(allocator: Allocator, key: []const u8, value: []const u8, lease: i64) ![]u8 {
        const key_hex = try toHex(allocator, key);
        defer allocator.free(key_hex);
        const value_hex = try toHex(allocator, value);
        defer allocator.free(value_hex);
        return std.fmt.allocPrint(allocator, "UPDATE revision SET current_revision=current_revision+1 WHERE id=1;" ++
            "INSERT INTO kv(key,value,create_revision,mod_revision,version,lease) " ++
            "VALUES(X'{s}',X'{s}',(SELECT current_revision FROM revision WHERE id=1),(SELECT current_revision FROM revision WHERE id=1),1,{d}) " ++
            "ON CONFLICT(key) DO UPDATE SET value=excluded.value,mod_revision=excluded.mod_revision,version=kv.version+1,lease=excluded.lease;" ++
            "INSERT INTO kv_history(key,value,create_revision,mod_revision,version,lease,deleted) " ++
            "SELECT key,value,create_revision,mod_revision,version,lease,0 FROM kv WHERE key=X'{s}';" ++
            "DELETE FROM lease_keys WHERE key=X'{s}';" ++
            "INSERT INTO lease_keys(lease_id,key) SELECT lease,key FROM kv WHERE key=X'{s}' AND lease!=0;", .{ key_hex, value_hex, lease, key_hex, key_hex, key_hex });
    }

    pub fn deleteRangeSql(allocator: Allocator, key: []const u8, range_end: []const u8) ![]u8 {
        const predicate = try keyPredicate(allocator, key, range_end);
        defer allocator.free(predicate);
        return std.fmt.allocPrint(allocator, "UPDATE revision SET current_revision=current_revision+1 WHERE id=1 AND EXISTS(SELECT 1 FROM kv WHERE {s});" ++
            "INSERT INTO kv_history(key,value,create_revision,mod_revision,version,lease,deleted) SELECT key,value,create_revision,(SELECT current_revision FROM revision WHERE id=1),version,lease,1 FROM kv WHERE {s};" ++
            "DELETE FROM kv WHERE {s};DELETE FROM lease_keys WHERE {s};", .{ predicate, predicate, predicate, predicate });
    }

    /// Capture the exact deleted rows before mutating them, inside the same
    /// Raft transaction. The first result set is the prev_kvs and its length
    /// is the authoritative deleted count.
    pub fn atomicDeleteRangeSql(allocator: Allocator, key: []const u8, range_end: []const u8) ![]u8 {
        const predicate = try keyPredicate(allocator, key, range_end);
        defer allocator.free(predicate);
        return std.fmt.allocPrint(allocator,
            "BEGIN;DROP TABLE IF EXISTS tandem_delete_prev;CREATE TEMP TABLE tandem_delete_prev AS SELECT key,value,create_revision,mod_revision,version,lease FROM kv WHERE {s};" ++
            "UPDATE revision SET current_revision=current_revision+1 WHERE id=1 AND EXISTS(SELECT 1 FROM tandem_delete_prev);" ++
            "INSERT INTO kv_history(key,value,create_revision,mod_revision,version,lease,deleted) SELECT key,value,create_revision,(SELECT current_revision FROM revision WHERE id=1),version,lease,1 FROM tandem_delete_prev;" ++
            "DELETE FROM lease_keys WHERE key IN (SELECT key FROM tandem_delete_prev);" ++
            "DELETE FROM kv WHERE key IN (SELECT key FROM tandem_delete_prev);" ++
            "SELECT key,value,create_revision,mod_revision,version,lease FROM tandem_delete_prev ORDER BY key;" ++
            "SELECT current_revision FROM revision WHERE id=1;DROP TABLE tandem_delete_prev;COMMIT;", .{predicate});
    }

    pub fn currentRevisionSql() []const u8 {
        return "SELECT current_revision FROM revision WHERE id = 1";
    }

    pub fn raftTermSql() []const u8 {
        return "SELECT raft_term FROM revision WHERE id = 1";
    }

    pub fn readKeySql() []const u8 {
        return "SELECT key, value, create_revision, mod_revision, version, lease FROM kv WHERE key = ?";
    }

    pub fn keyHistorySql() []const u8 {
        return "SELECT key, value, create_revision, mod_revision, version, lease, deleted FROM kv_history WHERE key = ? ORDER BY mod_revision";
    }

    /// Bound the replay and preserve commit order, including transaction sub-events.
    pub fn watchHistorySql(allocator: Allocator, start: i64, end: i64) ![]u8 {
        return std.fmt.allocPrint(allocator, "SELECT h.key,h.value,h.create_revision,h.mod_revision,h.version,h.lease,h.deleted," ++
            "p.key,p.value,p.mod_revision,p.version,p.lease FROM kv_history h " ++
            "LEFT JOIN kv_history p ON p.id=(SELECT MAX(b.id) FROM kv_history b WHERE b.key=h.key AND b.id<h.id) " ++
            "AND p.deleted=0 WHERE h.mod_revision>={d} AND h.mod_revision<={d} ORDER BY h.mod_revision,h.id", .{ start, end });
    }

    pub fn compactSql(revision: i64) ![]u8 {
        var buf = std.ArrayList(u8).empty;
        defer buf.deinit(std.heap.page_allocator);
        try buf.appendSlice(std.heap.page_allocator, "DELETE FROM kv_history WHERE mod_revision < ");
        try formatInt(std.heap.page_allocator, &buf, revision);
        try buf.appendSlice(std.heap.page_allocator, " AND id NOT IN (SELECT MAX(id) FROM kv_history WHERE mod_revision < ");
        try formatInt(std.heap.page_allocator, &buf, revision);
        try buf.appendSlice(std.heap.page_allocator, " GROUP BY key); UPDATE compaction SET compact_revision = ");
        try formatInt(std.heap.page_allocator, &buf, revision);
        try buf.appendSlice(std.heap.page_allocator, " WHERE id = 1;");
        return try buf.toOwnedSlice(std.heap.page_allocator);
    }

    pub fn createLeaseSql() []const u8 {
        return "INSERT INTO leases (id, ttl, created_at, expires_at) VALUES (?, ?, strftime('%s','now'), strftime('%s','now') + ?)";
    }

    /// Grant a lease with an explicit wall-clock deadline.
    ///
    /// rqlite rewrites `now` in write statements to a deterministic,
    /// Raft-assigned instant so that every replica applies the same write. That
    /// instant is not the host clock: on a node running in UTC+8 it read eight
    /// hours ahead of real time. Comparing that value against a `strftime` read
    /// back through `/db/query` (which is *not* rewritten) made every lease look
    /// far from expiry, so a lapsed lease never released its key. The clock is
    /// therefore supplied by the caller, which keeps grant, keepalive and the
    /// expiry scan on one UTC source and keeps the write deterministic — the
    /// timestamp is a replicated input rather than something each replica
    /// computes for itself, as KIP-29 §6 requires.
    pub fn createLeaseForSql(allocator: Allocator, id: i64, ttl: i64, now: i64) ![]u8 {
        return std.fmt.allocPrint(allocator, "BEGIN;INSERT INTO leases(id,ttl,created_at,expires_at) VALUES({d},{d},{d},{d});UPDATE lease_sequence SET last_id=MAX(last_id,{d}) WHERE id=1;COMMIT;", .{ id, ttl, now, now + ttl, id });
    }

    pub fn allocateLeaseSql(allocator: Allocator, ttl: i64, now: i64) ![]u8 {
        return std.fmt.allocPrint(allocator, "BEGIN;UPDATE lease_sequence SET last_id=last_id+1 WHERE id=1;INSERT INTO leases(id,ttl,created_at,expires_at) SELECT last_id,{d},{d},{d} FROM lease_sequence WHERE id=1;SELECT last_id FROM lease_sequence WHERE id=1;COMMIT;", .{ ttl, now, now + ttl });
    }

    /// Take ownership of one expired lease, or report that someone else won.
    ///
    /// KIP-29 requires expiry to run under exactly one owner rather than on
    /// every instance's local clock, so the claim is a conditional update
    /// against the owner column. The condition is part of the same Raft
    /// transaction as the claim, so two instances racing on the same lease
    /// cannot both believe they own it: exactly one UPDATE reports a row.
    pub fn claimLeaseForSql(allocator: Allocator, id: i64, owner: []const u8, now: i64) ![]u8 {
        var buf = std.ArrayList(u8).empty;
        errdefer buf.deinit(allocator);
        try buf.appendSlice(allocator, "UPDATE leases SET owner='");
        try appendSqlString(allocator, &buf, owner);
        try buf.appendSlice(allocator, "',owner_expires_at=");
        try formatInt(allocator, &buf, now + 30);
        try buf.appendSlice(allocator, " WHERE id=");
        try formatInt(allocator, &buf, id);
        try buf.appendSlice(allocator, " AND expires_at<=");
        try formatInt(allocator, &buf, now);
        try buf.appendSlice(allocator, " AND (owner='' OR owner='");
        try appendSqlString(allocator, &buf, owner);
        try buf.appendSlice(allocator, "' OR owner_expires_at<=");
        try formatInt(allocator, &buf, now);
        try buf.appendSlice(allocator, ")");
        return buf.toOwnedSlice(allocator);
    }

    pub fn renewLeaseOwnerSql(allocator: Allocator, id: i64, owner: []const u8, now: i64) ![]u8 {
        var buf = std.ArrayList(u8).empty;
        errdefer buf.deinit(allocator);
        try buf.appendSlice(allocator, "UPDATE leases SET owner_expires_at=");
        try formatInt(allocator, &buf, now + 30);
        try buf.appendSlice(allocator, " WHERE id=");
        try formatInt(allocator, &buf, id);
        try buf.appendSlice(allocator, " AND owner='");
        try appendSqlString(allocator, &buf, owner);
        try buf.appendSlice(allocator, "' AND owner_expires_at>");
        try formatInt(allocator, &buf, now);
        return buf.toOwnedSlice(allocator);
    }

    /// Release ownership so another instance can take over, used when this
    /// instance shuts down or stops being able to reap.
    pub fn releaseLeasesForSql(allocator: Allocator, owner: []const u8) ![]u8 {
        var buf = std.ArrayList(u8).empty;
        errdefer buf.deinit(allocator);
        try buf.appendSlice(allocator, "UPDATE leases SET owner='',owner_expires_at=0 WHERE owner='");
        try appendSqlString(allocator, &buf, owner);
        try buf.appendSlice(allocator, "';");
        return buf.toOwnedSlice(allocator);
    }

    pub fn keepAliveLeaseForSql(allocator: Allocator, id: i64, now: i64) ![]u8 {
        return std.fmt.allocPrint(allocator, "UPDATE leases SET expires_at={d}+ttl WHERE id={d}", .{ now, id });
    }

    pub fn revokeLeaseForSql(allocator: Allocator, id: i64) ![]u8 {
        return std.fmt.allocPrint(allocator, "BEGIN; DELETE FROM lease_keys WHERE lease_id={d}; DELETE FROM leases WHERE id={d}; COMMIT;", .{ id, id });
    }

    pub fn attachLeaseKeyForSql(allocator: Allocator, id: i64, key: []const u8) ![]u8 {
        var buf = std.ArrayList(u8).empty;
        errdefer buf.deinit(allocator);
        try buf.appendSlice(allocator, "INSERT OR IGNORE INTO lease_keys(lease_id,key) VALUES(");
        try formatInt(allocator, &buf, id);
        try buf.appendSlice(allocator, ",");
        // A key is a BLOB column, and a single-quoted literal is TEXT: a raw
        // NUL or 0xFF in the key is either a syntax error or silently
        // truncated at the NUL. The hex literal round-trips every byte.
        try appendBlob(allocator, &buf, key);
        try buf.appendSlice(allocator, ")");
        return buf.toOwnedSlice(allocator);
    }

    pub fn revokeLeaseSql() []const u8 {
        return "DELETE FROM leases WHERE id = ?";
    }

    pub fn keepAliveLeaseSql() []const u8 {
        return "UPDATE leases SET expires_at = strftime('%s','now') + ttl WHERE id = ?";
    }

    /// Leases whose deadline has passed. `now` is the caller's UTC clock for the
    /// same reason as `createLeaseForSql`: the scan runs on the read path, so a
    /// `strftime` here would not agree with the write-path clock the deadline
    /// was written with.
    pub fn expiredLeasesSql(allocator: Allocator, now: i64) ![]u8 {
        return std.fmt.allocPrint(allocator, "SELECT id FROM leases WHERE expires_at <= {d}", .{now});
    }

    pub fn leaseInfoSql() []const u8 {
        return "SELECT id, ttl, created_at, expires_at FROM leases WHERE id = ?";
    }

    pub fn allLeasesSql() []const u8 {
        return "SELECT id FROM leases";
    }

    pub fn addLeaseKeySql() []const u8 {
        return "INSERT OR IGNORE INTO lease_keys (lease_id, key) VALUES (?, ?)";
    }

    pub fn removeLeaseKeySql() []const u8 {
        return "DELETE FROM lease_keys WHERE lease_id = ? AND key = ?";
    }

    pub fn leaseKeysSql() []const u8 {
        return "SELECT key FROM lease_keys WHERE lease_id = ?";
    }

    pub fn deleteLeaseKeysSql() []const u8 {
        return "DELETE FROM lease_keys WHERE lease_id = ?";
    }

    pub fn leaseKeysForIdSql(allocator: Allocator, lease_id: i64) ![]u8 {
        return std.fmt.allocPrint(allocator, "SELECT key FROM lease_keys WHERE lease_id = {d}", .{lease_id});
    }

    /// Whether the lease is still granted. Revoking one that was never
    /// granted, or has already been revoked, has to report NotFound rather
    /// than delete nothing and answer OK.
    pub fn leaseExistsSql(allocator: Allocator, lease_id: i64) ![]u8 {
        return std.fmt.allocPrint(allocator, "SELECT COUNT(*) FROM leases WHERE id = {d}", .{lease_id});
    }

    /// The granted TTL and the seconds left, for a live lease. The deadline
    /// travels with the statement rather than being computed by SQL: rqlite
    /// rewrites `now` in a write to a Raft-assigned instant, so a read-side
    /// comparison would be against a different clock.
    pub fn leaseTtlSql(allocator: Allocator, lease_id: i64, now: i64) ![]u8 {
        return std.fmt.allocPrint(allocator, "SELECT ttl,expires_at-{d} FROM leases WHERE id={d}", .{ now, lease_id });
    }

    /// Every granted lease, for LeaseLeases.
    pub fn allLeaseIdsSql(allocator: Allocator) ![]u8 {
        return allocator.dupe(u8, "SELECT id FROM leases ORDER BY id");
    }

    pub fn revokeLeaseForIdSql(allocator: Allocator, lease_id: i64) ![]u8 {
        return std.fmt.allocPrint(allocator, "BEGIN; DELETE FROM lease_keys WHERE lease_id = {d}; DELETE FROM leases WHERE id = {d}; COMMIT;", .{ lease_id, lease_id });
    }

    /// Revoke a lease and every key attached to it as ONE revision.
    ///
    /// Deleting the keys one at a time advanced the revision once per key, so
    /// revoking a lease holding two keys moved the store two revisions where
    /// etcd moves one. A watch resuming across the revoke therefore saw the
    /// deletions split across revisions, and a client counting revisions to
    /// order its own writes saw a gap that was never a real write.
    pub fn revokeLeaseWithKeysSql(allocator: Allocator, lease_id: i64) ![]u8 {
        return std.fmt.allocPrint(allocator,
            "BEGIN;" ++
            "UPDATE revision SET current_revision=current_revision+1 WHERE id=1 AND EXISTS(SELECT 1 FROM kv WHERE lease={d});" ++
            "INSERT INTO kv_history(key,value,create_revision,mod_revision,version,lease,deleted) " ++
            "SELECT key,value,create_revision,(SELECT current_revision FROM revision WHERE id=1),version,lease,1 FROM kv WHERE lease={d};" ++
            "DELETE FROM kv WHERE lease={d};" ++
            "DELETE FROM lease_keys WHERE lease_id={d};" ++
            "DELETE FROM leases WHERE id={d};" ++
            "COMMIT;",
            .{ lease_id, lease_id, lease_id, lease_id, lease_id });
    }

    /// Expire one leased key: record the tombstone, drop the row and the lease
    /// association, all in a single rqlite transaction.
    ///
    /// The key is emitted as a hex BLOB literal because `kv.key` is a BLOB
    /// column. A single-quoted literal is TEXT and a raw NUL in the key makes
    /// the statement a syntax error, so a binary key would break expiry. The
    /// `kv` statements re-check the lease on the row: the caller picked the key
    /// out of `lease_keys`, and a concurrent Put may have re-attached it to
    /// another lease in between, in which case that key is not this lease's to
    /// delete. The association table is keyed on `lease_id`, not `lease`.
    pub fn expireLeaseKeySql(allocator: Allocator, key: []const u8, lease_id: i64) ![]u8 {
        var buf = std.ArrayList(u8).empty;
        errdefer buf.deinit(allocator);
        try buf.appendSlice(allocator, "BEGIN TRANSACTION;");
        // The revision advances first, and only while the row is still present
        // and still held by this lease. Advancing it afterwards and testing
        // EXISTS on `kv` could never match, because the delete had already run:
        // the tombstone was then written at a revision the store never
        // recorded, so responses and watch ranges lagged the data.
        // The EXISTS guard closes the subquery, so it must not use the
        // semicolon-terminated helper.
        try buf.appendSlice(allocator, "UPDATE revision SET current_revision=current_revision+1 WHERE id=1 AND EXISTS(SELECT 1 FROM kv WHERE key=");
        try appendBlob(allocator, &buf, key);
        try buf.appendSlice(allocator, " AND lease=");
        try formatInt(allocator, &buf, lease_id);
        try buf.appendSlice(allocator, ");");
        try appendLeasedKvOp(allocator, &buf, "INSERT INTO kv_history(key,value,create_revision,mod_revision,version,lease,deleted) SELECT key,value,create_revision,(SELECT current_revision FROM revision WHERE id=1),version,lease,1 FROM kv WHERE key=", key, lease_id);
        try appendLeasedKvOp(allocator, &buf, "DELETE FROM kv WHERE key=", key, lease_id);
        try buf.appendSlice(allocator, "DELETE FROM lease_keys WHERE key=");
        try appendBlob(allocator, &buf, key);
        try buf.appendSlice(allocator, " AND lease_id=");
        try formatInt(allocator, &buf, lease_id);
        try buf.appendSlice(allocator, ";COMMIT;");
        return buf.toOwnedSlice(allocator);
    }

    /// Fence an expiry worker that lost its owner deadline while waiting on
    /// rqlite. Every mutation rechecks ownership and the lease deadline in
    /// the same Raft transaction as the key deletion.
    pub fn expireLeaseKeyOwnedSql(allocator: Allocator, key: []const u8, lease_id: i64, owner: []const u8, now: i64) ![]u8 {
        const hex = try toHex(allocator, key);
        defer allocator.free(hex);
        const guard = try leaseOwnerGuardSql(allocator, lease_id, owner, now);
        defer allocator.free(guard);
        return std.fmt.allocPrint(allocator,
            "BEGIN;UPDATE revision SET current_revision=current_revision+1 WHERE id=1 AND EXISTS(SELECT 1 FROM kv WHERE key=X'{s}' AND lease={d}) AND {s};" ++
            "INSERT INTO kv_history(key,value,create_revision,mod_revision,version,lease,deleted) SELECT key,value,create_revision,(SELECT current_revision FROM revision WHERE id=1),version,lease,1 FROM kv WHERE key=X'{s}' AND lease={d} AND {s};" ++
            "DELETE FROM kv WHERE key=X'{s}' AND lease={d} AND {s};" ++
            "DELETE FROM lease_keys WHERE key=X'{s}' AND lease_id={d} AND {s};COMMIT;",
            .{ hex, lease_id, guard, hex, lease_id, guard, hex, lease_id, guard, hex, lease_id, guard });
    }

    pub fn revokeLeaseOwnedSql(allocator: Allocator, lease_id: i64, owner: []const u8, now: i64) ![]u8 {
        const guard = try leaseOwnerGuardSql(allocator, lease_id, owner, now);
        defer allocator.free(guard);
        return std.fmt.allocPrint(allocator, "BEGIN;DELETE FROM lease_keys WHERE lease_id={d} AND {s};DELETE FROM leases WHERE id={d} AND {s};COMMIT;", .{ lease_id, guard, lease_id, guard });
    }

    pub fn compareSql() []const u8 {
        return "SELECT key, value, create_revision, mod_revision, version, lease FROM kv WHERE key = ?";
    }
};

fn leaseOwnerGuardSql(allocator: Allocator, lease_id: i64, owner: []const u8, now: i64) ![]u8 {
    var buf = std.ArrayList(u8).empty;
    errdefer buf.deinit(allocator);
    try buf.appendSlice(allocator, "EXISTS(SELECT 1 FROM leases WHERE id=");
    try formatInt(allocator, &buf, lease_id);
    try buf.appendSlice(allocator, " AND owner='");
    try appendSqlString(allocator, &buf, owner);
    try buf.appendSlice(allocator, "' AND owner_expires_at>");
    try formatInt(allocator, &buf, now);
    try buf.appendSlice(allocator, " AND expires_at<=");
    try formatInt(allocator, &buf, now);
    try buf.appendSlice(allocator, ")");
    return buf.toOwnedSlice(allocator);
}

/// `stmt<key BLOB literal> AND lease=<id>;` — the lease guard keeps an expiry
/// from deleting a key that a concurrent Put has re-attached elsewhere.
fn appendLeasedKvOp(allocator: Allocator, buf: *std.ArrayList(u8), stmt: []const u8, key: []const u8, lease_id: i64) !void {
    try buf.appendSlice(allocator, stmt);
    try appendBlob(allocator, buf, key);
    try buf.appendSlice(allocator, " AND lease=");
    try formatInt(allocator, buf, lease_id);
    try buf.appendSlice(allocator, ";");
}

/// Append `value` as a single-quoted SQL string, doubling any embedded quote.
/// Used only for the instance identifier written into `leases.owner`, never
/// for key or value bytes: those are BLOB columns and go through
/// `appendBlob`, because a raw NUL in a TEXT literal is a syntax error.
fn appendSqlString(allocator: Allocator, buf: *std.ArrayList(u8), value: []const u8) !void {
    for (value) |byte| {
        if (byte == '\'') try buf.appendSlice(allocator, "''") else try buf.append(allocator, byte);
    }
}

// ─── Compare Operation ───────────────────────────────────────────────

pub const CompareOperation = struct {
    key: []const u8,
    target: CompareTarget,
    result: CompareResult,
    value: []const u8,
    target_value: i64 = 0,

    pub const CompareTarget = enum {
        version,
        create,
        mod,
        value,
        lease,
    };

    pub const CompareResult = enum {
        equal,
        greater,
        less,
        not_equal,
    };

    pub fn evaluate(self: CompareOperation, kv: ?MvccKv) bool {
        const stored = kv orelse {
            switch (self.target) {
                .version => return compareInt(0, self.result, 0),
                .create => return compareInt(0, self.result, 0),
                .mod => return compareInt(0, self.result, 0),
                .lease => return compareInt(0, self.result, self.target_value),
                .value => return self.result == .equal and self.value.len == 0,
            }
        };

        return switch (self.target) {
            .version => compareInt(stored.version, self.result, self.target_value),
            .create => compareInt(stored.create_revision, self.result, self.target_value),
            .mod => compareInt(stored.mod_revision, self.result, self.target_value),
            .lease => compareInt(stored.lease, self.result, self.target_value),
            .value => compareBytes(stored.value, self.result, self.value),
        };
    }
};

fn compareInt(stored: i64, result: CompareOperation.CompareResult, target: i64) bool {
    return switch (result) {
        .equal => stored == target,
        .greater => stored > target,
        .less => stored < target,
        .not_equal => stored != target,
    };
}

fn compareBytes(stored: []const u8, result: CompareOperation.CompareResult, target: []const u8) bool {
    return switch (result) {
        .equal => std.mem.eql(u8, stored, target),
        .greater => std.mem.order(u8, stored, target) == .gt,
        .less => std.mem.order(u8, stored, target) == .lt,
        .not_equal => !std.mem.eql(u8, stored, target),
    };
}

pub const MvccKv = struct {
    key: []const u8,
    value: []const u8,
    create_revision: i64,
    mod_revision: i64,
    version: i64,
    lease: i64,
};

// ─── Txn Builder ─────────────────────────────────────────────────────

pub const TxnBuilder = struct {
    pub fn buildTxnSql(allocator: Allocator, txn: TxnRequest) ![]u8 {
        var buf = std.ArrayList(u8).empty;
        errdefer buf.deinit(allocator);

        try buf.appendSlice(allocator, "BEGIN TRANSACTION;");
        try buf.appendSlice(allocator, "DROP TABLE IF EXISTS tandem_txn_result;CREATE TEMP TABLE tandem_txn_result (id INTEGER PRIMARY KEY, ok INTEGER NOT NULL, revision INTEGER NOT NULL DEFAULT 0, affected INTEGER NOT NULL DEFAULT 0, prev_key TEXT, prev_value BLOB, prev_create_revision INTEGER, prev_mod_revision INTEGER, prev_version INTEGER, prev_lease INTEGER);");
        // A Range inside a transaction has to be answered from the state that
        // transaction commits. KIP-29 §6 forbids assembling the response with a
        // second query: a concurrent commit could land in between and the
        // client would be shown a range that never existed alongside its own
        // write. Capturing the rows here keeps branch, revision and results in
        // one Raft transaction.
        try buf.appendSlice(allocator, "DROP TABLE IF EXISTS tandem_txn_range;CREATE TEMP TABLE tandem_txn_range (op_id INTEGER NOT NULL, seq INTEGER NOT NULL, key BLOB, value BLOB, create_revision INTEGER, mod_revision INTEGER, version INTEGER, lease INTEGER, cnt INTEGER, more INTEGER);");
        try buf.appendSlice(allocator, "INSERT INTO tandem_txn_result(id,ok,revision,affected) SELECT 0,");
        try appendPredicate(allocator, &buf, txn.compare);
        try buf.appendSlice(allocator, ",(SELECT current_revision FROM revision WHERE id=1),0;");
        try buf.appendSlice(allocator, "UPDATE revision SET current_revision=current_revision+1 WHERE id=1 AND (");
        try appendMutationPredicate(allocator, &buf, txn.success, true);
        try buf.appendSlice(allocator, " OR ");
        try appendMutationPredicate(allocator, &buf, txn.failure, false);
        try buf.appendSlice(allocator, ");UPDATE tandem_txn_result SET revision=(SELECT current_revision FROM revision WHERE id=1);");
        for (txn.success, 0..) |op, i| try appendOp(allocator, &buf, op, true, i + 1);
        for (txn.failure, 0..) |op, i| try appendOp(allocator, &buf, op, false, txn.success.len + i + 1);
        // Ranges are read after the branch's writes, matching etcd, which
        // evaluates the selected branch top to bottom.
        for (txn.success, 0..) |op, i| try appendRangeCapture(allocator, &buf, op, true, i + 1);
        for (txn.failure, 0..) |op, i| try appendRangeCapture(allocator, &buf, op, false, txn.success.len + i + 1);
        try buf.appendSlice(allocator, "SELECT id,ok,revision,affected,prev_key,prev_value,prev_create_revision,prev_mod_revision,prev_version,prev_lease FROM tandem_txn_result ORDER BY id;");
        try buf.appendSlice(allocator, "SELECT op_id,seq,key,value,create_revision,mod_revision,version,lease,cnt,more FROM tandem_txn_range ORDER BY op_id,seq;");
        try buf.appendSlice(allocator, "DROP TABLE tandem_txn_result;DROP TABLE tandem_txn_range;COMMIT;");

        return try buf.toOwnedSlice(allocator);
    }
};

/// Capture the rows a Range op inside a transaction will answer with. Only the
/// selected branch's rows are stored (`ok=<gate>`), and the count is taken
/// before LIMIT so `count` and `more` describe the same snapshot. The op's own
/// `limit` is not carried into the Txn op, so every matching row is captured
/// and the limit is reported through the captured count.
///
/// A Range that names a revision reads history, not the live table. etcd
/// answers such an op from the state as of that revision
/// (etcdserver/txn/range.go), and answering from `kv` instead returned the
/// current value for a revision the caller had already moved past — wrong
/// data, with nothing in the response to distinguish it from a correct read.
fn appendRangeCapture(allocator: Allocator, buf: *std.ArrayList(u8), op: TxnOp, success: bool, op_id: usize) !void {
    if (op.kind != .range) return;
    const gate: u8 = if (success) '1' else '0';
    const predicate = try SqlBuilder.keyPredicate(allocator, op.key, op.range_end);
    defer allocator.free(predicate);
    try buf.appendSlice(allocator, "INSERT INTO tandem_txn_range(op_id,seq,key,value,create_revision,mod_revision,version,lease,cnt,more) SELECT ");
    const prefix = try std.fmt.allocPrint(allocator, "{d},ROW_NUMBER() OVER (ORDER BY key)-1,", .{op_id});
    defer allocator.free(prefix);
    try buf.appendSlice(allocator, prefix);
    // The historical source mirrors rangeSql: the newest row per key at or
    // before the requested revision, excluding keys deleted by then.
    const source = if (op.revision > 0)
        try std.fmt.allocPrint(allocator, "(SELECT * FROM kv_history WHERE id IN (SELECT MAX(id) FROM kv_history WHERE mod_revision<={d} GROUP BY key) AND deleted=0)", .{op.revision})
    else
        try allocator.dupe(u8, "kv");
    defer allocator.free(source);
    try buf.appendSlice(allocator, "key,value,create_revision,mod_revision,version,lease,COUNT(*) OVER(),0 FROM ");
    try buf.appendSlice(allocator, source);
    try buf.appendSlice(allocator, " WHERE ");
    try buf.appendSlice(allocator, predicate);
    try buf.appendSlice(allocator, " AND (SELECT ok FROM tandem_txn_result WHERE id=0)=");
    try buf.append(allocator, gate);
    try buf.appendSlice(allocator, " ORDER BY key;");
}

fn appendMutationPredicate(allocator: Allocator, buf: *std.ArrayList(u8), ops: []const TxnOp, success: bool) !void {
    try buf.appendSlice(allocator, if (success) "((SELECT ok FROM tandem_txn_result WHERE id=0)=1 AND (0" else "((SELECT ok FROM tandem_txn_result WHERE id=0)=0 AND (0");
    for (ops) |op| switch (op.kind) {
        .put => try buf.appendSlice(allocator, " OR 1"),
        .delete => {
            const predicate = try SqlBuilder.keyPredicate(allocator, op.key, op.range_end);
            defer allocator.free(predicate);
            try buf.appendSlice(allocator, " OR EXISTS(SELECT 1 FROM kv WHERE ");
            try buf.appendSlice(allocator, predicate);
            try buf.appendSlice(allocator, ")");
        },
        .range => {},
    };
    try buf.appendSlice(allocator, "))");
}

fn appendPredicate(allocator: Allocator, buf: *std.ArrayList(u8), compares: []const CompareOperation) !void {
    if (compares.len == 0) return buf.appendSlice(allocator, "1");
    try buf.appendSlice(allocator, "CASE WHEN ");
    for (compares, 0..) |compare, i| {
        if (i > 0) try buf.appendSlice(allocator, " AND ");
        try buf.appendSlice(allocator, "COALESCE((SELECT ");
        try buf.appendSlice(allocator, switch (compare.target) {
            .version => "version",
            .create => "create_revision",
            .mod => "mod_revision",
            .value => "value",
            .lease => "lease",
        });
        try buf.appendSlice(allocator, " FROM kv WHERE key=");
        try appendQuoted(allocator, buf, compare.key);
        try buf.appendSlice(allocator, if (compare.target == .value) "),X'') " else "),0) ");
        try buf.appendSlice(allocator, switch (compare.result) {
            .equal => "=",
            .greater => ">",
            .less => "<",
            .not_equal => "<>",
        });
        if (compare.target == .value) try appendBlob(allocator, buf, compare.value) else try formatInt(allocator, buf, compare.target_value);
    }
    try buf.appendSlice(allocator, " THEN 1 ELSE 0 END");
}

fn appendOp(allocator: Allocator, buf: *std.ArrayList(u8), op: TxnOp, success: bool, op_id: usize) !void {
    const gate = if (success) "1" else "0";
    const predicate = try SqlBuilder.keyPredicate(allocator, op.key, op.range_end);
    defer allocator.free(predicate);
    const prefix = try std.fmt.allocPrint(allocator, "INSERT INTO tandem_txn_result(id,ok,revision) SELECT {d},ok,revision FROM tandem_txn_result WHERE id=0 AND ok={s};", .{ op_id, gate });
    defer allocator.free(prefix);
    try buf.appendSlice(allocator, prefix);
    const selected = try std.fmt.allocPrint(allocator, " AND (SELECT ok FROM tandem_txn_result WHERE id=0)={s}", .{gate});
    defer allocator.free(selected);
    switch (op.kind) {
        .put => {
            const previous = try std.fmt.allocPrint(allocator, "UPDATE tandem_txn_result SET (prev_key,prev_value,prev_create_revision,prev_mod_revision,prev_version,prev_lease)=(SELECT key,value,create_revision,mod_revision,version,lease FROM kv WHERE {s}) WHERE id={d};", .{ predicate, op_id });
            defer allocator.free(previous);
            try buf.appendSlice(allocator, previous);
            try buf.appendSlice(allocator, "INSERT INTO kv(key,value,create_revision,mod_revision,version,lease) SELECT ");
            try appendQuoted(allocator, buf, op.key);
            try buf.appendSlice(allocator, ",");
            try appendBlob(allocator, buf, op.value);
            try buf.appendSlice(allocator, ",current_revision,current_revision,1,");
            try formatInt(allocator, buf, op.lease);
            try buf.appendSlice(allocator, " FROM revision WHERE id=1");
            try buf.appendSlice(allocator, selected);
            try buf.appendSlice(allocator, " ON CONFLICT(key) DO UPDATE SET value=excluded.value,mod_revision=excluded.mod_revision,version=kv.version+1,lease=excluded.lease;");
            const history = try std.fmt.allocPrint(allocator, "INSERT INTO kv_history(key,value,create_revision,mod_revision,version,lease,deleted) SELECT key,value,create_revision,mod_revision,version,lease,0 FROM kv WHERE {s}{s};DELETE FROM lease_keys WHERE {s}{s};INSERT INTO lease_keys(lease_id,key) SELECT lease,key FROM kv WHERE {s} AND lease!=0{s};", .{ predicate, selected, predicate, selected, predicate, selected });
            defer allocator.free(history);
            try buf.appendSlice(allocator, history);
        },
        .delete => {
            const deletion = try std.fmt.allocPrint(allocator, "UPDATE tandem_txn_result SET affected=(SELECT COUNT(*) FROM kv WHERE {s}) WHERE id={d};INSERT INTO kv_history(key,value,create_revision,mod_revision,version,lease,deleted) SELECT key,value,create_revision,(SELECT current_revision FROM revision WHERE id=1),version,lease,1 FROM kv WHERE {s}{s};DELETE FROM lease_keys WHERE {s}{s};DELETE FROM kv WHERE {s}{s};", .{ predicate, op_id, predicate, selected, predicate, selected, predicate, selected });
            defer allocator.free(deletion);
            try buf.appendSlice(allocator, deletion);
        },
        .range => {},
    }
}

fn appendBlob(allocator: Allocator, buf: *std.ArrayList(u8), value: []const u8) !void {
    const hex = try toHex(allocator, value);
    defer allocator.free(hex);
    try buf.appendSlice(allocator, "X'");
    try buf.appendSlice(allocator, hex);
    try buf.appendSlice(allocator, "'");
}

/// Appends `value` as a SQLite blob literal.
///
/// Keys used to be written as `CAST(X'..' AS TEXT)`. rqlite marshals a TEXT
/// column with Go's json.Marshal, which replaces every byte that is not valid
/// UTF-8 with U+FFFD, so a key such as ff 41 22 came back as ef bf bd 41 22 and
/// a different key. Comparing and storing the blob directly is what
/// KIP-29 §5 requires, and it round-trips every byte.
fn appendQuoted(allocator: Allocator, buf: *std.ArrayList(u8), value: []const u8) !void {
    try appendBlob(allocator, buf, value);
}

pub const TxnRequest = struct {
    compare: []const CompareOperation,
    success: []const TxnOp,
    failure: []const TxnOp,
};

pub const TxnOp = struct {
    kind: Kind,
    key: []const u8,
    value: []const u8,
    range_end: []const u8,
    lease: i64,
    /// The revision a Range op asked for, or 0 for the current state. etcd
    /// answers a historical read inside a Txn from history, so dropping this
    /// made the op return current data for a revision the caller named.
    revision: i64 = 0,

    pub const Kind = enum {
        range,
        put,
        delete,
    };
};

// ─── Utility Functions ───────────────────────────────────────────────

fn formatInt(allocator: Allocator, buf: *std.ArrayList(u8), value: i64) !void {
    var tmp: [32]u8 = undefined;
    const s = std.fmt.bufPrint(&tmp, "{d}", .{value}) catch unreachable;
    try buf.appendSlice(allocator, s);
}

fn toHex(allocator: Allocator, data: []const u8) ![]u8 {
    var buf = std.ArrayList(u8).empty;
    errdefer buf.deinit(allocator);
    for (data) |b| {
        const hex = try std.fmt.allocPrint(allocator, "{x:0>2}", .{b});
        defer allocator.free(hex);
        try buf.appendSlice(allocator, hex);
    }
    return try buf.toOwnedSlice(allocator);
}

// ─── Tests ───────────────────────────────────────────────────────────

const testing = std.testing;

test "rqlite client init" {
    var client = try RqliteClient.init(testing.allocator, "http://127.0.0.1:4001");
    defer client.deinit();
}

test "sql builder schema" {
    const sql = SqlBuilder.schemaSql();
    try testing.expect(sql.len > 0);
    try testing.expect(std.mem.indexOf(u8, sql, "CREATE TABLE") != null);
    try testing.expect(std.mem.indexOf(u8, sql, "kv") != null);
    try testing.expect(std.mem.indexOf(u8, sql, "revision") != null);
}

test "sql builder range" {
    const sql = try SqlBuilder.rangeSql(testing.allocator, "a", "z", 100, 0, false, false);
    defer testing.allocator.free(sql);
    try testing.expect(std.mem.indexOf(u8, sql, "SELECT") != null);
    try testing.expect(std.mem.indexOf(u8, sql, "FROM kv") != null);
    try testing.expect(std.mem.indexOf(u8, sql, "ORDER BY key") != null);
    try testing.expect(std.mem.indexOf(u8, sql, "LIMIT 100") != null);
}

test "sql builder range with revision" {
    const sql = try SqlBuilder.rangeSql(testing.allocator, "a", "", 0, 42, false, false);
    defer testing.allocator.free(sql);
    try testing.expect(std.mem.indexOf(u8, sql, "mod_revision<=42") != null);
}

test "sql builder range count only" {
    const sql = try SqlBuilder.rangeSql(testing.allocator, "a", "", 0, 0, false, true);
    defer testing.allocator.free(sql);
    try testing.expect(std.mem.indexOf(u8, sql, "COUNT(*)") != null);
}

test "sql builder range keys only" {
    const sql = try SqlBuilder.rangeSql(testing.allocator, "a", "", 0, 0, true, false);
    defer testing.allocator.free(sql);
    try testing.expect(std.mem.indexOf(u8, sql, "key,'' AS value") != null);
}

test "sql builder put" {
    const sql = try SqlBuilder.putSql(testing.allocator, "mykey", "myvalue", 0);
    defer testing.allocator.free(sql);
    try testing.expect(std.mem.indexOf(u8, sql, "UPDATE revision") != null);
    try testing.expect(std.mem.indexOf(u8, sql, "UPDATE revision") != null);
    try testing.expect(std.mem.indexOf(u8, sql, "ON CONFLICT(key) DO UPDATE") != null);
    try testing.expect(std.mem.indexOf(u8, sql, "INSERT INTO kv_history") != null);
    try testing.expect(std.mem.indexOf(u8, sql, "INSERT INTO kv_history") != null);
}

test "sql builder delete range" {
    const sql = try SqlBuilder.deleteRangeSql(testing.allocator, "mykey", "mykez");
    defer testing.allocator.free(sql);
    try testing.expect(std.mem.indexOf(u8, sql, "UPDATE revision") != null);
    try testing.expect(std.mem.indexOf(u8, sql, "DELETE FROM kv") != null);
    try testing.expect(std.mem.indexOf(u8, sql, "DELETE FROM lease_keys") != null);
}

test "a transaction captures its range rows in the same request" {
    // KIP-29 §6: the response for a Range inside a Txn has to come from the
    // transaction that produced it. Answering it with a later query would let
    // a concurrent commit land in between, so the rows are captured into a
    // temp table and read out of the same /db/request response.
    const sql = try TxnBuilder.buildTxnSql(testing.allocator, .{
        .compare = &.{.{ .key = "a", .target = .version, .result = .equal, .target_value = 0, .value = "" }},
        .success = &.{
            .{ .kind = .put, .key = "a", .value = "v", .range_end = "", .lease = 0 },
            .{ .kind = .range, .key = "p/", .value = "", .range_end = "p0", .lease = 0 },
        },
        .failure = &.{.{ .kind = .range, .key = "q/", .value = "", .range_end = "q0", .lease = 0 }},
    });
    defer testing.allocator.free(sql);
    // Both result sets are selected before either temp table is dropped.
    const op_rows = std.mem.indexOf(u8, sql, "SELECT id,ok,revision,affected").?;
    const range_rows = std.mem.indexOf(u8, sql, "SELECT op_id,seq,key,value").?;
    const drop = std.mem.indexOf(u8, sql, "DROP TABLE tandem_txn_result;").?;
    try testing.expect(op_rows < range_rows);
    try testing.expect(range_rows < drop);
    // A Range is captured only when its own branch is the one selected, so the
    // failure branch's rows stay empty when the comparison succeeds.
    try testing.expect(std.mem.indexOf(u8, sql, "COUNT(*) OVER()") != null);
    try testing.expect(std.mem.indexOf(u8, sql, "ROW_NUMBER() OVER (ORDER BY key)-1") != null);
}

test "lease SQL carries binary keys as blob literals" {
    // A key holding a NUL and a 0xFF cannot be written as a single-quoted
    // literal: SQLite truncates at the NUL, so a binary key either failed with
    // a syntax error or was silently stored short and then never matched again.
    const key = "leased/key\x00\xff";
    const attach = try SqlBuilder.attachLeaseKeyForSql(testing.allocator, 7, key);
    defer testing.allocator.free(attach);
    try testing.expectEqualStrings("INSERT OR IGNORE INTO lease_keys(lease_id,key) VALUES(7,X'6c65617365642f6b657900ff')", attach);

    const expire = try SqlBuilder.expireLeaseKeySql(testing.allocator, key, 7);
    defer testing.allocator.free(expire);
    try testing.expectEqualStrings("6c65617365642f6b657900ff", try hexAfter(expire, "key=X'"));
    // lease_keys is keyed on lease_id, while kv is keyed on lease.
    try testing.expect(std.mem.indexOf(u8, expire, "FROM lease_keys WHERE key=X'6c65617365642f6b657900ff' AND lease_id=7") != null);
    // The revision has to advance before the delete, otherwise the EXISTS
    // guard can never match and the tombstone is stored at a revision the
    // store never recorded.
    const bump = std.mem.indexOf(u8, expire, "UPDATE revision").?;
    const tombstone = std.mem.indexOf(u8, expire, "INSERT INTO kv_history").?;
    const removal = std.mem.indexOf(u8, expire, "DELETE FROM kv").?;
    try testing.expect(bump < tombstone);
    try testing.expect(tombstone < removal);
    // Every statement must be terminated, including the one that closes the
    // EXISTS subquery, or the batch fails with "incomplete input".
    try testing.expect(std.mem.indexOf(u8, expire, "AND lease=7);") != null);
}

test "lease deadlines are supplied by the caller, not by SQL now" {
    // rqlite rewrites `now` in a write statement to a Raft-assigned instant
    // so every replica applies the same write. On a UTC+8 node that instant is
    // eight hours ahead of the host clock, so a deadline compared against a
    // read-path strftime never looks expired and a lapsed lease holds its key
    // forever. The timestamp therefore travels with the statement.
    const grant = try SqlBuilder.createLeaseForSql(testing.allocator, 7, 30, 1_700_000_000);
    defer testing.allocator.free(grant);
    try testing.expect(std.mem.indexOf(u8, grant, "INSERT INTO leases(id,ttl,created_at,expires_at) VALUES(7,30,1700000000,1700000030)") != null);
    try testing.expect(std.mem.indexOf(u8, grant, "UPDATE lease_sequence SET last_id=MAX(last_id,7)") != null);
    try testing.expect(std.mem.indexOf(u8, grant, "strftime") == null);

    const keepalive = try SqlBuilder.keepAliveLeaseForSql(testing.allocator, 7, 1_700_000_000);
    defer testing.allocator.free(keepalive);
    try testing.expectEqualStrings("UPDATE leases SET expires_at=1700000000+ttl WHERE id=7", keepalive);
    try testing.expect(std.mem.indexOf(u8, keepalive, "strftime") == null);

    const expired = try SqlBuilder.expiredLeasesSql(testing.allocator, 1_700_000_000);
    defer testing.allocator.free(expired);
    try testing.expectEqualStrings("SELECT id FROM leases WHERE expires_at <= 1700000000", expired);
    try testing.expect(std.mem.indexOf(u8, expired, "strftime") == null);
}

test "lease IDs and expiry ownership are serialized and recoverable" {
    const allocate = try SqlBuilder.allocateLeaseSql(testing.allocator, 20, 100);
    defer testing.allocator.free(allocate);
    try testing.expect(std.mem.indexOf(u8, allocate, "UPDATE lease_sequence SET last_id=last_id+1") != null);
    try testing.expect(std.mem.indexOf(u8, allocate, "INSERT INTO leases(id,ttl,created_at,expires_at) SELECT last_id,20,100,120") != null);
    const claim = try SqlBuilder.claimLeaseForSql(testing.allocator, 7, "node'1", 120);
    defer testing.allocator.free(claim);
    try testing.expect(std.mem.indexOf(u8, claim, "owner='node''1'") != null);
    try testing.expect(std.mem.indexOf(u8, claim, "owner_expires_at=150") != null);
    try testing.expect(std.mem.indexOf(u8, claim, "owner_expires_at<=120") != null);
    const expired = try SqlBuilder.expireLeaseKeyOwnedSql(testing.allocator, "k\x00", 7, "node'1", 121);
    defer testing.allocator.free(expired);
    try testing.expect(std.mem.indexOf(u8, expired, "key=X'6b00'") != null);
    try testing.expect(std.mem.indexOf(u8, expired, "owner='node''1' AND owner_expires_at>121 AND expires_at<=121") != null);
}

test "standalone delete captures previous rows before mutation" {
    const sql = try SqlBuilder.atomicDeleteRangeSql(testing.allocator, "a", "z");
    defer testing.allocator.free(sql);
    const capture = std.mem.indexOf(u8, sql, "CREATE TEMP TABLE tandem_delete_prev AS SELECT").?;
    const history = std.mem.indexOf(u8, sql, "INSERT INTO kv_history").?;
    const removal = std.mem.indexOf(u8, sql, "DELETE FROM kv WHERE").?;
    try testing.expect(capture < history);
    try testing.expect(history < removal);
    try testing.expect(std.mem.indexOf(u8, sql, "SELECT key,value,create_revision,mod_revision,version,lease FROM tandem_delete_prev ORDER BY key") != null);
}

/// The hex digits following the first `prefix`, used to assert on how a key was
/// encoded without re-implementing the encoder.
fn hexAfter(sql: []const u8, prefix: []const u8) ![]const u8 {
    const start = (std.mem.indexOf(u8, sql, prefix) orelse return error.MissingPrefix) + prefix.len;
    const rest = sql[start..];
    const end = std.mem.indexOfScalar(u8, rest, '\'') orelse return error.UnterminatedLiteral;
    return rest[0..end];
}

test "compare operation evaluate - key exists" {
    const kv = MvccKv{
        .key = "test",
        .value = "hello",
        .create_revision = 1,
        .mod_revision = 3,
        .version = 3,
        .lease = 0,
    };

    const cmp = CompareOperation{
        .key = "test",
        .target = .version,
        .result = .equal,
        .value = &.{},
    };

    try testing.expect(cmp.evaluate(kv) == false);
}

test "compare operation evaluate - key doesn't exist" {
    const cmp = CompareOperation{
        .key = "missing",
        .target = .version,
        .result = .equal,
        .value = &.{},
    };

    try testing.expect(cmp.evaluate(null) == true);
}

test "compare operation evaluate - value equal" {
    const kv = MvccKv{
        .key = "test",
        .value = "hello",
        .create_revision = 1,
        .mod_revision = 1,
        .version = 1,
        .lease = 0,
    };

    const cmp = CompareOperation{
        .key = "test",
        .target = .value,
        .result = .equal,
        .value = "hello",
    };

    try testing.expect(cmp.evaluate(kv) == true);
}

test "compare operation evaluate - value not equal" {
    const kv = MvccKv{
        .key = "test",
        .value = "hello",
        .create_revision = 1,
        .mod_revision = 1,
        .version = 1,
        .lease = 0,
    };

    const cmp = CompareOperation{
        .key = "test",
        .target = .value,
        .result = .not_equal,
        .value = "world",
    };

    try testing.expect(cmp.evaluate(kv) == true);
}

test "formatInt utility" {
    var buf = std.ArrayList(u8).empty;
    defer buf.deinit(testing.allocator);

    try formatInt(testing.allocator, &buf, 42);
    try testing.expectEqualStrings("42", buf.items);

    buf.clearRetainingCapacity();
    try formatInt(testing.allocator, &buf, -1);
    try testing.expectEqualStrings("-1", buf.items);
}

test "txn builder emits guarded atomic SQL" {
    const sql = try TxnBuilder.buildTxnSql(testing.allocator, .{
        .compare = &[_]CompareOperation{.{
            .key = "ready",
            .target = .version,
            .result = .equal,
            .value = &.{},
            .target_value = 1,
        }},
        .success = &[_]TxnOp{.{ .kind = .put, .key = "done", .value = "yes", .range_end = "", .lease = 0 }},
        .failure = &[_]TxnOp{.{ .kind = .delete, .key = "done", .value = "", .range_end = "", .lease = 0 }},
    });
    defer testing.allocator.free(sql);
    try testing.expect(std.mem.indexOf(u8, sql, "BEGIN TRANSACTION;") != null);
    try testing.expect(std.mem.indexOf(u8, sql, "tandem_txn_result") != null);
    try testing.expect(std.mem.indexOf(u8, sql, "COMMIT;") != null);
    try testing.expect(std.mem.indexOf(u8, sql, "X'646f6e65'") != null);
}

test "a lease compare evaluates the stored lease, not the version" {
    // Mapping CompareTarget.LEASE onto .version made a lease compare answer
    // with a confident but wrong boolean. Pin each target against a row whose
    // version and lease deliberately differ.
    const stored = MvccKv{ .key = "k", .value = "v", .create_revision = 1, .mod_revision = 2, .version = 3, .lease = 77 };
    const base = CompareOperation{ .key = "k", .target = .version, .result = .equal, .value = "" };

    var lease_compare = base;
    lease_compare.target = .lease;
    lease_compare.target_value = 77;
    try std.testing.expect(lease_compare.evaluate(stored));

    lease_compare.target_value = 3; // the version, which must not satisfy a lease compare
    try std.testing.expect(!lease_compare.evaluate(stored));

    var version_compare = base;
    version_compare.target = .version;
    version_compare.target_value = 3;
    try std.testing.expect(version_compare.evaluate(stored));
}

test "a binary key is emitted as a blob literal, not a lossy TEXT cast" {
    // rqlite marshals TEXT with Go's json.Marshal, so CAST(X'ff..' AS TEXT)
    // comes back with invalid UTF-8 replaced by U+FFFD. The predicate builder
    // must emit a blob literal for the key to round-trip.
    var buf = std.ArrayList(u8).empty;
    defer buf.deinit(testing.allocator);
    try appendQuoted(testing.allocator, &buf, &.{ 0xff, 0x41, 0x22 });
    try testing.expectEqualStrings("X'ff4122'", buf.items);

    var predicate = std.ArrayList(u8).empty;
    defer predicate.deinit(testing.allocator);
    try predicate.appendSlice(testing.allocator, "key=");
    try appendQuoted(testing.allocator, &predicate, &.{ 0x00, 0x80 });
    try testing.expectEqualStrings("key=X'0080'", predicate.items);
}

/// Whether a response body is rqlite's transient "no leader yet" answer.
///
/// rqlite returns HTTP 200 with `{"error":"leader not found"}` while a node is
/// still bootstrapping or holding an election. A linearizable read cannot be
/// served in that window, so the caller waits rather than failing startup.
fn isLeadershipPending(body: []const u8) bool {
    return std.mem.indexOf(u8, body, "leader not found") != null or
        std.mem.indexOf(u8, body, "no leader") != null;
}

test "the rqlite status parser reads nested leader and raft fields" {
    // rqlite nests `leader` and `raft` inside `store`, and both carry keys
    // that also appear at the top level. A textual search for the first
    // `"node_id":"` or `"term":` in the document returns whichever one comes
    // first in the JSON, not the one that was asked for, so this exercises the
    // nesting rather than a flat fixture.
    const body =
        \\{"build":{"version":"10.3.6"},
        \\ "store":{"ready":true,"node_id":"n2",
        \\   "leader":{"addr":"10.0.0.1:4002","node_id":"n1"},
        \\   "raft":{"term":7,"applied_index":4211,"commit_index":4211},
        \\   "fsm_index":4200,"db_applied_index":4200}}
    ;
    const status = try parseStatusResponse(testing.allocator, body);
    defer testing.allocator.free(status.leader_address);
    defer testing.allocator.free(status.leader_id);
    defer testing.allocator.free(status.node_id);
    defer testing.allocator.free(status.version);

    // The leader is n1, not this node's own id.
    try testing.expectEqualStrings("n1", status.leader_id);
    try testing.expectEqualStrings("10.0.0.1:4002", status.leader_address);
    try testing.expectEqualStrings("n2", status.node_id);
    try testing.expectEqual(@as(i64, 4211), status.raft_applied_index);
    try testing.expectEqual(@as(i64, 7), status.raft_term);
    try testing.expectEqualStrings("10.3.6", status.version);
}

test "a status without a leader reports none rather than a fabricated one" {
    // A node that has not elected itself still answers /status, and the
    // caller reports "no leader" instead of the constant 1 this used to send.
    const body = \\{"store":{"ready":false,"node_id":"n1","leader":{},"raft":{"term":0,"applied_index":0}}}
    ;
    const status = try parseStatusResponse(testing.allocator, body);
    defer testing.allocator.free(status.leader_address);
    defer testing.allocator.free(status.leader_id);
    defer testing.allocator.free(status.node_id);
    defer testing.allocator.free(status.version);

    try testing.expectEqualStrings("", status.leader_id);
    try testing.expectEqualStrings("", status.leader_address);
    try testing.expectEqual(@as(i64, 0), status.raft_applied_index);
}
