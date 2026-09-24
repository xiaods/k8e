pub const api = @cImport({
    @cDefine("CARES_STATICLIB", "1");
    @cInclude("ares.h");
});
