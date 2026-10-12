const build_options = @import("grpc_lite_options");

pub const string = build_options.version;
pub const user_agent = "grpc-lite/" ++ string;

test "version values share the package source" {
    const std = @import("std");
    _ = try std.SemanticVersion.parse(string);
    try std.testing.expectEqualStrings("grpc-lite/" ++ string, user_agent);
}
