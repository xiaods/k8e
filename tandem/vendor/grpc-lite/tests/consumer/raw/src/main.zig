const std = @import("std");
const grpc = @import("grpc_lite");
const options = @import("consumer_options");

pub fn main() !void {
    _ = grpc.version;
    _ = grpc.Channel;
    _ = grpc.ClientTlsOptions;
    _ = grpc.Server;
    _ = grpc.ServerTlsOptions;
    if (options.tls) {
        if (grpc.Server.init(std.heap.page_allocator, .{
            .tls = .{
                .certificate_chain_pem = "invalid",
                .private_key_pem = "invalid",
            },
        })) |server_value| {
            var server = server_value;
            server.deinit();
            return error.InvalidCertificateAccepted;
        } else |err| {
            if (err != error.InvalidCertificate) return err;
        }
    }
}
