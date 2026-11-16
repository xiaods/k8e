package grpc

import "testing"

func TestSanitizePipPackage(t *testing.T) {
	valid := []string{
		"requests",
		"requests>=2.28",
		"requests[security]>=2.28",
		"requests>=2.0,<3.0",
		"my-pkg_name",
	}
	for _, p := range valid {
		if _, err := sanitizePipPackage(p); err != nil {
			t.Errorf("sanitizePipPackage(%q) = %v, want nil", p, err)
		}
	}

	invalid := []string{
		"",
		"requests; rm -rf /",
		"requests && id",
		"requests | cat",
		"requests`id`",
		"requests$(id)",
		"requests foo",           // whitespace
		"requests'quote",         // quote
		"requests/../etc/passwd", // path traversal
		"-evil",                  // option injection
		"--index-url=http://x",   // option injection
		"requests\nid",           // newline
	}
	for _, p := range invalid {
		if _, err := sanitizePipPackage(p); err == nil {
			t.Errorf("sanitizePipPackage(%q) = nil, want an error", p)
		}
	}
}

func TestShellSingleQuote(t *testing.T) {
	if got := shellSingleQuote("requests>=2.0"); got != "'requests>=2.0'" {
		t.Fatalf("shellSingleQuote = %q, want 'requests>=2.0'", got)
	}
	// Embedded single quotes are escaped for /bin/sh.
	if got := shellSingleQuote("a'b"); got != `'a'\''b'` {
		t.Fatalf("shellSingleQuote = %q, want 'a'\\''b'", got)
	}
}
