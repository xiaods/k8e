pub const api = @cImport({
    @cDefine("MBEDTLS_USER_CONFIG_FILE", "\"mbedtls_user_config.h\"");
    @cInclude("psa/crypto.h");
    @cInclude("mbedtls/ctr_drbg.h");
    @cInclude("mbedtls/entropy.h");
    @cInclude("mbedtls/error.h");
    @cInclude("mbedtls/pk.h");
    @cInclude("mbedtls/ssl.h");
    @cInclude("mbedtls/x509_crt.h");
});
